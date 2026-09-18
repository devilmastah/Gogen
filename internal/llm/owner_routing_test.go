package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

// dynamicCatalog is a /models server with a mutable model list: set
// replaces the listed models.
type dynamicCatalog struct {
	mu     sync.Mutex
	models []string
}

func (d *dynamicCatalog) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" && r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		d.mu.Lock()
		models := append([]string(nil), d.models...)
		d.mu.Unlock()
		data := make([]map[string]any, 0, len(models))
		for _, id := range models {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}
}

func (d *dynamicCatalog) set(models ...string) {
	d.mu.Lock()
	d.models = models
	d.mu.Unlock()
}

// invalidateCatalogForTest forces the next listModels to re-fetch every
// endpoint (bypassing the TTL cache and the failure backoff).
func (p *OpenAIProvider) invalidateCatalogForTest() {
	p.modelsMu.Lock()
	p.modelsCache = nil
	p.modelsCachedAt = time.Time{}
	p.modelsFetchFailedAt = time.Time{}
	p.modelsMu.Unlock()
}

// TestRoutingFollowsOwningProfile pins the per-profile routing contract: the
// ModelRef routing key (owning profile + bare ID) is the source of truth. A
// model the user picked under one profile routes to that profile's endpoint
// even when another profile's catalog lists the SAME model ID — a same-ID
// model on a different provider is a collision, not ownership.
func TestRoutingFollowsOwningProfile(t *testing.T) {
	def := &dynamicCatalog{models: []string{"remote-model"}}
	loc := &dynamicCatalog{models: []string{"local-model"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}, "local-model", t.TempDir(), nil)

	// The user picks the model under the "local" profile: the routing key
	// (profile + ID) is set together and cannot drift.
	if err := p.SetModelRef(ModelRef{Profile: "local", ID: "local-model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantLocal := p.profiles[1].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("a model picked under the local profile must route to the local endpoint")
	}

	// The default gateway starts listing the SAME model ID (a collision):
	// the routing key still wins — the model does not move.
	def.set("remote-model", "local-model")
	p.invalidateCatalogForTest()
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("a same-ID model on another profile is a collision, not ownership: the routing key wins")
	}
}

// TestSetProfilesKeepsAndDropsOwnerRouting pins the settings-save path:
// SetProfiles keeps the routing key when the owning profile is still
// registered, and resets it to the default profile when the owner is removed
// (the model then falls back to the default client; re-validation surfaces
// the gap to the user).
func TestSetProfilesKeepsAndDropsOwnerRouting(t *testing.T) {
	def := &dynamicCatalog{models: []string{"remote-model"}}
	loc := &dynamicCatalog{models: []string{"local-model"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	profiles := []ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}
	p := NewOpenAIProviderWithProfiles(profiles, "local-model", t.TempDir(), nil)
	if err := p.SetModelRef(ModelRef{Profile: "local", ID: "local-model"}); err != nil {
		t.Fatal(err)
	}

	// Re-saving the same profile set keeps the routing key: the owner is
	// still registered.
	if err := p.SetProfiles(profiles); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantLocal := p.profiles[1].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("SetProfiles keeping the owner profile must keep the routing key")
	}

	// Removing the owner profile resets the routing key to the default
	// profile: the model falls back to the default client.
	if err := p.SetProfiles([]ProviderProfile{{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"}}); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantDef := p.profiles[0].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantDef {
		t.Fatal("model of a removed profile must fall back to the default client")
	}
}

// TestUnknownModelOnNonOpenCodeProfileRoutesDirectly pins the per-profile
// routing contract for non-OpenCode profiles: a model picked under one
// routes to THAT profile's stream client even when no catalog lists it
// (custom/router model IDs) — no discovery, no fallback to the default
// profile, zero network I/O.
func TestUnknownModelOnNonOpenCodeProfileRoutesDirectly(t *testing.T) {
	def := &dynamicCatalog{models: []string{"remote-model"}}
	loc := &dynamicCatalog{models: []string{"local-model"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}, "custom-router-model", t.TempDir(), nil)
	if err := p.SetModelRef(ModelRef{Profile: "local", ID: "custom-router-model"}); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantLocal := p.profiles[1].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("an unknown model picked under the local profile must route to the local endpoint")
	}
}

// TestOpenCodeDefaultSecondProviderSelection pins the reported scenario: the
// default profile is OpenCode (zen/go twins) and a second plain provider
// lists a model ID that the OpenCode catalogs ALSO list. Selecting that
// model under the second provider must route to the second provider's
// endpoint — not to the OpenCode default, whose catalog merely happens to
// list the same ID.
func TestOpenCodeDefaultSecondProviderSelection(t *testing.T) {
	zenSrv := httptest.NewServer(openCodeCatalogHandler([]string{"zen-only", "shared-model"}, nil))
	goSrv := httptest.NewServer(openCodeCatalogHandler([]string{"go-only", "shared-model"}, nil))
	localSrv := catalogHandler("shared-model", "local-only")
	t.Cleanup(zenSrv.Close)
	t.Cleanup(goSrv.Close)
	t.Cleanup(localSrv.Close)

	zenClient := newTestOpenAIClient(zenSrv)
	goClient := newTestOpenAIClient(goSrv)
	localClient := newTestOpenAIClient(localSrv)
	p := &OpenAIProvider{
		profiles: []*providerProfile{
			{name: "default", zenStream: &zenClient, zenCatalog: &zenClient, goStream: &goClient, goCatalog: &goClient},
			{name: "local", stream: &localClient, catalog: &localClient},
		},
		modelClient: make(map[ModelRef]*openai.Client),
	}

	// The user selects the colliding model under the "local" provider.
	if err := p.SetModelRef(ModelRef{Profile: "local", ID: "shared-model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantLocal := p.profiles[1].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("a model selected under the second provider must route to that provider, not the OpenCode default")
	}

	// The same ID selected under the default (OpenCode) profile routes to
	// the Go twin (Go-over-Zen precedence on overlap).
	if err := p.SetModelRef(ModelRef{Profile: "default", ID: "shared-model"}); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(t.Context()); got != &goClient {
		t.Fatal("the same ID under the OpenCode default must route to the Go twin")
	}
}
