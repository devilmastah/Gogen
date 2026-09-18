package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gogen/internal/config"
)

// catalogHandler serves a fixed /v1/models catalog (mirrors
// openCodeCatalogHandler's wire shape; both /models and /v1/models base URL
// shapes are accepted).
func catalogHandler(models ...string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" && r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		data := make([]map[string]any, 0, len(models))
		for _, id := range models {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	return srv
}

// TestListModelsAggregatesProfiles verifies the aggregated catalog: models
// from every registered profile are concatenated into one list, each tagged
// with its owning profile name. A duplicate model ID appears once per
// profile that lists it (providers are separate instances — the picker shows
// each copy under its own group, and routing is per (profile, ID)).
func TestListModelsAggregatesProfiles(t *testing.T) {
	defSrv := catalogHandler("gpt-4o", "shared-model")
	locSrv := catalogHandler("llama3.1", "shared-model")
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local-llama", BaseURL: locSrv.URL, APIKey: "k2"},
	}, "", t.TempDir(), nil)

	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 {
		t.Fatalf("aggregated catalog has %d entries, want 4 (no cross-provider dedupe): %+v", len(models), models)
	}
	byRef := make(map[string]ModelInfo, len(models))
	for _, m := range models {
		byRef[m.Provider+"/"+m.ID] = m
	}
	if got := byRef["default/gpt-4o"].Provider; got != "default" {
		t.Fatalf("gpt-4o provider = %q, want default", got)
	}
	if got := byRef["local-llama/llama3.1"].Provider; got != "local-llama" {
		t.Fatalf("llama3.1 provider = %q, want local-llama", got)
	}
	if _, ok := byRef["default/shared-model"]; !ok {
		t.Fatal("shared-model missing under the default profile")
	}
	if _, ok := byRef["local-llama/shared-model"]; !ok {
		t.Fatal("shared-model missing under the local-llama profile")
	}
}

// TestListModelsKeepsLiveProfilesWhenOneHangs verifies that a provider
// which outlives the fetch budget (offline/hanging endpoint) does not empty
// the model picker: the catalogs that already succeeded are still returned,
// and only an all-failed fetch reports an error. Regression for the early
// ctx.Done() exit in fetchModelsWithProfiles, which discarded every
// successful catalog when a single profile failed to answer in time.
func TestListModelsKeepsLiveProfilesWhenOneHangs(t *testing.T) {
	live := catalogHandler("live-model", "live-model-2")
	t.Cleanup(live.Close)
	// The hanging profile never answers: it blocks until the client
	// cancels the request (fetch budget expiry), like an offline host that
	// does not refuse the connection.
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hung.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "live", BaseURL: live.URL, APIKey: "k1"},
		{Name: "hung", BaseURL: hung.URL, APIKey: "k2"},
	}, "", t.TempDir(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels with one hanging profile must still return the live catalog, got: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("aggregated catalog has %d models, want 2 from the live profile", len(models))
	}
	for _, m := range models {
		if m.Provider != "live" {
			t.Fatalf("model %q provider = %q, want live (the hanging profile must be skipped, not fatal)", m.ID, m.Provider)
		}
	}
}

// TestListModelsHangingProfileCostsOnlyItsOwnBudget verifies that a hung
// provider delays the fetch by at most its per-profile window
// (profileCatalogTimeout), even with no outer deadline: the live catalogs
// are merged and served when the hung profile's own budget expires, instead
// of waiting out the whole fetch budget. The per-profile wait must never
// depend on modelsCatalogTimeout.
func TestListModelsHangingProfileCostsOnlyItsOwnBudget(t *testing.T) {
	live := catalogHandler("live-model")
	t.Cleanup(live.Close)
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hung.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "live", BaseURL: live.URL, APIKey: "k1"},
		{Name: "hung", BaseURL: hung.URL, APIKey: "k2"},
	}, "", t.TempDir(), nil)

	// The live profile's localhost request must win the race against its
	// per-profile window even on a loaded -race CI runner (macOS has flaked
	// at 300ms); the window still bounds the hung profile well below the
	// full modelsCatalogTimeout budget.
	const window = time.Second
	old := profileCatalogTimeout
	profileCatalogTimeout = window
	defer func() { profileCatalogTimeout = old }()

	start := time.Now()
	models, err := p.ListModels(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if elapsed > 3*window {
		t.Fatalf("fetch took %s with a hanging profile; the per-profile budget must bound it", elapsed)
	}
	if len(models) != 1 || models[0].ID != "live-model" || models[0].Provider != "live" {
		t.Fatalf("models = %+v, want the live catalog only", models)
	}
}

// TestClientForModelRoutesToOwningProfile verifies per-model routing: a
// model's chat/stream requests go to the endpoint of the profile the model
// was picked under (the ModelRef routing key) — the same ID picked under a
// different profile routes to THAT profile's endpoint, not the first
// catalog that lists it.
func TestClientForModelRoutesToOwningProfile(t *testing.T) {
	defSrv := catalogHandler("gpt-4o", "llama3.1")
	locSrv := catalogHandler("llama3.1")
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local-llama", BaseURL: locSrv.URL, APIKey: "k2"},
	}, "", t.TempDir(), nil)

	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	if len(p.profiles) != 2 {
		p.modelsMu.RUnlock()
		t.Fatalf("profiles = %d, want 2", len(p.profiles))
	}
	defStream := p.profiles[0].stream
	locStream := p.profiles[1].stream
	p.modelsMu.RUnlock()

	// The same ID under two profiles: the routing key (profile + ID)
	// decides, not the catalog listing.
	if err := p.SetModelRef(ModelRef{Profile: "default", ID: "llama3.1"}); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(context.Background()); got != defStream {
		t.Fatal("llama3.1 picked under default routed to the wrong endpoint (want default profile)")
	}
	if err := p.SetModelRef(ModelRef{Profile: "local-llama", ID: "llama3.1"}); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(context.Background()); got != locStream {
		t.Fatal("llama3.1 picked under local-llama routed to the wrong endpoint (want local-llama profile)")
	}
	// Unknown model on the default profile falls back to the default
	// profile's client.
	if err := p.SetModelRef(ModelRef{Profile: "default", ID: "not-listed"}); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(context.Background()); got != defStream {
		t.Fatal("unknown model on the default profile should fall back to the default client")
	}
}

// TestSetProfilesRefreshesCatalog verifies the live provider-set swap: the
// catalog cache is invalidated, the next ListModels reflects only the new
// profiles, and a model whose profile was removed no longer routes to the
// old endpoint (it falls back to the default client).
func TestSetProfilesRefreshesCatalog(t *testing.T) {
	defSrv := catalogHandler("gpt-4o")
	locSrv := catalogHandler("llama3.1")
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local-llama", BaseURL: locSrv.URL, APIKey: "k2"},
	}, "", t.TempDir(), nil)
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Swap: drop local-llama, add a different second endpoint.
	newSrv := catalogHandler("qwen2.5")
	t.Cleanup(newSrv.Close)
	if err := p.SetProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "other", BaseURL: newSrv.URL, APIKey: "k3"},
	}); err != nil {
		t.Fatal(err)
	}
	// Caches were invalidated by the swap.
	p.modelsMu.RLock()
	cacheStale := p.modelsCachedAt.IsZero() || len(p.modelProfile) != 0
	p.modelsMu.RUnlock()
	if !cacheStale {
		t.Fatal("SetProfiles did not invalidate the catalog cache")
	}

	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]ModelInfo, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}
	if _, ok := byID["llama3.1"]; ok {
		t.Fatal("llama3.1 still listed after its profile was removed")
	}
	if byID["qwen2.5"].Provider != "other" {
		t.Fatalf("qwen2.5 provider = %q, want other", byID["qwen2.5"].Provider)
	}
	p.model = "llama3.1"
	if got := p.clientForModel(context.Background()); got != p.fallbackClient() {
		t.Fatal("removed model routed to a stale endpoint (want default fallback)")
	}
}

// TestDefaultBaseURLTracksSetProfiles verifies the live default-endpoint
// fallbacks (/props probe target, models.dev resolution, and the
// preserve_reasoning kwargs gate) resolve against the CURRENT default
// profile after a runtime SetProfiles swap — never the construction-time
// endpoint.
func TestDefaultBaseURLTracksSetProfiles(t *testing.T) {
	oldSrv := catalogHandler("gpt-4o")
	newSrv := catalogHandler("gpt-4o")
	t.Cleanup(oldSrv.Close)
	t.Cleanup(newSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: oldSrv.URL, APIKey: "k1"},
	}, "", t.TempDir(), nil)
	if got := p.defaultBaseURL(); got != oldSrv.URL {
		t.Fatalf("initial defaultBaseURL = %q, want %q", got, oldSrv.URL)
	}
	if got := p.propsBaseURLForCurrentModel(); got != oldSrv.URL {
		t.Fatalf("initial props base URL = %q, want %q", got, oldSrv.URL)
	}

	if err := p.SetProfiles([]ProviderProfile{
		{Name: "default", BaseURL: newSrv.URL, APIKey: "k2"},
	}); err != nil {
		t.Fatal(err)
	}
	// Before any catalog fetch (modelProfile is empty) the fallbacks must
	// already resolve to the NEW default endpoint.
	if got := p.defaultBaseURL(); got != newSrv.URL {
		t.Fatalf("post-swap defaultBaseURL = %q, want %q", got, newSrv.URL)
	}
	if got := p.propsBaseURLForCurrentModel(); got != newSrv.URL {
		t.Fatalf("post-swap props base URL = %q, want %q", got, newSrv.URL)
	}
	if got := p.modelsDevURLsFor(ModelRef{ID: "gpt-4o"}); len(got) == 0 || got[0] != newSrv.URL {
		t.Fatalf("post-swap models.dev URLs = %v, want %q first", got, newSrv.URL)
	}
}

// TestPropsBaseURLTracksCurrentModel verifies the /props preserve-reasoning
// probe targets the CURRENT model's owning endpoint, not the default.
func TestPropsBaseURLTracksCurrentModel(t *testing.T) {
	defSrv := catalogHandler("gpt-4o")
	locSrv := catalogHandler("llama3.1")
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL + "/v1", APIKey: "k1"},
		{Name: "local-llama", BaseURL: locSrv.URL + "/v1", APIKey: "k2"},
	}, "", t.TempDir(), nil)
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.model = "llama3.1"
	if got := p.propsBaseURLForCurrentModel(); got != locSrv.URL+"/v1" {
		t.Fatalf("props base URL = %q, want %q (owning profile)", got, locSrv.URL+"/v1")
	}
	p.model = "gpt-4o"
	if got := p.propsBaseURLForCurrentModel(); got != defSrv.URL+"/v1" {
		t.Fatalf("props base URL = %q, want %q", got, defSrv.URL+"/v1")
	}
}

// TestModelsDevURLsForOwningProfile verifies models.dev resolution tries the
// model's OWNING profile's base URL first, so a model listed on a second
// endpoint resolves against its own registry entry.
func TestModelsDevURLsForOwningProfile(t *testing.T) {
	defSrv := catalogHandler("gpt-4o")
	locSrv := catalogHandler("llama3.1")
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL + "/v1", APIKey: "k1"},
		{Name: "local-llama", BaseURL: locSrv.URL + "/v1", APIKey: "k2"},
	}, "", t.TempDir(), nil)
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.modelsDevURLsFor(ModelRef{ID: "llama3.1"}); len(got) != 1 || got[0] != locSrv.URL+"/v1" {
		t.Fatalf("models.dev URLs for llama3.1 = %v, want [%s]", got, locSrv.URL+"/v1")
	}
	// Unknown model falls back to the default profile's URLs.
	if got := p.modelsDevURLsFor(ModelRef{ID: "unknown"}); len(got) != 1 || got[0] != defSrv.URL+"/v1" {
		t.Fatalf("models.dev URLs for unknown = %v, want [%s]", got, defSrv.URL+"/v1")
	}
}

// TestProviderProfilesBuilder pins the shared endpoint-list builder used by
// setup.go, the TUI subagent spawner, and the web workspace factory: the
// implicit "default" profile from the legacy fields comes first (it wins
// duplicate-model precedence), followed by the additional providers in
// order, and empty names normalize to "default".
func TestProviderProfilesBuilder(t *testing.T) {
	profiles := ProviderProfiles("legacy-key", "gpt-4o", "https://legacy.example/v1", []config.OpenAIProviderConfig{
		{Name: "local", BaseURL: "http://127.0.0.1:8080/v1", Model: "llama3.1"},
		{Name: "other", BaseURL: "https://other.example/v1", APIKey: "other-key"},
	})
	if len(profiles) != 3 {
		t.Fatalf("profiles = %d, want 3", len(profiles))
	}
	if profiles[0].Name != "default" || profiles[0].BaseURL != "https://legacy.example/v1" || profiles[0].APIKey != "legacy-key" || profiles[0].Model != "gpt-4o" {
		t.Fatalf("default profile = %+v, want legacy fields first", profiles[0])
	}
	if profiles[1].Name != "local" || profiles[1].Model != "llama3.1" {
		t.Fatalf("additional profile 1 = %+v", profiles[1])
	}
	if profiles[2].Name != "other" || profiles[2].APIKey != "other-key" {
		t.Fatalf("additional profile 2 = %+v", profiles[2])
	}
	empty := ProviderProfiles("", "", "", nil)
	if len(empty) != 1 || empty[0].Name != "default" {
		t.Fatalf("empty input should yield just the default profile, got %+v", empty)
	}
}

// TestNewOpenAIProviderSingleProfileRoutesThroughDefaultProfile pins the
// wrapper contract: the single-endpoint constructor routes everything
// through the default profile's stream client.
func TestNewOpenAIProviderSingleProfileRoutesThroughDefaultProfile(t *testing.T) {
	srv := catalogHandler("gpt-4o")
	t.Cleanup(srv.Close)

	p := NewOpenAIProvider("k", "gpt-4o", srv.URL+"/v1", t.TempDir())
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.profiles) != 1 || p.profiles[0].name != "default" {
		t.Fatalf("single-endpoint provider profiles = %+v, want one default", p.profiles)
	}
	if got := p.clientForModel(context.Background()); got != p.profiles[0].stream {
		t.Fatal("single-endpoint provider should route through the default profile's stream client")
	}
	if got := p.clientForModel(context.Background()); got != p.fallbackClient() {
		t.Fatal("fallback client should be the default profile's stream client")
	}
}

// TestFallbackTracksProfileSwaps pins the fallback contract: the fallback
// client is always the default profile's stream client, and SetProfiles
// swaps the whole client set so the fallback routing points at the NEW
// profile clients — never the stale construction-time ones.
func TestFallbackTracksProfileSwaps(t *testing.T) {
	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: "https://a.example/v1", APIKey: "k1"},
		{Name: "second", BaseURL: "https://b.example/v1", APIKey: "k2"},
	}, "gpt-4o", t.TempDir(), nil)

	// Construction: the fallback is the default profile's stream client.
	if len(p.profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(p.profiles))
	}
	oldStream := p.profiles[0].stream
	if got := p.fallbackClient(); got != oldStream {
		t.Fatal("fallback client must be the default profile's stream client")
	}
	if got := p.defaultBaseURL(); got != "https://a.example/v1" {
		t.Fatalf("default base URL = %q, want the default profile's", got)
	}

	// SetProfiles: the client set is rebuilt; the fallback routing must
	// point at the new default profile's stream, and the construction-time
	// client must NOT be reused (it still serves the old endpoint).
	if err := p.SetProfiles([]ProviderProfile{
		{Name: "default", BaseURL: "https://new.example/v1", APIKey: "k3"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(p.profiles) != 1 {
		t.Fatalf("profiles after SetProfiles = %d, want 1", len(p.profiles))
	}
	if p.profiles[0].stream == oldStream {
		t.Fatal("SetProfiles must build fresh clients, not reuse the construction-time client")
	}
	if got := p.fallbackClient(); got != p.profiles[0].stream {
		t.Fatal("fallback client must point at the new default profile's stream after SetProfiles")
	}
	if got := p.defaultBaseURL(); got != "https://new.example/v1" {
		t.Fatalf("default base URL after SetProfiles = %q, want the new profile's", got)
	}
}

// TestSetProfilesDropsOpenCodeRouting pins that the OpenCode routing
// helpers are profile-derived: after SetProfiles replaces an OpenCode
// endpoint with a plain one, the construction-time zen/go clients must not
// be used — hasOpenCodeProfile goes false and inferOpenCodeEndpoint
// returns nil. The reverse swap flips detection back on.
func TestSetProfilesDropsOpenCodeRouting(t *testing.T) {
	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: "https://opencode.ai/zen/v1/", APIKey: "k"},
	}, "", t.TempDir(), nil)
	if !p.hasOpenCodeProfile() {
		t.Fatal("opencode default should report an opencode profile")
	}
	if p.profiles[0].zenStream == nil {
		t.Fatal("precondition: the zen client should be wired for the opencode construction")
	}
	if err := p.SetProfiles([]ProviderProfile{
		{Name: "default", BaseURL: "https://plain.example/v1", APIKey: "k"},
	}); err != nil {
		t.Fatal(err)
	}
	if p.hasOpenCodeProfile() {
		t.Fatal("plain profiles must not report opencode after SetProfiles")
	}
	if got := p.inferOpenCodeEndpoint(p.profiles[0], "gpt-4o"); got != nil {
		t.Fatal("inferOpenCodeEndpoint must return nil once no opencode profile is registered")
	}
	// Reverse swap: SetProfiles to an OpenCode endpoint flips detection on.
	if err := p.SetProfiles([]ProviderProfile{
		{Name: "default", BaseURL: "https://opencode.ai/zen/v1/", APIKey: "k"},
	}); err != nil {
		t.Fatal(err)
	}
	if !p.hasOpenCodeProfile() {
		t.Fatal("opencode profile after SetProfiles should report opencode")
	}
}

// TestSetProfilesDiscardsInFlightFetch pins the generation guard: a catalog
// fetch that started before SetProfiles must not populate the cache with
// the OLD endpoint's models after the swap — the next fetch hits the NEW
// endpoint.
func TestSetProfilesDiscardsInFlightFetch(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started) // signal that the first fetch is in flight
		}
		<-release // hold the first fetch until SetProfiles has run
		if r.URL.Path != "/models" && r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "old-model", "object": "model"}},
		})
	}))
	defer oldSrv.Close()
	newSrv := catalogHandler("new-model")
	defer newSrv.Close()

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: oldSrv.URL, APIKey: "k"},
	}, "", t.TempDir(), nil)

	fetchDone := make(chan struct{})
	go func() {
		defer close(fetchDone)
		_, _ = p.listModels(context.Background())
	}()
	<-started // the old fetch is blocked in the handler

	if err := p.SetProfiles([]ProviderProfile{
		{Name: "default", BaseURL: newSrv.URL, APIKey: "k"},
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-fetchDone

	// The stale result must NOT be cached.
	p.modelsMu.RLock()
	_, cached := p.cachedModelsLocked()
	p.modelsMu.RUnlock()
	if cached {
		t.Fatal("stale fetch (old endpoint) must not populate the cache after SetProfiles")
	}
	// The next fetch hits the NEW endpoint.
	models, err := p.listModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].model.ID != "new-model" {
		t.Fatalf("models after SetProfiles = %+v, want [new-model]", models)
	}
}
