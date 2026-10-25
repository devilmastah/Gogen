package server

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) wsUpgrader() websocket.Upgrader {
	allowed := s.allowedOrigins
	return websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return checkWSOrigin(r, allowed)
		},
		// permessage-deflate: the browser negotiates it automatically, and a
		// full-session history payload (multi-MB of repetitive JSON —
		// reasoning blocks, code, tool results) shrinks ~10x on the wire.
		// Go test clients don't request compression by default, so existing
		// tests are unaffected.
		EnableCompression: true,
	}
}

// upgradedWS bundles the result of a shared WebSocket handshake: the raw
// connection (for ReadJSON in the read loop), the wrapped wsConn (for
// writes), and the cleanup the handler must defer.
type upgradedWS struct {
	conn    *websocket.Conn
	ws      *wsConn
	cleanup func()
}

// errWSUpgrade is returned by upgradeWS after the HTTP error response has
// already been written, so handlers can simply return.
var errWSUpgrade = errors.New("websocket upgrade failed")

// upgradeWS performs the handshake shared by the chat and editor endpoints:
// auth check, upgrade + connection rate limiting (both sockets of one tab
// count against the same global connection limiter), origin check, protocol
// upgrade, connection tracking (for shutdown force-close), read-deadline and
// pong setup, and wrapping in the write-queue wsConn. The returned cleanup
// tears the socket down in the same order HandleWS used: closeSend (lets the
// writer drain its queue) → unregister → raw close → release the connection
// slot. Callers must defer the cleanup.
func (s *Server) upgradeWS(w http.ResponseWriter, r *http.Request) (*upgradedWS, error) {
	if !s.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, errWSUpgrade
	}
	if s.upgradeLimiter != nil && !s.upgradeLimiter.allow(clientIP(r)) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return nil, errWSUpgrade
	}
	var release func()
	if s.connLimiter != nil {
		if !s.connLimiter.acquireConn() {
			http.Error(w, "too many connections", http.StatusServiceUnavailable)
			return nil, errWSUpgrade
		}
		release = s.connLimiter.releaseConn
	}
	upg := s.wsUpgrader()
	conn, err := upg.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		release()
		return nil, errWSUpgrade
	}
	s.registerWSConn(conn)
	// Pong handler extends the read deadline whenever the browser replies to
	// our pings. If the client stops responding (tab closed, network gone),
	// the read deadline elapses, ReadJSON fails, and the handler tears down —
	// which closes the write side too. This is what surfaces half-open
	// connections that would otherwise freeze the UI silently.
	if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
		log.Printf("websocket set read deadline: %v", err)
	}
	conn.SetPongHandler(func(string) error {
		if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
			log.Printf("websocket set read deadline: %v", err)
		}
		return nil
	})

	ws := newWSConn(conn)
	cleanup := func() {
		ws.closeSend()
		s.unregisterWSConn(conn)
		_ = conn.Close()
		if release != nil {
			release()
		}
	}
	return &upgradedWS{conn: conn, ws: ws, cleanup: cleanup}, nil
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	u, err := s.upgradeWS(w, r)
	if err != nil {
		return
	}
	defer u.cleanup()
	// Register the handler so teardown paths can wait for this goroutine:
	// after the upgrade, net/http no longer tracks it (see wsHandlerTracker).
	s.wsLive.enter()
	defer s.wsLive.leave()
	conn := u.conn
	ws := u.ws

	// The connection's pane: the session it is currently attached to and the
	// default target for messages without a sessionId. Lifecycle ops
	// (session_new/resume/fork) switch the pane; teardown detaches from
	// whatever the current pane is — WITHOUT cancelling any turn (the turn is
	// owned by the runtime, so disconnecting never kills it, §4).
	//
	// The resolution loops because it can race an explicit close
	// (session_close): first() returns a runtime the close has claimed
	// (clientless, latched closing) or already removed, and the attach is
	// REFUSED — no client may attach to a runtime that is leaving memory.
	// The handshake must land somewhere, so retry: the claimed runtime is
	// unregistered within its drain window (≤ wsStreamDrainWait plus the
	// flush) and the next first() returns another session, or nil → the
	// bootstrap loads the latest saved session from the store (the
	// just-closed one, flushed by its close) or creates a fresh default.
	// The registry can also be empty outright: every runtime was evicted —
	// the last pane was explicitly closed (session_close) or the last client
	// detached from an idle session (orphan eviction) — and bootstrapping
	// gives the connection and any legacy id-less message a target.
	deadline := time.Now().Add(5 * time.Second)
	var pane *sessionRuntime
	for {
		pane = s.registry.first()
		if pane == nil {
			pane = s.createBootstrapSession()
		}
		if pane == nil {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: no session available"})
			return
		}
		if s.attachSession(ws, r, pane, true, nil) {
			break
		}
		if time.Now().After(deadline) {
			// Pathological: every candidate stayed claimed for the whole
			// deadline. Fail the handshake the same way a missing session
			// does — the client's reconnect logic retries.
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: no session available"})
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	// (the current pane plus any background panes) — WITHOUT cancelling any
	// turn (the turn is owned by the runtime, so disconnecting never kills
	// it, §4). A killed tab cannot send session_detach per pane, and a stale
	// attachment would leak the dead socket and could leave a pending
	// delete-approval hanging forever instead of auto-denying.
	defer s.registry.detachAll(ws)

	// Interactive user shell for this connection, killed on disconnect. The
	// shell itself is spawned after the config/history handshake below so
	// connection setup never depends on pty availability.
	userTermHolder := &userTermHolder{}
	defer func() {
		if ut := userTermHolder.get(); ut != nil {
			ut.Close()
		}
	}()

	// Spawn the user shell after the handshake so a pty failure (sandboxed
	// or headless environments) can never delay the config/history messages.
	s.spawnUserTerminal(ws, userTermHolder)

	incoming := make(chan WSMessage, 8)
	go func() {
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				close(incoming)
				return
			}
			// Complete delete approvals here so they never sit behind a
			// main-loop turnMu.Lock() (the stream holds turnMu while waiting
			// for approval). Route by sessionId so each session's approvals
			// resolve independently.
			if msg.Type == "delete_approval_response" || msg.Type == "command_approval_response" {
				if rt := s.resolveRuntime(msg.SessionID); rt != nil {
					rt.completeApproval(msg.ApprovalID, msg.Approved)
				}
				continue
			}
			incoming <- msg
		}
	}()

	req := &wsRequest{
		server:  s,
		conn:    ws,
		request: r,
		holder:  userTermHolder,
		pane:    pane,
	}
	for msg := range incoming {
		req.msg = msg
		req.target, req.pane = s.resolveMessageTarget(msg, req.pane)
		if e, ok := wsHandlers[msg.Type]; ok {
			e.handle(req)
		}
	}
}

// resolveMessageTarget resolves the session runtime a message acts on: the
// id-resolved runtime for explicit ids, or the connection's pane for id-less
// messages. Session-scoped messages WITHOUT an explicit sessionId act on this
// connection's own pane, not the server-global default. The default is only
// the bootstrap fallback for the initial attach (pane := registry.first())
// and is moved by ANY tab's session_attach/session_new (setDefault is
// global), so an id-less cancel/set_mode/session_detach would silently hit
// the WRONG session in a multi-tab setup. The pane is the sender's current
// session, which is what an id-less message means. On first load the pane IS
// the default, so legacy behavior is unchanged. (Approval responses are
// intercepted in the read goroutine and keep default routing — an empty id
// there is a malformed client, and the goroutine must not touch the shared
// pane.) Returns the target and the (possibly re-aligned) pane.
func (s *Server) resolveMessageTarget(msg WSMessage, pane *sessionRuntime) (*sessionRuntime, *sessionRuntime) {
	target := s.resolveRuntime(msg.SessionID)
	if msg.SessionID == "" {
		// The pane pointer can reference a runtime that left the
		// registry while this connection was open — its session was
		// deleted by another tab (session_delete detaches every
		// attached client), or it was cap/orphan-evicted while the
		// pane was open. Routing an id-less message to the evicted
		// runtime would silently drop it (the handlers' evicted
		// guard), so fall back to the default session when one is
		// live. When the registry is EMPTY (this connection closed
		// its only pane via session_close / session_delete and has
		// not re-keyed yet), deliberately do NOT bootstrap: the
		// latest saved session is the one the user just closed, and
		// createBootstrapSession would resurrect it as an active
		// runtime (the sessions payload would show it "active" again
		// and session_close's eviction would appear undone). The
		// stale pane stays in place and the handlers' evicted guard
		// drops the message safely; the client re-keys (session_new /
		// session_attach) and the next explicit-id message re-aligns
		// the pane.
		if pane == nil || pane.evicted.Load() {
			if d := s.registry.first(); d != nil {
				pane = d
			}
		}
		target = pane
	}
	return target, pane
}

// editorReadTypes and editorWriteTypes are the message types served by the
// /ws/editor socket (HandleWSEditor). They must stay in lockstep with the
// wsHandlers map (the main chat socket routes the same types): missing
// either registration silently drops the message. TestWSEditorHandlerParity
// asserts the two agree.
var editorReadTypes = []string{
	"fs_list", "fs_read", "fs_search",
	"git_status", "git_file_diff", "git_commit_message",
}

var editorWriteTypes = []string{
	"fs_write", "fs_replace", "fs_apply_patch",
	"git_commit", "git_stage", "git_unstage", "git_push",
}

// typeInList reports whether t is present in list (small linear scan — the
// lists are a handful of constants).
func typeInList(list []string, t string) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}

// HandleWSEditor serves the editor WebSocket endpoint (/ws/editor). It is the
// workspace-scoped counterpart of HandleWS: it handles only the filesystem
// and git message types in editorReadTypes/editorWriteTypes and ignores
// chat/session messages. The editor
// socket is independent of any chat session, so a busy or streaming session
// never blocks editor saves behind the chat read loop. Write messages
// serialize on the workspace filesystem lock (fsMu) — they wait only
// for the actual mutation window of a
// running tool, never for the whole streaming turn.
func (s *Server) HandleWSEditor(w http.ResponseWriter, r *http.Request) {
	u, err := s.upgradeWS(w, r)
	if err != nil {
		return
	}
	defer u.cleanup()
	s.wsLive.enter()
	defer s.wsLive.leave()
	conn := u.conn
	ws := u.ws

	incoming := make(chan WSMessage, 8)
	go func() {
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				close(incoming)
				return
			}
			incoming <- msg
		}
	}()

	for msg := range incoming {
		switch {
		case typeInList(editorReadTypes, msg.Type):
			s.handleFSReadMessage(ws, r.Context(), msg)
		case typeInList(editorWriteTypes, msg.Type):
			s.handleFSWriteMessage(ws, r.Context(), msg)
		default:
			// Non-editor messages (chat, sessions, terminal) are not
			// supported on this socket; ignore them.
		}
	}
}
