package llm

import (
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"gogen/internal/buildinfo"
	"gogen/internal/config"
	"gogen/internal/modelinfo"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
)

const (
	// The OpenCode wire endpoints. The /zen/ path is the spelling older
	// configs and docs use; these /inference/ twins are the ones the clients
	// are built with.
	openCodeZenBaseURL = "https://opencode.ai/inference/v1/"
	openCodeGoBaseURL  = "https://opencode.ai/inference/go/v1/"
	// The OpenCode base URLs models.dev lists. The registry's "api" field is
	// the OpenAI-compatible path (it carries an extra /openai segment the
	// wire endpoints do not need), and it drifts: the registry has shipped
	// /zen/v1, then /inference/v1, and now /inference/openai/v1. Registry
	// lookups therefore try the CURRENT spelling first and fall back to the
	// earlier ones (see openCodeGoRegistryURLs / openCodeZenRegistryURLs), so
	// a spelling change on either side does not silently drop every OpenCode
	// context limit, price and description back to the 128k default.
	openCodeZenRegistryURL = "https://opencode.ai/inference/openai/v1/"
	openCodeGoRegistryURL  = "https://opencode.ai/inference/go/openai/v1/"
	// modelsCacheTTL avoids repeated full catalog fetches during startup
	// (ValidateRestoredModel → ListModels + ModelContextLimit) and /models flows.
	modelsCacheTTL = 60 * time.Second
	// modelsCatalogTimeout bounds /v1/models for ListModels / UI catalog.
	// Separate from the long SSE stream idle timeout so a hung catalog
	// endpoint cannot pin startup or the web WS loop for minutes.
	modelsCatalogTimeout = 8 * time.Second
	// modelsLimitLookupTimeout bounds the /v1/models attempt inside
	// ModelContextLimit. Local llama.cpp answers in milliseconds; a hung
	// remote catalog must fail fast so models.dev / heuristics can run.
	// ListModels keeps modelsCatalogTimeout.
	modelsLimitLookupTimeout = 1500 * time.Millisecond
	// propsProbeTimeout bounds GET /props for chat_template_caps discovery.
	propsProbeTimeout = 1500 * time.Millisecond
)

// modelsFetchBackoff gates re-fetching after a failed catalog fetch: a dead
// OpenCode endpoint costs one bounded attempt per window instead of one per
// chat request. A var (not const) so tests can shrink it, mirroring how
// modelsCacheTTL governs cache staleness.
var modelsFetchBackoff = 30 * time.Second

// propsProbeRetryTTL bounds negative caching of a failed capability probe: an
// endpoint that does not answer GET /props (404 on a plain OpenAI-compatible
// server, timeout, unparseable body) is not re-probed on every agent-loop
// round — the failed attempt is remembered for this window and skipped, then
// re-probed so a cold-start endpoint that comes up later is still discovered.
// A var (not const) so tests can shrink it, mirroring modelsFetchBackoff.
var propsProbeRetryTTL = 30 * time.Second

// profileCatalogTimeout bounds ONE provider's catalog query inside the
// overall fetch budget: profile queries run in parallel, so a hung endpoint
// (offline host that never refuses the connection) costs only this window
// instead of the full modelsCatalogTimeout — the merge keeps the catalogs
// that answered. A var (not const) so tests can shrink it, mirroring
// modelsFetchBackoff.
var profileCatalogTimeout = 3 * time.Second

// propsHTTPClient is a plain short-timeout client for capability probes.
// Intentionally not the SSE client (idle read deadlines are stream-oriented).
//
// Keep-alives are disabled. Probes target many short-lived llama.cpp
// endpoints (and, under t.Parallel(), repeatedly-started httptest servers that
// recycle ephemeral ports). A pooled idle connection to a since-closed
// endpoint can be reused against a different server; the transport retries such
// stale-connection failures for idempotent requests (GET /props) but not for
// POST /apply-template, so a lingering connection intermittently breaks the
// probe. Disabling keep-alives forces a fresh dial per probe, always reaching
// the live endpoint. The transport is cloned from DefaultTransport so proxy
// and TLS behavior are unchanged.
var propsHTTPClient = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return &http.Client{Timeout: propsProbeTimeout, Transport: t}
}()

// isOpencodeURL reports whether baseURL points to an OpenCode endpoint that
// should also expose the Go model family at openCodeGoBaseURL.
func isOpencodeURL(baseURL string) bool {
	return strings.Contains(baseURL, "opencode.ai")
}

// openCodeGoRegistryURLs returns the base URLs to try when asking models.dev
// for the Go endpoint's catalog: the registry's current spelling first, then
// the older spellings a registry snapshot (or a stale disk cache) may still
// carry — the pre-/openai/ inference path and the original /zen/ path.
func openCodeGoRegistryURLs() []string {
	return []string{
		openCodeGoRegistryURL,
		openCodeGoBaseURL,
		"https://opencode.ai/zen/go/v1/",
	}
}

// openCodeZenRegistryURLs is the Zen counterpart of openCodeGoRegistryURLs.
func openCodeZenRegistryURLs() []string {
	return []string{
		openCodeZenRegistryURL,
		openCodeZenBaseURL,
		"https://opencode.ai/zen/v1/",
	}
}

// openCodeRegistryURLs returns every models.dev base URL for the OpenCode
// endpoints, deduplicated: the Go endpoint first, matching
// inferOpenCodeEndpoint's Go-over-zen precedence.
func openCodeRegistryURLs() []string {
	urls := make([]string, 0, 6)
	seen := make(map[string]struct{}, 6)
	for _, u := range append(openCodeGoRegistryURLs(), openCodeZenRegistryURLs()...) {
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	return urls
}

// ProviderProfile is one registered OpenAI-compatible API endpoint: a base
// URL, an optional API key, and an optional default model. The first entry
// is the default profile (fallback client + new-session model seed); later
// entries are additional registered providers whose models are aggregated
// into the model catalog and routed per model (clientForModel).
type ProviderProfile struct {
	Name    string
	BaseURL string
	APIKey  string
	Model   string
}

// providerProfile is the internal endpoint set: the openai clients for one
// registered provider. An OpenCode profile additionally carries its zen/go
// twin clients; non-OpenCode profiles carry a single stream + catalog pair.
type providerProfile struct {
	name       string
	baseURL    string
	apiKey     string
	model      string
	stream     *openai.Client
	catalog    *openai.Client
	zenStream  *openai.Client
	zenCatalog *openai.Client
	goStream   *openai.Client
	goCatalog  *openai.Client
}

// modelProfileInfo records which registered provider profile serves a model:
// the profile name (model-picker grouping) and its base URL (models.dev and
// /props resolution).
type modelProfileInfo struct {
	name    string
	baseURL string
}

type OpenAIProvider struct {
	model string
	// currentModelProfile is the registered provider profile that owns the
	// current model (the ModelRef routing key; "" = the default profile).
	// Guarded by modelsMu exactly like model — the two change together
	// (SetModelRef / SetModel) so the routing key and the wire ID can never
	// drift apart. (Distinct from the modelProfile map, which records the
	// owning profile of each CATALOG model.)
	currentModelProfile string
	// profiles is the registered endpoint set — the single source of truth
	// for every client and endpoint. profiles[0] is the default profile
	// (fallback client, default base URL, new-session model seed). Nil for
	// direct-constructed providers (tests) that only exercise pure methods;
	// the accessors below return zero values for that shape.
	profiles []*providerProfile
	// modelClient maps the ModelRef routing key → streaming client for the
	// OpenCode profiles' twin routing (which of the zen/go twins serves the
	// model — Go takes precedence on overlap, see queryProfile).
	// Non-OpenCode profiles need no entry: clientForModel uses their stream
	// client directly. Guarded by modelsMu.
	modelClient map[ModelRef]*openai.Client
	// profilesGen is bumped by SetProfiles. A catalog fetch started before
	// the swap records the generation it began with; if the generation
	// changed when it completes, its results came from the OLD endpoint set
	// and are discarded instead of polluting the cache/routing with stale
	// models (the next fetch hits the new endpoints).
	profilesGen int
	// modelProfile records which profile serves each catalog model (the
	// ModelRef routing key → profile name + base URL), populated by
	// listModels alongside the model cache. Used for models.dev resolution
	// and the /props probe target (the OWNING profile's base URL — a bare
	// ID can no longer name it, since the same ID may sit under several
	// providers). Guarded by modelsMu.
	modelProfile map[ModelRef]modelProfileInfo
	// promptCacheKey scopes provider-side prompt caching (defaults to none).
	promptCacheKey param.Opt[string]
	// modelInfo resolves context limits from models.dev (optional enrichment).
	modelInfo *modelinfo.Resolver

	modelsMu       sync.RWMutex
	modelsCache    []catalogModel
	modelsCachedAt time.Time // zero means no successful cache entry
	// modelsFetchFailedAt is set when a catalog fetch fails; clientForModel
	// skips re-fetching until modelsFetchBackoff has elapsed.
	modelsFetchFailedAt time.Time
	modelsFetch         *modelsFetch

	// propsCaps caches llama.cpp GET /props chat_template_caps. Invalidated
	// on SetModel because router/multi-model hosts may change the template.
	// The cache is keyed by endpoint (propsBaseURL): with multiple
	// registered profiles, the probe must hit the CURRENT model's owning
	// endpoint. Only completed probes (200 + parseable JSON) are cached as a
	// value (propsChecked/propsPreserveReasoning); a failed attempt is
	// remembered only for propsProbeRetryTTL (propsNegBaseURL/propsNegAt) so
	// the agent loop does not re-dial an endpoint without /props on every
	// round, while a cold-start endpoint that later comes up is still
	// re-probed once the window elapses.
	propsMu                sync.Mutex
	propsChecked           bool
	propsBaseURL           string
	propsPreserveReasoning bool
	propsNegBaseURL        string
	propsNegAt             time.Time

	// effortsDerived caches per-model reasoning-effort options derived from
	// llama.cpp /props (+ /apply-template) capability probes (see
	// ProbeReasoningEfforts). Keyed by the ModelRef routing key (the same
	// ID under two providers may have different derived efforts); entries
	// record the endpoint they were derived from. Cleared by SetProfiles
	// (the endpoint set changed); NOT cleared by SetModel — each model is
	// probed once and the result stays valid across switches back to it.
	effortsMu      sync.Mutex
	effortsDerived map[ModelRef]derivedEfforts

	// preserveReasoningMode: auto (default, probe /props), on, off.
	// Guarded by modelsMu.
	preserveReasoningMode string

	// thinkingLevel controls reasoning_effort. Empty means omit (no thinking).
	// Guarded by modelsMu.
	thinkingLevel string

	// sessionHdr carries the current session ID (and its derived UUIDv5) for
	// the X-Opencode-Session header. Shared with the OpenCode clients' header
	// round-trippers and the /props probe; nil for direct-constructed
	// providers (tests) that only exercise pure methods.
	sessionHdr *sessionHeaderState

	// wire accumulates this session's upload (bytes by contributor) for the
	// debug-log wire-byte diagnostic. Untouched unless debug logging is
	// enabled.
	wire wireCounters

	// endpointCatalog is the process-shared catalog cache (see
	// EndpointCatalog): every session's provider otherwise downloaded the
	// same endpoint's /v1/models independently. Nil for direct-constructed
	// providers, which then fetch directly. Set at construction time via
	// SetEndpointCatalog, before the provider serves requests.
	endpointCatalog *EndpointCatalog

	// propsCaps is the process-shared /props capability cache (see
	// PropsCapsCache): every session's provider otherwise re-probed the same
	// endpoint's capability for itself. Nil for direct-constructed providers,
	// which keep their own per-provider memo instead.
	propsCaps *PropsCapsCache
}

// ProviderProfiles builds the registered OpenAI-compatible endpoint list for
// provider construction: the implicit "default" profile from the legacy
// config fields first, then the additional registered providers in order
// (profile order is the duplicate-model-ID precedence — the default profile
// wins on collisions). Shared by setup.go, the TUI subagent spawner, and
// the web workspace provider factory so every path aggregates the same
// endpoint set from the same source values.
func ProviderProfiles(openAIKey, openAIModel, openAIURL string, providers []config.OpenAIProviderConfig) []ProviderProfile {
	profiles := []ProviderProfile{{
		Name:    "default",
		BaseURL: openAIURL,
		APIKey:  openAIKey,
		Model:   openAIModel,
	}}
	for _, p := range providers {
		profiles = append(profiles, ProviderProfile{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model})
	}
	return profiles
}

type modelsFetch struct {
	done   chan struct{}
	models []catalogModel
	err    error
}

func (p *OpenAIProvider) ModelName() string {
	return p.currentModel()
}

func (p *OpenAIProvider) currentModel() string {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return p.model
}

func (p *OpenAIProvider) cachedModelsLocked() ([]catalogModel, bool) {
	if p.modelsCachedAt.IsZero() || time.Since(p.modelsCachedAt) >= modelsCacheTTL {
		return nil, false
	}
	return p.modelsCache, true
}

func NewOpenAIProvider(apiKey string, model string, baseURL string, workingDir string) *OpenAIProvider {
	return NewOpenAIProviderWithResolver(apiKey, model, baseURL, workingDir, nil)
}

// NewOpenAIProviderWithResolver is NewOpenAIProvider with an optional shared
// models.dev resolver. The multi-session web server passes one resolver per
// workspace so N per-session providers do not issue N parallel catalog
// fetches or race N atomic writes to the same .gogen/models.json cache file.
// A nil resolver builds a private one (single-session/TUI behavior).
func NewOpenAIProviderWithResolver(apiKey string, model string, baseURL string, workingDir string, resolver *modelinfo.Resolver) *OpenAIProvider {
	profiles := []ProviderProfile{{Name: "default", BaseURL: baseURL, APIKey: apiKey, Model: model}}
	return NewOpenAIProviderWithProfiles(profiles, model, workingDir, resolver)
}

// NewOpenAIProviderWithProfiles builds a multi-endpoint provider:
// profiles[0] is the default profile (fallback client, new-session model
// seed); every registered profile's catalog is aggregated into one model
// list, and each model routes to its owning profile's endpoint
// (clientForModel). A nil resolver builds a private one rooted at
// workingDir.
func NewOpenAIProviderWithProfiles(profiles []ProviderProfile, model string, workingDir string, resolver *modelinfo.Resolver) *OpenAIProvider {
	if resolver == nil {
		resolver = modelinfo.NewResolver(modelinfo.CachePath(workingDir))
	}
	st := newSessionHeaderState()
	built := make([]*providerProfile, 0, len(profiles))
	for _, pr := range profiles {
		name := strings.TrimSpace(pr.Name)
		if name == "" {
			name = "default"
		}
		built = append(built, buildProfileClients(providerProfile{
			name:    name,
			baseURL: strings.TrimSpace(pr.BaseURL),
			apiKey:  pr.APIKey,
			model:   pr.Model,
		}, st))
	}
	return newOpenAIProvider(built, model, resolver, st)
}

// newClientPair builds the stream + catalog client pair for one endpoint:
// the stream client carries the SSE transport (long idle reads), the catalog
// client the short-timeout one (see newSSEHTTPClient/newCatalogHTTPClient).
// Empty baseURL/apiKey omit the respective option (default endpoint / no
// auth header). A non-nil st wraps both transports so every request carries
// the current X-Opencode-Session header (OpenCode endpoints only — see
// buildProfileClients); nil leaves the clients untagged.
func newClientPair(baseURL, apiKey string, st *sessionHeaderState) (stream, catalog *openai.Client) {
	streamHTTP := newSSEHTTPClient()
	catalogHTTP := newCatalogHTTPClient()
	if st != nil {
		// The clients are freshly built per call, so re-wrapping the
		// Transport touches no shared state; openai-go reads both Client and
		// Transport from it.
		streamHTTP.Transport = &sessionHeaderRoundTripper{base: streamHTTP.Transport, st: st}
		catalogHTTP.Transport = &sessionHeaderRoundTripper{base: catalogHTTP.Transport, st: st}
	}
	streamOpts := []option.RequestOption{
		option.WithHTTPClient(streamHTTP),
		option.WithHeader("User-Agent", buildinfo.UserAgent()),
	}
	catalogOpts := []option.RequestOption{
		option.WithHTTPClient(catalogHTTP),
		option.WithHeader("User-Agent", buildinfo.UserAgent()),
	}
	if apiKey != "" {
		streamOpts = append(streamOpts, option.WithAPIKey(apiKey))
		catalogOpts = append(catalogOpts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		streamOpts = append(streamOpts, option.WithBaseURL(baseURL))
		catalogOpts = append(catalogOpts, option.WithBaseURL(baseURL))
	}
	s := openai.NewClient(streamOpts...)
	c := openai.NewClient(catalogOpts...)
	return &s, &c
}

// buildProfileClients constructs the stream/catalog client pair (plus the
// zen/go twins for OpenCode URLs) for one registered provider profile.
// OpenCode profiles route through the zen/go twins, so their plain catalog
// client stays nil. OpenCode clients are tagged with the per-session
// X-Opencode-Session header (st); third-party endpoints never see it. A nil
// st (direct-constructed providers) leaves every client untagged.
func buildProfileClients(prof providerProfile, st *sessionHeaderState) *providerProfile {
	if isOpencodeURL(prof.baseURL) {
		prof.stream, prof.catalog = newClientPair(prof.baseURL, prof.apiKey, st)
		prof.catalog = nil
		prof.zenStream, prof.zenCatalog = newClientPair(openCodeZenBaseURL, prof.apiKey, st)
		prof.goStream, prof.goCatalog = newClientPair(openCodeGoBaseURL, prof.apiKey, st)
		return &prof
	}
	prof.stream, prof.catalog = newClientPair(prof.baseURL, prof.apiKey, nil)
	return &prof
}

// newOpenAIProvider assembles the provider from its profile client sets.
// profiles[0] is the default profile; the fallback accessors read it
// directly, so there is no secondary client state to keep in sync. st is the
// shared session-header state the profile clients were built with (may be
// nil for direct-constructed providers).
func newOpenAIProvider(profiles []*providerProfile, model string, resolver *modelinfo.Resolver, st *sessionHeaderState) *OpenAIProvider {
	p := &OpenAIProvider{
		model:          model,
		modelClient:    make(map[ModelRef]*openai.Client),
		modelInfo:      resolver,
		profiles:       profiles,
		effortsDerived: make(map[ModelRef]derivedEfforts),
		sessionHdr:     st,
	}
	p.modelInfo.Warm() // non-blocking; populate cache before first limit lookup
	return p
}

// SetModelInfoCacheDir points the models.dev disk cache at
// <dir>/.gogen/models.json. Used when the agent switches projects.
func (p *OpenAIProvider) SetModelInfoCacheDir(dir string) {
	if p == nil || p.modelInfo == nil {
		return
	}
	p.modelInfo.SetCachePath(modelinfo.CachePath(dir))
	p.modelInfo.Warm()
}

// SetPromptCacheKey sets a stable key for provider-side prompt caching
// (maps to the OpenAI prompt_cache_key parameter). An empty key disables.
// Use a value derived from the working directory to keep cache hits
// scoped per-project while avoiding cross-user leakage.
func (p *OpenAIProvider) SetPromptCacheKey(key string) {
	if key == "" {
		p.promptCacheKey = param.Opt[string]{}
		return
	}
	p.promptCacheKey = param.NewOpt(key)
}

// SetSessionID sets the session whose ID tags this provider's OpenCode
// requests: the X-Opencode-Session header carries a UUIDv5 derived from id
// (see SessionUUID), so the value is stable across restarts of a resumed
// session. An empty id clears the header. The value is resolved per request,
// so the call is safe from any goroutine at any time — including after
// SetProfiles rebuilds the clients (they share the same state).
func (p *OpenAIProvider) SetSessionID(id string) {
	if p == nil || p.sessionHdr == nil {
		return
	}
	p.sessionHdr.SetSessionID(id)
}

// ProjectPromptCacheKey returns a stable, short hash of the working directory
// for use as an OpenAI prompt_cache_key. This keeps provider-side cache hits
// scoped per-project without leaking paths.
func ProjectPromptCacheKey(workingDir string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(workingDir))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], h.Sum64())
	return hex.EncodeToString(b[:])
}

// SetEndpointCatalog installs the process-shared catalog cache (see
// EndpointCatalog) so every provider in a workspace shares one /v1/models
// download per endpoint instead of one per session. Construction-time setter,
// like SetPromptCacheKey: call it before the provider serves requests. A nil
// argument detaches the provider (it fetches directly, as before).
func (p *OpenAIProvider) SetEndpointCatalog(c *EndpointCatalog) {
	if p == nil {
		return
	}
	p.modelsMu.Lock()
	p.endpointCatalog = c
	p.modelsMu.Unlock()
}

// SetPropsCapsCache installs the process-shared /props capability cache (see
// PropsCapsCache) so every provider in a workspace dials an endpoint's
// capability once instead of once per session. Construction-time setter, like
// SetPromptCacheKey. A nil argument detaches the provider (its own per-provider
// memo applies, as before).
func (p *OpenAIProvider) SetPropsCapsCache(c *PropsCapsCache) {
	if p == nil {
		return
	}
	p.modelsMu.Lock()
	p.propsCaps = c
	p.modelsMu.Unlock()
}

// PropsCapsCache returns the shared capability cache installed by
// SetPropsCapsCache (nil for standalone providers). Hosts that build a child
// provider themselves (the TUI subagent spawner) share the parent's cache with
// it.
func (p *OpenAIProvider) PropsCapsCache() *PropsCapsCache {
	if p == nil {
		return nil
	}
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return p.propsCaps
}

// EndpointCatalog returns the shared catalog cache installed by
// SetEndpointCatalog (nil for standalone providers). Hosts that build a
// child provider themselves (the TUI subagent spawner) share the parent's
// cache with it, so a child does not re-download the endpoint's catalog.
func (p *OpenAIProvider) EndpointCatalog() *EndpointCatalog {
	if p == nil {
		return nil
	}
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return p.endpointCatalog
}

// SetModel updates the provider model ID and invalidates template capability
// caches that may differ across models. The caller supplies only the bare
// ID, so the owning profile resets to the default profile (the model routes
// to the default endpoint — model IDs are not unique across providers, so a
// bare ID cannot name a non-default owner). Callers that know the owner use
// SetModelRef.
func (p *OpenAIProvider) SetModel(id string) error {
	p.modelsMu.Lock()
	p.model = id
	p.currentModelProfile = ""
	p.modelsMu.Unlock()
	// Template caps can change with the model on multi-model / router hosts.
	p.invalidatePropsCaps()
	return nil
}

// SetModelRef selects the model AND its owning profile in one step: the
// routing key (model + modelProfile) and the wire ID are set together so
// they can never drift apart. Like SetModel it invalidates the template
// capability caches.
func (p *OpenAIProvider) SetModelRef(ref ModelRef) error {
	p.modelsMu.Lock()
	p.model = ref.ID
	p.currentModelProfile = ref.ProfileName()
	p.modelsMu.Unlock()
	p.invalidatePropsCaps()
	return nil
}

// CurrentModelRef returns the current model's routing key: the owning
// profile plus the bare wire ID (a zero ID is the "no model selected"
// state). Thread-safe.
func (p *OpenAIProvider) CurrentModelRef() ModelRef {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return ModelRef{Profile: p.currentModelProfile, ID: p.model}
}

// SetProfiles replaces the registered provider set at runtime (the web
// settings modal's provider_save/provider_delete). The profile client sets
// are rebuilt and the catalog/routing caches are invalidated so the next
// ListModels re-fetches every endpoint. The current model is kept: it stays
// routable when its profile is still registered, otherwise the model's
// routing key resets to the default profile (the bare ID is kept),
// clientForModel falls back to the default client, and requireModelSelected
// re-validates it on the next turn.
//
// The swap replaces the whole endpoint set: every fallback (default base
// URL, API key, stream client) resolves through the NEW profiles, so no
// construction-time client can outlive the swap. Per-model lookups resolve
// through modelProfile, which the next fetch repopulates.
func (p *OpenAIProvider) SetProfiles(profiles []ProviderProfile) error {
	if p == nil {
		return nil
	}
	built := make([]*providerProfile, 0, len(profiles))
	for _, pr := range profiles {
		name := strings.TrimSpace(pr.Name)
		if name == "" {
			name = "default"
		}
		built = append(built, buildProfileClients(providerProfile{
			name:    name,
			baseURL: strings.TrimSpace(pr.BaseURL),
			apiKey:  pr.APIKey,
			model:   pr.Model,
		}, p.sessionHdr))
	}
	p.invalidatePropsCaps()
	p.invalidateReasoningEfforts()
	p.modelsMu.Lock()
	p.profiles = built
	p.profilesGen++
	p.modelsCache = nil
	p.modelsCachedAt = time.Time{}
	p.modelProfile = nil
	p.modelClient = make(map[ModelRef]*openai.Client)
	p.modelsFetchFailedAt = time.Time{}
	// The current model's routing key must name a profile that still
	// exists: a model whose provider was removed/renamed cannot keep
	// routing to it. Reset to the default profile (bare ID kept) so
	// re-validation surfaces the gap instead of silently re-homing the
	// model to a different provider.
	if p.model != "" && p.currentModelProfile != "" && !hasProfileName(built, p.currentModelProfile) {
		p.currentModelProfile = ""
	}
	p.modelsMu.Unlock()
	return nil
}

// hasProfileName reports whether the built profile set contains a profile
// registered under name.
func hasProfileName(profiles []*providerProfile, name string) bool {
	for _, prof := range profiles {
		if prof.name == name {
			return true
		}
	}
	return false
}

// defaultBaseURL returns the base URL of the default profile (profiles[0]).
// Live-correct after SetProfiles: the /props probe, models.dev resolution,
// and the preserve_reasoning kwargs gate must resolve against the CURRENT
// default endpoint, never the construction-time one. Empty for
// direct-constructed providers with no profiles (test shape) — that is the
// "default OpenAI API" sentinel the callers check for.
func (p *OpenAIProvider) defaultBaseURL() string {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	if len(p.profiles) == 0 {
		return ""
	}
	return p.profiles[0].baseURL
}

// apiKeyForBaseURL returns the API key of the registered profile whose base
// URL matches baseURL, falling back to the default profile's key when the
// endpoint is unknown (removed/renamed provider, or a direct-constructed
// test provider with no profiles) or when baseURL is empty.
//
// The llama.cpp capability probes (/props, /apply-template) must authenticate
// with the key of the endpoint they actually dial: with multiple registered
// providers the probe target is the CURRENT model's owning endpoint, not the
// default one. Live-correct after SetProfiles, like defaultBaseURL.
func (p *OpenAIProvider) apiKeyForBaseURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	if baseURL != "" {
		for _, prof := range p.profiles {
			if prof.baseURL == baseURL {
				return prof.apiKey
			}
		}
	}
	if len(p.profiles) == 0 {
		return ""
	}
	return p.profiles[0].apiKey
}

// fallbackClient returns the client that serves models with no known
// routing: the default profile's stream client. Nil for direct-constructed
// providers with no profiles (test shape); such providers never serve
// requests.
func (p *OpenAIProvider) fallbackClient() *openai.Client {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	if len(p.profiles) == 0 {
		return nil
	}
	return p.profiles[0].stream
}

// hasMultipleProfiles reports whether more than one registered provider
// endpoint is configured. Sole-model auto-select only applies to the single
// default endpoint: with additional profiles the aggregate catalog is a
// merge of several endpoints and "sole model" is not meaningful.
func (p *OpenAIProvider) hasMultipleProfiles() bool {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return len(p.profiles) > 1
}

// hasOpenCodeProfile reports whether any registered profile is an OpenCode
// endpoint (has zen/go twin clients). Live-correct after SetProfiles: a
// provider switched to or from OpenCode must drive catalog discovery and
// endpoint inference from the CURRENT endpoint set, never stale
// construction-time clients.
func (p *OpenAIProvider) hasOpenCodeProfile() bool {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	for _, prof := range p.profiles {
		if prof.zenStream != nil && prof.goStream != nil {
			return true
		}
	}
	return false
}

// SetThinkingLevel sets the reasoning_effort level. Empty string means omit
// the parameter (the model will not use reasoning/thinking).
func (p *OpenAIProvider) SetThinkingLevel(level string) {
	p.modelsMu.Lock()
	p.thinkingLevel = level
	p.modelsMu.Unlock()
}

// ModelPricing returns per-token pricing (USD per 1M tokens) for the given
// model from the models.dev registry, resolved against the model's OWNING
// profile's registry entry. Only cached map lookup — never blocks.
func (p *OpenAIProvider) ModelPricing(ref ModelRef) (input, output, cached float64, ok bool) {
	_, cost, _, _ := p.lookupModelsDevInfo(ref)
	if cost != nil {
		return cost.Input, cost.Output, cost.CacheRead, true
	}
	return 0, 0, 0, false
}

// ModelReasoningEfforts returns the effective reasoning-effort options for the
// given model: its models.dev accepted set when the model is known (empty for
// toggle/budget-only models, meaning no effort control); else the set derived
// from a llama.cpp capability probe when one has completed (empty when the
// endpoint reported no effort support); else DefaultReasoningEfforts. Never
// blocks — the models.dev registry and the derived cache are in-memory map
// reads; the probe itself runs off the hot path via ProbeReasoningEfforts.
// This is the same set applyThinkingLevel uses to gate the wire value.
func (p *OpenAIProvider) ModelReasoningEfforts(ref ModelRef) []string {
	efforts, _ := p.reasoningEffortResolution(ref)
	return efforts
}

// ReasoningEffortUnsupported reports whether the model definitively has NO
// reasoning-effort control: a known models.dev entry with an empty accepted
// set (toggle/budget-only), or a llama.cpp capability probe that reported no
// support — with no source reporting support. Unknown or not-yet-probed
// models are NOT unsupported (callers keep the DefaultReasoningEfforts
// fallback). Never blocks. The client hides the thinking chips in this case.
func (p *OpenAIProvider) ReasoningEffortUnsupported(ref ModelRef) bool {
	_, unsupported := p.reasoningEffortResolution(ref)
	return unsupported
}

// reasoningEffortResolution resolves the accepted reasoning-effort set and
// the definitive no-support flag for one model. Precedence:
//
//  1. The models.dev accepted set when the model is known and non-empty.
//  2. The llama.cpp capability-probe set when probed and non-empty (a runtime
//     template that references reasoning_effort wins over a registry entry
//     that declared none).
//  3. "unsupported" when a source positively reported no effort control
//     (registry known with an empty set, or a probe reporting no support)
//     and no source reported support.
//  4. DefaultReasoningEfforts otherwise (unknown / not yet probed).
//
// Never blocks: registry and derived-cache reads are in-memory map lookups.
func (p *OpenAIProvider) reasoningEffortResolution(ref ModelRef) (efforts []string, unsupported bool) {
	if p == nil || !ref.Valid() {
		return DefaultReasoningEfforts, false
	}
	// A bare-ID ref (Profile "") must resolve to the same owner the probe
	// cached under: ProbeReasoningEfforts keys effortsDerived by the
	// catalog-resolved owner profile, so the lookup must resolve it too.
	if ref.Profile == "" {
		ref = p.refForModelID(ref.ID)
	}
	ref = ref.Normalized()
	registryEfforts, _, registryKnown := p.firstModelsDevEntry(ref)
	if registryKnown && len(registryEfforts) > 0 {
		return append([]string(nil), registryEfforts...), false
	}
	p.effortsMu.Lock()
	d, ok := p.effortsDerived[ref]
	p.effortsMu.Unlock()
	if ok && d.probed && len(d.efforts) > 0 {
		return append([]string(nil), d.efforts...), false
	}
	if (registryKnown && len(registryEfforts) == 0) || (ok && d.probed && len(d.efforts) == 0) {
		return nil, true
	}
	return DefaultReasoningEfforts, false
}

// ModelDescription returns the models.dev description for the given model
// (resolved against its owning profile's registry entry), or "" when the
// model is unknown. Only cached map lookup — never blocks.
func (p *OpenAIProvider) ModelDescription(ref ModelRef) string {
	_, desc, _ := p.firstModelsDevEntry(ref)
	return desc
}

// firstModelsDevEntry returns the first models.dev entry that resolves for
// ref (trying each URL modelsDevURLsFor yields, in order): its accepted
// reasoning efforts and its description. ok reports that an entry was found
// at all — an entry may legitimately carry an EMPTY effort set
// (toggle/budget-only models), which is not the same as "unknown".
//
// It is the un-predicated sibling of lookupModelsDevInfo, which additionally
// requires a usable context limit or cost before accepting an entry. Callers
// that only need what the registry says about a model (its description, its
// accepted efforts) use this one, so the two acceptance rules stay distinct
// instead of being re-implemented per caller.
func (p *OpenAIProvider) firstModelsDevEntry(ref ModelRef) (efforts []string, desc string, ok bool) {
	if p == nil || p.modelInfo == nil || !ref.Valid() {
		return nil, "", false
	}
	for _, u := range p.modelsDevURLsFor(ref) {
		if _, _, e, d, err := p.modelInfo.Resolve(u, ref.ID); err == nil {
			return e, d, true
		}
	}
	return nil, "", false
}
