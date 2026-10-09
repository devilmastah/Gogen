package server

// Reply-contract tests for operations that used to fail SILENTLY. Every
// client-side wait hangs or corrupts state on a silent drop:
//
//   - cancel with nothing in flight: the web client holds a per-cancel
//     bookkeeping unit (suppressTurnEnds) from every cancel it sends; the
//     cancelled/turn_end pair that normally consumes it never comes, so the
//     next real turn_end would be swallowed and the client's busy state
//     would stick. The idle cancel is now answered with `cancel_ack`.
//   - a message naming a session that resolves nowhere: the client has
//     already rendered an optimistic bubble and armed its pending-ack
//     entry; a silent drop strands both and the stale entry then stamps the
//     NEXT user_acked with the wrong history index (edit/resend/fork at a
//     wrong point). Now answered with an "Error:" response, which is what
//     the client's response handler needs to revoke the bubble and drop the
//     pending entry.
//   - session_fork from a gone session: same stranded-bookkeeping class
//     (pendingSessionResponse / the edit-resend latch wait on this reply).
//   - session_new naming another pane's session: used to fall through and
//     replace the WRONG pane's session (the very replacement the re-align
//     exists to prevent); now rejected.
//
// Companion client test: scripts/test_session_change_guards.js (scenario 4
// pins cancel_ack draining exactly one unit).

import (
	"strings"
	"testing"
	"time"
)

// readForbiddenFrames drains frames for d, failing on any of the given types.
func readForbiddenFrames(t *testing.T, conn interface {
	ReadJSON(v interface{}) error
	SetReadDeadline(t time.Time) error
}, d time.Duration, forbidden ...string,
) {
	t.Helper()
	bad := make(map[string]bool, len(forbidden))
	for _, f := range forbidden {
		bad[f] = true
	}
	_ = conn.SetReadDeadline(time.Now().Add(d))
	for {
		var m WSMessage
		if err := conn.ReadJSON(&m); err != nil {
			return // deadline — expected
		}
		if bad[m.Type] {
			t.Fatalf("unexpected frame %q during drain", m.Type)
		}
	}
}

// An idle session's cancel must be ACKed, not swallowed: no turn is running,
// so no cancelled/turn_end pair will ever consume the sender's bookkeeping.
func TestCancelIdleSessionRepliesCancelAck(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "cancel", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send cancel: %v", err)
	}
	ack := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "cancel_ack" })
	if ack.Type != "cancel_ack" {
		t.Fatalf("idle cancel not acked (got %q)", ack.Type)
	}
	// The ack REPLACES the pair — an idle cancel must not emit one.
	readForbiddenFrames(t, conn, 400*time.Millisecond, "cancelled", "turn_end")
}

// A cancel that actually cancels still gets the real pair and must NOT get
// an extra cancel_ack (the pair alone drains the client's unit).
func TestCancelRunningTurnGetsPairNotAck(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	startBlockedTurn(t, rt, stub, "hold")

	if err := conn.WriteJSON(WSMessage{Type: "cancel", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send cancel: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "cancelled" && m.SessionID == a.SessionID
	})
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == a.SessionID
	})
	readForbiddenFrames(t, conn, 400*time.Millisecond, "cancel_ack")
	waitFor(t, 5*time.Second, func() bool {
		active, _ := rt.turnState()
		return !active
	})
}

// A cancel naming a session that resolves nowhere answers with cancel_ack
// (and, as before, must not touch any real session).
func TestCancelUnknownSessionRepliesCancelAck(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "cancel", SessionID: "no-such-session"}); err != nil {
		t.Fatalf("send cancel: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "cancel_ack" })
}

// A message naming a session that resolves nowhere (and is not this pane's
// session) is rejected with an Error response instead of vanishing — the
// sender's optimistic bubble + pending-ack entry must not be stranded.
func TestMessageToUnknownSessionRepliesError(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hi", SessionID: "no-such-session"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	if !strings.HasPrefix(resp.Content, "Error: ") {
		t.Fatalf("message to unknown session answered %q, want an Error: response", resp.Content)
	}
	// Nothing may have run anywhere: no turn artifacts on this connection
	// and the default session's message list untouched.
	readForbiddenFrames(t, conn, 400*time.Millisecond, "user_acked", "turn_end", "stream_end")
	if got := a.MessageCount(); got != 0 {
		t.Fatalf("default session message count = %d, want 0", got)
	}
}

// Forking from a session that is gone answers with an Error response: the
// client's in-flight session-change / edit-resend latch waits for this
// reply, and a silent drop left it blocked ("Session change already in
// progress") until the next turn_end or reconnect.
func TestForkFromUnknownSessionRepliesError(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	idsBefore := s.registry.activeIDs()
	if err := conn.WriteJSON(WSMessage{Type: "session_fork", MessageIndex: 0, SessionID: "no-such-session"}); err != nil {
		t.Fatalf("send fork: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	if !strings.HasPrefix(resp.Content, "Error: ") {
		t.Fatalf("fork from unknown session answered %q, want an Error: response", resp.Content)
	}
	// No fork side effects: no clear_chat, registry untouched.
	readForbiddenFrames(t, conn, 400*time.Millisecond, "clear_chat")
	idsAfter := s.registry.activeIDs()
	if len(idsBefore) != len(idsAfter) {
		t.Fatalf("registry changed by a rejected fork: before=%v after=%v", idsBefore, idsAfter)
	}
	if _, ok := s.registry.get(a.SessionID); !ok {
		t.Fatal("default session gone after a rejected fork")
	}
}

// session_new naming a session that is neither registered nor this pane's
// own must be rejected. Pre-fix it fell through and REPLACED the acting
// pane's session — the wrong-session replacement the re-align exists to
// prevent (the edit-resend path only ever names its own pane's id, which a
// pane-match fallback still accepts, even when evicted).
func TestSessionNewForForeignSessionRepliesError(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "session_new", SessionID: "no-such-session"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	if !strings.HasPrefix(resp.Content, "Error: ") {
		t.Fatalf("session_new for a foreign session answered %q, want an Error: response", resp.Content)
	}
	// The pane's session must be untouched: a pre-fix fall-through ran the
	// new-session flow (clear_chat + a replacement session for THIS pane).
	readForbiddenFrames(t, conn, 400*time.Millisecond, "clear_chat")
	if _, ok := s.registry.get(a.SessionID); !ok {
		t.Fatal("pane's session was replaced by session_new for a foreign id")
	}
}
