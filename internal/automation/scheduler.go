package automation

import (
	"context"
	"log"
	"sync"
	"time"
)

// Sweep timing and firing policy.
const (
	// DefaultSweepInterval matches the reference design: ±30 s granularity
	// is plenty for hourly/daily/weekly schedules.
	DefaultSweepInterval = 30 * time.Second
	// DefaultCatchupWindow bounds the missed-run policy: a due automation
	// older than this was missed while no host was running — it is recorded
	// as missed and skipped instead of firing stale prompts.
	DefaultCatchupWindow = time.Hour
	// maxConcurrentFires caps parallel headless runs so one sweep that
	// finds many due rows (after a long sleep) does not stampede the
	// machine with subprocesses.
	maxConcurrentFires = 4
)

// Scheduler runs the sweep loop: claim due automations from the store,
// fire each as a headless run, record the outcome. It is started by the
// hosts (TUI / web server) while they run; automations fire only while a
// host with `automations: on` is up.
//
// Catch-up policy: a due row is fired when it is overdue by less than the
// catch-up window (DefaultCatchupWindow); anything older is recorded as a
// missed run and the schedule advances without firing. Missed-run rows
// pile up no further work: the advance happens inside the claim, so a
// missed run can never fire later, and a claimed row can never be claimed
// twice (the next_run_at move is the dedup guarantee).
type Scheduler struct {
	store    *Store
	fire     FireFunc
	interval time.Duration
	catchup  time.Duration
	logf     func(format string, args ...any)

	// fires caps concurrent spawn slots (maxConcurrentFires). Slots are
	// taken inside the fire goroutine, so a saturated pool never blocks the
	// sweep loop.
	fires chan struct{}

	mu      sync.Mutex
	running bool
	stopCh  func()
	done    chan struct{}
}

// SchedulerOption customizes a Scheduler (tests).
type SchedulerOption func(*Scheduler)

// WithInterval overrides the sweep interval (tests use short intervals).
func WithInterval(d time.Duration) SchedulerOption {
	return func(s *Scheduler) { s.interval = d }
}

// WithCatchup overrides the catch-up window (tests).
func WithCatchup(d time.Duration) SchedulerOption {
	return func(s *Scheduler) { s.catchup = d }
}

// WithLogger redirects scheduler logging (hosts pass the TUI/web logger;
// the default is the standard logger).
func WithLogger(logf func(format string, args ...any)) SchedulerOption {
	return func(s *Scheduler) { s.logf = logf }
}

// NewScheduler builds a scheduler over the store. fire must be non-nil
// (NewProcessFire for real hosts; test doubles inject their own).
func NewScheduler(store *Store, fire FireFunc, opts ...SchedulerOption) *Scheduler {
	s := &Scheduler{
		store:    store,
		fire:     fire,
		interval: DefaultSweepInterval,
		catchup:  DefaultCatchupWindow,
		logf:     log.Printf,
		fires:    make(chan struct{}, maxConcurrentFires),
		done:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start runs the sweep loop in a goroutine: an immediate first sweep (which
// also applies the catch-up policy to runs missed while no host was up),
// then one sweep per interval until Stop or ctx cancellation. Idempotent:
// starting an already-running scheduler is a no-op.
//
// A panicking sweep is recovered and logged — one bad sweep must never
// kill the host process.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	done := make(chan struct{})
	stop := make(chan struct{})
	s.done = done
	s.stopCh = func() { close(stop) }
	s.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			s.mu.Lock()
			s.running = false
			s.stopCh = nil
			s.mu.Unlock()
		}()
		// Runs left "running" by a previous host process are closed out
		// before anything new fires.
		if n := s.store.MarkInterruptedRuns(); n > 0 {
			s.logf("automation scheduler: marked %d interrupted run(s) from a previous session", n)
		}
		s.sweep()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				s.sweep()
			}
		}
	}()
}

// Stop stops the loop. In-flight runs (spawned headless children) keep
// going in their own processes and are recorded by their waiter goroutine;
// Stop does not block on them. Safe to call more than once; Start may be
// called again afterwards (the settings toggle start/stops the scheduler).
//
// The stop channel is CONSUMED under the lock before it is closed: the loop
// goroutine only clears it when it exits, so two Stops in that window (a
// settings toggle-off followed by server shutdown, or a host that stops the
// scheduler in both its own teardown and the shared drain) both saw the same
// channel and the second close panicked.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	stopCh := s.stopCh
	s.stopCh = nil
	s.mu.Unlock()
	if stopCh != nil {
		stopCh()
	}
}

// Done returns the channel of the current (or most recent) loop run,
// closed when the sweep loop has exited.
func (s *Scheduler) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// sweep claims due rows and fires them. All errors are logged, never
// returned — the loop must keep ticking. Rows claimed by a sweep are always
// fired, even when the scheduler is stopped mid-sweep (Stop only prevents
// future sweeps): a claim already advanced the schedule, so dropping a
// claim would silently lose the occurrence with no history entry.
func (s *Scheduler) sweep() {
	defer func() {
		if r := recover(); r != nil {
			s.logf("automation scheduler: recovered from sweep panic: %v", r)
		}
	}()
	claims := s.store.ClaimDue(s.store.now(), s.catchup)
	for _, c := range claims {
		s.fireOne(c)
	}
	if len(claims) > 1 {
		s.logf("automation scheduler: fired %d due automation(s)", len(claims))
	}
}

// fireOne dispatches one claim: spawn the headless run, record the run row,
// and finish it when the child exits. The spawn-slot semaphore is acquired
// inside the spawned goroutine, never on the caller (the sweep goroutine):
// a full slot set (maxConcurrentFires long-running fires) parks the fire
// goroutine on the semaphore rather than the sweep, so the ticker keeps
// claiming due rows — advancing their next_run_at — and Stop() stays
// responsive instead of waiting behind a blocked send. Claimed rows always
// fire: the goroutine waits for a slot instead of dropping the occurrence.
func (s *Scheduler) fireOne(c Claim) {
	go func() {
		s.fires <- struct{}{}
		defer func() { <-s.fires }()
		defer func() {
			if r := recover(); r != nil {
				s.logf("automation scheduler: recovered from fire panic (%s): %v", c.Automation.ID, r)
			}
		}()
		auto := c.Automation
		runID, err := s.store.RecordFire(auto.ID, c.PlannedAt)
		if err != nil {
			s.logf("automation %s (%s): record fire: %v", auto.ID, auto.Title, err)
			return
		}
		handle, ferr := s.fire(&auto)
		if ferr != nil {
			// Could not even spawn (missing binary, bad working dir, ...).
			if err := s.store.RecordFinish(runID, RunFailed, ferr.Error(), ""); err != nil {
				s.logf("automation %s (%s): record failure: %v", auto.ID, auto.Title, err)
			}
			s.logf("automation %s (%s): fire failed: %v", auto.ID, auto.Title, ferr)
			return
		}
		s.pollSessionID(runID, handle)
		res := <-handle.Result
		status := RunCompleted
		if res.Err != nil {
			status = RunFailed
		}
		detail := res.Detail
		if detail == "" && res.Err != nil {
			detail = res.Err.Error()
		}
		if err := s.store.RecordFinish(runID, status, detail, res.SessionID); err != nil {
			s.logf("automation %s (%s): record finish: %v", auto.ID, auto.Title, err)
		}
		if res.Err != nil {
			s.logf("automation %s (%s): run failed: %v", auto.ID, auto.Title, res.Err)
			return
		}
		s.logf("automation %s (%s): run completed (session %s)", auto.ID, auto.Title, res.SessionID)
	}()
}

// pollSessionIDGiveUp bounds the session-id polling: the child writes the
// link file at session creation (well within a minute of spawning); after
// this long without a write, FireResult still carries the id at completion.
const pollSessionIDGiveUp = 5 * time.Minute

// pollSessionID links the child's session id into the run row as soon as
// the child reports it (best-effort). It exits on the handle's Done signal
// (child exited) or the give-up bound — never by receiving from
// handle.Result: that channel carries exactly one value and the fire
// goroutine is its sole consumer. (The runtime wakes blocked receivers in
// enqueue order and fireOne registers first, so a stray second receiver
// could only steal the value inside a freak preemption window — but
// single-ownership makes that outcome impossible instead of merely
// improbable, and Done gives the poller a deterministic exit at child
// exit instead of a up-to-giveUp linger of pointless file reads.)
// FireResult also carries the id, so a miss here only delays the link,
// never loses it.
func (s *Scheduler) pollSessionID(runID string, handle *FireHandle) {
	if handle.SessionIDFile == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		giveUp := time.NewTimer(pollSessionIDGiveUp)
		defer giveUp.Stop()
		for {
			select {
			case <-handle.Done:
				return
			case <-giveUp.C:
				return
			case <-ticker.C:
				if id := readSessionIDFile(handle.SessionIDFile); id != "" {
					s.store.RecordLink(runID, id)
					return
				}
			}
		}
	}()
}

// StartHostScheduler wires the default host setup: open the default store,
// build a subprocess fire, and start the scheduler — but only when enabled
// (the automations feature flag). The returned stop func is always
// non-nil and idempotent. A store or binary error is logged, never fatal:
// the host runs normally with automations off.
func StartHostScheduler(ctx context.Context, enabled bool, logf func(format string, args ...any)) func() {
	if !enabled {
		return func() {}
	}
	if logf == nil {
		logf = log.Printf
	}
	store, err := OpenStore("")
	if err != nil {
		logf("automation scheduler disabled: %v", err)
		return func() {}
	}
	fire, err := NewProcessFire("")
	if err != nil {
		logf("automation scheduler disabled: %v", err)
		return func() {}
	}
	sched := NewScheduler(store, fire.Fire, WithLogger(logf))
	sched.Start(ctx)
	logf("automation scheduler started (%s; interval %s)", store.dir, sched.interval)
	var once sync.Once
	return func() {
		once.Do(sched.Stop)
	}
}
