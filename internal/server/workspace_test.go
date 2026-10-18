package server

import (
	"testing"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/modelinfo"
	"gogen/internal/session"
)

// TestWorkspaceDefaultModelAccessors pins the mutex-guarded handoff the web
// startup validation goroutine uses to publish the resolved model: updates
// are visible to DefaultModel (including the cleared "" state), and
// Server.SetDefaultModel reaches the workspace.
func TestWorkspaceDefaultModelAccessors(t *testing.T) {
	dir := t.TempDir()
	stub := &blockingStub{}
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := newTestServer(t, a, &config.Config{})

	// Initial value is the agent's model at construction.
	if got := s.ws.DefaultModel(); got != "blocking-model" {
		t.Fatalf("initial DefaultModel = %q, want blocking-model", got)
	}
	// The validation handoff updates it (e.g. auto-selected sole model)...
	s.SetDefaultModel("sole-model")
	if got := s.ws.DefaultModel(); got != "sole-model" {
		t.Fatalf("DefaultModel after SetDefaultModel = %q, want sole-model", got)
	}
	// ...and the cleared state (stale restored model invalidated) propagates
	// so new sessions do not seed the invalid model.
	s.SetDefaultModel("")
	if got := s.ws.DefaultModel(); got != "" {
		t.Fatalf("DefaultModel after clear = %q, want empty", got)
	}
}

// TestProviderFactorySeedsResolvedDefaultModel verifies the provider factory
// reads the workspace default model through the synchronized accessor: a
// session created while the startup validation goroutine publishes the
// resolved model seeds from it, and the cleared "" state falls back to the
// config seed.
func TestProviderFactorySeedsResolvedDefaultModel(t *testing.T) {
	dir := t.TempDir()
	prov := llm.NewOpenAIProviderWithResolver("test-key", "", "http://127.0.0.1:9/v1", dir, modelinfo.NewResolver(""))
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := newTestServer(t, a, &config.Config{OpenAIModel: "cfg-model"})

	// Workspace starts with no model (the agent provider has none selected).
	if got := s.ws.DefaultModel(); got != "" {
		t.Fatalf("initial DefaultModel = %q, want empty", got)
	}
	// Validation auto-selected a sole model: new providers seed from it.
	s.SetDefaultModel("sole-model")
	if got := s.ws.ProviderFactory().ModelName(); got != "sole-model" {
		t.Fatalf("factory provider model = %q, want sole-model", got)
	}
	// Validation cleared the model: the factory falls back to the config seed.
	s.SetDefaultModel("")
	if got := s.ws.ProviderFactory().ModelName(); got != "cfg-model" {
		t.Fatalf("factory provider model after clear = %q, want cfg-model (config seed)", got)
	}
}
