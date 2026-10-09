package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gogen/internal/modelinfo"

	"github.com/openai/openai-go/v3"
)

func TestLookupModelsDevLimitFromDisk(t *testing.T) {
	reg := map[string]any{
		"opencode": map[string]any{
			"id":  "opencode",
			"api": "https://opencode.ai/zen/v1",
			"models": map[string]any{
				"claude-opus-4-8": map[string]any{
					"id":          "claude-opus-4-8",
					"description": "Claude Opus 4.8",
					"limit":       map[string]int{"context": 1000000, "output": 128000},
				},
			},
		},
		"opencode-go": map[string]any{
			"id":  "opencode-go",
			"api": "https://opencode.ai/zen/go/v1",
			"models": map[string]any{
				"mimo-v2.5-pro": map[string]any{
					"id":    "mimo-v2.5-pro",
					"limit": map[string]int{"context": 1048576, "output": 128000},
				},
			},
		},
	}
	dir := t.TempDir()
	cache := filepath.Join(dir, "models.json")
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, b, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &OpenAIProvider{
		profiles:  []*providerProfile{{name: "default", baseURL: "https://opencode.ai/zen/v1/"}},
		modelInfo: modelinfo.NewResolver(cache),
	}

	start := time.Now()
	got, _, _, desc := p.lookupModelsDevInfo(ModelRef{ID: "claude-opus-4-8"})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("lookup blocked for %v (disk cache should be instant)", elapsed)
	}
	if got != 1000000 {
		t.Fatalf("zen model: got %d, want 1000000", got)
	}
	if desc != "Claude Opus 4.8" {
		t.Fatalf("description = %q, want %q", desc, "Claude Opus 4.8")
	}

	got, _, _, _ = p.lookupModelsDevInfo(ModelRef{ID: "mimo-v2.5-pro"})
	if got != 1048576 {
		t.Fatalf("go model via dual URL: got %d, want 1048576", got)
	}
}

func TestLookupModelsDevInfoNilSafe(t *testing.T) {
	var p *OpenAIProvider
	if got, _, _, _ := p.lookupModelsDevInfo(ModelRef{ID: "x"}); got != 0 {
		t.Fatalf("got %d", got)
	}
	p = &OpenAIProvider{}
	if got, _, _, _ := p.lookupModelsDevInfo(ModelRef{ID: "x"}); got != 0 {
		t.Fatalf("got %d", got)
	}
}

// TestModelsDevLookupFollowsOwningProfileBeforeCatalog pins the pre-catalog
// case of the owning-profile rule: ModelContextLimit's models.dev step runs
// BEFORE any catalog fetch (per-session providers are freshly constructed on
// restore), so modelProfile is empty — the lookup must still target the
// ref's OWNING profile endpoint. The default-profile fallback used to run
// the lookup against the default (OpenCode) endpoints, resolving another
// provider's entry or nothing at all — the "deepseek-flash limit comes from
// opencode" regression.
func TestModelsDevLookupFollowsOwningProfileBeforeCatalog(t *testing.T) {
	for _, tc := range []struct {
		name string
		// defaultLimit is the context limit the DEFAULT profile's registry
		// entry lists for the model ID; 0 means the entry is absent.
		defaultLimit int
		wantLimit    int
	}{
		{
			// The OpenCode Go catalog lists deepseek-flash but models.dev
			// has no entry for it: the default fallback resolved nothing
			// and the session fell to the 128k default context size.
			name:         "owning profile entry only",
			defaultLimit: 0,
			wantLimit:    1000000,
		},
		{
			// Same model ID on both providers with different limits: the
			// lookup must take the owning profile's entry, never the
			// default provider's.
			name:         "colliding id in both providers",
			defaultLimit: 128000,
			wantLimit:    1000000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defSrv := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(defSrv.Close)
			deepSrv := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(deepSrv.Close)

			defModels := map[string]any{}
			if tc.defaultLimit > 0 {
				defModels["deepseek-flash"] = map[string]any{
					"id":    "deepseek-flash",
					"limit": map[string]int{"context": tc.defaultLimit},
				}
			}
			cache := filepath.Join(t.TempDir(), "models.json")
			writeModelsDevRegistry(t, cache, map[string]any{
				"gateway": map[string]any{
					"id":     "gateway",
					"api":    defSrv.URL,
					"models": defModels,
				},
				"deepseek": map[string]any{
					"id":  "deepseek",
					"api": deepSrv.URL,
					"models": map[string]any{
						"deepseek-flash": map[string]any{
							"id":    "deepseek-flash",
							"limit": map[string]int{"context": 1000000},
						},
					},
				},
			})

			// Direct construction: no resolver Warm() (the test registry
			// must stay authoritative) and no catalog fetch has run.
			defStream, defCatalog := newClientPair(defSrv.URL, "k1", nil)
			deepStream, deepCatalog := newClientPair(deepSrv.URL, "k2", nil)
			p := &OpenAIProvider{
				profiles: []*providerProfile{
					{name: "default", baseURL: defSrv.URL, stream: defStream, catalog: defCatalog},
					{name: "deepseek", baseURL: deepSrv.URL, stream: deepStream, catalog: deepCatalog},
				},
				modelClient: make(map[ModelRef]*openai.Client),
				modelInfo:   modelinfo.NewResolver(cache),
			}
			if err := p.SetModelRef(ModelRef{Profile: "deepseek", ID: "deepseek-flash"}); err != nil {
				t.Fatal(err)
			}
			if len(p.modelProfile) != 0 {
				t.Fatalf("modelProfile has %d entries, want 0 (pre-catalog state)", len(p.modelProfile))
			}

			if urls := p.modelsDevURLsFor(p.CurrentModelRef()); len(urls) != 1 || urls[0] != deepSrv.URL {
				t.Fatalf("models.dev URLs = %v, want [%s] (owning profile)", urls, deepSrv.URL)
			}
			if lim, _, _, _ := p.lookupModelsDevInfo(p.CurrentModelRef()); lim != tc.wantLimit {
				t.Fatalf("models.dev limit = %d, want %d (owning profile's entry)", lim, tc.wantLimit)
			}
			// The full session-sizing path takes the same entry.
			if got, err := p.ModelContextLimit(context.Background()); err != nil || got != tc.wantLimit {
				t.Fatalf("ModelContextLimit = %d, %v; want %d", got, err, tc.wantLimit)
			}
		})
	}
}
