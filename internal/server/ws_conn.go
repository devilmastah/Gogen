package server

import (
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	errWSClosed = errors.New("websocket closed")
	// errWSQueueFull is returned when a client's outbound backlog exceeds
	// wsMaxQueuedBytes (or the debug depth knob is exhausted): the caller
	// treats it as a dead/slow socket and detaches it.
	errWSQueueFull = errors.New("websocket send queue full")
)

// wsOut is one queued outbound message: the PREPARED wire frame plus its
// encoded size, which is what backpressure is accounted in.
//
// It is prepared ONCE per message and shared by every client a broadcast
// targets, so json.Marshal and the permessage-deflate pass happen once instead
// of once per socket — gorilla caches the compressed frame per connection
// option set (PreparedMessage.frame is keyed by isServer/compress/level), so a
// broadcast to K tabs costs one deflate. Before this, every client re-marshaled
// and re-deflated the same payload, which for a multi-MB session history was
// the dominant cost of a fan-out.
type wsOut struct {
	frame *websocket.PreparedMessage
	size  int
	typ   string // WSMessage.Type, for the write-error log line
	// msg is retained only for connections built with captureMsg (tests read
	// the queued message off the fake connection, which has no writer
	// goroutine). Production connections keep no second copy of the payload.
	msg *WSMessage
}

// prepareOut marshals and prepares one message for the wire.
func prepareOut(v *WSMessage) (*wsOut, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	frame, err := websocket.NewPreparedMessage(websocket.TextMessage, data)
	if err != nil {
		return nil, err
	}
	return &wsOut{frame: frame, size: len(data), typ: v.Type}, nil
}

type wsConn struct {
	conn *websocket.Conn

	// sendQ carries outbound frames to the writeLoop goroutine.
	//
	// It carries POINTERS to prepared frames (wsOut), not WSMessage values: a
	// buffered channel allocates its whole ring eagerly at make, and WSMessage
	// is a large struct (~2.1 KB). A value channel at wsSendQueueSize would
	// reserve ~8.3 MB of *empty* buffer per connection before a single frame is
	// sent. With pointers the same depth costs ~32 KB idle, and the real
	// backlog is bounded by wsMaxQueuedBytes rather than by the depth.
	sendQ chan *wsOut
	quit  chan struct{} // closed by closeSend to stop writers + writeLoop
	done  chan struct{} // closed when writeLoop exits, so writeJSON fails fast
	once  sync.Once

	// queuedBytes is the encoded size of everything waiting in sendQ (the
	// frame being written is subtracted when it leaves). Backpressure is
	// expressed in BYTES because a frame count cannot bound memory: 4096 tiny
	// token frames are under 1 MB, while 4096 6 KB terminal-output frames are
	// ~25 MB per socket (and the connection limiter allows 32).
	queuedBytes atomic.Int64
	// captureMsg retains the WSMessage in each queued frame. Test-only seam:
	// fake connections have no writer goroutine, so their helpers read the
	// message off sendQ. Production connections leave it false.
	captureMsg bool
}

const (
	// wsQuitDrainWait bounds the teardown flush: frames already queued when the
	// connection closes get this long to reach the socket before it is closed.
	wsQuitDrainWait = 250 * time.Millisecond
	wsSendQueueSize = 4096
	// wsMaxQueuedBytes bounds one connection's outbound backlog. 8 MB is a few
	// seconds of a slow-but-alive client; past it the socket is detached (the
	// browser reconnects and re-syncs) instead of letting one stalled tab pin
	// hundreds of megabytes.
	wsMaxQueuedBytes = 8 << 20
	// wsEnqueueWait bounds how long a producer waits for queue room once the
	// byte budget allows it (a full channel at this size only happens with
	// many tiny frames or a small debug depth).
	wsEnqueueWait     = 2 * time.Second
	wsPingInterval    = 30 * time.Second
	wsWriteTimeout    = 30 * time.Second
	wsReadTimeout     = 60 * time.Second
	wsTurnAcquireWait = 150 * time.Millisecond
	// UI cancel: wait briefly for StreamProcessInput to finish cancel repair.
	wsStreamDrainWait = 2 * time.Second
)

// drainStreamErr waits for the stream goroutine to signal exit.
// Returns true if the signal arrived, false on timeout (caller should keep ch).
func drainStreamErr(ch chan error) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(wsStreamDrainWait):
		log.Printf("warning: timed out waiting for stream goroutine to exit")
		return false
	}
}

func newWSConn(conn *websocket.Conn) *wsConn {
	qsize := wsSendQueueSize
	if n := wsDebugSendQueueSize(); n > 0 {
		qsize = n // GOGEN_WS_SENDQ_SIZE (debug build only)
	}
	w := &wsConn{
		conn:  conn,
		sendQ: make(chan *wsOut, qsize),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go w.writeLoop()
	return w
}

func (w *wsConn) writeLoop() {
	// Closing the conn on exit is critical: it tears down the read loop (so
	// HandleWS cleans up) AND makes the browser fire onclose so it reconnects.
	// Without this, a single transient write error kills the writer silently
	// while the LLM keeps "sending" into a dead queue and the UI freezes.
	defer w.conn.Close()
	defer close(w.done)
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	// Debug-only backpressure (tmp/live_stall_detach.js, debug builds):
	// stall.delay sleeps before each data write to simulate a client that
	// cannot drain its socket. The write deadline is set BEFORE the sleep,
	// so a stall ≥ wsWriteTimeout trips it and the writer dies (socket drop
	// → browser reconnects), while a smaller stall merely makes the writer
	// lag the stream and the send queue hits wsMaxQueuedBytes (detach). Compiled out of production builds (ws_conn_release.go).
	stall := newWSStallState()
	for {
		select {
		case <-w.quit:
			// Flush what is already queued before the socket closes: teardown
			// used to cut the writer off right here, so the last frames — often
			// the error that explains the teardown ("no session available") or
			// a turn's final state — were silently dropped.
			w.drainQueued()
			return
		case out := <-w.sendQ:
			if err := w.writeOut(out, stall); err != nil {
				return
			}
		case <-ticker.C:
			// Pings detect half-open connections (e.g. NAT/proxy idle
			// timeouts, backgrounded tabs) that pass write deadlines but
			// never reach the browser. A failed ping kills the writer,
			// triggering teardown + reconnect via the deferred Close.
			if err := w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				log.Printf("websocket set write deadline: %v", err)
				return
			}
			if err := w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}

func (w *wsConn) closeSend() {
	w.once.Do(func() {
		// Signal quit instead of closing sendQ so concurrent writeJSON
		// sends cannot panic on a closed channel.
		close(w.quit)
	})
}

// drainQueued writes the frames already in the queue, bounded by
// wsQuitDrainWait so a peer that stopped reading cannot hold teardown open.
// Called by the write loop on quit (see closeSend / upgradeWS cleanup).
func (w *wsConn) drainQueued() {
	deadline := time.Now().Add(wsQuitDrainWait)
	stall := newWSStallState()
	for {
		select {
		case out := <-w.sendQ:
			if time.Now().After(deadline) {
				return
			}
			if err := w.writeOut(out, stall); err != nil {
				return
			}
		default:
			return
		}
	}
}

// waitDrained blocks up to d for the write loop to exit, so teardown can let
// the queued frames reach the socket before it is closed underneath the writer.
func (w *wsConn) waitDrained(d time.Duration) {
	if d <= 0 || w.done == nil {
		return
	}
	select {
	case <-w.done:
	case <-time.After(d):
	}
}

func (w *wsConn) writeJSON(v WSMessage) error {
	if w == nil || w.conn == nil {
		return errWSClosed
	}
	out, err := prepareOut(&v)
	if err != nil {
		log.Printf("websocket marshal (%s): %v", v.Type, err)
		return err
	}
	if w.captureMsg {
		out.msg = &v
	}
	if err := w.enqueueOut(out); err != nil && !errors.Is(err, errWSClosed) {
		log.Printf("websocket write (%s): %v", v.Type, err)
		return err
	}
	return nil
}

// writePrepared enqueues a frame already prepared by the caller, so a broadcast
// marshals and deflates the payload once for all of its clients. msg is only
// consulted by the captureMsg test seam.
func (w *wsConn) writePrepared(msg *WSMessage, out *wsOut) error {
	if w == nil || w.conn == nil {
		return errWSClosed
	}
	if w.captureMsg && msg != nil {
		cp, outCopy := *msg, *out
		outCopy.msg = &cp
		return w.enqueueOut(&outCopy)
	}
	return w.enqueueOut(out)
}

// enqueueOut hands a frame to the writer, enforcing the byte bound.
func (w *wsConn) enqueueOut(out *wsOut) error {
	if w == nil || w.conn == nil || out == nil {
		return errWSClosed
	}
	select {
	case <-w.quit:
		return errWSClosed
	case <-w.done:
		return errWSClosed
	default:
	}
	size := int64(out.size)
	if w.queuedBytes.Add(size) > wsMaxQueuedBytes {
		// Over budget: refuse rather than queue. The caller detaches the
		// socket, which is the honest "this client cannot keep up" signal —
		// the browser reconnects and re-syncs from the session state.
		w.queuedBytes.Add(-size)
		return errWSQueueFull
	}
	select {
	case w.sendQ <- out:
		return nil
	case <-w.quit:
		w.queuedBytes.Add(-size)
		return errWSClosed
	case <-w.done:
		w.queuedBytes.Add(-size)
		return errWSClosed
	case <-time.After(wsEnqueueWait):
		// The queue depth (not the byte budget) is exhausted — a small debug
		// depth, or thousands of tiny frames. Blocking the producer longer
		// would stall the stream reader; this client is too slow.
		w.queuedBytes.Add(-size)
		return errWSQueueFull
	}
}

// writeOut writes one prepared frame and releases its queue budget. Called only
// from the single writer goroutine (gorilla forbids concurrent writers), which
// is why no mutex is needed around the connection.
func (w *wsConn) writeOut(out *wsOut, stall wsStallState) error {
	if err := w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		w.queuedBytes.Add(-int64(out.size))
		log.Printf("websocket set write deadline: %v", err)
		return err
	}
	if d := stall.delay(time.Now()); d > 0 {
		time.Sleep(d)
	}
	err := w.conn.WritePreparedMessage(out.frame)
	w.queuedBytes.Add(-int64(out.size))
	return err
}

// registerWSConn adds conn to the tracked set so the server can close it
// on graceful shutdown.
func (s *Server) registerWSConn(conn *websocket.Conn) {
	s.wsConnsMu.Lock()
	s.wsConns = append(s.wsConns, conn)
	s.wsConnsMu.Unlock()
}

// unregisterWSConn removes conn from the tracked set so shutdown does not
// close a connection that has already been cleaned up.
func (s *Server) unregisterWSConn(conn *websocket.Conn) {
	s.wsConnsMu.Lock()
	defer s.wsConnsMu.Unlock()
	for i, c := range s.wsConns {
		if c == conn {
			s.wsConns = append(s.wsConns[:i], s.wsConns[i+1:]...)
			return
		}
	}
}

// closeWSConns force-closes all tracked WebSocket connections. Safe to call
// concurrently with register/unregister. Never blocks on a single conn.
//
// Close only: gorilla permits Close to race every other method, but NOT
// SetWriteDeadline — the writeLoop sets one per frame, and setting it here from
// a teardown goroutine is a data race on the connection's deadline state (found
// by -race when the handler join below started calling this in tests). Closing
// the socket is what unblocks a reader parked in ReadJSON and a writer parked
// in WriteMessage anyway, so the deadlines bought nothing.
func (s *Server) closeWSConns() {
	s.wsConnsMu.Lock()
	conns := s.wsConns
	s.wsConns = nil
	s.wsConnsMu.Unlock()
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		c := conn
		go func() {
			_ = c.Close()
		}()
	}
}
