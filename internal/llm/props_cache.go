package llm

import (
	"sync"
	"time"
)

// PropsCapsCache shares llama.cpp /props capability results across every
// provider in the process.
//
// The capability (chat_template_caps.supports_preserve_reasoning) is a property
// of the ENDPOINT — of the endpoint+model pair on router hosts — not of a
// session. Cached per provider it was cached per session, so an endpoint that
// does not answer /props cost every session a re-probe every
// propsProbeRetryTTL window: one fresh TCP connection each (propsHTTPClient
// disables keep-alives) against an endpoint that may already be serializing
// generations.
//
// A nil *PropsCapsCache is not usable; providers without one (tests, direct
// construction) keep their own per-provider memo.
type PropsCapsCache struct {
	mu      sync.Mutex
	entries map[string]propsCapsEntry
	// nextSeq hands out a strictly increasing write-order stamp. Ordering the
	// eviction by a counter rather than time.Now keeps "written longest ago"
	// exact even when the wall clock is coarse (Windows resolves time.Now far
	// more coarsely than Linux/macOS, so a tight burst of writes can share one
	// timestamp and the eviction would then pick a victim at random) and even
	// if the clock steps backwards. Guarded by mu.
	nextSeq uint64
}

type propsCapsEntry struct {
	// checked marks a completed probe (200 with parseable JSON): supported is
	// its answer and is served for the endpoint's lifetime.
	checked   bool
	supported bool
	// negativeAt marks an INCOMPLETE probe (transport error, timeout, non-200,
	// unparseable body). It is not an answer — a cold-start probe must not pin
	// "unsupported" for the endpoint's lifetime — but re-dialing within
	// propsProbeRetryTTL is exactly what this memo prevents.
	negativeAt time.Time
	// seq is the entry's write order (see PropsCapsCache.nextSeq); the smallest
	// seq is evicted when the map reaches its cap. Lookup deliberately does not
	// touch it.
	seq uint64
}

// maxPropsCapsEntries bounds the shared capability map: one entry per
// endpoint+model is tiny, but a process that switches models for months would
// otherwise retain every key (Invalidate drops only the key being switched
// to), so at the cap the entry learned longest ago is dropped — the one model
// churn is least likely to need again.
const maxPropsCapsEntries = 128

// NewPropsCapsCache builds an empty shared capability cache.
func NewPropsCapsCache() *PropsCapsCache {
	return &PropsCapsCache{entries: make(map[string]propsCapsEntry)}
}

// propsCapsKey identifies one capability probe target: the endpoint's base URL
// plus the model ID, because router-style hosts (llama.cpp router mode,
// llama-route) select the template per model and can therefore answer
// differently for each. Not secret, so no hashing is needed.
func propsCapsKey(baseURL, modelID string) string {
	return baseURL + "\x00" + modelID
}

// Lookup returns the cached capability for key. known reports whether the
// caller can answer without dialing: true for a completed probe (supported is
// its answer) and also for a recent failed attempt (supported=false — the
// endpoint did not answer within the retry window). known=false means the
// caller must probe.
func (c *PropsCapsCache) Lookup(key string) (supported, known bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if !ok {
		return false, false
	}
	if e.checked {
		return e.supported, true
	}
	if !e.negativeAt.IsZero() && time.Since(e.negativeAt) < propsProbeRetryTTL {
		return false, true
	}
	return false, false
}

// RecordSuccess stores a completed probe's answer.
func (c *PropsCapsCache) RecordSuccess(key string, supported bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	e := propsCapsEntry{checked: true, supported: supported, seq: c.takeSeqLocked()}
	c.putLocked(key, e)
	c.mu.Unlock()
}

// RecordFailure remembers an incomplete probe for propsProbeRetryTTL, so the
// endpoint is dialed once per window process-wide instead of once per session.
func (c *PropsCapsCache) RecordFailure(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	e := c.entries[key]
	if !e.checked {
		e.negativeAt = time.Now()
		e.seq = c.takeSeqLocked()
		c.putLocked(key, e)
	}
	c.mu.Unlock()
}

// takeSeqLocked returns the next write-order stamp. Caller holds c.mu.
func (c *PropsCapsCache) takeSeqLocked() uint64 {
	c.nextSeq++
	return c.nextSeq
}

// putLocked stores e under key, evicting the entry written longest ago (the
// smallest seq) when the map is at its cap. Caller holds c.mu.
func (c *PropsCapsCache) putLocked(key string, e propsCapsEntry) {
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxPropsCapsEntries {
		oldestKey := ""
		var oldestSeq uint64
		for k, cand := range c.entries {
			if oldestKey == "" || cand.seq < oldestSeq {
				oldestKey, oldestSeq = k, cand.seq
			}
		}
		if oldestKey != "" {
			delete(c.entries, oldestKey)
		}
	}
	c.entries[key] = e
}

// Invalidate drops key: a model switch or an endpoint-set change can change
// the template a router host reports, so the next round re-probes.
func (c *PropsCapsCache) Invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}
