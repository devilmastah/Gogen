package agent

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// nCtxServer serves /v1/models with a single model whose provider JSON
// reports n_ctx, like a local llama.cpp / LM Studio endpoint.
func nCtxServer(t *testing.T, nCtx int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","n_ctx":` + strconv.Itoa(nCtx) + `}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRestoreRefreshesStaleContextLimit pins the restore refresh contract: a
// session snapshot persists the context limit that was resolved when it was
// saved, which may be the 128k display fallback (cold models.dev registry,
// an endpoint that was down, a pre-fix catalog/models.dev lookup). Restoring
// used to pre-warm that value and skip the refresh because it was positive,
// so the session kept the fallback for its whole life while /models already
// resolved the real window. Validation must re-resolve it.
func TestRestoreRefreshesStaleContextLimit(t *testing.T) {
	srv := nCtxServer(t, 262144)
	p := llm.NewOpenAIProvider("", "", srv.URL+"/v1", t.TempDir())
	cm := contextmgr.NewManager(p, contextmgr.Settings{})
	a := NewAgent(p, NewExecutor(t.TempDir()), cm)

	a.RestoreSessionLocal(SessionSnapshot{
		ContextLimit: 128000, // the stale fallback persisted with the session
		Model:        "m1",
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}, "sess-1")
	if got := a.ContextLimit(); got != 128000 {
		t.Fatalf("pre-warm: limit = %d, want 128000", got)
	}

	a.ValidateRestoredModel(t.Context(), "m1")
	if got := a.ContextLimit(); got != 262144 {
		t.Fatalf("after ValidateRestoredModel: limit = %d, want 262144", got)
	}
	if snap := a.ContextStats(t.Context()).Snapshot; !snap.LimitResolved {
		t.Fatal("refreshed limit must be marked resolved")
	}
}

// TestRestoreUnresolvableLimitStaysUnresolved pins the other half: when the
// provider cannot resolve the window (no n_ctx, no models.dev entry), the
// snapshot's value stays on display but must NOT be reported as the model's
// resolved window — wire consumers then fall back to the model list instead
// of showing the stale number as fact.
func TestRestoreUnresolvableLimitStaysUnresolved(t *testing.T) {
	srv := modelsServer(t, "m1") // no n_ctx in the provider JSON
	p := llm.NewOpenAIProvider("", "", srv.URL+"/v1", t.TempDir())
	cm := contextmgr.NewManager(p, contextmgr.Settings{})
	a := NewAgent(p, NewExecutor(t.TempDir()), cm)

	a.RestoreSessionLocal(SessionSnapshot{
		ContextLimit: 64000,
		Model:        "m1",
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}, "sess-1")

	a.ValidateRestoredModel(t.Context(), "m1")

	if got := a.ContextLimit(); got != 64000 {
		t.Fatalf("display limit = %d, want the snapshot's 64000 kept", got)
	}
	if snap := a.ContextStats(t.Context()).Snapshot; snap.LimitResolved {
		t.Fatal("unresolvable window must not be reported as resolved")
	}
}

// TestFreshSessionResolvesContextLimit is the control: without a restored
// snapshot the first use resolves the provider's real window.
func TestFreshSessionResolvesContextLimit(t *testing.T) {
	srv := nCtxServer(t, 262144)
	p := llm.NewOpenAIProvider("", "", srv.URL+"/v1", t.TempDir())
	cm := contextmgr.NewManager(p, contextmgr.Settings{})
	a := NewAgent(p, NewExecutor(t.TempDir()), cm)
	if err := p.SetModel("m1"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	a.Context.EnsureContextLimit(t.Context())
	if got := a.ContextLimit(); got != 262144 {
		t.Fatalf("fresh session: limit = %d, want 262144", got)
	}
}

// TestUnresolvedLimitIsNotPersisted pins snapshot honesty: the display
// fallback of an unresolved window must not be written as the session's
// context limit, or a later restore pre-warms the guess as a resolved value.
func TestUnresolvedLimitIsNotPersisted(t *testing.T) {
	srv := modelsServer(t, "m1") // provider JSON has no n_ctx
	p := llm.NewOpenAIProvider("", "", srv.URL+"/v1", t.TempDir())
	cm := contextmgr.NewManager(p, contextmgr.Settings{})
	a := NewAgent(p, NewExecutor(t.TempDir()), cm)
	store := &stubSessionStore{}
	a.SessionStore = store
	a.SetSessionID("sess-persist")
	if err := p.SetModel("m1"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	a.Context.EnsureContextLimit(t.Context()) // cannot resolve this window
	a.Messages = []llm.Message{{Role: "user", Content: "hi"}}
	a.FlushSession()

	snap, ok := store.sessions["sess-persist"]
	if !ok {
		t.Fatal("session was not persisted")
	}
	if snap.ContextLimit != 0 {
		t.Fatalf("persisted ContextLimit = %d, want 0 (unresolved window)", snap.ContextLimit)
	}
}
