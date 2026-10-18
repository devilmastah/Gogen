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

// TestSetModeAndThinkingLevelViaWS drives set_mode / set_thinking_level /
// set_model through a real WebSocket connection and asserts each returns a
// config reply promptly. This guards the acquireTurnForHandler contract:
// it acquires the session turn lock, so handlers must NOT re-lock it
// (re-locking sync.RWMutex from the same goroutine deadlocks).
func TestSetModeAndThinkingLevelViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()

	// Drain the attach handshake: session_state, then (from the async attach
	// goroutine) the basic config + the full config-with-stats. The handler
	// replies below are therefore unambiguous.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

	if err := conn.WriteJSON(WSMessage{Type: "set_mode", Mode: "plan", SessionID: sid}); err != nil {
		t.Fatalf("send set_mode: %v", err)
	}
	modeCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })
	if modeCfg.Mode != "plan" {
		t.Fatalf("set_mode config Mode = %q, want plan", modeCfg.Mode)
	}

	if err := conn.WriteJSON(WSMessage{Type: "set_thinking_level", ThinkingLevel: "low", SessionID: sid}); err != nil {
		t.Fatalf("send set_thinking_level: %v", err)
	}
	thinkCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })
	if thinkCfg.ThinkingLevel != "low" {
		t.Fatalf("set_thinking_level config ThinkingLevel = %q, want low", thinkCfg.ThinkingLevel)
	}
}

// TestSetModelDoesNotDeadlock verifies the set_model handler's prompt
// reply on the error path (the blocking stub has no models, so SelectModel
// errors) and that the session stays usable afterwards. It guards the
// handler's lock contract (the acquireTurnForHandler handoff) against
// regressions.
func TestSetModelDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	// The blocking stub's provider has no models, so SelectModel errors; the
	// assertion is the no-deadlock/error-notice shape (set_model errors go
	// out on the notice channel, never "response") and that a subsequent
	// session_new still works.
	if err := conn.WriteJSON(WSMessage{Type: "set_model", Model: "nope", SessionID: sid}); err != nil {
		t.Fatalf("send set_model: %v", err)
	}
	// SelectModel on the stub errors ("no models available"); the handler
	// must reply with a model error notice — promptly (no deadlock).
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "model" || resp.Success || resp.Content == "" {
		t.Fatalf("set_model error notice = %+v, want model error", resp)
	}
	// The session must still be usable afterwards.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
}

// TestSetModelIsPerSession verifies the per-session model contract:
// set_model on one session applies only to that session's provider and
// echoes config to that pane only — no cross-pane broadcast, and the
// workspace default (ws.Model) is never mutated. Uses a mock provider whose
// SelectModel succeeds, so the success path is exercised end-to-end.
func TestSetModelIsPerSession(t *testing.T) {
	dir := t.TempDir()
	exec := agent.NewExecutor(dir)
	prov := llmtest.NewMockProvider()
	prov.Models = []llm.ModelInfo{
		{ID: "mock-model", ContextLimit: 128000, Current: true},
		{ID: "m2", ContextLimit: 128000},
	}
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	s := newTestServer(t, a, &config.Config{})
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sidA := s.registry.first().agent.SessionID
	defaultModel := s.ws.Model

	// New pane: session_new switches the connection to a fresh session B and
	// attaches this socket to it, so a cross-pane config broadcast (the
	// rejected workspace-global design) for B would arrive here BEFORE the
	// requesting pane's echo — the first config after set_model discriminates
	// the two designs.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	// Skip any queued configs for A (connect-handshake stats echo) and wait
	// for B's handshake config, so the queue is clean before set_model.
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID != sidA })

	if err := conn.WriteJSON(WSMessage{Type: "set_model", Model: "m2", SessionID: sidA}); err != nil {
		t.Fatalf("send set_model: %v", err)
	}
	echo := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	if echo.SessionID != sidA {
		t.Fatalf("first config after set_model(A) is for session %q, want %q (cross-pane broadcast)", echo.SessionID, sidA)
	}
	if echo.Model != "m2" {
		t.Fatalf("set_model config Model = %q, want m2", echo.Model)
	}
	if s.ws.Model != defaultModel {
		t.Fatalf("workspace default mutated by set_model: %q → %q (per-session D1)", defaultModel, s.ws.Model)
	}
}

// TestWorkspaceNewSessionAgentKeepsSavedModel pins the per-session resume
// rule: a resumed session keeps its SAVED model even when it differs
// from the workspace default, and the async ValidateRestoredModel must not
// clear it (it is listed by the provider).
func TestWorkspaceNewSessionAgentKeepsSavedModel(t *testing.T) {
	dir := t.TempDir()
	exec := agent.NewExecutor(dir)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	ws := &Workspace{
		Exec:       exec,
		Store:      store,
		Config:     &config.Config{WorkingDir: dir},
		WorkingDir: dir,
		GlobalMode: true,
		Model:      "m1", // workspace default — must NOT override the saved model
		ProviderFactory: func() llm.LLMProvider {
			p := llmtest.NewMockProvider()
			_ = p.SetModel("m1")
			p.Models = []llm.ModelInfo{
				{ID: "m1", ContextLimit: 128000, Current: true},
				{ID: "m2", ContextLimit: 128000},
			}
			return p
		},
	}
	id := session.NewID()
	snap := agent.SessionSnapshot{
		WorkingDir:   dir,
		Model:        "m2", // saved model differs from the workspace default
		ContextLimit: 4096,
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}
	a := ws.NewSessionAgent(&snap, id)
	if a.CurrentModel() != "m2" {
		t.Fatalf("resumed provider model = %q, want saved model m2 (per-session D1)", a.CurrentModel())
	}
	// The runtime owner spawns ValidateRestoredModel after registration;
	// run it explicitly here. It must find m2 listed and leave the model
	// alone (the per-session D1 rule).
	a.ValidateRestoredModel(context.Background(), "m2")
	if a.CurrentModel() != "m2" {
		t.Fatalf("provider model changed after validation = %q, want m2", a.CurrentModel())
	}
}
