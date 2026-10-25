package server

import (
	"sync"
	"time"
)

// wsHandlerDrainWait bounds how long a teardown path waits for the WebSocket
// handler goroutines to unwind. Long enough for a handler to react to the
// closed socket, short enough that a wedged client cannot stall shutdown.
const wsHandlerDrainWait = 2 * time.Second

// wsHandlerTracker counts the live WebSocket handler goroutines (/ws and
// /ws/editor) so shutdown paths and tests can wait for them to drain.
//
// Why this exists: once websocket.Upgrader hijacks the connection, net/http no
// longer tracks the handler goroutine. httptest.Server.Close (and, in
// production, net/http's Shutdown) returns while a read loop is still running
// its last message. Those handlers detach sessions, and a detach can
// orphan-evict a runtime, whose eviction flushes the session store — a write
// that lands after the caller believed the server was torn down. In tests that
// race is the suite's documented "TempDir RemoveAll cleanup: ... directory not
// empty" flake; in production it can outlive the shutdown flush it should have
// been part of.
//
// A mutex-polled counter rather than sync.WaitGroup: a handler may be joining
// (Add) while a waiter is parked at zero, which is the case WaitGroup
// explicitly forbids.
type wsHandlerTracker struct {
	mu sync.Mutex
	n  int
}

// enter records a starting handler. Call it after the upgrade succeeded, so a
// rejected handshake never counts.
func (t *wsHandlerTracker) enter() {
	t.mu.Lock()
	t.n++
	t.mu.Unlock()
}

func (t *wsHandlerTracker) leave() {
	t.mu.Lock()
	t.n--
	t.mu.Unlock()
}

// live reports the handler count. Test-only helper.
func (t *wsHandlerTracker) live() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// wait blocks until no handler is running or the timeout expires, reporting
// whether the tracker drained. Polling (not a cond var) because the waiters
// here are shutdown paths with a bounded budget, and a cond would need a
// timed-wait helper that sync.Cond does not provide.
func (t *wsHandlerTracker) wait(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if t.live() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WaitWSHandlers blocks until no /ws or /ws/editor handler goroutine is
// running, or timeout expires; it reports whether the handlers drained. Call
// it after closing the connections (srv.Close, closeWSConns): a handler only
// exits once its socket read fails.
//
// ShutdownSessions deliberately does not call this — it is also used by paths
// that keep serving — so a caller that is tearing the server down fully should
// close the connections first and then wait here before its final flush.
func (s *Server) WaitWSHandlers(timeout time.Duration) bool {
	return s.wsLive.wait(timeout)
}
