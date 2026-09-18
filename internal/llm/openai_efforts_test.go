package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/modelinfo"

	"github.com/openai/openai-go/v3"
)

// unslothQwen38Template is the effort-relevant core of the real Unsloth
// Qwen3.8-27B chat template (as returned by GET /props): a |default, a
// high→xhigh alias, and a not-in tuple validation whose raise_exception
// message enumerates the accepted values.
const unslothQwen38Template = `{%- set reasoning_instructions = '' %}
{%- if enable_thinking is undefined or enable_thinking is true %}
    {%- set resolved_reasoning_effort = reasoning_effort|default('xhigh') %}
    {%- if resolved_reasoning_effort == 'high' %}
        {%- set resolved_reasoning_effort = 'xhigh' %}
    {%- endif %}
    {%- if resolved_reasoning_effort not in ('xhigh', 'medium', 'low') %}
        {{- raise_exception('Unexpected reasoning effort ' ~ reasoning_effort ~ '. Supported types are xhigh (default), medium, and low.') }}
    {%- endif %}
{%- endif %}`

// effortTemplateWithoutValidation references reasoning_effort but declares no
// vocabulary — the sentinel /apply-template fallback must kick in.
const effortTemplateWithoutValidation = `{%- if reasoning_effort %}<think>effort={{ reasoning_effort }}</think>{%- endif %}`

func TestParseReasoningEffortsTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want []string
		ok   bool
	}{
		{
			name: "unsloth-tuple-with-default-and-alias",
			src:  unslothQwen38Template,
			want: []string{"xhigh", "medium", "low"},
			ok:   true,
		},
		{
			name: "qwen35-list",
			src:  `{%- if reasoning_effort is defined %}{%- if reasoning_effort not in ['low', 'medium', 'high'] %}{%- set reasoning_effort = 'medium' %}{%- endif %}{%- endif %}`,
			want: []string{"low", "medium", "high"},
			ok:   true,
		},
		{
			name: "double-quoted-tuple",
			src:  `{% if reasoning_effort not in ("low", "high") %}...{% endif %}`,
			want: []string{"low", "high"},
			ok:   true,
		},
		{
			name: "exception-message-only",
			src:  `{% if reasoning_effort is defined %}{{ raise_exception('Unexpected reasoning effort ' ~ reasoning_effort ~ '. Supported types are xhigh (default), medium, and low.') }}{% endif %}`,
			want: []string{"xhigh", "medium", "low"},
			ok:   true,
		},
		{
			name: "no-effort-reference",
			src:  `{% if enable_thinking is defined and enable_thinking is false %}<think>\n\n</think>{% else %}<think>\n{% endif %}`,
			ok:   false,
		},
		{
			name: "reference-without-validation",
			src:  effortTemplateWithoutValidation,
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseReasoningEffortsTemplate(tc.src)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("efforts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseSupportedTypesMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want []string
		ok   bool
	}{
		{
			name: "llama-server-error",
			msg:  "\n------------\nWhile executing CallExpression at line 64, column 28 in source:\n...', 'low') %}\n^\nError: Jinja Exception: Unexpected reasoning effort max. Supported types are xhigh (default), medium, and low.",
			want: []string{"xhigh", "medium", "low"},
			ok:   true,
		},
		{
			name: "quoted-values-with-colon",
			msg:  "Invalid effort. Supported types are: 'low', 'medium', 'high'.",
			want: []string{"low", "medium", "high"},
			ok:   true,
		},
		{
			name: "not-an-enumeration",
			msg:  "Jinja Exception: Unexpected reasoning effort max",
			ok:   false,
		},
		{
			name: "garbage-token-invalidates",
			msg:  "Supported types are xhigh, medium, and something weird here.",
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSupportedTypesMessage(tc.msg)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("efforts = %v, want %v", got, tc.want)
			}
		})
	}
}

// effortsPropsServer serves a llama.cpp-style GET /props plus an optional
// /apply-template handler, counting hits on both.
func effortsPropsServer(t *testing.T, template string, caps map[string]bool, applyHandler http.HandlerFunc) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	propsHits := new(atomic.Int32)
	applyHits := new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("/props", func(w http.ResponseWriter, r *http.Request) {
		propsHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat_template_caps": caps,
			"chat_template":      template,
		})
	})
	mux.HandleFunc("/apply-template", func(w http.ResponseWriter, r *http.Request) {
		applyHits.Add(1)
		if applyHandler != nil {
			applyHandler(w, r)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, propsHits, applyHits
}

func TestProbeReasoningEffortsFromProps(t *testing.T) {
	t.Parallel()

	newProvider := func(t *testing.T, srv *httptest.Server, model string) *OpenAIProvider {
		t.Helper()
		return NewOpenAIProviderWithResolver("key", model, srv.URL+"/v1", t.TempDir(), nil)
	}

	t.Run("parsed-from-template", func(t *testing.T) {
		srv, propsHits, applyHits := effortsPropsServer(t, unslothQwen38Template,
			map[string]bool{"supports_reasoning_effort": true}, nil)
		p := newProvider(t, srv, "qwen3.8")

		changed, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8")
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("first probe: want changed=true (default → derived)")
		}
		if got := p.ModelReasoningEfforts(ModelRef{ID: "qwen3.8"}); !slices.Equal(got, []string{"xhigh", "medium", "low"}) {
			t.Fatalf("ModelReasoningEfforts = %v, want [xhigh medium low]", got)
		}
		if applyHits.Load() != 0 {
			t.Fatalf("apply-template hits = %d, want 0 (static parse suffices)", applyHits.Load())
		}

		changed, err = p.ProbeReasoningEfforts(context.Background(), "qwen3.8")
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Fatal("second probe: want changed=false (cached)")
		}
		if propsHits.Load() != 1 {
			t.Fatalf("props hits = %d, want 1 (cached)", propsHits.Load())
		}
	})

	t.Run("sentinel-error-enumerates", func(t *testing.T) {
		srv, _, applyHits := effortsPropsServer(t, effortTemplateWithoutValidation,
			map[string]bool{"supports_reasoning_effort": true},
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"code":    500,
						"message": "Error: Jinja Exception: Unexpected reasoning effort __gogen_probe__. Supported types are xhigh (default), medium, and low.",
					},
				})
			})
		p := newProvider(t, srv, "qwen3.8")

		changed, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8")
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		if got := p.ModelReasoningEfforts(ModelRef{ID: "qwen3.8"}); !slices.Equal(got, []string{"xhigh", "medium", "low"}) {
			t.Fatalf("ModelReasoningEfforts = %v, want [xhigh medium low]", got)
		}
		if applyHits.Load() != 1 {
			t.Fatalf("apply-template hits = %d, want 1", applyHits.Load())
		}
	})

	t.Run("sentinel-accepted-anything", func(t *testing.T) {
		srv, _, applyHits := effortsPropsServer(t, effortTemplateWithoutValidation,
			map[string]bool{"supports_reasoning_effort": true},
			func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"prompt": "<|im_start|>user\nhi<|im_end|>"})
			})
		p := newProvider(t, srv, "qwen3.8")

		if _, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8"); err != nil {
			t.Fatal(err)
		}
		if got := p.ModelReasoningEfforts(ModelRef{ID: "qwen3.8"}); !slices.Equal(got, DefaultReasoningEfforts) {
			t.Fatalf("ModelReasoningEfforts = %v, want default %v", got, DefaultReasoningEfforts)
		}
		if applyHits.Load() != 1 {
			t.Fatalf("apply-template hits = %d, want 1", applyHits.Load())
		}
	})

	t.Run("no-effort-support", func(t *testing.T) {
		srv, _, applyHits := effortsPropsServer(t, `{% if enable_thinking %}x{% endif %}`,
			map[string]bool{"supports_reasoning_effort": false}, nil)
		p := newProvider(t, srv, "qwen3.8")

		changed, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8")
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true (default → no effort control)")
		}
		if got := p.ModelReasoningEfforts(ModelRef{ID: "qwen3.8"}); len(got) != 0 {
			t.Fatalf("ModelReasoningEfforts = %v, want empty (no effort control)", got)
		}
		if applyHits.Load() != 0 {
			t.Fatalf("apply-template hits = %d, want 0", applyHits.Load())
		}
	})

	t.Run("non-llama-endpoint-not-cached", func(t *testing.T) {
		srv, propsHits, _ := effortsPropsServer(t, "", map[string]bool{}, nil)
		p := newProvider(t, srv, "qwen3.8")

		if _, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8"); err == nil {
			t.Fatal("want errEffortsUnavailable for a server without capability keys")
		}
		if got := p.ModelReasoningEfforts(ModelRef{ID: "qwen3.8"}); !slices.Equal(got, DefaultReasoningEfforts) {
			t.Fatalf("ModelReasoningEfforts = %v, want default (nothing cached)", got)
		}
		// Not cached: a second probe must retry the endpoint.
		if _, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8"); err == nil {
			t.Fatal("want errEffortsUnavailable on retry")
		}
		if propsHits.Load() != 2 {
			t.Fatalf("props hits = %d, want 2 (failures are not cached)", propsHits.Load())
		}
	})
}

func TestProbeEffortValuesViaApplyTemplateSendsModel(t *testing.T) {
	t.Parallel()

	var bodyMu sync.Mutex
	var gotBody map[string]any
	var gotModelParam string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"chat_template_caps": map[string]bool{"supports_reasoning_effort": true},
				"chat_template":      effortTemplateWithoutValidation,
			})
		case "/apply-template":
			bodyMu.Lock()
			gotModelParam = r.URL.Query().Get("model")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			bodyMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"prompt": "hi"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewOpenAIProviderWithProfiles(
		[]ProviderProfile{{Name: "default", BaseURL: srv.URL + "/v1", APIKey: "k"}},
		"qwen3.8", t.TempDir(), nil)
	if _, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8"); err != nil {
		t.Fatal(err)
	}

	bodyMu.Lock()
	defer bodyMu.Unlock()
	if gotModelParam != "qwen3.8" {
		t.Fatalf("apply-template ?model= %q, want qwen3.8", gotModelParam)
	}
	if gotBody["model"] != "qwen3.8" {
		t.Fatalf("body model = %v, want qwen3.8", gotBody["model"])
	}
	if _, ok := gotBody["messages"]; !ok {
		t.Fatalf("body missing messages: %v", gotBody)
	}
	kwargs, _ := gotBody["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "__gogen_probe__" {
		t.Fatalf("body chat_template_kwargs = %v, want sentinel", gotBody["chat_template_kwargs"])
	}
}

// effortRegistryAt writes a models.dev-style registry whose provider api is
// the given URL (so provider base URL and registry key match).
func effortRegistryAt(t *testing.T, api string, models map[string][]string) *modelinfo.Resolver {
	t.Helper()
	reg := map[string]any{
		"opencode": map[string]any{
			"id":     "opencode",
			"api":    api,
			"models": map[string]any{},
		},
	}
	modelsMap := reg["opencode"].(map[string]any)["models"].(map[string]any)
	for id, efforts := range models {
		entry := map[string]any{
			"id":    id,
			"limit": map[string]int{"context": 200000},
		}
		if efforts == nil {
			entry["reasoning_options"] = []map[string]any{{"type": "toggle"}}
		} else {
			entry["reasoning_options"] = []map[string]any{{"type": "effort", "values": efforts}}
		}
		modelsMap[id] = entry
	}
	dir := t.TempDir()
	cache := dir + "/models.json"
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return modelinfo.NewResolver(cache)
}

func TestModelReasoningEffortsPrecedence(t *testing.T) {
	t.Parallel()
	srv, _, _ := effortsPropsServer(t, unslothQwen38Template,
		map[string]bool{"supports_reasoning_effort": true}, nil)
	resolver := effortRegistryAt(t, srv.URL+"/v1", map[string][]string{
		"glm-5.2": {"high", "max"},
	})
	p := NewOpenAIProviderWithResolver("key", "glm-5.2", srv.URL+"/v1", t.TempDir(), resolver)

	// Unknown to the registry: derived set applies after the probe. The
	// probe targets the CURRENT model (production callers always probe the
	// model they just selected), so switch first.
	if err := p.SetModel("other"); err != nil {
		t.Fatal(err)
	}
	if got := p.ModelReasoningEfforts(ModelRef{ID: "other"}); !slices.Equal(got, DefaultReasoningEfforts) {
		t.Fatalf("unprobed ModelReasoningEfforts = %v, want default", got)
	}
	if _, err := p.ProbeReasoningEfforts(context.Background(), "other"); err != nil {
		t.Fatal(err)
	}
	if got := p.ModelReasoningEfforts(ModelRef{ID: "other"}); !slices.Equal(got, []string{"xhigh", "medium", "low"}) {
		t.Fatalf("probed ModelReasoningEfforts = %v, want [xhigh medium low]", got)
	}

	// Known to the registry: the models.dev set wins over the derived one.
	if err := p.SetModel("glm-5.2"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ProbeReasoningEfforts(context.Background(), "glm-5.2"); err != nil {
		t.Fatal(err)
	}
	if got := p.ModelReasoningEfforts(ModelRef{ID: "glm-5.2"}); !slices.Equal(got, []string{"high", "max"}) {
		t.Fatalf("known-model ModelReasoningEfforts = %v, want [high max] (registry wins)", got)
	}
}

func TestReasoningEffortUnsupported(t *testing.T) {
	t.Parallel()

	t.Run("registry-toggle-only", func(t *testing.T) {
		resolver := effortRegistry(t, map[string][]string{
			"glm-5":   nil, // toggle-only: no effort control
			"glm-5.2": {"high", "max"},
		})
		known := &OpenAIProvider{
			profiles:  []*providerProfile{{name: "default", baseURL: "https://opencode.ai/zen/v1/"}},
			modelInfo: resolver,
		}
		if !known.ReasoningEffortUnsupported(ModelRef{ID: "glm-5"}) {
			t.Fatal("toggle-only model: want unsupported=true (models.dev, no probe needed)")
		}
		if got := known.ModelReasoningEfforts(ModelRef{ID: "glm-5"}); len(got) != 0 {
			t.Fatalf("toggle-only ModelReasoningEfforts = %v, want empty", got)
		}
		if known.ReasoningEffortUnsupported(ModelRef{ID: "glm-5.2"}) {
			t.Fatal("model with effort options: want unsupported=false")
		}
		if got := known.ModelReasoningEfforts(ModelRef{ID: "glm-5.2"}); !slices.Equal(got, []string{"high", "max"}) {
			t.Fatalf("ModelReasoningEfforts = %v, want [high max]", got)
		}
		if known.ReasoningEffortUnsupported(ModelRef{ID: "unknown-model"}) {
			t.Fatal("unknown model: want unsupported=false (no positive evidence)")
		}
		if got := known.ModelReasoningEfforts(ModelRef{ID: "unknown-model"}); !slices.Equal(got, DefaultReasoningEfforts) {
			t.Fatalf("unknown ModelReasoningEfforts = %v, want default", got)
		}
	})

	t.Run("probe-no-support", func(t *testing.T) {
		srv, _, _ := effortsPropsServer(t, `{% if enable_thinking %}x{% endif %}`,
			map[string]bool{"supports_reasoning_effort": false}, nil)
		p := NewOpenAIProviderWithResolver("key", "qwen3.8", srv.URL+"/v1", t.TempDir(), nil)
		if p.ReasoningEffortUnsupported(ModelRef{ID: "qwen3.8"}) {
			t.Fatal("unprobed: want unsupported=false")
		}
		if _, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8"); err != nil {
			t.Fatal(err)
		}
		if !p.ReasoningEffortUnsupported(ModelRef{ID: "qwen3.8"}) {
			t.Fatal("probed no-support: want unsupported=true")
		}
	})

	t.Run("probe-supported", func(t *testing.T) {
		srv, _, _ := effortsPropsServer(t, unslothQwen38Template,
			map[string]bool{"supports_reasoning_effort": true}, nil)
		p := NewOpenAIProviderWithResolver("key", "qwen3.8", srv.URL+"/v1", t.TempDir(), nil)
		if _, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8"); err != nil {
			t.Fatal(err)
		}
		if p.ReasoningEffortUnsupported(ModelRef{ID: "qwen3.8"}) {
			t.Fatal("probe supported: want unsupported=false")
		}
	})

	t.Run("registry-toggle-only-but-probe-supports", func(t *testing.T) {
		// Contradiction: the registry entry declares no effort control, but
		// the runtime llama.cpp template references reasoning_effort. The
		// runtime wins: chips shown with the derived set.
		srv, _, _ := effortsPropsServer(t, unslothQwen38Template,
			map[string]bool{"supports_reasoning_effort": true}, nil)
		resolver := effortRegistryAt(t, srv.URL+"/v1", map[string][]string{"qwen3.8": nil})
		p := NewOpenAIProviderWithResolver("key", "qwen3.8", srv.URL+"/v1", t.TempDir(), resolver)
		if !p.ReasoningEffortUnsupported(ModelRef{ID: "qwen3.8"}) {
			t.Fatal("pre-probe: registry toggle-only → unsupported=true")
		}
		if _, err := p.ProbeReasoningEfforts(context.Background(), "qwen3.8"); err != nil {
			t.Fatal(err)
		}
		if p.ReasoningEffortUnsupported(ModelRef{ID: "qwen3.8"}) {
			t.Fatal("post-probe: runtime supports effort → unsupported=false")
		}
		if got := p.ModelReasoningEfforts(ModelRef{ID: "qwen3.8"}); !slices.Equal(got, []string{"xhigh", "medium", "low"}) {
			t.Fatalf("ModelReasoningEfforts = %v, want [xhigh medium low]", got)
		}
	})
}

func TestReasoningEffortsInvalidatedOnSetProfiles(t *testing.T) {
	t.Parallel()
	srv, propsHits, _ := effortsPropsServer(t, unslothQwen38Template,
		map[string]bool{"supports_reasoning_effort": true}, nil)
	p := NewOpenAIProviderWithProfiles(
		[]ProviderProfile{{Name: "default", BaseURL: srv.URL + "/v1", APIKey: "k"}},
		"m", t.TempDir(), nil)

	if _, err := p.ProbeReasoningEfforts(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	if got := p.ModelReasoningEfforts(ModelRef{ID: "m"}); !slices.Equal(got, []string{"xhigh", "medium", "low"}) {
		t.Fatalf("ModelReasoningEfforts = %v, want [xhigh medium low]", got)
	}

	// A profile swap (even to the same endpoint) clears the derivation.
	if err := p.SetProfiles([]ProviderProfile{{Name: "default", BaseURL: srv.URL + "/v1", APIKey: "k"}}); err != nil {
		t.Fatal(err)
	}
	if got := p.ModelReasoningEfforts(ModelRef{ID: "m"}); !slices.Equal(got, DefaultReasoningEfforts) {
		t.Fatalf("post-SetProfiles ModelReasoningEfforts = %v, want default", got)
	}
	if _, err := p.ProbeReasoningEfforts(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	if got := p.ModelReasoningEfforts(ModelRef{ID: "m"}); !slices.Equal(got, []string{"xhigh", "medium", "low"}) {
		t.Fatalf("re-probed ModelReasoningEfforts = %v, want [xhigh medium low]", got)
	}
	if propsHits.Load() != 2 {
		t.Fatalf("props hits = %d, want 2 (one per probe generation)", propsHits.Load())
	}
}

// TestPropsProbeUsesOwningProfileAPIKey pins that the llama.cpp capability
// probes (/props and /apply-template, shared by the reasoning-effort and
// preserve-reasoning paths) authenticate with the API key of the endpoint
// they actually dial. With two registered profiles, a non-default model's
// /props request must carry the owning (secondary) profile's Bearer token,
// not the default profile's.
func TestPropsProbeUsesOwningProfileAPIKey(t *testing.T) {
	t.Parallel()

	newAuthServer := func(t *testing.T, wantKey string, hits *atomic.Int32) *httptest.Server {
		t.Helper()
		mux := http.NewServeMux()
		mux.HandleFunc("/props", func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"chat_template_caps": map[string]bool{"supports_reasoning_effort": true},
				"chat_template":      unslothQwen38Template,
			})
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}

	defHits := new(atomic.Int32)
	secHits := new(atomic.Int32)
	defSrv := newAuthServer(t, "default-key", defHits)
	secSrv := newAuthServer(t, "secondary-key", secHits)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL + "/v1", APIKey: "default-key"},
		{Name: "secondary", BaseURL: secSrv.URL + "/v1", APIKey: "secondary-key"},
	}, "sec-model", t.TempDir(), nil)
	// The catalog associates sec-model with the secondary profile; emulate
	// the post-fetch routing state directly.
	p.modelsMu.Lock()
	p.modelsCache = []catalogModel{{model: openai.Model{ID: "sec-model"}, profile: "secondary"}}
	p.modelsCachedAt = time.Now()
	p.modelProfile = map[ModelRef]modelProfileInfo{
		{Profile: "secondary", ID: "sec-model"}: {name: "secondary", baseURL: secSrv.URL + "/v1"},
	}
	p.modelsMu.Unlock()

	changed, err := p.ProbeReasoningEfforts(context.Background(), "sec-model")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("want changed=true (default → derived)")
	}
	if got := p.ModelReasoningEfforts(ModelRef{ID: "sec-model"}); !slices.Equal(got, []string{"xhigh", "medium", "low"}) {
		t.Fatalf("ModelReasoningEfforts = %v, want [xhigh medium low]", got)
	}
	if secHits.Load() != 1 {
		t.Fatalf("secondary /props hits = %d, want 1", secHits.Load())
	}
	if defHits.Load() != 0 {
		t.Fatalf("default /props hits = %d, want 0 (probe must target the owning profile)", defHits.Load())
	}
}
