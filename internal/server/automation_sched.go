package server

import (
	"context"
	"log"
	"sync"
	"time"

	"gogen/internal/automation"
)

// The web host's automation scheduler lifecycle: started with the server
// (when the automations feature flag is on) and started/stopped live by the
// settings toggle (handleWSFeatureFlags → SetAutomationsEnabled). The
// scheduler fires due automations as detached `gogen -p` subprocesses —
// a crashing run can never take the server down — and records every run in
// the global store (see internal/automation).

// automationHost guards lazy scheduler construction + the live toggle.
// Lock order: automationHost.mu → (scheduler internals). No other server
// mutex is taken while holding it — and mu is never held across
// automationStore() (which takes it itself); construction claims the
// `building` slot first so concurrent starts cannot spawn two schedulers.
type automationHost struct {
	mu       sync.Mutex
	building bool
	sched    *automation.Scheduler
	store    *automation.Store
	fire     automation.FireFunc
	storeDir string
	// interval overrides the scheduler sweep interval (tests).
	interval time.Duration
}

// automationStore returns the shared automation store, opening it on first
// use. The scheduler and the automations WS ops SHOULD share this one
// instance: the read paths (List/Get/Runs) serve the store's in-memory copy,
// which every mutation of that same instance refreshes, so a second instance
// would show the tab and the sweep slightly different views (and each would
// wait on the other's file lock). A store mutation from another instance or
// another process — the CLI — is no longer a corruption risk: every mutation
// re-reads the file under the cross-process lock (see automation.Store). An
// open failure is NOT cached: the next call retries (a corrupted store fixed
// on disk heals without a restart; each failure surfaces as an error notice to
// the requesting tab).
func (s *Server) automationStore() (*automation.Store, error) {
	s.automations.mu.Lock()
	defer s.automations.mu.Unlock()
	if s.automations.store != nil {
		return s.automations.store, nil
	}
	dir := s.automations.storeDir
	if dir == "" {
		dir = automation.DefaultDir()
	}
	st, err := automation.OpenStore(dir)
	if err != nil {
		return nil, err
	}
	s.automations.store = st
	return st, nil
}

// setAutomationFire overrides the fire func + store dir for tests (nil fire
// = the real subprocess fire).
func (s *Server) setAutomationFire(f automation.FireFunc, storeDir string) {
	s.automations.mu.Lock()
	defer s.automations.mu.Unlock()
	s.automations.fire = f
	s.automations.storeDir = storeDir
}

// setAutomationInterval overrides the sweep interval for tests (0 = the
// default). Must be called before the scheduler is constructed.
func (s *Server) setAutomationInterval(d time.Duration) {
	s.automations.mu.Lock()
	defer s.automations.mu.Unlock()
	s.automations.interval = d
}

// startAutomations starts the scheduler if the automations feature flag is
// on (idempotent). The flag is checked at entry and RE-checked at
// registration, so a settings toggle-off landing while the store/fire were
// being built cannot leave a scheduler sweeping with the flag off. A store
// error is logged, never fatal: the server runs normally with automations
// off.
func (s *Server) startAutomations() {
	s.automations.mu.Lock()
	if s.ws == nil || !s.ws.GetAutomationsEnabled() {
		s.automations.mu.Unlock()
		return
	}
	if sched := s.automations.sched; sched != nil {
		s.automations.mu.Unlock()
		sched.Start(context.Background())
		return
	}
	if s.automations.building {
		// A concurrent start is constructing the scheduler; it will Start()
		// it before releasing the slot.
		s.automations.mu.Unlock()
		return
	}
	s.automations.building = true
	s.automations.mu.Unlock()
	defer func() {
		s.automations.mu.Lock()
		s.automations.building = false
		s.automations.mu.Unlock()
	}()

	store, err := s.automationStore()
	if err != nil {
		log.Printf("automation scheduler disabled: %v", err)
		return
	}
	fire := s.automations.fire
	if fire == nil {
		pf, ferr := automation.NewProcessFire("")
		if ferr != nil {
			log.Printf("automation scheduler disabled: %v", ferr)
			return
		}
		fire = pf.Fire
	}
	sched := automation.NewScheduler(store, fire, automation.WithLogger(log.Printf))
	if d := s.automations.interval; d > 0 {
		// Test hook: rebuild with the overridden interval.
		sched = automation.NewScheduler(store, fire, automation.WithInterval(d), automation.WithLogger(log.Printf))
	}
	sched.Start(context.Background())
	// Registration re-checks the flag: between the entry check and here a
	// settings toggle-off may have landed (its stopAutomations was a no-op —
	// sched was still nil mid-build). The freshly built scheduler is
	// registered (a later toggle-on restarts it) but immediately stopped,
	// so it never sweeps with the feature flag off.
	started := false
	s.automations.mu.Lock()
	if s.automations.sched == nil {
		s.automations.sched = sched
		if s.ws != nil && s.ws.GetAutomationsEnabled() {
			started = true
		} else {
			sched.Stop() // toggle-off landed during construction
		}
	} else {
		sched.Stop() // a concurrent start won: never run two schedulers
	}
	s.automations.mu.Unlock()
	if started {
		log.Printf("automation scheduler started")
	}
}

// stopAutomations stops the running scheduler (the settings toggle-off and
// server shutdown). The instance is kept for a later toggle-on (Start is
// restartable). Safe to call when nothing is running.
func (s *Server) stopAutomations() {
	s.automations.mu.Lock()
	defer s.automations.mu.Unlock()
	if s.automations.sched != nil {
		s.automations.sched.Stop()
	}
}
