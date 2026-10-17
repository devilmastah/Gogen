package agent

import (
	"testing"

	"gogen/internal/config"
	"gogen/internal/llm/llmtest"
	"gogen/internal/session"
)

// TestSessionAgentFactoryIgnoresTypedNilStore pins the persistence guard
// against the typed-nil trap: a nil *session.Store stored in the
// SessionPersister interface is NOT == nil, so a factory that kept it verbatim
// would slip past every persistence guard and reach
// (*session.Store).Save with a nil receiver — the nil-pointer panic the web
// bootstrap path hit when a host without persistence created a session agent
// (the factory's thinking-level seed flushes immediately). A typed-nil store
// must read as "persistence disabled" instead.
func TestSessionAgentFactoryIgnoresTypedNilStore(t *testing.T) {
	var store *session.Store // nil concrete pointer: the shape the workspace field has
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewSessionAgent(SessionAgentOptions{
		Provider:      prov,
		Executor:      exec,
		Store:         store,
		Config:        &config.Config{ContextLimit: 128000},
		WorkingDir:    exec.GetWorkingDir(),
		ThinkingLevel: ThinkingMedium, // the seed path whose flush dereferenced the nil store
	}, nil, "typed-nil-store")

	if a.SessionStore != nil {
		t.Fatalf("SessionStore = %#v, want nil (a typed-nil store must disable persistence)", a.SessionStore)
	}
	if a.ThinkingLevel != ThinkingMedium {
		t.Fatalf("thinking level = %q, want medium", a.ThinkingLevel)
	}
	// Flushing must be a no-op, not a nil-receiver dereference.
	a.FlushSession()
}
