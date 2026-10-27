package server

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultMaxWSConns = 32
	authCookieName    = "gogen_web_token"
	// authCookieMaxAgeSecs is the session cookie lifetime. The auth token is
	// persisted across restarts for non-loopback binds, so the cookie can
	// outlive a single server run (30 days); a re-login via the printed
	// link/QR mints a fresh cookie.
	authCookieMaxAgeSecs = 30 * 24 * 60 * 60
)

type rateLimitState struct {
	mu       sync.Mutex
	conns    int
	maxConns int
}

func newRateLimitState(maxConns int) *rateLimitState {
	if maxConns <= 0 {
		maxConns = defaultMaxWSConns
	}
	return &rateLimitState{maxConns: maxConns}
}

func (r *rateLimitState) acquireConn() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns >= r.maxConns {
		return false
	}
	r.conns++
	return true
}

func (r *rateLimitState) releaseConn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns > 0 {
		r.conns--
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

const (
	// maxIPLimiters caps the per-IP limiter map. Beyond it the limiter would
	// grow without bound: the source address of an upgrade is attacker-chosen.
	maxIPLimiters = 10_000
	// ipLimiterEvictSample bounds the work of one eviction: at most this many
	// entries are inspected to choose a victim.
	ipLimiterEvictSample = 8
)

// ipLimiter tracks per-IP connection attempt rates for HTTP/WS upgrades.
type ipLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	lastUsed map[string]time.Time
	rate     rate.Limit
	burst    int
}

func newIPLimiter(perSec float64, burst int) *ipLimiter {
	return &ipLimiter{
		limiters: make(map[string]*rate.Limiter),
		lastUsed: make(map[string]time.Time),
		rate:     rate.Limit(perSec),
		burst:    burst,
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	lim, ok := l.limiters[ip]
	if !ok {
		// Evict before inserting so the map never grows past the cap.
		if len(l.limiters) >= maxIPLimiters {
			l.evictOldestLocked(ipLimiterEvictSample)
		}
		lim = rate.NewLimiter(l.rate, l.burst)
		l.limiters[ip] = lim
	}
	l.lastUsed[ip] = now
	return lim.Allow()
}

// evictOldestLocked removes one limiter/lastUsed pair to make room, and reports
// how many entries the scan inspected (0 on an empty map).
//
// It inspects at most sample entries — map iteration order is randomized, so
// that is an arbitrary sample — and evicts the oldest of them. Plain random
// eviction (deleting whatever key iteration yields first) would eventually
// drop the limiter that is currently throttling an IP, handing that IP a fresh
// burst; taking the oldest of a sample makes that practically impossible
// without paying for the true LRU. The earlier implementation scanned the
// whole map (up to 10k entries) under l.mu for every new IP over the cap, so a
// client rotating source addresses turned every upgrade into an O(10k) walk
// while holding the lock. Work per call is now bounded by sample; the returned
// count makes that contract observable (and assertable in tests).
func (l *ipLimiter) evictOldestLocked(sample int) int {
	var oldest string
	var oldestTime time.Time
	inspected := 0
	for k, t := range l.lastUsed {
		if inspected >= sample {
			break
		}
		inspected++
		if oldest == "" || t.Before(oldestTime) {
			oldest, oldestTime = k, t
		}
	}
	if oldest != "" {
		delete(l.limiters, oldest)
		delete(l.lastUsed, oldest)
	}
	return inspected
}

func setAuthCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   authCookieMaxAgeSecs,
	})
}
