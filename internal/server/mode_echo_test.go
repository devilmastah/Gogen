package server

import (
	"testing"
	"time"
)

// TestSetModeBroadcastsToOtherAttachedClients pins the cross-client mode
// convergence contract: mode is per-SESSION state read by every client
// attached to that session (each tab renders it in its mode picker), so a
// change made on one connection must reach the others exactly like every
// other config change (feature flags broadcast through broadcastConfigAll).
//
// Without the broadcast, a second tab/window on the same session keeps
// showing the old mode; the picker's "already active" short-circuit then
// makes clicking the correct row a silent no-op, so the user believes the
// session is in the other mode while the server keeps the old one (the tool
// list the model sees follows the SERVER's mode).
func TestSetModeBroadcastsToOtherAttachedClients(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	drainHandshake(t, conn1)
	sid := a.SessionID

	// A second client attached to the SAME session (second tab/window).
	conn2 := dialWS(t, srv, "/ws")
	// Its own attach handshake (the bootstrap pane is the same session);
	// leftovers can only carry the pre-change mode ("act"), so they cannot
	// satisfy the plan-mode assertion below.
	drainHandshake(t, conn2)

	// conn1 switches the session to plan mode; the echo must reach conn1...
	if err := conn1.WriteJSON(WSMessage{Type: "set_mode", Mode: "plan", SessionID: sid}); err != nil {
		t.Fatalf("send set_mode: %v", err)
	}
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid && m.Mode == "plan"
	})

	// ...and conn2, which renders the same session's mode picker.
	readUntil(t, conn2, 2*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid && m.Mode == "plan"
	})
}

// TestChatModeCommandBroadcastsToOtherAttachedClients pins the other half of
// the mode-convergence contract: the same mode change TYPED INTO THE CHAT
// ("/plan", "mode plan") used to echo only to the acting connection, while the
// picker handler above broadcast — so a second tab kept rendering the old mode
// after an identical change made a different way. Both paths now finish
// through echoConfigReply, which broadcasts.
func TestChatModeCommandBroadcastsToOtherAttachedClients(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	drainHandshake(t, conn1)
	sid := a.SessionID

	conn2 := dialWS(t, srv, "/ws")
	drainHandshake(t, conn2)

	// A chat command (not the picker): the handler holds the turn, applies
	// the mode, releases, and echoes off the read loop.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "/plan", SessionID: sid}); err != nil {
		t.Fatalf("send /plan: %v", err)
	}
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid && m.Mode == "plan"
	})
	readUntil(t, conn2, 2*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid && m.Mode == "plan"
	})
}
