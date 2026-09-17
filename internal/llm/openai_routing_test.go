package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/modelinfo"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// openCodeCatalogHandler mimics the OpenCode Zen/Go gateways: GET /models
// returns the model list, but GET /models/{model} is NOT implemented (404).
// clientForModel's discovery must never rely on the single-model endpoint.
func openCodeCatalogHandler(models []string, getHits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			data := make([]map[string]any, 0, len(models))
			for _, id := range models {
				data = append(data, map[string]any{"id": id, "object": "model"})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		if getHits != nil {
			getHits.Add(1)
		}
		http.NotFound(w, r)
	}
}

func newTestOpenAIClient(srv *httptest.Server) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(srv.Client()),
	)
}

// TestFetchModelsGoTakesPrecedenceOverZen verifies the merged catalog list is
// deduped with the Go entry kept for models present on both endpoints, and
// that routing points overlap models at the Go client.
func TestFetchModelsGoTakesPrecedenceOverZen(t *testing.T) {
	zenSrv := httptest.NewServer(openCodeCatalogHandler([]string{"zen-only", "shared-model"}, nil))
	defer zenSrv.Close()
	goSrv := httptest.NewServer(openCodeCatalogHandler([]string{"shared-model", "go-only"}, nil))
	defer goSrv.Close()

	zenClient := newTestOpenAIClient(zenSrv)
	goClient := newTestOpenAIClient(goSrv)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{
			name:      "default",
			zenStream: &zenClient,
			goStream:  &goClient,
		}},
		modelClient: make(map[string]*openai.Client),
	}

	models, _, perProfile, err := p.fetchModelsWithProfiles(context.Background(), true)
	if err != nil {
		t.Fatalf("fetchModelsWithProfiles: %v", err)
	}
	// One default profile whose routing is the merged zen/go result
	// (Go precedence).
	if len(perProfile) != 1 {
		t.Fatalf("perProfile has %d entries, want 1 (single default profile)", len(perProfile))
	}
	routing := perProfile[0].routing
	if len(models) != 3 {
		t.Fatalf("merged list has %d entries, want 3 (overlap deduped)", len(models))
	}
	counts := make(map[string]int, len(models))
	for _, m := range models {
		counts[m.ID]++
	}
	if counts["shared-model"] != 1 {
		t.Fatalf("shared-model listed %d times, want 1 (Go precedence dedupes)", counts["shared-model"])
	}
	if got := routing["shared-model"]; got != &goClient {
		t.Fatalf("shared-model routed to the wrong client (want Go)")
	}
	if got := routing["zen-only"]; got != &zenClient {
		t.Fatalf("zen-only routed to the wrong client")
	}
	if got := routing["go-only"]; got != &goClient {
		t.Fatalf("go-only routed to the wrong client")
	}
}

// TestClientForModelDiscoversViaCatalogLists verifies clientForModel builds
// routing from the /models lists (the OpenCode gateways 404 GET /models/{id}),
// and that Go-only, Zen-only, and overlap models resolve to the right
// endpoints without ever issuing a single-model GET.
func TestClientForModelDiscoversViaCatalogLists(t *testing.T) {
	var getHits atomic.Int32
	zenSrv := httptest.NewServer(openCodeCatalogHandler([]string{"zen-only", "shared-model"}, &getHits))
	defer zenSrv.Close()
	goSrv := httptest.NewServer(openCodeCatalogHandler([]string{"shared-model", "go-only"}, &getHits))
	defer goSrv.Close()

	zenClient := newTestOpenAIClient(zenSrv)
	goClient := newTestOpenAIClient(goSrv)
	primary := newTestOpenAIClient(zenSrv) // user-configured base URL (Zen)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{
			name:       "default",
			stream:     &primary,
			zenStream:  &zenClient,
			zenCatalog: &zenClient,
			goStream:   &goClient,
			goCatalog:  &goClient,
		}},
		modelClient: make(map[string]*openai.Client),
	}

	cases := []struct {
		model string
		want  *openai.Client
	}{
		{"go-only", &goClient},      // Go-only model → Go endpoint
		{"zen-only", &zenClient},    // Zen-only model → Zen endpoint
		{"shared-model", &goClient}, // overlap → Go takes precedence
	}
	for _, tc := range cases {
		p.model = tc.model
		if got := p.clientForModel(t.Context()); got != tc.want {
			t.Fatalf("model %q: routed to the wrong endpoint", tc.model)
		}
	}

	// Unknown model: no catalog entry, no models.dev info → primary client.
	p.model = "not-a-real-model"
	if got := p.clientForModel(t.Context()); got != p.profiles[0].stream {
		t.Fatalf("unknown model: expected primary client fallback")
	}

	if n := getHits.Load(); n != 0 {
		t.Fatalf("GET /models/{id} was used %d times; OpenCode does not implement it", n)
	}
}

// TestClientForModelNonOpenCodeUsesPrimaryClient pins the local/single-endpoint
// contract: providers without the OpenCode zen/go clients (llama.cpp, vLLM,
// etc.) never run catalog discovery or models.dev lookups — they route
// straight to the primary client with zero network I/O, exactly like the
// pre-fix behavior.
func TestClientForModelNonOpenCodeUsesPrimaryClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	primary := newTestOpenAIClient(srv)
	p := &OpenAIProvider{
		profiles:    []*providerProfile{{name: "default", stream: &primary}},
		modelClient: make(map[string]*openai.Client),
		model:       "some-local-model",
	}

	if got := p.clientForModel(t.Context()); got != p.profiles[0].stream {
		t.Fatalf("non-OpenCode provider: expected the primary client")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("non-OpenCode discovery issued %d HTTP requests, want 0", n)
	}
}

// writeModelsDevRegistry writes a models.dev-style registry JSON to path.
func writeModelsDevRegistry(t *testing.T, path string, reg map[string]any) {
	t.Helper()
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestClientForModelFallbackUsesModelsDevGoFirst verifies that when both
// catalogs are unreachable, clientForModel falls back to the models.dev
// registry with Go preferred, so a Go-subscription model still lands on the
// Go endpoint instead of the user-configured (Zen) one.
func TestClientForModelFallbackUsesModelsDevGoFirst(t *testing.T) {
	down := func(w http.ResponseWriter, _ *http.Request) {
		// Fail fast: openai-go retries 5xx with exponential backoff, which
		// would stall the test by seconds for no coverage benefit.
		w.Header().Set("x-should-retry", "false")
		http.Error(w, "down", http.StatusInternalServerError)
	}
	zenSrv := httptest.NewServer(http.HandlerFunc(down))
	defer zenSrv.Close()
	goSrv := httptest.NewServer(http.HandlerFunc(down))
	defer goSrv.Close()

	dir := t.TempDir()
	cache := filepath.Join(dir, "models.json")
	writeModelsDevRegistry(t, cache, map[string]any{
		"opencode": map[string]any{
			"id":  "opencode",
			"api": "https://opencode.ai/zen/v1",
			"models": map[string]any{
				"claude-opus-4-8": map[string]any{"id": "claude-opus-4-8"},
			},
		},
		"opencode-go": map[string]any{
			"id":  "opencode-go",
			"api": "https://opencode.ai/zen/go/v1",
			"models": map[string]any{
				"mimo-v2.5-pro": map[string]any{"id": "mimo-v2.5-pro"},
			},
		},
	})

	zenClient := newTestOpenAIClient(zenSrv)
	goClient := newTestOpenAIClient(goSrv)
	primary := newTestOpenAIClient(zenSrv)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{
			name:       "default",
			stream:     &primary,
			zenStream:  &zenClient,
			zenCatalog: &zenClient,
			goStream:   &goClient,
			goCatalog:  &goClient,
		}},
		modelClient: make(map[string]*openai.Client),
		modelInfo:   modelinfo.NewResolver(cache),
	}

	p.model = "mimo-v2.5-pro"
	if got := p.clientForModel(t.Context()); got != &goClient {
		t.Fatalf("go-only model with catalogs down: routed to the wrong endpoint (want Go)")
	}
	p.model = "claude-opus-4-8"
	if got := p.clientForModel(t.Context()); got != &zenClient {
		t.Fatalf("zen-only model with catalogs down: routed to the wrong endpoint (want Zen)")
	}
	p.model = "not-in-registry"
	if got := p.clientForModel(t.Context()); got != p.profiles[0].stream {
		t.Fatalf("model unknown to models.dev: expected primary client fallback")
	}
}

// TestClientForModelBacksOffFailedCatalogFetch verifies a failed catalog
// fetch is not re-attempted on every chat request: within the backoff window
// clientForModel skips the fetch and falls through to the deterministic
// fallback, so a dead OpenCode endpoint costs one bounded attempt per
// modelsFetchBackoff instead of one per request.
func TestClientForModelBacksOffFailedCatalogFetch(t *testing.T) {
	var modelsHits atomic.Int32
	down := func(w http.ResponseWriter, _ *http.Request) {
		modelsHits.Add(1)
		// Fail fast: openai-go retries 5xx with exponential backoff, which
		// would stall the test for no coverage benefit.
		w.Header().Set("x-should-retry", "false")
		http.Error(w, "down", http.StatusInternalServerError)
	}
	zenSrv := httptest.NewServer(http.HandlerFunc(down))
	defer zenSrv.Close()
	goSrv := httptest.NewServer(http.HandlerFunc(down))
	defer goSrv.Close()

	zenClient := newTestOpenAIClient(zenSrv)
	goClient := newTestOpenAIClient(goSrv)
	primary := newTestOpenAIClient(zenSrv)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{
			name:       "default",
			stream:     &primary,
			zenStream:  &zenClient,
			zenCatalog: &zenClient,
			goStream:   &goClient,
			goCatalog:  &goClient,
		}},
		modelClient: make(map[string]*openai.Client),
	}

	// First call: both catalog endpoints 500 → one fetch attempt per endpoint.
	p.model = "first-unknown"
	if got := p.clientForModel(t.Context()); got != p.profiles[0].stream {
		t.Fatalf("first call: expected primary client fallback")
	}
	if n := modelsHits.Load(); n != 2 {
		t.Fatalf("first call issued %d /models requests, want 2 (zen + go)", n)
	}

	// Second call is within modelsFetchBackoff: the failed fetch must not be
	// re-attempted (the fallback entry is cached instead).
	p.model = "second-unknown"
	if got := p.clientForModel(t.Context()); got != p.profiles[0].stream {
		t.Fatalf("second call: expected primary client fallback")
	}
	if n := modelsHits.Load(); n != 2 {
		t.Fatalf("second call within backoff issued %d /models requests total, want 2 (backoff gate failed)", n)
	}
}

// TestClientForModelCancelledContextReturnsFast verifies an already-cancelled
// context aborts the catalog fetch immediately instead of blocking for the
// full modelsCatalogTimeout, so Ctrl+C during the first chat request does not
// stall for up to 8s.
func TestClientForModelCancelledContextReturnsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond; the request must be aborted by context cancellation.
		<-r.Context().Done()
	}))
	defer srv.Close()

	zenClient := newTestOpenAIClient(srv)
	goClient := newTestOpenAIClient(srv)
	primary := newTestOpenAIClient(srv)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{
			name:       "default",
			stream:     &primary,
			zenStream:  &zenClient,
			zenCatalog: &zenClient,
			goStream:   &goClient,
			goCatalog:  &goClient,
		}},
		modelClient: make(map[string]*openai.Client),
		model:       "some-model",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_ = p.clientForModel(ctx)
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("clientForModel with cancelled ctx took %v; the catalog fetch must abort immediately, not wait out %s", elapsed, modelsCatalogTimeout)
	}
}

// TestFetchModelsOpenCodeKeepsSurvivingTwinWhenPeerHangs verifies that when an
// OpenCode profile's Go twin hangs, the completed Zen catalog is not
// discarded: queryProfile joins both queries and merges whatever the
// surviving endpoint listed, instead of returning an empty error result that
// emptied the whole OpenCode profile's models.
func TestFetchModelsOpenCodeKeepsSurvivingTwinWhenPeerHangs(t *testing.T) {
	zenSrv := httptest.NewServer(openCodeCatalogHandler([]string{"zen-only"}, nil))
	defer zenSrv.Close()
	// The Go twin never answers: it blocks until the query context is
	// cancelled (the per-profile budget), like an offline host that does not
	// refuse the connection.
	hungSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hungSrv.Close()

	zenClient := newTestOpenAIClient(zenSrv)
	goClient := newTestOpenAIClient(hungSrv)

	// A per-profile window generous enough that the live Zen round trip
	// always wins the race against it: a localhost catalog request costs
	// microseconds, but a loaded -race CI runner (macOS has flaked at a
	// 300ms window) can delay the response well past a tight budget and
	// wrongly report the survivor as a failure too. The window still bounds
	// the hung twin far below the full modelsCatalogTimeout budget.
	const window = time.Second
	old := profileCatalogTimeout
	profileCatalogTimeout = window
	defer func() { profileCatalogTimeout = old }()

	p := &OpenAIProvider{
		profiles: []*providerProfile{{
			name:       "default",
			zenStream:  &zenClient,
			zenCatalog: &zenClient,
			goStream:   &goClient,
			goCatalog:  &goClient,
		}},
		modelClient: make(map[string]*openai.Client),
	}

	start := time.Now()
	models, _, _, err := p.fetchModelsWithProfiles(context.Background(), true)
	if err != nil {
		t.Fatalf("fetchModelsWithProfiles: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*window {
		t.Fatalf("fetch took %s; the hanging twin must be bounded by profileCatalogTimeout (%s), not the %s budget", elapsed, window, modelsCatalogTimeout)
	}
	if len(models) != 1 || models[0].ID != "zen-only" {
		t.Fatalf("models = %+v, want the surviving zen catalog", models)
	}
}

// TestListModelsPreservesManualRoutingEntries verifies a successful catalog
// refresh merges routing into modelClient instead of replacing it, so
// fallback entries clientForModel cached for models absent from the catalog
// (unknown/custom models) survive the refresh.
func TestListModelsPreservesManualRoutingEntries(t *testing.T) {
	zenSrv := httptest.NewServer(openCodeCatalogHandler([]string{"zen-model"}, nil))
	defer zenSrv.Close()
	goSrv := httptest.NewServer(openCodeCatalogHandler([]string{"go-model"}, nil))
	defer goSrv.Close()

	zenClient := newTestOpenAIClient(zenSrv)
	goClient := newTestOpenAIClient(goSrv)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{
			name:       "default",
			zenStream:  &zenClient,
			zenCatalog: &zenClient,
			goStream:   &goClient,
			goCatalog:  &goClient,
		}},
		modelClient: map[string]*openai.Client{"custom-model": &goClient},
	}

	models, err := p.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("listModels returned %d models, want 2", len(models))
	}
	if got := p.modelClient["custom-model"]; got != &goClient {
		t.Fatalf("manual fallback entry for custom-model was wiped by the catalog refresh")
	}
	if got := p.modelClient["go-model"]; got != &goClient {
		t.Fatalf("catalog model go-model not routed to the Go client")
	}
	if got := p.modelClient["zen-model"]; got != &zenClient {
		t.Fatalf("catalog model zen-model not routed to the Zen client")
	}
	if !p.modelsFetchFailedAt.IsZero() {
		t.Fatalf("successful refresh must clear modelsFetchFailedAt")
	}
}
