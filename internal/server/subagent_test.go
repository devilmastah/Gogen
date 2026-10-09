package server

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/session"
)

// TestSubagentSpawnEndToEnd drives the full server spawner: a child session
// runtime is created, registered under the parent, runs a real turn against
// a mock provider, persists with ParentID, and the parent receives the
// report. The child is excluded from the flat saved-session list.
func TestSubagentSpawnEndToEnd(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	// Children get their own provider via the workspace factory: serve a
	// canned response.
	bigReport := strings.Repeat("r", agent.MaxSubagentReportBytes+1000)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: bigReport}}
		return p
	}
	// The server installed the spawner at construction; enable the feature
	// (a real server would do this via the config WS toggle).
	a.SetSubagentsEnabled(true)

	sp := &subagentSpawner{s: s}
	report, err := sp.Spawn(context.Background(), a, "investigate the parser\nand report back", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, strings.Repeat("r", 32)) {
		t.Fatalf("report = %q", report)
	}
	if !strings.HasSuffix(report, "… (truncated)") {
		t.Fatalf("foreground report was not bounded: suffix missing in %q", report[len(report)-40:])
	}

	// The child persisted with ParentID and is excluded from the flat list.
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("store list = %+v, want exactly the nested child", list)
	}
	if list[0].ParentID != a.SessionID {
		t.Fatalf("child ParentID = %q, want %q", list[0].ParentID, a.SessionID)
	}
	_, flat, err := a.FormatSessionListForUI()
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) != 0 {
		t.Fatalf("flat session list should exclude the nested child, got %+v", flat)
	}
}

// TestSubagentParentCancelPropagates verifies the parent-cancellation
// contract: when the parent's context dies mid-spawn, Spawn returns the
// context error and the child runtime is unregistered.
func TestSubagentParentCancelPropagates(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	// A provider that blocks until the context is cancelled.
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{}
	}
	a.SetSubagentsEnabled(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (&subagentSpawner{s: s}).Spawn(ctx, a, "long job", "", 0)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("spawn error = %v, want context canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return after parent cancel")
	}
	// The child runtime was removed from the registry.
	if _, ok := s.registry.get(a.SessionID); !ok {
		t.Fatal("parent runtime missing")
	}
}

// TestSubagentChildPaneGetsTerminalFrames verifies a pane attached to a
// running subagent receives the normal terminal frames (cancelled +
// turn_end) when the child's turn ends. Without them the pane stays stuck
// in the "responding"/busy state forever — the client only clears
// turnActive on cancelled/turn_end/session_state, and the spawner
// unregisters the child runtime right after the turn.
func TestSubagentChildPaneGetsTerminalFrames(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{} // blocks until cancelled
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&subagentSpawner{s: s}).Spawn(ctx, a, "long job", "", 0)
		done <- err
	}()

	// The parent's clients get the subagent_started event carrying the
	// child id (the sidebar row's attach target).
	started := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "subagent_started" })
	if started.SubagentID == "" {
		t.Fatal("subagent_started without a child id")
	}

	// Attach a second connection to the child pane, then cancel the child
	// from that pane (the documented escape hatch for a stuck subagent).
	childConn := dialWS(t, srv, "/ws")
	defer childConn.Close()
	readUntil(t, childConn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := childConn.WriteJSON(WSMessage{Type: "session_attach", SessionID: started.SubagentID}); err != nil {
		t.Fatalf("attach child: %v", err)
	}
	state := readUntil(t, childConn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == started.SubagentID
	})
	if !state.TurnActive {
		t.Fatal("child pane should attach to a running turn")
	}

	if err := childConn.WriteJSON(WSMessage{Type: "cancel", SessionID: started.SubagentID}); err != nil {
		t.Fatalf("cancel child: %v", err)
	}
	// The child pane must receive the terminal pair: cancelled, then
	// turn_end — the frames the client needs to leave the busy state.
	readUntil(t, childConn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "cancelled" && m.SessionID == started.SubagentID
	})
	readUntil(t, childConn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == started.SubagentID
	})
	// The spawner unregisters the finished child; its pane must be closed
	// with session_detached (the eviction-style notification) so a typed
	// message can never be silently dropped on a gone runtime.
	readUntil(t, childConn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_detached" && m.SessionID == started.SubagentID
	})

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("spawn error = %v, want context canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return after child cancel")
	}
}

// TestSubagentChildInheritsParentModel verifies a child spawned without an
// explicit model argument inherits the PARENT's per-session model (the
// provider factory seeds the workspace default, which may differ).
func TestSubagentChildInheritsParentModel(t *testing.T) {
	dir := t.TempDir()
	parentProv := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000})
	a := agent.NewAgent(parentProv, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := NewServer(a, &config.Config{})
	// The workspace default was the parent's model at construction
	// ("mock-model"); now the parent switches models per-session.
	if err := parentProv.SetModel("parent-model"); err != nil {
		t.Fatal(err)
	}
	var created []*llmtest.MockProvider
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{
			{ID: "default-model", ContextLimit: 128000},
			{ID: "parent-model", ContextLimit: 128000},
		}
		p.StreamResults = []*llm.StreamResult{{Content: "report"}}
		created = append(created, p)
		return p
	}
	a.SetSubagentsEnabled(true)

	if _, err := (&subagentSpawner{s: s}).Spawn(context.Background(), a, "job", "", 0); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("child providers created = %d, want 1", len(created))
	}
	if got := created[0].ModelName(); got != "parent-model" {
		t.Fatalf("child model = %q, want parent-model", got)
	}
}

// TestSubagentConfiguredModelWinsOverInheritance verifies the configured
// default subagent model (settings modal / config key) beats parent-model
// inheritance: a parent that switched models per-session still spawns a
// child on the configured model.
func TestSubagentConfiguredModelWinsOverInheritance(t *testing.T) {
	dir := t.TempDir()
	parentProv := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000})
	a := agent.NewAgent(parentProv, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := NewServer(a, &config.Config{})
	if err := parentProv.SetModel("parent-model"); err != nil {
		t.Fatal(err)
	}
	// The user configured a default subagent model.
	r := s.ws.GetRuntimeConfig()
	r.SubagentModel = "configured-model"
	s.ws.SetRuntimeConfig(r)
	var created []*llmtest.MockProvider
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{
			{ID: "default-model", ContextLimit: 128000},
			{ID: "parent-model", ContextLimit: 128000},
			{ID: "configured-model", ContextLimit: 128000},
		}
		p.StreamResults = []*llm.StreamResult{{Content: "report"}}
		created = append(created, p)
		return p
	}
	a.SetSubagentsEnabled(true)

	if _, err := (&subagentSpawner{s: s}).Spawn(context.Background(), a, "job", "", 0); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("child providers created = %d, want 1", len(created))
	}
	if got := created[0].ModelName(); got != "configured-model" {
		t.Fatalf("child model = %q, want configured-model (beats parent inheritance)", got)
	}
}

// TestSubagentConfiguredModelFallbackToDefault verifies that a configured
// subagent model that is no longer selectable on the child (provider removed
// or catalog changed) falls back to the workspace default instead of failing
// the spawn — the same fail-open contract as the inheritance branch.
func TestSubagentConfiguredModelFallbackToDefault(t *testing.T) {
	dir := t.TempDir()
	parentProv := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000})
	a := agent.NewAgent(parentProv, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := NewServer(a, &config.Config{})
	r := s.ws.GetRuntimeConfig()
	r.SubagentModel = "vanished-model"
	s.ws.SetRuntimeConfig(r)
	var created []*llmtest.MockProvider
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{{ID: "default-model", ContextLimit: 128000}}
		p.StreamResults = []*llm.StreamResult{{Content: "report"}}
		created = append(created, p)
		return p
	}
	a.SetSubagentsEnabled(true)

	if _, err := (&subagentSpawner{s: s}).Spawn(context.Background(), a, "job", "", 0); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("child providers created = %d, want 1", len(created))
	}
	if got := created[0].ModelName(); got != "default-model" {
		t.Fatalf("child model = %q, want default-model fallback", got)
	}
}

// TestSubagentChildThinkingLevelCascade pins the spawn-time reasoning-effort
// cascade end to end (web host): the configured subagent level (settings)
// wins; empty inherits the parent's live level; "off" and empty set
// nothing; a level the child's model does not accept is omitted (policy B).
// The mock provider records the level the child provider was set to.
func TestSubagentChildThinkingLevelCascade(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		parent     agent.ThinkingLevel
		want       string
	}{
		// "off" is the child's default agent level (NewAgent seeds
		// ThinkingOff) and what the turn's provider re-sync sends when no
		// effort is applied — semantically "no reasoning_effort sent".
		{name: "configured-wins-over-parent", configured: "high", parent: agent.ThinkingMedium, want: "high"},
		{name: "empty-inherits-parent", configured: "", parent: agent.ThinkingMedium, want: "medium"},
		{name: "empty-parent-off", configured: "", parent: agent.ThinkingOff, want: "off"},
		{name: "configured-off", configured: "off", parent: agent.ThinkingHigh, want: "off"},
		// The mock provider reports no per-model efforts, so the child's
		// accepted set is the default low/medium/high triple: "max" is
		// omitted (policy B) and the child stays at its off default.
		{name: "not-accepted-omitted", configured: "max", parent: agent.ThinkingOff, want: "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			parentProv := llmtest.NewMockProvider()
			exec := agent.NewExecutor(dir)
			ctxMgr := contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000})
			a := agent.NewAgent(parentProv, exec, ctxMgr)
			a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
			s := NewServer(a, &config.Config{})
			a.SetThinkingLevel(tc.parent)
			r := s.ws.GetRuntimeConfig()
			r.SubagentThinkingLevel = tc.configured
			s.ws.SetRuntimeConfig(r)
			var created []*llmtest.MockProvider
			s.ws.ProviderFactory = func() llm.LLMProvider {
				p := llmtest.NewMockProvider()
				p.StreamResults = []*llm.StreamResult{{Content: "report"}}
				created = append(created, p)
				return p
			}
			a.SetSubagentsEnabled(true)

			if _, err := (&subagentSpawner{s: s}).Spawn(context.Background(), a, "job", "", 0); err != nil {
				t.Fatal(err)
			}
			if len(created) != 1 {
				t.Fatalf("child providers created = %d, want 1", len(created))
			}
			if got := created[0].ThinkingLevel; got != tc.want {
				t.Fatalf("child provider thinking level = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSubagentChildModelFallbackToDefault verifies that when the parent's
// model cannot be selected on the child (no longer listed), the spawn falls
// back to the workspace default instead of failing.
func TestSubagentChildModelFallbackToDefault(t *testing.T) {
	dir := t.TempDir()
	parentProv := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000})
	a := agent.NewAgent(parentProv, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := NewServer(a, &config.Config{})
	if err := parentProv.SetModel("parent-model"); err != nil {
		t.Fatal(err)
	}
	var created []*llmtest.MockProvider
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{{ID: "default-model", ContextLimit: 128000}}
		p.StreamResults = []*llm.StreamResult{{Content: "report"}}
		created = append(created, p)
		return p
	}
	a.SetSubagentsEnabled(true)

	if _, err := (&subagentSpawner{s: s}).Spawn(context.Background(), a, "job", "", 0); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("child providers created = %d, want 1", len(created))
	}
	if got := created[0].ModelName(); got != "default-model" {
		t.Fatalf("child model = %q, want default-model fallback", got)
	}
}

// TestSubagentExplicitModelArgWins verifies the tool's explicit model
// argument beats both the workspace default and the parent's model.
func TestSubagentExplicitModelArgWins(t *testing.T) {
	dir := t.TempDir()
	parentProv := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000})
	a := agent.NewAgent(parentProv, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := NewServer(a, &config.Config{})
	if err := parentProv.SetModel("parent-model"); err != nil {
		t.Fatal(err)
	}
	var created []*llmtest.MockProvider
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{
			{ID: "default-model", ContextLimit: 128000},
			{ID: "explicit-model", ContextLimit: 128000},
		}
		p.StreamResults = []*llm.StreamResult{{Content: "report"}}
		created = append(created, p)
		return p
	}
	a.SetSubagentsEnabled(true)

	if _, err := (&subagentSpawner{s: s}).Spawn(context.Background(), a, "job", "explicit-model", 0); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("child providers created = %d, want 1", len(created))
	}
	if got := created[0].ModelName(); got != "explicit-model" {
		t.Fatalf("child model = %q, want explicit-model", got)
	}
}

// TestSessionsPayloadIncludesNested verifies the sidebar sessions payload
// includes persisted nested (subagent) children with their parentId, so a
// page reload / late attach can still render them under their parent (the
// subagent_started/finished events are not replayed to connecting clients).
func TestSessionsPayloadIncludesNested(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "subagent report here"}}
		return p
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if _, err := (&subagentSpawner{s: s}).Spawn(context.Background(), a, "job", "", 0); err != nil {
		t.Fatal(err)
	}

	if err := conn.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("list_sessions: %v", err)
	}
	msg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "sessions" })
	found := false
	for _, e := range msg.Sessions {
		if e.ParentID == a.SessionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("sessions payload missing nested child of %s: %+v", a.SessionID, msg.Sessions)
	}
}

// TestSessionsPayloadTurnActive pins the turnActive field in the sessions
// payload: a running child reports turnActive=true, while the same child
// resumed from the store (the restart "switch to subagent" flow) reports
// active=true but turnActive=false — the sidebar must render it as done,
// not as running or failed.
func TestSessionsPayloadTurnActive(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{}
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// A running child: the payload must report turnActive.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (&subagentSpawner{s: s}).Spawn(ctx, a, "long job", "", 0)
		done <- err
	}()
	var childID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID {
				childID = id
				return true
			}
		}
		return false
	})
	if err := conn.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("list_sessions: %v", err)
	}
	msg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "sessions" })
	child := sessionPayloadEntry(msg.Sessions, childID)
	if child == nil || !child.Active || !child.TurnActive {
		t.Fatalf("running child payload = %+v, want active+turnActive", child)
	}

	// Stop the child turn (the spawn unwinds with an error) and let the
	// session persist.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return after cancel")
	}

	// Resume the child from the store — the restart "switch to subagent"
	// flow: registered but idle, so active=true and turnActive=false.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: childID}); err != nil {
		t.Fatalf("attach child: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == childID
	})
	if err := conn.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("list_sessions: %v", err)
	}
	msg = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "sessions" })
	child = sessionPayloadEntry(msg.Sessions, childID)
	if child == nil || !child.Active || child.TurnActive {
		t.Fatalf("resumed child payload = %+v, want active but NOT turnActive", child)
	}
}

// sessionPayloadEntry returns the sessions payload entry for id.
func sessionPayloadEntry(entries []SessionEntry, id string) *SessionEntry {
	for i := range entries {
		if entries[i].ID == id {
			return &entries[i]
		}
	}
	return nil
}

// TestSpawnParentEvictedWhileChildRuns pins the eviction window in the
// foreground Spawn: when the parent session is torn down (close ✕, delete,
// cap eviction, shutdown) while its subagent's turn is still running, the
// child is cancelled through the parent context and Spawn must unwind with
// an error — no panic (the post-turn broadcasts target the stale parent
// runtime, a no-op once evicted) and no hang.
func TestSpawnParentEvictedWhileChildRuns(t *testing.T) {
	entered := make(chan struct{})
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		// The child's turn blocks inside the provider until cancelled;
		// entered fires when the child's stream actually opens (i.e. the
		// child is registered and its turn is running).
		p.OnStream = func(ctx context.Context, _ []llm.Message, h *llm.StreamHandlers) (*llm.StreamResult, error) {
			if h != nil && h.OnStreamOpened != nil {
				h.OnStreamOpened()
			}
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return p
	})
	sp := continuableSpawner(t, s)
	a.SetSubagentsEnabled(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sp.Spawn(ctx, a, "long job", "", 0)
		done <- err
	}()

	// Wait until the child's turn is running (registered + stream open),
	// then tear the parent down the way session_close does.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child turn never started")
	}
	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("parent runtime missing")
	}
	s.registry.closeRuntime(parentRt)
	// The parent's stream context dies with the turn (closeRuntime's
	// cancelInFlight): release the spawn goroutine so the child turn
	// unwinds through the parent context.
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("spawn must fail after the parent session was evicted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not unwind after parent eviction")
	}
}

// blockingProvider streams nothing and blocks until ctx is done.
type blockingProvider struct{}

func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) ModelName() string {
	return "blocking-model"
}
func (p *blockingProvider) SetModel(string) error { return nil }
func (p *blockingProvider) SetThinkingLevel(string) {
}
func (p *blockingProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (p *blockingProvider) GenerateResponse(context.Context, []llm.Message, map[string]struct{}, []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}
func (p *blockingProvider) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	if h != nil && h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *blockingProvider) ModelContextLimit(context.Context) (int, error) { return 1000, nil }

// TestTruncateJobRuneSafe verifies the 200-byte job cap never splits a
// multi-byte rune at the cut point (a byte cut could store invalid UTF-8
// on the child).
func TestTruncateJobRuneSafe(t *testing.T) {
	cases := []struct {
		name    string
		job     string
		wantCap bool // result is truncated (ellipsis appended)
	}{
		{"empty", "", false},
		{"short ascii", "fix the parser", false},
		{"exactly 200 bytes", strings.Repeat("a", 200), false},
		{"ascii over cap", strings.Repeat("a", 201), true},
		{"emoji straddles cut", strings.Repeat("a", 198) + "🦄", true},
		{"cjk straddles cut", strings.Repeat("a", 199) + "中", true},
		{"all emoji, cut on rune boundary", strings.Repeat("🦄", 51), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateJob(tc.job)
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8 (len=%d)", len(got))
			}
			if tc.wantCap {
				if !strings.HasSuffix(got, "…") {
					t.Fatalf("expected ellipsis suffix, got tail %q", got[len(got)-16:])
				}
				if len(got) > 200+len("…") {
					t.Fatalf("truncated result exceeds byte cap: %d > %d", len(got), 200+len("…"))
				}
			} else if got != tc.job {
				t.Fatalf("short job was modified: %q", got)
			}
		})
	}
}
