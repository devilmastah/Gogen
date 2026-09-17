package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// propsProbeServer answers GET /props (counted) with the given capabilities.
func propsProbeServer(t *testing.T, hits *atomic.Int32, caps map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if caps == nil {
			http.Error(w, "no props here", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat_template_caps":` + jsonBoolMap(caps) + `}`))
	}))
}

func jsonBoolMap(m map[string]bool) string {
	out := "{"
	first := true
	for k, v := range m {
		if !first {
			out += ","
		}
		first = false
		if v {
			out += `"` + k + `":true`
		} else {
			out += `"` + k + `":false`
		}
	}
	return out + "}"
}

// propsProvider builds a provider whose default profile points at srv, sharing
// cache when non-nil.
func propsProvider(t *testing.T, srv *httptest.Server, cache *PropsCapsCache) *OpenAIProvider {
	t.Helper()
	client := openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(srv.Client()),
		option.WithMaxRetries(0),
	)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{name: "default", baseURL: srv.URL + "/", apiKey: "test", stream: &client}},
		model:    "test-model",
	}
	p.SetPropsCapsCache(cache)
	return p
}

// TestPropsCapsSharedAcrossProviders is the traffic property: the capability of
// one endpoint+model is dialed once per process, not once per session.
func TestPropsCapsSharedAcrossProviders(t *testing.T) {
	var hits atomic.Int32
	srv := propsProbeServer(t, &hits, map[string]bool{"supports_preserve_reasoning": true})
	defer srv.Close()

	shared := NewPropsCapsCache()
	for i := 0; i < 3; i++ {
		p := propsProvider(t, srv, shared)
		if !p.templateSupportsPreserveReasoning(context.Background()) {
			t.Fatalf("session %d: capability reported unsupported", i+1)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("/props hits = %d, want 1 for three sessions sharing the endpoint", got)
	}
}

// TestPropsCapsSharedNegativeWindow pins the expensive case: an endpoint that
// does NOT answer /props must not be re-dialed by every session on every round.
func TestPropsCapsSharedNegativeWindow(t *testing.T) {
	var hits atomic.Int32
	srv := propsProbeServer(t, &hits, nil) // 404 on /props
	defer srv.Close()

	shared := NewPropsCapsCache()
	for i := 0; i < 3; i++ {
		p := propsProvider(t, srv, shared)
		if p.templateSupportsPreserveReasoning(context.Background()) {
			t.Fatalf("session %d: endpoint without /props reported the capability", i+1)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("/props hits = %d, want 1 (the negative result is shared for the retry window)", got)
	}
}

// TestPropsCapsInvalidateForcesReprobe pins the invalidation contract a model
// switch relies on: a router host can report a different template per model, so
// the cached answer must be droppable.
func TestPropsCapsInvalidateForcesReprobe(t *testing.T) {
	var hits atomic.Int32
	srv := propsProbeServer(t, &hits, map[string]bool{"supports_preserve_reasoning": true})
	defer srv.Close()

	shared := NewPropsCapsCache()
	p := propsProvider(t, srv, shared)
	if !p.templateSupportsPreserveReasoning(context.Background()) {
		t.Fatal("first probe: want supported")
	}
	p.invalidatePropsCaps()
	if !p.templateSupportsPreserveReasoning(context.Background()) {
		t.Fatal("post-invalidation probe: want supported")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("/props hits = %d, want 2 (invalidation forces a re-probe)", got)
	}
}

// TestPropsCapsCacheKeyIncludesModel pins the router-host rule: the capability
// is keyed by endpoint AND model, so two models on one endpoint never share an
// answer.
func TestPropsCapsCacheKeyIncludesModel(t *testing.T) {
	t.Parallel()
	if propsCapsKey("https://host/v1", "a") == propsCapsKey("https://host/v1", "b") {
		t.Fatal("different models on one endpoint must not share a capability entry")
	}
	if propsCapsKey("https://host/v1", "a") == propsCapsKey("https://other/v1", "a") {
		t.Fatal("different endpoints must not share a capability entry")
	}
}

// TestPropsCapsCacheWithoutSharedCacheKeepsLocalMemo pins the fallback: a
// provider built without a shared cache (tests, direct construction) keeps
// behaving exactly as before — its own memo answers within the retry window.
func TestPropsCapsCacheWithoutSharedCacheKeepsLocalMemo(t *testing.T) {
	var hits atomic.Int32
	srv := propsProbeServer(t, &hits, nil)
	defer srv.Close()

	p := propsProvider(t, srv, nil)
	for i := 0; i < 3; i++ {
		if p.templateSupportsPreserveReasoning(context.Background()) {
			t.Fatal("endpoint without /props reported the capability")
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("/props hits = %d, want 1 (the per-provider negative memo still applies)", got)
	}
	// The per-provider memo expires with propsProbeRetryTTL, exactly as before.
	p.propsMu.Lock()
	p.propsNegAt = time.Now().Add(-2 * propsProbeRetryTTL)
	p.propsMu.Unlock()
	if p.templateSupportsPreserveReasoning(context.Background()) {
		t.Fatal("endpoint without /props reported the capability")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("/props hits = %d, want 2 after the retry window elapsed", got)
	}
}

// TestPropsCapsCacheEvictsOldestWhenFull bounds the shared map: past the cap,
// the entry written longest ago is dropped while the newest stays cached.
func TestPropsCapsCacheEvictsOldestWhenFull(t *testing.T) {
	cache := NewPropsCapsCache()
	for i := 0; i < maxPropsCapsEntries+1; i++ {
		cache.RecordSuccess(propsCapsKey(fmt.Sprintf("https://endpoint-%d/v1", i), "model"), i%2 == 0)
	}
	cache.mu.Lock()
	size := len(cache.entries)
	_, keptFirst := cache.entries[propsCapsKey("https://endpoint-0/v1", "model")]
	_, keptLast := cache.entries[propsCapsKey(fmt.Sprintf("https://endpoint-%d/v1", maxPropsCapsEntries), "model")]
	cache.mu.Unlock()
	if size != maxPropsCapsEntries {
		t.Fatalf("entries = %d, want %d", size, maxPropsCapsEntries)
	}
	if keptFirst {
		t.Fatal("the oldest entry should have been evicted")
	}
	if !keptLast {
		t.Fatal("the newest entry must stay cached")
	}
}
