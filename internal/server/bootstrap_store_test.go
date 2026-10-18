package server

import (
	"testing"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	llmtest "gogen/internal/llm/llmtest"
)

// TestCreateBootstrapSessionWithoutStore pins the store-less host path: a
// server built around an agent without persistence (the login-flow tests, any
// embedding host that keeps persistence off) has a nil *session.Store on the
// workspace. Creating the bootstrap session must not hand that typed nil to
// the session agent's SessionPersister interface — it is not == nil, so the
// factory's thinking-level seed would flush into a nil store receiver and
// panic the WS upgrade (the "http: panic serving" chain from
// createBootstrapSession).
func TestCreateBootstrapSessionWithoutStore(t *testing.T) {
	dir := t.TempDir()
	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr) // no SessionStore: persistence stays off
	s := newTestServer(t, a, &config.Config{})

	if s.ws.Store != nil {
		t.Fatal("setup: workspace store should be nil without a persistent agent store")
	}
	rt := s.createBootstrapSession()
	if rt == nil || rt.agent == nil {
		t.Fatal("createBootstrapSession returned no runtime")
	}
	if rt.agent.SessionStore != nil {
		t.Fatalf("session agent store = %#v, want nil (typed-nil leaked through)", rt.agent.SessionStore)
	}
	if rt.agent.SessionID == "" {
		t.Fatal("bootstrap session has no id")
	}
	// The session must be usable: a flush is a no-op rather than a panic.
	rt.agent.FlushSession()
}
