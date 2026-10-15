// Package modelinfo resolves a model's context window size from the
// models.dev registry, given only a provider base URL — no provider-specific
// code required. Designed to slot into gogen's internal/llm package.
//
// The full registry JSON is downloaded at most once (then refreshed on a TTL
// in the background). Lookups are pure in-memory / disk-cache matches and
// never block on the network.
package modelinfo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gogen/internal/ioutil"
)

const (
	registryURL = "https://models.dev/api.json"
	cacheTTL    = 5 * time.Minute
	// fetchTimeout bounds background network I/O only — Resolve never waits on it.
	fetchTimeout = 3 * time.Second
	// failBackoff avoids hammering models.dev (or re-entering refresh) after a miss.
	failBackoff = 30 * time.Second
)

// Limit holds token limits for a single model.
type Limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// Cost holds per-token pricing for a model (USD per 1M tokens).
type Cost struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

// reasoningOption is one entry in a model's reasoning_options array.
// Models.dev uses three types: "toggle" (on/off, no graded effort),
// "effort" (accepted reasoning_effort string values), and "budget_tokens"
// (reasoning-token budget bounds, not string effort).
type reasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"` // JSON null entries decode to ""; see ReasoningEfforts
	Min    int64    `json:"min,omitempty"`
	Max    int64    `json:"max,omitempty"`
}

type modelEntry struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Limit            Limit             `json:"limit"`
	Cost             Cost              `json:"cost"`
	ReasoningOptions []reasoningOption `json:"reasoning_options"`
}

// ReasoningEfforts returns the accepted reasoning_effort string values
// declared by effort-type reasoning options, in registry order. Null entries
// in the registry's values array decode to "" and are skipped. Models with
// only toggle/budget control (or no options) yield an empty slice, meaning
// "no reasoning_effort control".
func (m modelEntry) ReasoningEfforts() []string {
	var out []string
	for _, opt := range m.ReasoningOptions {
		if opt.Type != "effort" {
			continue
		}
		for _, v := range opt.Values {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

type providerEntry struct {
	ID     string                `json:"id"`
	Name   string                `json:"name"`
	API    string                `json:"api"` // base URL, e.g. "https://opencode.ai/inference/v1"
	NPM    string                `json:"npm"`
	Models map[string]modelEntry `json:"models"`
}

// registry is the full models.dev response: map of providerID -> providerEntry.
type registry map[string]providerEntry

// Resolver fetches the models.dev registry once and answers lookups from memory.
type Resolver struct {
	mu         sync.RWMutex
	data       registry
	byAPI      map[string]providerEntry // normalizeURL(api) → provider
	fetchedAt  time.Time
	cachePath  string // optional on-disk cache, e.g. <project>/.gogen/models.json
	client     *http.Client
	refreshing bool
	lastFail   time.Time
	url        string // empty means registryURL; overridden in tests
}

// CachePath returns <workingDir>/.gogen/models.json for a project-local
// models.dev registry cache.
func CachePath(workingDir string) string {
	if workingDir == "" {
		workingDir = "."
	}
	return filepath.Join(workingDir, ".gogen", "models.json")
}

// NewResolver creates a Resolver. cachePath may be empty to disable disk caching.
func NewResolver(cachePath string) *Resolver {
	return &Resolver{
		cachePath: cachePath,
		client:    &http.Client{Timeout: fetchTimeout},
	}
}

// SetCachePath updates the on-disk cache location. Existing in-memory data is
// cleared so the next lookup loads from the new path (or a background fetch).
func (r *Resolver) SetCachePath(path string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cachePath == path {
		return
	}
	r.cachePath = path
	r.data = nil
	r.byAPI = nil
	r.fetchedAt = time.Time{}
	r.lastFail = time.Time{}
}

// Warm starts a background registry fetch. Safe to call multiple times;
// concurrent calls coalesce. Call at startup so lookups hit memory soon.
func (r *Resolver) Warm() {
	if r == nil {
		return
	}
	r.refreshAsync()
}

// setDataLocked installs a registry and rebuilds the API-URL index.
// Caller must hold r.mu for writing.
func (r *Resolver) setDataLocked(data registry) {
	r.data = data
	r.fetchedAt = time.Now()
	r.lastFail = time.Time{}
	r.byAPI = make(map[string]providerEntry, len(data))
	for id, p := range data {
		if p.ID == "" {
			p.ID = id
		}
		key := normalizeURL(p.API)
		if key == "" {
			continue
		}
		r.byAPI[key] = p
	}
}

// load ensures r.data is populated from memory or disk. It never performs
// network I/O; a missing cache kicks off a background refresh and returns an
// error so the caller can fall back immediately.
func (r *Resolver) load() error {
	r.mu.RLock()
	if r.data != nil {
		stale := time.Since(r.fetchedAt) >= cacheTTL
		r.mu.RUnlock()
		if stale {
			r.refreshAsync()
		}
		return nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	if r.data != nil {
		stale := time.Since(r.fetchedAt) >= cacheTTL
		r.mu.Unlock()
		if stale {
			r.refreshAsync()
		}
		return nil
	}

	if diskData, diskErr := r.readDiskCache(); diskErr == nil {
		r.setDataLocked(diskData)
		r.mu.Unlock()
		r.refreshAsync()
		return nil
	}
	r.mu.Unlock()

	r.refreshAsync()
	return fmt.Errorf("model registry not loaded yet")
}

// refreshAsync downloads the registry at most once at a time. Failures leave
// existing data untouched and apply failBackoff before the next attempt.
func (r *Resolver) refreshAsync() {
	r.mu.Lock()
	if r.refreshing {
		r.mu.Unlock()
		return
	}
	if r.data != nil && time.Since(r.fetchedAt) < cacheTTL {
		r.mu.Unlock()
		return
	}
	// Respect the fail-backoff window after a failed fetch so a dead endpoint
	// is not re-probed on every lookup (data present but stale included).
	if !r.lastFail.IsZero() && time.Since(r.lastFail) < failBackoff {
		r.mu.Unlock()
		return
	}
	r.lastFail = time.Time{}
	r.refreshing = true
	r.mu.Unlock()

	go func() {
		data, err := r.fetchRemote()

		r.mu.Lock()
		if err != nil {
			r.refreshing = false
			r.lastFail = time.Now()
			r.mu.Unlock()
			return
		}
		r.setDataLocked(data)
		cachePath := r.cachePath
		r.mu.Unlock()
		// Persist outside the lock and clear the flag only AFTER the write:
		// refreshing is the completion signal callers (and tests) poll, so it
		// must not report the refresh done while its disk write is still in
		// flight.
		r.writeDiskCache(cachePath, data)
		r.mu.Lock()
		r.refreshing = false
		r.mu.Unlock()
	}()
}

func (r *Resolver) endpoint() string {
	if r.url != "" {
		return r.url
	}
	return registryURL
}

func (r *Resolver) fetchRemote() (registry, error) {
	req, err := http.NewRequest(http.MethodGet, r.endpoint(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev returned status %d", resp.StatusCode)
	}
	var reg registry
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return nil, err
	}
	return reg, nil
}

func (r *Resolver) readDiskCache() (registry, error) {
	if r.cachePath == "" {
		return nil, fmt.Errorf("no cache path configured")
	}
	b, err := os.ReadFile(r.cachePath)
	if err != nil {
		return nil, err
	}
	var reg registry
	if err := json.Unmarshal(b, &reg); err != nil {
		return nil, err
	}
	return reg, nil
}

// writeDiskCache persists data to cachePath. The path is passed explicitly
// rather than read from r.cachePath so callers outside the lock cannot race
// with SetCachePath.
func (r *Resolver) writeDiskCache(cachePath string, data registry) {
	if cachePath == "" {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	_ = ioutil.WriteFileAtomicNoSync(cachePath, b, 0o644)
}

// normalizeURL strips a trailing slash so "https://x/v1" and "https://x/v1/"
// compare equal.
func normalizeURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// providerFor loads the registry and returns the provider entry matching
// baseURL. It never blocks on the network.
func (r *Resolver) providerFor(baseURL string) (providerEntry, error) {
	if err := r.load(); err != nil {
		return providerEntry{}, fmt.Errorf("loading model registry: %w", err)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	provider, ok := r.byAPI[normalizeURL(baseURL)]
	if !ok {
		return providerEntry{}, fmt.Errorf("no provider in models.dev registry matches base URL %q", baseURL)
	}
	return provider, nil
}

// Resolve returns context limit, pricing, accepted reasoning-effort values,
// and the model description in a single registry lookup — callers that need
// several of these must resolve once here instead of probing the registry
// separately (redundant map lookups). The efforts slice is a fresh copy;
// like the returned *Cost, it must not be mutated by callers.
func (r *Resolver) Resolve(baseURL, modelID string) (Limit, *Cost, []string, string, error) {
	provider, err := r.providerFor(baseURL)
	if err != nil {
		return Limit{}, nil, nil, "", err
	}
	if m, ok := provider.Models[modelID]; ok {
		return m.Limit, &m.Cost, m.ReasoningEfforts(), m.Description, nil
	}
	return Limit{}, nil, nil, "", fmt.Errorf("provider %q found, but no entry for model %q", provider.ID, modelID)
}
