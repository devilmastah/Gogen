package llm

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
)

// EndpointCatalog shares one catalog fetch per endpoint across every provider
// in the process.
//
// Providers are per-session, so each session's provider used to fetch the same
// endpoint's /v1/models independently: N sessions cost N catalog downloads per
// TTL window — the same request repeated N times, against an endpoint that may
// well be serializing generations. The catalog is a property of the ENDPOINT,
// not of a session, so it belongs in one process-scoped cache.
//
// Sharing is safe because a catalog entry is pure endpoint data: the model
// list and its raw JSON. Everything session-specific — the per-entry
// profile tags, the model → client routing map (the OpenCode profiles'
// zen/go twin routing) — is still derived per provider from the shared
// lists.
//
// A nil *EndpointCatalog is not usable; providers built without one (tests,
// direct construction) fetch directly, exactly as before.
type EndpointCatalog struct {
	mu      sync.Mutex
	entries map[string]*endpointCatalogEntry
	// ttl is how long a downloaded catalog is served before another fetch is
	// allowed. Mirrors the provider-level cache TTL so a session that
	// refreshes does not force its siblings to re-download.
	ttl time.Duration
}

// endpointCatalogEntry is one endpoint's cached catalog plus the shared
// single-flight state for a fetch in progress.
type endpointCatalogEntry struct {
	models   []openai.Model
	loadedAt time.Time
	// failedAt/err remember a failed fetch for modelsFetchBackoff so a dead
	// endpoint costs one bounded attempt per window process-wide, instead of
	// one per session. A brief probe that fails does not arm it.
	failedAt time.Time
	err      error
	// inflight is non-nil while a fetch runs; concurrent callers (including
	// callers from other providers) wait on it instead of starting their own.
	inflight *endpointCatalogLoad
}

type endpointCatalogLoad struct {
	done   chan struct{}
	models []openai.Model
	err    error
}

// maxEndpointCatalogEntries bounds the shared entry map. One entry per
// endpoint (base URL + API-key hash) is small, but a long-lived process
// talking to rotating endpoints or credentials would otherwise retain every
// entry for its lifetime — the TTL refreshes data, it never evicts. At the
// cap, the entry loaded longest ago is dropped (the one least likely to still
// describe an endpoint in use); an entry with a fetch in flight is never
// dropped, so its waiters keep their result.
const maxEndpointCatalogEntries = 64

// NewEndpointCatalog builds an empty shared catalog cache.
func NewEndpointCatalog() *EndpointCatalog {
	return &EndpointCatalog{entries: make(map[string]*endpointCatalogEntry), ttl: modelsCacheTTL}
}

// CatalogKey identifies one endpoint for the shared catalog: its base URL plus
// a hash of the API key, so two endpoints (or two keys on one endpoint) never
// share a cached catalog. The key is hashed rather than concatenated so a
// credential cannot leak into logs or map dumps.
func CatalogKey(baseURL, apiKey string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(baseURL))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(apiKey))
	return fmt.Sprintf("%x", h.Sum64())
}

// Models returns the endpoint's catalog, downloading it at most once per TTL
// across the whole process. fetch performs the actual request and is only
// called on a miss.
//
// The fetch runs detached from the initiating caller's context but bounded by
// modelsCatalogTimeout: a caller with a short budget (the best-effort context
// probe) must not abort a download that other sessions are waiting on, and a
// hung endpoint must not leave the entry in flight forever. Every caller —
// including the initiator — observes its OWN context while waiting, so a
// cancelled turn still returns immediately.
//
// armBackoff mirrors the provider-level contract: only a full-budget fetch
// records a failure against the shared backoff, so a brief probe's timeout is
// not evidence that the endpoint is dead.
func (c *EndpointCatalog) Models(ctx context.Context, key string, armBackoff bool, fetch func(context.Context) ([]openai.Model, error)) ([]openai.Model, error) {
	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		if len(c.entries) >= maxEndpointCatalogEntries {
			c.evictOldestLocked(key)
		}
		e = &endpointCatalogEntry{}
		c.entries[key] = e
	}
	if l := e.inflight; l != nil {
		c.mu.Unlock()
		return waitCatalogLoad(ctx, l)
	}
	if len(e.models) > 0 && time.Since(e.loadedAt) < c.ttl {
		out := append([]openai.Model(nil), e.models...)
		c.mu.Unlock()
		return out, nil
	}
	if !e.failedAt.IsZero() && time.Since(e.failedAt) < modelsFetchBackoff {
		err := e.err
		c.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("endpoint catalog unavailable (backoff)")
		}
		return nil, err
	}
	l := &endpointCatalogLoad{done: make(chan struct{})}
	e.inflight = l
	c.mu.Unlock()

	go func() {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelsCatalogTimeout)
		defer cancel()
		models, err := fetch(fctx)

		c.mu.Lock()
		l.models, l.err = models, err
		e.inflight = nil
		switch {
		case err == nil:
			e.models = models
			e.loadedAt = time.Now()
			e.failedAt = time.Time{}
			e.err = nil
		case armBackoff:
			// Remember the failure for the shared backoff window so a dead
			// endpoint costs one attempt process-wide. The previous list is
			// dropped rather than served stale — a confident error beats a
			// catalog that may no longer describe the endpoint — while the
			// in-flight load still carries whatever this attempt collected
			// to its waiters (a partial list can be better than nothing; the
			// caller decides).
			e.models = nil
			e.loadedAt = time.Time{}
			e.failedAt = time.Now()
			e.err = err
		}
		c.mu.Unlock()
		close(l.done)
	}()

	return waitCatalogLoad(ctx, l)
}

// waitCatalogLoad waits for the load to settle, returning whatever the fetch
// produced plus its error. After a failure the models can be a partial list (a
// fetch that crawled part of the catalog before the connection died); the
// caller decides whether the partial list outweighs the error — the OpenCode
// twin merge keeps such a list, exactly as it did before the fetch was shared.
func waitCatalogLoad(ctx context.Context, l *endpointCatalogLoad) ([]openai.Model, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return append([]openai.Model(nil), l.models...), l.err
	}
}

// evictOldestLocked drops the entry loaded (or failed) longest ago, skipping
// keepKey and any entry with a fetch in flight — its waiters hold the load,
// and evicting it would strand them. Caller holds c.mu.
func (c *EndpointCatalog) evictOldestLocked(keepKey string) {
	oldestKey := ""
	var oldest time.Time
	for k, cand := range c.entries {
		if k == keepKey || cand.inflight != nil {
			continue
		}
		at := cand.loadedAt
		if cand.failedAt.After(at) {
			at = cand.failedAt
		}
		if oldestKey == "" || at.Before(oldest) {
			oldestKey, oldest = k, at
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}
