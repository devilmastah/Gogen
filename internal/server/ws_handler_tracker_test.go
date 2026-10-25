package server

import (
	"testing"
	"time"
)

// TestWSHandlerTrackerSettles pins the tracker's two jobs: it must report the
// live handler count, and WaitWSHandlers must return only once the handlers
// have wound down (or the budget expired). The flake it exists to prevent —
// a session flush landing during t.TempDir() removal — depends on the wait
// being a real join, not a no-op that returns immediately.
func TestWSHandlerTrackerSettles(t *testing.T) {
	t.Run("idle tracker drains immediately", func(t *testing.T) {
		var tr wsHandlerTracker
		if !tr.wait(time.Second) {
			t.Fatal("idle tracker reported a timeout")
		}
		if got := tr.live(); got != 0 {
			t.Fatalf("live = %d, want 0", got)
		}
	})

	t.Run("wait blocks until the handler leaves", func(t *testing.T) {
		var tr wsHandlerTracker
		tr.enter()
		released := make(chan struct{})
		go func() {
			defer close(released)
			time.Sleep(25 * time.Millisecond)
			tr.leave()
		}()
		if !tr.wait(time.Second) {
			t.Fatal("wait timed out with a handler that was about to leave")
		}
		<-released
		if got := tr.live(); got != 0 {
			t.Fatalf("live = %d, want 0", got)
		}
	})

	t.Run("a stuck handler is bounded by the timeout", func(t *testing.T) {
		var tr wsHandlerTracker
		tr.enter()
		defer tr.leave()
		start := time.Now()
		if tr.wait(20 * time.Millisecond) {
			t.Fatal("wait reported a drain while a handler was still live")
		}
		if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
			t.Fatalf("wait returned after %s, want at least the 20ms budget", elapsed)
		}
	})
}

// TestWSHandlersAreTracked pins the wiring: a live chat socket and a live
// editor socket are both counted, and both are released when the sockets
// close. Without this the join in drainServer/ForceClose would silently wait
// on nothing.
func TestWSHandlersAreTracked(t *testing.T) {
	s, _, _ := newLifecycleServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	if got := s.wsLive.live(); got != 0 {
		t.Fatalf("live handlers before dialing = %d, want 0", got)
	}

	conn := dialWS(t, srv, "/ws")
	if !waitForBounded(time.Second, func() bool { return s.wsLive.live() == 1 }) {
		t.Fatalf("live handlers after dialing = %d, want 1", s.wsLive.live())
	}

	conn.Close()
	if !s.WaitWSHandlers(5 * time.Second) {
		t.Fatalf("handlers did not drain after the socket closed (live = %d)", s.wsLive.live())
	}
	if got := s.wsLive.live(); got != 0 {
		t.Fatalf("live handlers after close = %d, want 0", got)
	}
}

// waitForBounded polls cond until it holds or the budget expires.
func waitForBounded(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}
