package contextmgr

import (
	"context"
	"sync"
	"testing"

	"gogen/internal/config"
)

// flakyLimitProvider reports a configurable context window, simulating a
// provider whose window is unknown at first (models.dev registry still
// warming, endpoint briefly unreachable) and resolvable later. It counts
// probes so the retry throttle can be asserted.
type flakyLimitProvider struct {
	stubProvider
	mu    sync.Mutex
	limit int
	calls int
}

func (p *flakyLimitProvider) ModelContextLimit(context.Context) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.limit, nil
}

func (p *flakyLimitProvider) setLimit(n int) {
	p.mu.Lock()
	p.limit = n
	p.mu.Unlock()
}

func (p *flakyLimitProvider) probeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// noLimitRetryThrottle disables (and restores) the retry throttle around a
// test that asserts the retried probe.
func noLimitRetryThrottle(t *testing.T) {
	t.Helper()
	prev := limitRetryInterval
	limitRetryInterval = 0
	t.Cleanup(func() { limitRetryInterval = prev })
}

// TestUnresolvedContextLimitIsRetried pins the retry contract: a session
// restored with a pre-warmed limit whose re-probe cannot resolve the window
// (provider reports 0) keeps the value on display but leaves it marked
// unresolved, so a later probe picks up the real window instead of pinning
// the display fallback for the session's lifetime.
func TestUnresolvedContextLimitIsRetried(t *testing.T) {
	noLimitRetryThrottle(t)

	p := &flakyLimitProvider{}
	m := NewManager(p, Settings{})
	m.SetContextLimit(64000) // restore pre-warm, marked resolved

	// The new model's window cannot be resolved yet.
	m.RefreshAfterModelChange(context.Background())
	if got := m.ContextLimit(); got != 64000 {
		t.Fatalf("limit after failed refresh = %d, want the pre-warmed 64000 kept", got)
	}
	if m.Snapshot(nil, nil).LimitResolved {
		t.Fatal("unresolved window must not be reported as resolved")
	}

	// The registry/endpoint answers later: the next turn's probe resolves it.
	p.setLimit(200000)
	m.EnsureContextLimit(context.Background())
	if got := m.ContextLimit(); got != 200000 {
		t.Fatalf("limit after retry = %d, want 200000", got)
	}
	if !m.Snapshot(nil, nil).LimitResolved {
		t.Fatal("resolved window must be marked resolved")
	}
}

// TestUnresolvedContextLimitProbeIsThrottled pins the retry throttle: after
// a probe fails to resolve the window, the per-round EnsureContextLimit path
// must not pay another provider probe immediately (an unresolvable model
// would otherwise stall every round for the probe's budget), while an
// explicit model change still forces a fresh attempt.
func TestUnresolvedContextLimitProbeIsThrottled(t *testing.T) {
	p := &flakyLimitProvider{}
	m := NewManager(p, Settings{})

	m.EnsureContextLimit(context.Background())
	if got := p.probeCount(); got != 1 {
		t.Fatalf("probes after first resolution = %d, want 1", got)
	}
	m.EnsureContextLimit(context.Background())
	if got := p.probeCount(); got != 1 {
		t.Fatalf("probes after throttled retry = %d, want 1 (still throttled)", got)
	}

	// A model change is an explicit refresh: it probes regardless of the
	// throttle.
	m.RefreshAfterModelChange(context.Background())
	if got := p.probeCount(); got != 2 {
		t.Fatalf("probes after model change = %d, want 2", got)
	}
}

// TestUnresolvedContextLimitNotStampedResolved pins the no-snapshot case: a
// failed first probe must not store the 128k display fallback as the model's
// window (which used to make the session claim a resolved 128k while /models
// resolved the real size).
func TestUnresolvedContextLimitNotStampedResolved(t *testing.T) {
	m := NewManager(&flakyLimitProvider{}, Settings{})
	m.EnsureContextLimit(context.Background())

	if got := m.ContextLimit(); got != 0 {
		t.Fatalf("ContextLimit = %d, want 0 (unknown; no real window stored)", got)
	}
	snap := m.Snapshot(nil, nil)
	if snap.LimitResolved {
		t.Fatal("unresolved window must not be reported as resolved")
	}
	if snap.Limit != config.DefaultContextLimit {
		t.Fatalf("snapshot limit = %d, want the %d display fallback", snap.Limit, config.DefaultContextLimit)
	}
	if got := m.ResolvedContextLimit(); got != 0 {
		t.Fatalf("ResolvedContextLimit = %d, want 0 (unresolved)", got)
	}
}

// TestResolvedContextLimitReportsResolvedWindow pins the persistence
// accessor: it reports the window only once it is genuinely resolved, so a
// session snapshot never stores a display-guess.
func TestResolvedContextLimitReportsResolvedWindow(t *testing.T) {
	m := NewManager(&flakyLimitProvider{limit: 64000}, Settings{})
	if got := m.ResolvedContextLimit(); got != 0 {
		t.Fatalf("before resolution: ResolvedContextLimit = %d, want 0", got)
	}
	m.EnsureContextLimit(context.Background())
	if got := m.ResolvedContextLimit(); got != 64000 {
		t.Fatalf("after resolution: ResolvedContextLimit = %d, want 64000", got)
	}

	// A restored (pre-warmed) limit is resolved by construction.
	m2 := NewManager(&flakyLimitProvider{}, Settings{})
	m2.SetContextLimit(32000)
	if got := m2.ResolvedContextLimit(); got != 32000 {
		t.Fatalf("restored limit: ResolvedContextLimit = %d, want 32000", got)
	}
}
