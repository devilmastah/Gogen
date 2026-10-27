package server

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// fillLimiters populates l directly with n entries whose lastUsed stamps are
// all older than any allow() call made by the test, so "oldest of a sample"
// is unambiguous.
func fillLimiters(l *ipLimiter, n int, stamp time.Time) []string {
	ips := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&0xff, i&0xff)
		l.limiters[ip] = rate.NewLimiter(l.rate, l.burst)
		l.lastUsed[ip] = stamp
		ips = append(ips, ip)
	}
	return ips
}

// TestIPLimiterCapsEntries pins the cap: a client rotating source addresses
// must not grow the limiter map without bound, and the map must never exceed
// maxIPLimiters.
func TestIPLimiterCapsEntries(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(1, 1)
	total := maxIPLimiters + 2_000
	var last string
	for i := 0; i < total; i++ {
		last = fmt.Sprintf("192.168.%d.%d", i>>8, i&0xff)
		l.allow(last)
		if n := len(l.limiters); n > maxIPLimiters {
			t.Fatalf("after %d IPs: %d limiters, want <= %d", i+1, n, maxIPLimiters)
		}
		if n, m := len(l.limiters), len(l.lastUsed); n != m {
			t.Fatalf("limiters (%d) and lastUsed (%d) drifted", n, m)
		}
	}
	if _, ok := l.limiters[last]; !ok {
		t.Fatalf("most recently seen IP %q was evicted", last)
	}
}

// TestIPLimiterKeepsActiveLimiter pins the property that motivates the
// sampled-oldest eviction: an IP whose limiter is actively throttling it must
// keep that limiter. A random victim would eventually reset it and grant a
// fresh burst.
func TestIPLimiterKeepsActiveLimiter(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(1, 1)
	const hot = "203.0.113.7"
	fillLimiters(l, maxIPLimiters-1, time.Now().Add(-time.Hour))

	// Spend the burst, then confirm the bucket is empty (the second call is
	// throttled) — this is the state an eviction must not reset.
	if !l.allow(hot) {
		t.Fatal("first allow for a fresh IP was throttled")
	}
	if l.allow(hot) {
		t.Fatal("burst of 1 allowed two requests")
	}
	hotLim := l.limiters[hot]
	if hotLim == nil {
		t.Fatal("hot limiter missing after allow")
	}

	// Churn new IPs while the hot IP keeps being used (each allow refreshes
	// its lastUsed stamp). Every one of those inserts is over the cap, so each
	// runs an eviction.
	for i := 0; i < 64; i++ {
		// Distinct stamps keep the ordering unambiguous on coarse clocks.
		time.Sleep(time.Millisecond)
		if l.allow(hot) {
			t.Fatalf("iteration %d: hot IP was allowed again — its limiter was evicted and recreated", i)
		}
		l.allow(fmt.Sprintf("198.51.100.%d", i))
	}
	if got := l.limiters[hot]; got != hotLim {
		t.Fatal("hot IP's limiter object was replaced")
	}
	if l.allow(hot) {
		t.Fatal("hot IP's bucket was reset by eviction")
	}
}

// TestIPLimiterEvictIsBounded pins the bounded work of one eviction: however
// large the map is, the scan inspects at most `sample` entries (the old
// implementation walked all 10k under the lock).
func TestIPLimiterEvictIsBounded(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(1, 1)
	fillLimiters(l, maxIPLimiters+2_000, time.Now().Add(-time.Hour))

	before := len(l.limiters)
	inspected := l.evictOldestLocked(ipLimiterEvictSample)
	if inspected != ipLimiterEvictSample {
		t.Fatalf("inspected %d entries, want %d", inspected, ipLimiterEvictSample)
	}
	if got := len(l.limiters); got != before-1 {
		t.Fatalf("limiters = %d after eviction, want %d", got, before-1)
	}
	if got, m := len(l.limiters), len(l.lastUsed); got != m {
		t.Fatalf("limiters (%d) and lastUsed (%d) drifted", got, m)
	}

	// An empty map must not panic and inspects nothing.
	empty := newIPLimiter(1, 1)
	if n := empty.evictOldestLocked(ipLimiterEvictSample); n != 0 {
		t.Fatalf("empty map inspected %d entries", n)
	}
}

// TestIPLimiterEvictPicksOldest pins the victim choice: given a sample that
// covers the map, the least-recently-used entry goes and the newest stays
// (sampling only bounds how many entries the choice considers).
func TestIPLimiterEvictPicksOldest(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(1, 1)
	base := time.Now().Add(-time.Hour)
	ips := fillLimiters(l, 500, base)
	// Spread the stamps so ips[0] is the oldest and ips[len-1] the newest.
	for i, ip := range ips {
		l.lastUsed[ip] = base.Add(time.Duration(i) * time.Second)
	}

	if n := l.evictOldestLocked(len(l.lastUsed)); n != len(ips) {
		t.Fatalf("inspected %d entries, want %d", n, len(ips))
	}
	if _, ok := l.limiters[ips[0]]; ok {
		t.Fatalf("oldest entry %q survived eviction", ips[0])
	}
	if _, ok := l.limiters[ips[len(ips)-1]]; !ok {
		t.Fatalf("newest entry %q was evicted", ips[len(ips)-1])
	}
}
