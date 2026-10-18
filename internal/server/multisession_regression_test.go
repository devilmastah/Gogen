package server

// Regression tests for the multi-session review fixes:
//
//  1. ensureSessionRuntime/sessionResume must never hand out an unregistered
//     orphan runtime when two connections concurrently attach the same
//     inactive session (E9 dedupe).
//  2. Connection teardown must detach the socket from EVERY session it is
//     attached to (background panes included), not just the current pane —
//     otherwise dead sockets leak and D10 auto-deny never fires for a
//     background session whose last client died with a pending approval.
//  3. Deleting the server-global default session from a pane that is on a
//     different session must be a plain delete — not a pane hijack into a
//     fresh session.
//  4. A fast consecutive turn's stream handles must survive the previous
//     turn's cleanup (stream.end() before turnMu.Unlock), so the new turn
//     stays cancellable.

import (
	"context"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/session"
)

// TestConcurrentEnsureSessionRuntimeDedupes verifies E9 under concurrency:
// N connections attaching the same inactive session must all receive the SAME
// registered runtime. Pre-fix, a loser of the register race returned its own
// freshly built but UNREGISTERED runtime — two live agents for one session id
// (last-writer-wins persistence) and a runtime invisible to delete/prune/
// shutdown.
func TestConcurrentEnsureSessionRuntimeDedupes(t *testing.T) {
	s, _, store := newLifecycleServer(t)
	store.Save("disk-session", agent.SessionSnapshot{
		WorkingDir: s.ws.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "saved"}},
	})

	const n = 16
	var wg sync.WaitGroup
	results := make(chan *sessionRuntime, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt, err := s.ensureSessionRuntime("disk-session")
			if err != nil {
				t.Errorf("ensureSessionRuntime: %v", err)
				return
			}
			results <- rt
		}()
	}
	wg.Wait()
	close(results)

	registered, ok := s.registry.get("disk-session")
	if !ok {
		t.Fatal("session not registered after concurrent attach")
	}
	for rt := range results {
		if rt != registered {
			t.Fatal("attach returned an unregistered orphan runtime instead of the registered one")
		}
	}
	// Exactly one runtime for the id: default session + disk-session.
	if ids := s.registry.activeIDs(); len(ids) != 2 {
		t.Fatalf("active sessions = %v, want [default disk-session]", ids)
	}
}

// TestTeardownDetachesAllAttachedSessions verifies that closing a connection
// removes it from every session it was attached to — the current pane AND any
// background panes. Pre-fix, only the current pane was detached; a
// background pane kept the dead socket in its clients set until the next
// broadcast failed, and a pending delete-approval would hang forever instead
// of auto-denying.
func TestTeardownDetachesAllAttachedSessions(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID

	// session_new switches the pane to a fresh session B; session A stays
	// attached in the background (attachment is client-managed, D5).
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sidB := cfg.SessionID

	rtA, okA := s.registry.get(sidA)
	rtB, okB := s.registry.get(sidB)
	if !okA || !okB {
		t.Fatalf("sessions registered: A=%v B=%v", okA, okB)
	}
	if rtA.clientCount() != 1 || rtB.clientCount() != 1 {
		t.Fatalf("before close: A clients=%d B clients=%d, want 1 each", rtA.clientCount(), rtB.clientCount())
	}

	// Kill the tab without sending session_detach: the teardown must detach
	// BOTH sessions (the current pane and the background pane).
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		return rtA.clientCount() == 0 && rtB.clientCount() == 0
	})
}

// TestDeleteDefaultSessionDoesNotHijackPane verifies that sessionDelete keys
// "current" off the pane's session, NOT the server-global default
// (registry.first()). The default is only the fallback target for messages
// without a sessionId and can differ from this connection's pane in multi-tab
// setups; deleting it must be a plain background delete. Pre-fix, deleting
// the default while the pane was on another session cleared the pane and
// started a brand-new session.
func TestDeleteDefaultSessionDoesNotHijackPane(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	origID := a.SessionID
	pane := s.registry.first()

	// /new creates session B and makes it the pane (and the default).
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new: %v", err)
	}
	sidB := pane.agent.SessionID

	// Another tab attached the ORIGINAL session, making it the server-global
	// default while this pane stays on B.
	s.registry.setDefault(origID)
	if s.registry.first().agent.SessionID != origID {
		t.Fatal("setup: origID should be the default")
	}

	// Delete the default session from this pane's sidebar.
	res, handled, err := s.runSessionCommand(context.Background(), nil, &pane, "resume del "+origID)
	if err != nil || !handled {
		t.Fatalf("delete: handled=%v err=%v", handled, err)
	}
	if res.Action != agent.SessionActionNone {
		t.Fatalf("delete action = %q, want none (deleting the default must not clear the pane)", res.Action)
	}
	if pane.agent.SessionID != sidB {
		t.Fatalf("pane session = %q, want %q (delete of the default hijacked the pane)", pane.agent.SessionID, sidB)
	}
	if s.registry.first().agent.SessionID != sidB {
		t.Fatalf("default after delete = %q, want %q (should re-point at the next session)", s.registry.first().agent.SessionID, sidB)
	}
	if _, ok := s.registry.get(origID); ok {
		t.Fatal("deleted session still registered")
	}
}

// TestConsecutiveTurnsSecondTurnCancellable pins the turn cleanup order:
// stream.end() must run before turnMu.Unlock(), so a fast consecutive turn's
// begin() can never be clobbered by the previous turn's end() (which would
// silently drop the new turn's cancel handles — cancel would become a no-op
// and the turn would run to completion). Turn 1 completes, turn 2 starts, and
// a second connection cancels it: the new turn must actually stop.
func TestConsecutiveTurnsSecondTurnCancellable(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	// Turn 1 runs to completion.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "first", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	stub.releaseN(1)
	readUntil(t, conn1, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })

	// A second connection attaches (to cancel from a different socket).
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Turn 2 starts immediately after turn 1 — the exact interleaving where
	// the previous turn's deferred cleanup could clobber the new handles.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "second", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(2)
	if err := conn2.WriteJSON(WSMessage{Type: "cancel", SessionID: sid}); err != nil {
		t.Fatalf("send cancel: %v", err)
	}
	msg := readUntil(t, conn1, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "cancelled" || m.Type == "turn_end"
	})
	if msg.Type != "cancelled" {
		t.Fatalf("first terminal message after cancel = %q, want cancelled (turn 2's cancel handles were clobbered)", msg.Type)
	}
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })
}

// TestMessageToEvictedPaneDropped verifies the evicted-flag guard: a message
// sent to a pane whose session left the registry is dropped silently instead
// of starting a headless turn on an unregistered runtime (invisible to
// cancel/prune/shutdown). The eviction happens between the handler resolving
// the runtime and acquiring its turnMu — the window the eviction TryLock +
// sticky flag close.
func TestMessageToEvictedPaneDropped(t *testing.T) {
	dir := t.TempDir()
	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := newTestServer(t, a, &config.Config{WebMaxActiveSessions: 2})
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := a.SessionID
	rtA, ok := s.registry.get(sidA)
	if !ok {
		t.Fatal("default session not registered")
	}

	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// conn2: /new → B (B becomes the server-global default), then /new → C.
	// The cap now spares every session a client is watching (conn1 still
	// shows A), so the eviction is driven explicitly — the guard under test
	// is the sticky evicted flag, not which eviction path set it.
	if err := conn2.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("new 1: %v", err)
	}
	_ = readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	if err := conn2.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("new 2: %v", err)
	}
	_ = readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })

	s.registry.closeRuntime(rtA)
	if !rtA.evicted.Load() {
		t.Fatal("evicted runtime not marked")
	}

	// An id-less message routes to conn1's pane (A, evicted): it must be
	// dropped silently — no turn on the orphan, no message appended, no
	// re-registration.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Drain conn1 briefly: the only allowed messages are the leftover
	// handshake/session_detached ones; any turn artifact means the message
	// was processed on the evicted runtime.
	deadline := time.Now().Add(750 * time.Millisecond)
	if err := conn1.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		var m WSMessage
		if err := conn1.ReadJSON(&m); err != nil {
			break // deadline or closed — expected for a silent drop
		}
		switch m.Type {
		case "user_acked", "response", "stream_end", "turn_end":
			t.Fatalf("message to evicted pane was processed (got %q)", m.Type)
		}
	}
	if got := rtA.agent.MessageCount(); got != 0 {
		t.Fatalf("message reached the evicted runtime: message count = %d, want 0", got)
	}
	if _, ok := s.registry.get(sidA); ok {
		t.Fatal("message re-registered the evicted session")
	}
}

// TestIdlessOpsTargetPaneNotDefault verifies that session-scoped messages
// without a sessionId act on the sender's own pane, not the server-global
// default. The default is global and moves with any tab's session_attach/
// session_new, so routing an id-less set_mode/cancel to it would let one
// tab's toolbar action mutate (or cancel) ANOTHER tab's session.
func TestIdlessOpsTargetPaneNotDefault(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	sidA := a.SessionID
	rtA, ok := s.registry.get(sidA)
	if !ok {
		t.Fatal("default session not registered")
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// conn2 opens a new session B, which becomes the server-global default.
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn2.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("new: %v", err)
	}
	_ = readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	rtB := s.registry.first()
	sidB := rtB.agent.SessionID
	if sidB == sidA {
		t.Fatal("setup: /new should create a different session")
	}

	// conn1 sends an id-less set_mode: it must hit conn1's pane (A), not the
	// default (B).
	if err := conn1.WriteJSON(WSMessage{Type: "set_mode", Mode: "plan"}); err != nil {
		t.Fatalf("send set_mode: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		mode, _ := rtA.agent.ModeAndThinkingLevel()
		return mode.String() == "plan"
	})
	modeB, _ := rtB.agent.ModeAndThinkingLevel()
	if modeB.String() == "plan" {
		t.Fatal("id-less set_mode hit the default session instead of the sender's pane")
	}
}
