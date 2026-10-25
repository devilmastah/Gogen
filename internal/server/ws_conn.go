package server

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var errWSClosed = errors.New("websocket closed")

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex

	// sendQ carries outbound frames to the writeLoop goroutine.
	//
	// It is a channel of POINTERS, not of values, and that is a memory
	// decision: a buffered channel allocates its whole ring eagerly at make,
	// and WSMessage is a large struct (~2.1 KB — it carries the model list,
	// the session list, image attachments, the history rewind snapshot and
	// the queue payloads). A value channel at wsSendQueueSize would therefore
	// reserve ~8.3 MB of *empty* buffer per connection, allocated before a
	// single frame is sent — by far the biggest thing a web server holds
	// (two sockets per browser tab = ~17 MB, and the connection limiter
	// allows 32). With pointers the same depth, FIFO order and backpressure
	// semantics cost ~32 KB idle, and only frames actually queued hold
	// memory.
	sendQ chan *WSMessage
	quit  chan struct{} // closed by closeSend to stop writers + writeLoop
	done  chan struct{} // closed when writeLoop exits, so writeJSON fails fast
	once  sync.Once
}

const (
	wsSendQueueSize   = 4096
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
		sendQ: make(chan *WSMessage, qsize),
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
	// lag the stream and the send queue fills (detach via enqueueJSON's 5s
	// timeout). Compiled out of production builds (ws_conn_release.go).
	stall := newWSStallState()
	for {
		select {
		case <-w.quit:
			return
		case msg := <-w.sendQ:
			w.mu.Lock()
			if err := w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				w.mu.Unlock()
				log.Printf("websocket set write deadline: %v", err)
				return
			}
			if d := stall.delay(time.Now()); d > 0 {
				time.Sleep(d)
			}
			err := w.conn.WriteJSON(msg)
			w.mu.Unlock()
			if err != nil {
				return
			}
		case <-ticker.C:
			// Pings detect half-open connections (e.g. NAT/proxy idle
			// timeouts, backgrounded tabs) that pass write deadlines but
			// never reach the browser. A failed ping kills the writer,
			// triggering teardown + reconnect via the deferred Close.
			w.mu.Lock()
			if err := w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				w.mu.Unlock()
				log.Printf("websocket set write deadline: %v", err)
				return
			}
			err := w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			w.mu.Unlock()
			if err != nil {
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

func (w *wsConn) writeJSON(v WSMessage) error {
	err := w.enqueueJSON(v)
	if err != nil && !errors.Is(err, errWSClosed) {
		log.Printf("websocket write (%s): %v", v.Type, err)
	}
	return err
}

func (w *wsConn) enqueueJSON(v WSMessage) error {
	if w == nil || w.conn == nil {
		return errWSClosed
	}
	select {
	case <-w.quit:
		return errWSClosed
	case <-w.done:
		return errWSClosed
	default:
	}
	// One heap message per queued frame (see the sendQ comment): the queue
	// holds pointers, so memory tracks the actual backlog instead of the
	// queue's depth. The pointer is what the writeLoop marshals; the JSON on
	// the wire is identical to the value-channel form.
	msg := &v
	select {
	case w.sendQ <- msg:
		return nil
	case <-w.quit:
		return errWSClosed
	case <-w.done:
		return errWSClosed
	default:
		// Queue full: block briefly rather than stall the LLM stream reader forever.
		select {
		case w.sendQ <- msg:
			return nil
		case <-w.quit:
			return errWSClosed
		case <-w.done:
			return errWSClosed
		case <-time.After(5 * time.Second):
			return fmt.Errorf("websocket send queue full")
		}
	}
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
