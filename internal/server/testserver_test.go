package server

import (
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
)

// newTestServer builds a Server for a test and registers the teardown that
// drains its async writers before the caller's t.TempDir() removal.
//
// The web server's turns, delivery workers, subagent spawns and automation
// sweeps run on their own goroutines and flush into the session store when
// they finish. A test that returns as soon as it observed the effect leaves
// those writes in flight across t.TempDir()'s RemoveAll — "directory not
// empty" on Unix, plus "Access is denied" on Windows, where the atomic
// write's rename cannot replace a file the removal has just unlinked. Cleanups
// run LIFO, so this one is registered before the test's own and therefore runs
// after them, right before the temp dir goes away.
func newTestServer(t *testing.T, a *agent.Agent, cfg *config.Config) *Server {
	t.Helper()
	s := NewServer(a, cfg)
	t.Cleanup(func() { drainServer(s) })
	return s
}

// drainServer cancels and flushes everything the server can still be running,
// waits (bounded, best-effort) for the runtimes to go quiet, and evicts them.
//
// Shutdown first, poll second: ShutdownSessions cancels the in-flight streams,
// so a turn parked in a blocking test provider unwinds immediately instead of
// holding the turn lock for the whole quiescence window. The poll then covers
// the delivery worker's pop→start handoff (sessionRuntimeQuiescent), the last
// thing that can still write after its queue was drained. A runtime that never
// goes quiet (a deliberately stuck test turn) costs the window once and the
// eviction proceeds regardless.
func drainServer(s *Server) {
	if s == nil || s.registry == nil {
		return
	}
	// Join the hijacked WebSocket handler goroutines FIRST: net/http stops
	// tracking a connection at the upgrade, so a read loop can still be
	// dispatching its last message (detach → orphan eviction → store flush)
	// after the caller's srv.Close(). Close the sockets before joining — a
	// handler stays parked in ReadJSON until its connection fails (measured:
	// 5ms to join once closed, versus the full budget when not) — so the flush
	// sweep below is the last writer and no write lands in the caller's
	// t.TempDir() removal ("directory not empty", the suite's documented flake).
	s.closeWSConns()
	s.WaitWSHandlers(wsHandlerDrainWait)
	// Join the settings handlers' persist+broadcast goroutines: they outlive
	// the handler that launched them, and a config write still in flight when
	// the caller's t.TempDir() is removed either fails its rename or races
	// RemoveAll ("directory not empty"). Safe to Wait here because the handler
	// goroutines were joined above — no handler can launch another one now.
	s.waitPersistsWithin(2 * time.Second)
	s.stopAutomations()
	s.ShutdownSessions()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		quiet := true
		for _, id := range s.registry.activeIDs() {
			rt, ok := s.registry.get(id)
			if !ok {
				continue
			}
			if !sessionRuntimeQuiescent(rt) {
				quiet = false
				break
			}
		}
		if quiet {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, id := range s.registry.activeIDs() {
		if rt, ok := s.registry.get(id); ok {
			// Claim and drain before evicting: the quiescence poll above can
			// be won by a delivery worker that is mid pop→start handoff, and
			// the turn it starts would flush into .gogen/sessions after the
			// caller's t.TempDir() removal began (the flake this whole
			// teardown exists to prevent). The drain is best-effort: a
			// deliberately stuck test turn costs the window once, exactly as
			// it does in the poll.
			rt.evicted.Store(true)
			rt.drainTurnLock(wsStreamDrainWait)
			s.registry.evictRuntime(rt)
		}
	}
}
