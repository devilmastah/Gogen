package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// catalogServer serves a one-model catalog and counts how many times it was
// downloaded. fail makes it answer 500 instead.
func catalogServer(t *testing.T, hits *atomic.Int32, fail bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if fail {
			http.Error(w, "catalog unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "model-a", "object": "model"}},
		})
	}))
}

// providerFor returns a provider shaped like a session's (its own client and
// transport) pointed at srv, optionally sharing cat.
func providerFor(t *testing.T, srv *httptest.Server, apiKey string, cat *EndpointCatalog) *OpenAIProvider {
	t.Helper()
	client := openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(srv.Client()),
		// The SDK's own transport retry would multiply the request count and
		// blur the property under test (how many times the CACHE fetches).
		option.WithMaxRetries(0),
	)
	p := &OpenAIProvider{
		profiles:    []*providerProfile{{name: "default", baseURL: srv.URL + "/", apiKey: apiKey, stream: &client}},
		modelClient: make(map[ModelRef]*openai.Client),
	}
	p.SetEndpointCatalog(cat)
	return p
}

// TestEndpointCatalogSharesFetchAcrossProviders is the traffic property: N
// sessions in one workspace download the endpoint's catalog ONCE per TTL window,
// not once each.
func TestEndpointCatalogSharesFetchAcrossProviders(t *testing.T) {
	var hits atomic.Int32
	srv := catalogServer(t, &hits, false)
	defer srv.Close()

	shared := NewEndpointCatalog()
	session1 := providerFor(t, srv, "test", shared)
	session2 := providerFor(t, srv, "test", shared)
	session3 := providerFor(t, srv, "test", shared)

	for i, p := range []*OpenAIProvider{session1, session2, session3} {
		models, err := p.ListModels(context.Background())
		if err != nil {
			t.Fatalf("session %d ListModels: %v", i+1, err)
		}
		if len(models) != 1 || models[0].ID != "model-a" {
			t.Fatalf("session %d models = %+v", i+1, models)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("catalog downloads = %d, want 1 for three sessions sharing the endpoint", got)
	}
}

// TestEndpointCatalogUnsharedProvidersFetchIndependently pins the fallback: a
// provider with no shared catalog (tests, direct construction) behaves exactly
// as before and fetches for itself.
func TestEndpointCatalogUnsharedProvidersFetchIndependently(t *testing.T) {
	var hits atomic.Int32
	srv := catalogServer(t, &hits, false)
	defer srv.Close()

	for i := 0; i < 2; i++ {
		p := providerFor(t, srv, "test", nil)
		if _, err := p.ListModels(context.Background()); err != nil {
			t.Fatalf("ListModels: %v", err)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("catalog downloads = %d, want 2 (no shared cache)", got)
	}
}

// TestEndpointCatalogKeysArePerEndpointAndKey pins isolation: endpoints (and
// distinct credentials) never share a cached catalog, and the key carries no
// readable credential.
func TestEndpointCatalogKeysArePerEndpointAndKey(t *testing.T) {
	t.Parallel()
	a := CatalogKey("https://host/v1", "key-one")
	if a != CatalogKey("https://host/v1", "key-one") {
		t.Fatal("CatalogKey must be stable for the same endpoint and key")
	}
	if a == CatalogKey("https://host/v1", "key-two") {
		t.Fatal("different credentials must not share a catalog")
	}
	if a == CatalogKey("https://other/v1", "key-one") {
		t.Fatal("different endpoints must not share a catalog")
	}
	if strings.Contains(a, "key-one") {
		t.Fatalf("CatalogKey leaked the credential: %q", a)
	}
}

// TestEndpointCatalogFailureIsSharedAndBounded pins the failure half: one
// process-wide bounded attempt per backoff window, so N sessions cannot each
// re-probe a dead endpoint.
func TestEndpointCatalogFailureIsSharedAndBounded(t *testing.T) {
	var hits atomic.Int32
	srv := catalogServer(t, &hits, true)
	defer srv.Close()

	shared := NewEndpointCatalog()
	for i := 0; i < 3; i++ {
		p := providerFor(t, srv, "test", shared)
		if _, err := p.ListModels(context.Background()); err == nil {
			t.Fatalf("session %d: expected a catalog failure", i+1)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("catalog downloads = %d, want 1 (shared backoff after the failure)", got)
	}
}

// TestEndpointCatalogProbeDoesNotArmSharedBackoff pins the armBackoff contract
// across the shared cache: a brief probe's failure is not evidence the endpoint
// is dead, so it must not stop a full-budget fetch from trying.
func TestEndpointCatalogProbeDoesNotArmSharedBackoff(t *testing.T) {
	var hits atomic.Int32
	srv := catalogServer(t, &hits, true)
	defer srv.Close()

	shared := NewEndpointCatalog()
	probe := providerFor(t, srv, "test", shared)
	if _, err := probe.listModelsProbe(context.Background()); err == nil {
		t.Fatal("expected the probe to fail")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("probe downloads = %d, want 1", got)
	}

	full := providerFor(t, srv, "test", shared)
	if _, err := full.listModels(context.Background()); err == nil {
		t.Fatal("expected the full fetch to fail")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("downloads = %d, want 2: a probe failure must not arm the shared backoff", got)
	}
}

// TestEndpointCatalogKeepsPartialModelsOnError pins the error contract: a
// fetch that dies after collecting part of the catalog hands its waiters the
// partial list ALONG WITH the error, so the caller can weigh the two (the
// OpenCode twin merge keeps a non-empty list). The failure is not cached as
// data — only the backoff is, so the next call inside the window is served
// the error alone.
func TestEndpointCatalogKeepsPartialModelsOnError(t *testing.T) {
	cat := NewEndpointCatalog()
	var calls atomic.Int32
	fetch := func(context.Context) ([]openai.Model, error) {
		calls.Add(1)
		return []openai.Model{{ID: "partial-a"}, {ID: "partial-b"}}, errors.New("pagination interrupted")
	}
	got, err := cat.Models(context.Background(), "endpoint-key", true, fetch)
	if err == nil {
		t.Fatal("expected the fetch error")
	}
	if len(got) != 2 || got[0].ID != "partial-a" || got[1].ID != "partial-b" {
		t.Fatalf("models = %+v, want the partial list collected before the error", got)
	}
	got, err = cat.Models(context.Background(), "endpoint-key", true, fetch)
	if err == nil {
		t.Fatal("expected the backoff error")
	}
	if len(got) != 0 {
		t.Fatalf("backoff call returned models %+v, want none (only the backoff is cached)", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("fetch calls = %d, want 1 (the backoff must serve the second call)", calls.Load())
	}
}

// TestEndpointCatalogEvictsOldestWhenFull bounds the shared map: past the cap,
// the entry loaded longest ago is dropped while the newest stays cached.
func TestEndpointCatalogEvictsOldestWhenFull(t *testing.T) {
	cat := NewEndpointCatalog()
	fetch := func(context.Context) ([]openai.Model, error) {
		return []openai.Model{{ID: "model-a"}}, nil
	}
	for i := 0; i < maxEndpointCatalogEntries+1; i++ {
		key := fmt.Sprintf("key-%d", i)
		if _, err := cat.Models(context.Background(), key, false, fetch); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	cat.mu.Lock()
	size := len(cat.entries)
	_, keptFirst := cat.entries["key-0"]
	_, keptLast := cat.entries[fmt.Sprintf("key-%d", maxEndpointCatalogEntries)]
	cat.mu.Unlock()
	if size != maxEndpointCatalogEntries {
		t.Fatalf("entries = %d, want %d", size, maxEndpointCatalogEntries)
	}
	if keptFirst {
		t.Fatal("the oldest entry should have been evicted")
	}
	if !keptLast {
		t.Fatal("the newest entry must stay cached")
	}
}

// TestEndpointCatalogEvictionIgnoresCoarseClock pins the root cause of the
// Windows failure of the test above: Windows resolves time.Now far more
// coarsely than Linux/macOS, so a tight burst of catalog loads can share one
// loadedAt and a time-ordered eviction then drops a victim at random. The
// eviction must follow LOAD ORDER, not the clock. Every loadedAt is forced
// identical here (exactly what a coarse timer produces), so only a
// sequence-ordered eviction reliably drops the entry that loaded first.
func TestEndpointCatalogEvictionIgnoresCoarseClock(t *testing.T) {
	cat := NewEndpointCatalog()
	fetch := func(context.Context) ([]openai.Model, error) {
		return []openai.Model{{ID: "model-a"}}, nil
	}
	for i := 0; i < maxEndpointCatalogEntries; i++ {
		if _, err := cat.Models(context.Background(), fmt.Sprintf("key-%d", i), false, fetch); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	// Flatten the clock: a coarse timer would hand every load the same tick.
	cat.mu.Lock()
	for _, e := range cat.entries {
		e.loadedAt = time.Time{}
	}
	cat.mu.Unlock()

	// One more load trips the cap, so one entry is evicted.
	if _, err := cat.Models(context.Background(), "key-new", false, fetch); err != nil {
		t.Fatalf("over-cap fetch: %v", err)
	}
	cat.mu.Lock()
	size := len(cat.entries)
	_, keptFirst := cat.entries["key-0"]
	_, keptNew := cat.entries["key-new"]
	cat.mu.Unlock()
	if size != maxEndpointCatalogEntries {
		t.Fatalf("entries = %d, want %d", size, maxEndpointCatalogEntries)
	}
	if keptFirst {
		t.Fatal("eviction followed the flattened clock; the first-loaded entry must be dropped")
	}
	if !keptNew {
		t.Fatal("the entry being inserted must stay cached")
	}
}
