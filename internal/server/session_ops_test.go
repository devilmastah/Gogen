package server

import (
	"context"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/session"
)

// newLifecycleServer builds a server with a mock provider and a real store so
// lifecycle ops can be exercised against the registry.
func newLifecycleServer(t *testing.T) (*Server, *agent.Agent, *session.Store) {
	t.Helper()
	dir := t.TempDir()
	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	s := NewServer(a, &config.Config{})
	// Quiesce async writers BEFORE the caller's t.TempDir() removal (cleanups
	// run LIFO, so this later-registered cleanup runs after the test's own and
	// before RemoveAll). A WS read-loop goroutine can outlive the test body —
	// session_detach's orphan eviction flushes and rewrites the store, a resume
	// restores (and flushes) the session it swapped in — and that write lands
	// in .gogen/sessions while the temp dir is being deleted: "TempDir
	// RemoveAll cleanup: ... directory not empty" (the same flake class the
	// continuation/continuable helpers fix for their own tests).
	t.Cleanup(func() {
		s.stopAutomations()
		s.ShutdownSessions()
		for _, id := range s.registry.activeIDs() {
			if rt, ok := s.registry.get(id); ok {
				s.registry.evictRuntime(rt)
			}
		}
	})
	return s, a, store
}

func TestSessionNewRegistryOp(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	origID := a.SessionID

	pane := s.registry.first()
	res, handled, err := s.runSessionCommand(context.Background(), nil, &pane, "/new")
	if err != nil || !handled {
		t.Fatalf("runSessionCommand: handled=%v err=%v", handled, err)
	}
	if res.Action != agent.SessionActionClearChat {
		t.Fatalf("action = %q, want clear_chat", res.Action)
	}
	newID := pane.agent.SessionID
	if newID == origID {
		t.Fatal("session_new did not create a new session")
	}
	if got := s.registry.activeIDs(); len(got) != 2 {
		t.Fatalf("active sessions = %d (%v), want 2", len(got), got)
	}
	if s.registry.first() != pane {
		t.Fatal("new session is not the default after session_new")
	}
	// The old session's runtime stays alive and resume-able.
	if _, ok := s.registry.get(origID); !ok {
		t.Fatal("old session was evicted by session_new")
	}
	// Messages without a sessionId now route to the new default.
	if s.resolveRuntime("") != pane {
		t.Fatal("resolveRuntime(\"\") does not return the new default")
	}
}

func TestSessionResumeDedupesAndLoads(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	origID := a.SessionID
	pane := s.registry.first()

	// Create a second session on disk (not in the registry).
	store.Save("disk-session", agent.SessionSnapshot{
		WorkingDir: s.ws.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "saved"}},
	})

	// Resume the active default → no-op ack: dedupe (no new runtime) and
	// NO clear_chat/history. The pane already shows this session, so the
	// generic clear-and-replay reply would only flash a wipe/rebuild of
	// the on-screen transcript (an explicit "/resume <current-id>").
	res, handled, err := s.runSessionCommand(context.Background(), nil, &pane, "resume "+origID)
	if err != nil || !handled {
		t.Fatalf("resume active: handled=%v err=%v", handled, err)
	}
	if res.Action != "" {
		t.Fatalf("resume active action = %q, want none (no clear_chat)", res.Action)
	}
	if len(res.History) != 0 {
		t.Fatalf("resume active history = %d messages, want none (no replay)", len(res.History))
	}
	if want := "Already on session " + origID + "."; res.Output != want {
		t.Fatalf("resume active output = %q, want %q", res.Output, want)
	}
	if len(s.registry.activeIDs()) != 1 {
		t.Fatalf("resume of active session created a duplicate: %v", s.registry.activeIDs())
	}
	if pane.agent.SessionID != origID {
		t.Fatalf("pane session = %q, want %q", pane.agent.SessionID, origID)
	}

	// Resume an inactive session → loaded from the store via the factory.
	// The successful result is EMPTY: the resume ships the attach sequence
	// (session_state + history + config) itself (see sessionResume), so no
	// response/clear_chat result is left for writeSessionCommandResult.
	res, handled, err = s.runSessionCommand(context.Background(), nil, &pane, "resume disk-session")
	if err != nil || !handled {
		t.Fatalf("resume inactive: handled=%v err=%v", handled, err)
	}
	if pane.agent.SessionID != "disk-session" {
		t.Fatalf("pane session = %q, want disk-session", pane.agent.SessionID)
	}
	if got := pane.agent.MessageCount(); got != 1 {
		t.Fatalf("resumed message count = %d, want 1", got)
	}
	if len(s.registry.activeIDs()) != 2 {
		t.Fatalf("active sessions = %v, want [orig disk-session]", s.registry.activeIDs())
	}
	if res.Output != "" || res.Action != "" || len(res.History) != 0 {
		t.Fatalf("resume result = %+v, want empty (the attach payload was already sent)", res)
	}

	// "resume latest" follows the agent/TUI/README rule — the newest saved
	// top-level session OTHER than the current one — even when the CURRENT
	// session is the newest on disk (an explicit "/resume <current-id>" is
	// the no-op ack asserted above).
	store.Save("newer-session", agent.SessionSnapshot{
		WorkingDir: s.ws.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "newer"}},
	})
	// Flush the current session (disk-session) so it becomes the strictly
	// newest on disk: "latest" must still skip it.
	time.Sleep(10 * time.Millisecond)
	pane.agent.FlushSession()
	if latest, err := store.LatestID(s.ws.WorkingDir); err != nil || latest != "disk-session" {
		t.Fatalf("precondition: LatestID = %q (err=%v), want the current disk-session", latest, err)
	}
	res, handled, err = s.runSessionCommand(context.Background(), nil, &pane, "resume latest")
	if err != nil || !handled {
		t.Fatalf("resume latest: handled=%v err=%v", handled, err)
	}
	if pane.agent.SessionID != "newer-session" {
		t.Fatalf("resume latest = %q, want newer-session (the current session must be excluded)", pane.agent.SessionID)
	}
	if res.Output != "" || res.Action != "" || len(res.History) != 0 {
		t.Fatalf("resume latest result = %+v, want empty (the attach payload was already sent)", res)
	}
}

// TestSessionResumeOfOpenPaneDoesNotSwitch pins the multi-pane rule enforced
// server-side: a resume resolving to a session already open as a background
// pane of the SAME connection must not re-key the current pane onto it (one
// session in two panes desyncs both). The connection's pane stays put and the
// reply points at the pane that already owns the session. A passive
// (approval-only) attachment is not a pane and still takes the normal switch.
func TestSessionResumeOfOpenPaneDoesNotSwitch(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	sidA := a.SessionID
	pane := s.registry.first()
	ws := &wsConn{} // stand-in socket: writes are no-ops (nil conn)

	rtB, _ := s.registerSeededSession(nil, "")
	sidB := rtB.agent.SessionID
	if !rtB.attach(ws) {
		t.Fatal("attach B as a viewer")
	}
	res, handled, err := s.runSessionCommand(context.Background(), ws, &pane, "resume "+sidB)
	if err != nil || !handled {
		t.Fatalf("resume open pane: handled=%v err=%v", handled, err)
	}
	if pane.agent.SessionID != sidA {
		t.Fatalf("resume re-keyed the pane onto an already-open session: %q (want %q)", pane.agent.SessionID, sidA)
	}
	if res.Output != "" || res.Action != "" || len(res.History) != 0 {
		t.Fatalf("open-pane resume result = %+v, want empty (the focus-pane reply was written directly)", res)
	}
	if !rtB.viewedBy(ws) {
		t.Fatal("open-pane resume detached the target pane")
	}

	rtC, _ := s.registerSeededSession(nil, "")
	if !rtC.attachPassive(ws) {
		t.Fatal("attach C passively")
	}
	if _, handled, err := s.runSessionCommand(context.Background(), ws, &pane, "resume "+rtC.agent.SessionID); err != nil || !handled {
		t.Fatalf("resume passive: handled=%v err=%v", handled, err)
	}
	if pane.agent.SessionID != rtC.agent.SessionID {
		t.Fatalf("resume of a passively-attached session did not switch: pane=%q want %q", pane.agent.SessionID, rtC.agent.SessionID)
	}
}

func TestSessionForkSourceUntouched(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	origID := a.SessionID
	pane := s.registry.first()

	// Give the pane a conversation (user + assistant).
	rt := pane
	if _, err := rt.agent.StreamProcessInput(context.Background(), "hello", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	srcCount := rt.agent.MessageCount()

	res, handled, err := s.runSessionCommand(context.Background(), nil, &pane, "fork last")
	if err != nil || !handled {
		t.Fatalf("fork: handled=%v err=%v", handled, err)
	}
	if pane.agent.SessionID == origID {
		t.Fatal("fork did not create a new session")
	}
	if res.Action != agent.SessionActionClearChat {
		t.Fatalf("fork action = %q, want clear_chat", res.Action)
	}
	if got := pane.agent.MessageCount(); got != srcCount {
		t.Fatalf("forked message count = %d, want %d", got, srcCount)
	}
	// Source session untouched: still registered, messages intact.
	src, ok := s.registry.get(origID)
	if !ok {
		t.Fatal("source session was evicted by fork")
	}
	if src.agent.MessageCount() != srcCount {
		t.Fatalf("source message count changed by fork: %d → %d", srcCount, src.agent.MessageCount())
	}
}

// TestSessionAttachLoadsInactiveSession verifies session_attach:
// attaching a saved-but-inactive session loads it into the registry, switches
// the connection's pane to it, and re-sends its state.
func TestSessionAttachLoadsInactiveSession(t *testing.T) {
	s, _, store := newLifecycleServer(t)
	store.Save("disk-session", agent.SessionSnapshot{
		WorkingDir: s.ws.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "saved"}},
	})
	if _, ok := s.registry.get("disk-session"); ok {
		t.Fatal("disk-session should not be registered yet")
	}

	srv := startWSServer(t, s)
	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: "disk-session"}); err != nil {
		t.Fatalf("send attach: %v", err)
	}
	// Attach reply: session_state, then config + history for the session.
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if state.SessionID != "disk-session" {
		t.Fatalf("session_state sessionId = %q, want disk-session", state.SessionID)
	}
	// Attach reply: session_state, then config + history. The history is
	// snapshotted and sent as soon as it is available — never waiting on a
	// running turn's turnMu (a turn holds the lock for its entire duration,
	// and the handshake must not block on it) — so config and history may
	// arrive in either order. The full config (context stats) can trail both.
	var cfg WSMessage
	var hist WSMessage
	for gotCfg, gotHist := false, false; !gotCfg || !gotHist; {
		m := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
			return m.Type == "config" || m.Type == "history"
		})
		switch m.Type {
		case "config":
			if !gotCfg {
				cfg = m
				gotCfg = true
			}
		case "history":
			if !gotHist {
				hist = m
				gotHist = true
			}
		}
	}
	if cfg.SessionID != "disk-session" {
		t.Fatalf("config sessionId = %q, want disk-session", cfg.SessionID)
	}
	if len(hist.History) != 1 {
		t.Fatalf("history = %d entries, want 1", len(hist.History))
	}
	rt, ok := s.registry.get("disk-session")
	if !ok {
		t.Fatal("session_attach did not register the loaded session")
	}
	if rt.clientCount() != 1 {
		t.Fatalf("attached clients = %d, want 1", rt.clientCount())
	}
	// The connection's pane is now disk-session: a plain message (no
	// sessionId) routes to it.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "user_acked" })
	if got := userMessageCount(rt.agent); got != 2 {
		t.Fatalf("disk-session user message count = %d, want 2 (message routed to attached pane)", got)
	}
	// Drain the turn before returning: user_acked fires while the turn is
	// still running, and its final FlushSession writes the session file —
	// without this wait that write races t.TempDir's RemoveAll cleanup
	// (the flaky "directory not empty" failure). turn_end is broadcast
	// after StreamProcessInput returns, so it bounds all of the turn's
	// persistence writes.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == rt.agent.SessionID
	})
}

// TestSessionAttachUnknownSessionRemoved verifies that attaching a session
// that exists neither in the registry nor on disk yields session_removed so
// the client can drop the pane.
func TestSessionAttachUnknownSessionRemoved(t *testing.T) {
	s, _, _ := newLifecycleServer(t)
	srv := startWSServer(t, s)
	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: "no-such-session"}); err != nil {
		t.Fatalf("send attach: %v", err)
	}
	removed := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_removed" })
	if removed.SessionID != "no-such-session" {
		t.Fatalf("session_removed sessionId = %q, want no-such-session", removed.SessionID)
	}
}

// TestShutdownSessionsFlushesAll verifies shutdown: every registered
// session agent is flushed, so a multi-session web server exits without
// losing in-memory state.
func TestShutdownSessionsFlushesAll(t *testing.T) {
	s, _, store := newLifecycleServer(t)
	pane := s.registry.first()

	// Two live sessions, each with one turn's messages.
	if _, err := pane.agent.StreamProcessInput(context.Background(), "first session msg", nil); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	firstID := pane.agent.SessionID
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := pane.agent.StreamProcessInput(context.Background(), "second session msg", nil); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	secondID := pane.agent.SessionID

	s.ShutdownSessions()

	for _, id := range []string{firstID, secondID} {
		snap, err := store.LoadInWorkingDir(s.ws.WorkingDir, id)
		if err != nil {
			t.Fatalf("session %s not flushed: %v", id, err)
		}
		if len(snap.Messages) == 0 {
			t.Fatalf("session %s flushed with no messages", id)
		}
	}
}

// TestWorkingDirChangeSyncsAllSessions verifies E12: a workspace working-dir
// change is applied to every session agent (shared executor + per-agent
// WorkingDir), serialized under each session's turn lock.
func TestWorkingDirChangeSyncsAllSessions(t *testing.T) {
	s, _, _ := newLifecycleServer(t)
	pane := s.registry.first()
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new: %v", err)
	}
	ids := s.registry.activeIDs()
	if len(ids) != 2 {
		t.Fatalf("active sessions = %v, want 2", ids)
	}

	newDir := t.TempDir()
	s.applyWorkingDirToAll(newDir)
	for _, id := range ids {
		rt, ok := s.registry.get(id)
		if !ok {
			t.Fatalf("session %s missing", id)
		}
		if rt.agent.WorkingDir != newDir {
			t.Fatalf("session %s WorkingDir = %q, want %q", id, rt.agent.WorkingDir, newDir)
		}
		if rt.agent.Executor.GetWorkingDir() != newDir {
			t.Fatalf("session %s executor dir = %q, want %q", id, rt.agent.Executor.GetWorkingDir(), newDir)
		}
	}
}

// TestTypedNewOverWS verifies the wire protocol for a typed /new: the client
// receives clear_chat, then a config whose sessionId is the NEW session, and
// a subsequent message routes to the new default session.
func TestTypedNewOverWS(t *testing.T) {
	dir := t.TempDir()
	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	s := NewServer(a, &config.Config{})
	origID := a.SessionID
	srv := startWSServer(t, s)

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "/new"}); err != nil {
		t.Fatalf("send /new: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	cfg := configAfterClear(t, conn, origID)
	if _, ok := s.registry.get(cfg.SessionID); !ok {
		t.Fatal("typed /new did not register the new session")
	}
	if s.registry.first().agent.SessionID != cfg.SessionID {
		t.Fatal("typed /new did not make the new session the default")
	}
	// A subsequent plain message goes to the new session.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	acked := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "user_acked" })
	if acked.SessionID != cfg.SessionID {
		t.Fatalf("user_acked sessionId = %q, want %q (message routed to new default)", acked.SessionID, cfg.SessionID)
	}
	rt, _ := s.registry.get(cfg.SessionID)
	// Count USER messages only: the user message is appended before
	// user_acked fires, but the assistant reply is appended asynchronously by
	// the stream goroutine, so MessageCount() races it (it may already
	// include the mock's "ok" reply by the time we read it).
	if got := userMessageCount(rt.agent); got != 1 {
		t.Fatalf("new session user message count = %d, want 1 (message routed to new default)", got)
	}
	// Drain the turn before returning: user_acked fires while the turn is
	// still running, and its final FlushSession writes the session file —
	// without this wait that write races t.TempDir's RemoveAll cleanup
	// (the flaky "directory not empty" failure). turn_end is broadcast
	// after StreamProcessInput returns, so it bounds all of the turn's
	// persistence writes.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == cfg.SessionID
	})
}

// userMessageCount returns the number of user-role messages in the agent's
// conversation. Unlike MessageCount, it is stable immediately after a
// user_acked frame: StreamProcessInput appends the user message before
// OnStart fires, while the assistant reply is appended later by the turn's
// stream goroutine.
func userMessageCount(a *agent.Agent) int {
	n := 0
	for _, m := range a.SnapshotMessages() {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

func TestSessionDeleteEvictsAndReplacesCurrent(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	origID := a.SessionID
	pane := s.registry.first()

	// Seed a second active session via /new (the pane switches to it).
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new: %v", err)
	}
	secondID := pane.agent.SessionID

	// Delete origID (not the pane's current session) → plain delete.
	res, handled, err := s.runSessionCommand(context.Background(), nil, &pane, "resume del "+origID)
	if err != nil || !handled {
		t.Fatalf("delete inactive: handled=%v err=%v", handled, err)
	}
	if res.Action != agent.SessionActionNone {
		t.Fatalf("delete non-current action = %q, want none", res.Action)
	}
	if _, ok := s.registry.get(origID); ok {
		t.Fatal("deleted session still registered")
	}
	if _, err := store.LoadInWorkingDir(s.ws.WorkingDir, origID); err == nil {
		t.Fatal("deleted session file still exists")
	}
	if pane.agent.SessionID != secondID {
		t.Fatalf("pane session = %q, want %q (delete of non-current must not switch)", pane.agent.SessionID, secondID)
	}

	// Delete the pane's current session → a fresh one is started (mirrors
	// the single-agent "was current" behavior).
	res, handled, err = s.runSessionCommand(context.Background(), nil, &pane, "resume del "+secondID)
	if err != nil || !handled {
		t.Fatalf("delete current: handled=%v err=%v", handled, err)
	}
	if res.Action != agent.SessionActionClearChat {
		t.Fatalf("delete current action = %q, want clear_chat", res.Action)
	}
	if pane.agent.SessionID == secondID {
		t.Fatal("deleting the current session did not start a new one")
	}
	if _, ok := s.registry.get(secondID); ok {
		t.Fatal("deleted current session still registered")
	}
}

// TestEnsureSessionRuntimeRegisteredSetsDefault verifies that attaching an
// ALREADY-REGISTERED session (the sidebar session list re-attaches on every
// focus) moves it to the front of the registration order. Pre-fix,
// ensureSessionRuntime's early return skipped setDefault, so the default —
// the target for messages without an explicit sessionId and the first
// eviction candidate — stayed on the last session_new/resume/fork target.
// That stale default could route an id-less pane's toolbar action to the
// WRONG session and cancel its running turn via the cancel-then-lock
// handlers, and eviction could target a session the user just
// opened while an untouched one stayed protected.
func TestEnsureSessionRuntimeRegisteredSetsDefault(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	origID := a.SessionID
	pane := s.registry.first()

	// Create session B via /new: it becomes the default (order front).
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new: %v", err)
	}
	sidB := pane.agent.SessionID
	if s.registry.first().agent.SessionID != sidB {
		t.Fatal("setup: /new should make B the default")
	}

	// Attach the ORIGINAL session (registered — the sidebar session-list focus).
	rt, err := s.ensureSessionRuntime(origID)
	if err != nil {
		t.Fatalf("ensureSessionRuntime(orig): %v", err)
	}
	if rt.agent.SessionID != origID {
		t.Fatalf("ensureSessionRuntime returned %q, want %q", rt.agent.SessionID, origID)
	}
	// The just-attached session must now be the default (front of order).
	if s.registry.first().agent.SessionID != origID {
		t.Fatalf("default after attach = %q, want %q (attach of a registered session must move it to the front)",
			s.registry.first().agent.SessionID, origID)
	}
	// And a subsequent attach of B moves B to the front again.
	if _, err := s.ensureSessionRuntime(sidB); err != nil {
		t.Fatalf("ensureSessionRuntime(B): %v", err)
	}
	if s.registry.first().agent.SessionID != sidB {
		t.Fatalf("default after attach B = %q, want %q", s.registry.first().agent.SessionID, sidB)
	}
}

// TestDeleteFailureKeepsRuntimeRegistered verifies that a FAILED file delete
// leaves the session live: the runtime stays registered and the pane keeps
// pointing at it, so the error surfaces cleanly instead of leaving the pane
// on an evicted runtime whose next message would turn into an invisible
// headless turn. Pre-fix, sessionDelete evicted the runtime BEFORE
// Store.Delete, so a delete error stranded the pane on an unregistered agent
// (invisible to cancel/prune/shutdown).
func TestDeleteFailureKeepsRuntimeRegistered(t *testing.T) {
	dir := t.TempDir()
	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr)
	store := session.NewStoreWithOptions(false, session.StoreOptions{}) // disabled: Store.Delete will fail
	a.SessionStore = store
	s := NewServer(a, &config.Config{})
	origID := a.SessionID
	pane := s.registry.first()

	_, handled, err := s.runSessionCommand(context.Background(), nil, &pane, "resume del "+origID)
	if err == nil || !handled {
		t.Fatalf("delete with disabled store: handled=%v err=%v, want an error", handled, err)
	}
	// The runtime and the pane must be untouched by the failed delete.
	if pane.agent.SessionID != origID {
		t.Fatalf("pane session = %q, want %q (failed delete hijacked the pane)", pane.agent.SessionID, origID)
	}
	if _, ok := s.registry.get(origID); !ok {
		t.Fatal("runtime was evicted by a FAILED delete")
	}
}

// TestPruneKeepsEvictedSession verifies E2/E11: a session evicted by the
// registry cap is flushed before eviction, and the prune that runs as a side
// effect of the SAME create (session_new/fork) must not delete its freshly
// flushed file — the client's pane was told session_detached ("resume to
// continue") and must be able to re-attach it from the store. Pre-fix, the
// evicted id dropped out of activeIDs() before pruneSessions ran, so an
// over-budget store pruned the evicted session's file away immediately.
func TestPruneKeepsEvictedSession(t *testing.T) {
	dir := t.TempDir()
	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{MaxCount: 2})
	a.SessionStore = store
	s := NewServer(a, &config.Config{WebMaxActiveSessions: 2})
	origID := a.SessionID
	pane := s.registry.first()

	// Give the default session a persisted file (one turn + shutdown flush).
	if _, err := pane.agent.StreamProcessInput(context.Background(), "hello", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	s.ShutdownSessions()
	if _, err := store.LoadInWorkingDir(s.ws.WorkingDir, origID); err != nil {
		t.Fatalf("setup: default session not persisted: %v", err)
	}

	// /new → B: no eviction (cap 2, only the default registered). The prune
	// runs with the full active set [B, orig]; the store is over budget (3
	// files, cap 2), so the non-active extras (disk-1/disk-2) are dropped.
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new: %v", err)
	}
	sidB := pane.agent.SessionID

	// /new → C: evicts the oldest idle non-default session. After the first
	// /new the registration order is [B, orig] (B is the default), so orig
	// is the victim.
	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "/new"); err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, ok := s.registry.get(origID); ok {
		t.Fatal("setup: orig should have been evicted by the second /new")
	}
	if _, ok := s.registry.get(sidB); !ok {
		t.Fatal("setup: B should still be registered")
	}

	// The evicted session's file must survive the prune that ran as a side
	// effect of the create that evicted it.
	if _, err := store.LoadInWorkingDir(s.ws.WorkingDir, origID); err != nil {
		t.Fatalf("evicted session's file was pruned away: %v", err)
	}
}

// TestAttachDoesNotWipeSessionFile pins the reported bug: closing an open
// session turned its sidebar title into a hash. The root cause was in the
// session agent factory: NewSessionAgent seeded the workspace thinking level
// BEFORE RestoreSessionLocal, and SetThinkingLevel flushes — so merely
// OPENING a saved session (attach/resume) wrote an EMPTY snapshot over its
// real file (messages wiped, index label wiped). The restore then repopulated
// memory, but nothing re-saved until the next turn, so a list right after
// detach showed the session without a title (and a quit before the next flush
// lost the messages entirely). The factory must never flush before the
// restore; the level seed now runs after it (and only when the snapshot has
// no saved level).
func TestAttachDoesNotWipeSessionFile(t *testing.T) {
	s, _, store := newLifecycleServer(t)
	saved := agent.SessionSnapshot{
		WorkingDir: s.ws.WorkingDir,
		Model:      "m1",
		Mode:       "act",
		Label:      "My Title",
		Messages: []llm.Message{
			{Role: "user", Content: "first message"},
			{Role: "assistant", Content: "ok"},
		},
	}
	if err := store.Save("disk-session", saved); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Attach (loads the session through the workspace factory), then detach —
	// exactly the open-then-close flow from the sidebar.
	rt, err := s.ensureSessionRuntime("disk-session")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if rt.agent.MessageCount() != 2 {
		t.Fatalf("attached message count = %d, want 2", rt.agent.MessageCount())
	}
	rt.detach(nil)

	// The on-disk state must be untouched: messages and label still there,
	// and the sidebar list still carries the title.
	snap, err := store.LoadInWorkingDir(s.ws.WorkingDir, "disk-session")
	if err != nil {
		t.Fatalf("load after attach+detach: %v", err)
	}
	if len(snap.Messages) != 2 {
		t.Fatalf("messages after attach+detach = %d, want 2 (attach wiped the file)", len(snap.Messages))
	}
	if snap.Label != "My Title" {
		t.Fatalf("label after attach+detach = %q, want %q", snap.Label, "My Title")
	}
	_, sessions, err := rt.agent.FormatSessionListForUI()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, si := range sessions {
		if si.ID == "disk-session" {
			found = true
			if si.Label != "My Title" {
				t.Fatalf("sidebar label = %q, want %q (title became a hash)", si.Label, "My Title")
			}
		}
	}
	if !found {
		t.Fatal("session missing from the sidebar list after attach+detach")
	}
}

// TestAttachRestoresSavedThinkingLevel verifies that a resumed session keeps
// its saved thinking level (the workspace default seed must not override it —
// the seed now only applies when the snapshot has no level).
func TestAttachRestoresSavedThinkingLevel(t *testing.T) {
	s, _, store := newLifecycleServer(t)
	if err := store.Save("disk-session", agent.SessionSnapshot{
		WorkingDir:    s.ws.WorkingDir,
		ThinkingLevel: "high",
		Messages:      []llm.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Make the workspace default different, to prove it does not win.
	s.ws.ThinkingLevel = "off"
	rt, err := s.ensureSessionRuntime("disk-session")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := string(rt.agent.ThinkingLevel); got != "high" {
		t.Fatalf("restored thinking level = %q, want %q (workspace default overrode the saved level)", got, "high")
	}
}

// TestForkInheritsSourceState verifies that a web fork carries the source
// session's mode, thinking level, and todos — the TUI fork keeps them
// (it replaces messages on the same agent), and the web fork must mirror
// that instead of silently starting a fresh act-mode session with empty
// todos and the default thinking level.
func TestForkInheritsSourceState(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	a.TodoManager = agent.NewTodoManager(s.ws.WorkingDir)
	pane := s.registry.first()

	if _, err := pane.agent.StreamProcessInput(context.Background(), "hello", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	pane.agent.SetMode(agent.ModePlan)
	pane.agent.SetThinkingLevel(agent.ThinkingHigh)
	if _, err := pane.agent.TodoManager.AddTodo("do something"); err != nil {
		t.Fatalf("todo: %v", err)
	}

	if _, _, err := s.runSessionCommand(context.Background(), nil, &pane, "fork last"); err != nil {
		t.Fatalf("fork: %v", err)
	}
	if got := pane.agent.Mode.String(); got != "plan" {
		t.Fatalf("fork mode = %q, want %q (source was plan mode)", got, "plan")
	}
	if got := string(pane.agent.ThinkingLevel); got != "high" {
		t.Fatalf("fork thinking level = %q, want %q", got, "high")
	}
	if pane.agent.TodoManager.Empty() {
		t.Fatal("fork lost the source's todos")
	}
	// The source is untouched.
	src, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("source session was evicted by fork")
	}
	if m, _ := src.agent.ModeAndThinkingLevel(); m.String() != "plan" {
		t.Fatalf("source mode changed by fork: %q", m.String())
	}
}

// TestDeleteSessionWithRunningTurnNoResurrection verifies E10 + the delete
// resurrection fix: deleting a session with an in-flight turn cancels +
// drains the turn, and the deleted file must not come back (a late turn
// flush, or a new turn started in the drain→evict window, re-creating it).
func TestDeleteSessionWithRunningTurnNoResurrection(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()
	sid := a.SessionID

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// conn1 starts a turn that blocks in the provider.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "in-flight", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// conn2 deletes the session while the turn is in flight: the delete
	// cancels the turn (the stub returns on ctx cancellation), drains it,
	// and removes the file.
	if err := conn2.WriteJSON(WSMessage{Type: "session_delete", SessionID: sid}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	readUntil(t, conn2, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "session_removed" && m.SessionID == sid
	})
	waitFor(t, 10*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})

	// The file must be gone — the drained turn must not resurrect it.
	if _, err := store.LoadInWorkingDir(dir, sid); err == nil {
		t.Fatal("deleted session file was resurrected by the running turn")
	}
}

// TestSessionDeleteBackgroundPaneDetaches verifies that deleting a session
// which is a BACKGROUND pane of the connection (not the current *pane) still
// detaches the socket from the deleted runtime. Pre-fix, the `if *pane == rt`
// guard skipped the detach for background panes, leaving the live socket in
// the evicted runtime's clients set until connection teardown (a per-delete
// leak that kept the runtime referenced).
func TestSessionDeleteBackgroundPaneDetaches(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sidA := cfg.SessionID
	rtA, ok := s.registry.get(sidA)
	if !ok {
		t.Fatal("default session not registered")
	}
	if rtA.clientCount() != 1 {
		t.Fatalf("default session clientCount = %d, want 1 (this connection)", rtA.clientCount())
	}

	// Typed /new switches the connection's pane to a fresh session; the old
	// session stays attached as a background pane.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "/new", SessionID: sidA}); err != nil {
		t.Fatalf("send /new: %v", err)
	}
	clear := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	sidB := clear.SessionID
	if sidB == "" || sidB == sidA {
		t.Fatalf("clear_chat sessionId = %q, want the new pane's id", sidB)
	}
	if rtA.clientCount() != 1 {
		t.Fatalf("background session clientCount = %d, want 1 (still attached)", rtA.clientCount())
	}

	// Delete the BACKGROUND session: the connection must be detached from it
	// even though it is not the current pane.
	if err := conn.WriteJSON(WSMessage{Type: "session_delete", SessionID: sidA}); err != nil {
		t.Fatalf("send delete: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_removed" && m.SessionID == sidA
	})
	waitFor(t, 5*time.Second, func() bool { return rtA.clientCount() == 0 })
	if _, ok := s.registry.get(sidA); ok {
		t.Fatal("deleted background session still registered")
	}
	if _, err := store.LoadInWorkingDir(s.ws.GetWorkingDir(), sidA); err == nil {
		t.Fatal("deleted background session file still exists")
	}
	_ = a
}

// TestSessionDeleteDetachesOtherTabs verifies fix A: deleting a session that
// ANOTHER connection is attached to detaches that connection's socket from
// the deleted runtime. Pre-fix, sessionDelete only detached the requesting
// connection; the other tab's socket lingered in the deleted runtime's
// clients set until its own teardown — and teardown's detachAll only sweeps
// REGISTERED sessions, so it would never have been cleaned up.
func TestSessionDeleteDetachesOtherTabs(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	rt, ok := s.registry.get(sid)
	if !ok {
		t.Fatal("session not registered")
	}
	if rt.clientCount() != 2 {
		t.Fatalf("clients attached = %d, want 2 (both tabs)", rt.clientCount())
	}

	// conn1 deletes the session (its current pane → a fresh session starts).
	if err := conn1.WriteJSON(WSMessage{Type: "session_delete", SessionID: sid}); err != nil {
		t.Fatalf("send delete: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})
	// Both connections' sockets must be detached from the deleted runtime —
	// no stale attachment for teardown's detachAll to miss.
	if rt.clientCount() != 0 {
		t.Fatalf("deleted runtime still has %d attached clients", rt.clientCount())
	}
	// conn2 also receives session_removed so its client drops the pane.
	removed := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_removed" && m.SessionID == sid
	})
	if removed.SessionID != sid {
		t.Fatalf("session_removed sessionId = %q, want %q", removed.SessionID, sid)
	}
}

// TestIdlessMessageAfterPaneDeletedRoutesToDefault verifies the read-loop
// fallback (fix A): when the connection's pane references a runtime that was
// evicted (its session was deleted by another tab), an id-less message must
// not be silently dropped on the evicted runtime — it falls back to the
// default session, the same target a fresh connection gets. The client
// re-keys the pane (session_new / session_attach) and the next explicit-id
// message re-aligns it.
func TestIdlessMessageAfterPaneDeletedRoutesToDefault(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// conn1 deletes the session — conn2's pane now references an evicted
	// runtime.
	if err := conn1.WriteJSON(WSMessage{Type: "session_delete", SessionID: sid}); err != nil {
		t.Fatalf("send delete: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})

	// The delete (wasCurrent) started a fresh default session.
	defaultRT := s.registry.first()
	if defaultRT == nil || defaultRT.agent.SessionID == sid {
		t.Fatal("no live default session after delete")
	}

	// conn2 sends an id-less message: it must reach the live default, not
	// vanish on the evicted pane.
	if err := conn2.WriteJSON(WSMessage{Type: "message", Content: "after delete"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	if active, _ := defaultRT.turnState(); !active {
		t.Fatal("id-less message after pane eviction did not start a turn on the default session")
	}
}
