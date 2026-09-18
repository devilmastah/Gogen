package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// singleModelServer serves /v1/models with exactly one model, like a local
// llama.cpp / LM Studio endpoint with a single loaded model.
func singleModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"only-model","object":"model","created":0,"owned_by":"test"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// modelsServer serves /v1/models with exactly the given model ids.
func modelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var sb strings.Builder
		sb.WriteString(`{"object":"list","data":[`)
		for i, id := range ids {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, `{"id":%q,"object":"model","created":0,"owned_by":"test"}`, id)
		}
		sb.WriteString("]}")
		w.Write([]byte(sb.String()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failingModelsServer answers every request with HTTP 500 so catalog
// lookups error out (endpoint unreachable / auth failure simulation).
func failingModelsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail fast: openai-go retries 5xx with exponential backoff, which
		// would make every catalog probe in this test sleep ~1.2s.
		w.Header().Set("x-should-retry", "false")
		http.Error(w, "catalog unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newSingleModelAgent returns an agent over a real provider serving exactly
// one model, with no model configured.
func newSingleModelAgent(t *testing.T) (*Agent, *llm.OpenAIProvider) {
	t.Helper()
	srv := singleModelServer(t)
	return newAgentOverServer(t, srv)
}

// newAgentOverServer returns an agent over a real provider backed by the
// given /v1/models endpoint, with no model configured.
func newAgentOverServer(t *testing.T, srv *httptest.Server) (*Agent, *llm.OpenAIProvider) {
	t.Helper()
	p := llm.NewOpenAIProvider("", "", srv.URL+"/v1", t.TempDir())
	ctxMgr := contextmgr.NewManager(p, contextmgr.Settings{})
	return NewAgent(p, NewExecutor(t.TempDir()), ctxMgr), p
}

// restoreWithModel restores a snapshot that carries a context limit and the
// given model (the shape a saved session takes after at least one turn).
func restoreWithModel(a *Agent, model string) {
	a.RestoreSessionLocal(SessionSnapshot{
		ContextLimit: 128000,
		Model:        model,
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}, "sess-1")
}

// TestSoleModelAutoSelectFirstTurn pins the baseline: with no model
// configured and a provider serving exactly one model, the first turn
// auto-selects the sole model.
func TestSoleModelAutoSelectFirstTurn(t *testing.T) {
	a, p := newSingleModelAgent(t)
	if err := a.requireModelSelected(t.Context()); err != nil {
		t.Fatalf("requireModelSelected: %v", err)
	}
	if got := p.ModelName(); got != "only-model" {
		t.Fatalf("model = %q, want %q", got, "only-model")
	}
}

// TestSoleModelAutoSelectAfterRestore reproduces the regression: a restored
// session whose snapshot carries a context limit but no model previously
// short-circuited the auto-select probe (ValidateRestoredModel skipped the
// refresh because the limit was already resolved, and requireModelSelected
// skipped it via EnsureContextLimit's resolved-limit early return). The sole
// model must still be chosen.
func TestSoleModelAutoSelectAfterRestore(t *testing.T) {
	a, p := newSingleModelAgent(t)
	a.RestoreSessionLocal(SessionSnapshot{
		ContextLimit: 128000,
		Model:        "",
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}, "sess-1")

	a.ValidateRestoredModel(t.Context(), "")
	if got := p.ModelName(); got != "only-model" {
		t.Fatalf("model after ValidateRestoredModel = %q, want %q", got, "only-model")
	}
	if err := a.requireModelSelected(t.Context()); err != nil {
		t.Fatalf("requireModelSelected: %v", err)
	}
}

// TestSoleModelAutoSelectAfterStaleRestore covers the stale-model path: a
// restored model that no longer exists at the provider is cleared, and a
// single-model provider auto-selects its sole model immediately instead of
// leaving the session with no model until the next turn.
func TestSoleModelAutoSelectAfterStaleRestore(t *testing.T) {
	a, p := newSingleModelAgent(t)
	a.RestoreSessionLocal(SessionSnapshot{
		ContextLimit: 128000,
		Model:        "stale-model",
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}, "sess-1")
	if got := p.ModelName(); got != "stale-model" {
		t.Fatalf("model after restore = %q, want %q", got, "stale-model")
	}

	a.ValidateRestoredModel(t.Context(), "stale-model")
	if got := p.ModelName(); got != "only-model" {
		t.Fatalf("model after validation = %q, want %q (auto-selected after stale clear)", got, "only-model")
	}
}

// TestStaleRestoreClearedOnMultiModelProvider covers the reported symptom on
// a multi-model provider: a restored session whose model is not served by
// the current provider must have it cleared by validation, and the first
// turn must surface "no model selected" instead of sending the stale model.
func TestStaleRestoreClearedOnMultiModelProvider(t *testing.T) {
	srv := modelsServer(t, "m1", "m2")
	a, p := newAgentOverServer(t, srv)
	restoreWithModel(a, "stale-model")

	a.ValidateRestoredModel(t.Context(), "stale-model")
	if got := p.ModelName(); got != "" {
		t.Fatalf("model after validation = %q, want cleared (empty)", got)
	}
	if err := a.requireModelSelected(t.Context()); err == nil {
		t.Fatal("requireModelSelected: want error (stale model cleared, nothing to select)")
	}
}

// TestRequireModelSelectedRechecksUnverifiedRestore pins the turn-path
// re-check: when the async validation never confirmed the restored model
// (still running, or the startup catalog fetch failed), the first turn must
// re-check and refuse to use a model the provider no longer lists.
func TestRequireModelSelectedRechecksUnverifiedRestore(t *testing.T) {
	srv := modelsServer(t, "m1", "m2")
	a, p := newAgentOverServer(t, srv)
	restoreWithModel(a, "stale-model")
	// No ValidateRestoredModel call: simulate a restore whose validation has
	// not yet run or could not reach the catalog.
	if err := a.requireModelSelected(t.Context()); err == nil {
		t.Fatal("requireModelSelected: want error (stale restored model must not be used)")
	}
	if got := p.ModelName(); got != "" {
		t.Fatalf("model after requireModelSelected = %q, want cleared", got)
	}
}

// TestRequireModelSelectedSkipsRecheckWhenVerified pins the fast path: once
// validation confirmed the restored model, the first turn must not probe the
// catalog again (no network I/O for a verified model).
func TestRequireModelSelectedSkipsRecheckWhenVerified(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"only-model","object":"model","created":0,"owned_by":"test"}]}`))
	}))
	t.Cleanup(srv.Close)
	a, p := newAgentOverServer(t, srv)
	restoreWithModel(a, "only-model")

	a.ValidateRestoredModel(t.Context(), "only-model")
	if got := p.ModelName(); got != "only-model" {
		t.Fatalf("model after validation = %q, want %q", got, "only-model")
	}
	hits.Store(0) // ignore the validation probe itself
	if err := a.requireModelSelected(t.Context()); err != nil {
		t.Fatalf("requireModelSelected: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("requireModelSelected made %d catalog request(s) for a verified model, want 0", n)
	}
}

// twoProfileAgent returns an agent over a real two-profile provider: the
// default profile's endpoint lists defaultIDs, the "deepseek" profile's
// lists extraIDs (the merged catalog is in profile order, default first).
func twoProfileAgent(t *testing.T, defaultIDs, extraIDs []string) (*Agent, *llm.OpenAIProvider) {
	t.Helper()
	srvDefault := modelsServer(t, defaultIDs...)
	srvExtra := modelsServer(t, extraIDs...)
	p := llm.NewOpenAIProviderWithProfiles([]llm.ProviderProfile{
		{Name: "default", BaseURL: srvDefault.URL + "/v1"},
		{Name: "deepseek", BaseURL: srvExtra.URL + "/v1"},
	}, "", t.TempDir(), nil)
	ctxMgr := contextmgr.NewManager(p, contextmgr.Settings{})
	return NewAgent(p, NewExecutor(t.TempDir()), ctxMgr), p
}

// TestLegacyRestoreResolvesModelOwner pins the legacy-snapshot owner
// re-resolution: a snapshot saved before per-profile routing (no
// ModelProvider field) restores as a default-profile model, and the first
// catalog validation must re-home it with the pre-refactor ownership rule
// (first profile in profile order that lists the model) — a model only one
// non-default provider lists must route there, not to the default endpoint
// (which would reject it as unknown).
func TestLegacyRestoreResolvesModelOwner(t *testing.T) {
	a, p := twoProfileAgent(t, []string{"shared-model"}, []string{"shared-model", "local-only"})

	// Legacy shape: Model set, no ModelProvider (restoreWithModel's shape).
	restoreWithModel(a, "local-only")
	if got := p.CurrentModelRef(); got != (llm.ModelRef{Profile: "default", ID: "local-only"}) {
		t.Fatalf("setup: restored ref = %+v, want default/local-only (legacy shape)", got)
	}

	a.ValidateRestoredModel(t.Context(), "local-only")
	if got := p.CurrentModelRef(); got != (llm.ModelRef{Profile: "deepseek", ID: "local-only"}) {
		t.Fatalf("ref after validation = %+v, want deepseek/local-only (legacy owner re-resolved)", got)
	}
}

// TestLegacyRestoreCollisionKeepsDefaultProfile pins the collision case: a
// legacy restore of a model listed by BOTH the default and a non-default
// profile keeps the default profile — the pre-refactor rule (profile-order
// precedence) routed it there too, so legacy sessions behave exactly as
// before the refactor.
func TestLegacyRestoreCollisionKeepsDefaultProfile(t *testing.T) {
	a, p := twoProfileAgent(t, []string{"shared-model"}, []string{"shared-model", "local-only"})

	restoreWithModel(a, "shared-model")
	a.ValidateRestoredModel(t.Context(), "shared-model")
	if got := p.CurrentModelRef(); got != (llm.ModelRef{Profile: "default", ID: "shared-model"}) {
		t.Fatalf("ref after validation = %+v, want default/shared-model (first-listing profile, pre-refactor rule)", got)
	}
}

// TestNonLegacyRestoreKeepsSavedProfile pins the guard: a snapshot WITH a
// ModelProvider carries a deliberate owner and must never be re-homed by
// validation — even when another profile lists the same ID first in
// profile order.
func TestNonLegacyRestoreKeepsSavedProfile(t *testing.T) {
	a, p := twoProfileAgent(t, []string{"shared-model"}, []string{"shared-model", "local-only"})

	a.RestoreSessionLocal(SessionSnapshot{
		ContextLimit:  128000,
		Model:         "shared-model",
		ModelProvider: "deepseek",
		Messages:      []llm.Message{{Role: "user", Content: "hi"}},
	}, "sess-1")

	a.ValidateRestoredModel(t.Context(), "shared-model")
	if got := p.CurrentModelRef(); got != (llm.ModelRef{Profile: "deepseek", ID: "shared-model"}) {
		t.Fatalf("ref after validation = %+v, want deepseek/shared-model (saved owner must survive validation)", got)
	}
}

// TestRequireModelSelectedFailOpenOnCatalogError pins the fail-open path: a
// restored model cannot be verified while the catalog is unreachable, so it
// is kept (and the turn proceeds) instead of being cleared on a transient
// network blip.
func TestRequireModelSelectedFailOpenOnCatalogError(t *testing.T) {
	srv := failingModelsServer(t)
	a, p := newAgentOverServer(t, srv)
	restoreWithModel(a, "stale-model")

	// Validation cannot reach the provider: fail open, keep the model.
	a.ValidateRestoredModel(t.Context(), "stale-model")
	if got := p.ModelName(); got != "stale-model" {
		t.Fatalf("model after failed validation = %q, want kept %q (fail-open)", got, "stale-model")
	}
	// The first turn re-checks; while the catalog is still down it stays
	// fail-open rather than failing with a confusing provider error.
	if err := a.requireModelSelected(t.Context()); err != nil {
		t.Fatalf("requireModelSelected with unreachable catalog: %v (want fail-open)", err)
	}
	if got := p.ModelName(); got != "stale-model" {
		t.Fatalf("model after failed re-check = %q, want kept %q (fail-open)", got, "stale-model")
	}
}
