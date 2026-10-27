package automation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingFire is a FireFunc double: it records the fired automations and
// delivers a canned result (or a spawn error).
type recordingFire struct {
	mu      sync.Mutex
	fired   []Automation
	result  FireResult
	delay   time.Duration // how long the child "runs" before the result
	spawnEr error
}

func (f *recordingFire) Fire(a *Automation) (*FireHandle, error) {
	f.mu.Lock()
	f.fired = append(f.fired, *a)
	spawnErr, delay, result := f.spawnEr, f.delay, f.result
	f.mu.Unlock()
	if spawnErr != nil {
		return nil, spawnErr
	}
	ch := make(chan FireResult, 1)
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		ch <- result
	}()
	return &FireHandle{Result: ch}, nil
}

func (f *recordingFire) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fired)
}

// schedulerTestLogger returns a scheduler logger that stays safe after the
// test body has returned. The scheduler's fire goroutines outlive the test
// — Stop deliberately does not wait for in-flight fires — and t.Logf
// PANICS once testing has completed ("Log in goroutine after ... has
// completed"), so a straggler's completion log aborts the whole package.
// The gate serializes with t.Cleanup: a log already inside the critical
// section flushes while logging is still allowed, and anything after the
// gate closes is dropped.
func schedulerTestLogger(t *testing.T) func(format string, args ...any) {
	t.Helper()
	var mu sync.Mutex
	done := false
	t.Cleanup(func() {
		mu.Lock()
		done = true
		mu.Unlock()
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return
		}
		t.Logf(format, args...)
	}
}

// stopScheduler stops s and WAITS for the sweep loop to exit. Stop alone only
// signals the loop, so a sweep already past its tick check could still claim a
// row (and touch the store directory) after the test body returned — which is
// harmless on Linux but makes t.TempDir cleanup fail on Windows ("The
// directory is not empty"). Every scheduler test must use this.
func stopScheduler(t *testing.T, s *Scheduler) {
	t.Helper()
	s.Stop()
	if done := s.Done(); done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("scheduler sweep loop did not exit after Stop")
		}
	}
}

// newSchedulerTest builds a store + scheduler with millisecond sweeps and a
// controllable clock.
func newSchedulerTest(t *testing.T, fire FireFunc) (*Scheduler, *Store, *timeCursor) {
	t.Helper()
	st, cursor := newTestStore(t)
	s := NewScheduler(st, fire, WithInterval(10*time.Millisecond), WithLogger(schedulerTestLogger(t)))
	return s, st, cursor
}

// dueAutomation installs an enabled daily automation due at the cursor time.
func dueAutomation(t *testing.T, st *Store, cursor *timeCursor) Automation {
	a := Automation{
		Title: "Nightly triage", Prompt: "triage the board", WorkingDir: "/tmp/proj",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"}, Enabled: true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	setStoredNextRun(t, st, a.ID, cursor.t)
	return a
}

func waitCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", timeout, what)
}

// awaitRunTerminal waits until every run row of the automation reached a
// terminal status, so in-flight writer goroutines are done before the test
// ends (its temp dir must survive their final file writes).
func awaitRunTerminal(t *testing.T, st *Store, id string) {
	t.Helper()
	terminal := map[string]bool{RunCompleted: true, RunFailed: true, RunMissed: true, RunInterrupted: true}
	waitCond(t, 5*time.Second, "run rows to reach a terminal status", func() bool {
		for _, r := range st.Runs(id) {
			if !terminal[r.Status] {
				return false
			}
		}
		return len(st.Runs(id)) > 0
	})
}

func TestSchedulerFiresDueAndRecords(t *testing.T) {
	fire := &recordingFire{result: FireResult{SessionID: "sess-abc"}}
	s, st, cursor := newSchedulerTest(t, fire.Fire)
	a := dueAutomation(t, st, cursor)

	ctx := context.Background()
	s.Start(ctx)
	defer stopScheduler(t, s)

	waitCond(t, 2*time.Second, "automation fired", func() bool { return fire.count() == 1 })
	waitCond(t, 2*time.Second, "run recorded", func() bool {
		runs := st.Runs(a.ID)
		return len(runs) == 1 && runs[0].Status == RunCompleted
	})
	runs := st.Runs(a.ID)
	r := runs[0]
	if r.SessionID != "sess-abc" || r.PlannedAt != cursor.t || r.FiredAt == nil || r.EndedAt == nil {
		t.Fatalf("run record incomplete: %+v", r)
	}
	if r.Detail != "" {
		t.Fatalf("successful run should have no detail, got %q", r.Detail)
	}
	got, _ := st.Get(a.ID)
	if got.LastRunStatus != RunCompleted {
		t.Fatalf("last_run_status = %q, want completed", got.LastRunStatus)
	}
	// The claim advanced the schedule: no second fire.
	waitCond(t, 300*time.Millisecond, "no duplicate fire", func() bool { return fire.count() <= 1 })
	if got.NextRunAt == nil || !got.NextRunAt.After(cursor.t) {
		t.Fatalf("next_run_at not advanced: %v", got.NextRunAt)
	}
}

func TestSchedulerSpawnFailureRecorded(t *testing.T) {
	fire := &recordingFire{spawnEr: fmt.Errorf("fork/exec: no such file or directory")}
	s, st, cursor := newSchedulerTest(t, fire.Fire)
	a := dueAutomation(t, st, cursor)

	s.Start(context.Background())
	defer stopScheduler(t, s)

	waitCond(t, 2*time.Second, "failed run recorded", func() bool {
		runs := st.Runs(a.ID)
		return len(runs) == 1 && runs[0].Status == RunFailed
	})
	runs := st.Runs(a.ID)
	if !strings.Contains(runs[0].Detail, "no such file") {
		t.Fatalf("failure detail lost: %+v", runs[0])
	}
	got, _ := st.Get(a.ID)
	if got.LastRunStatus != RunFailed {
		t.Fatalf("last_run_status = %q, want failed", got.LastRunStatus)
	}
}

func TestSchedulerRunFailureRecorded(t *testing.T) {
	fire := &recordingFire{result: FireResult{Err: exec.ErrNotFound, Detail: "exit status 1: model unavailable"}}
	s, st, cursor := newSchedulerTest(t, fire.Fire)
	a := dueAutomation(t, st, cursor)

	s.Start(context.Background())
	defer stopScheduler(t, s)

	waitCond(t, 2*time.Second, "failed run recorded", func() bool {
		runs := st.Runs(a.ID)
		return len(runs) == 1 && runs[0].Status == RunFailed
	})
	runs := st.Runs(a.ID)
	if runs[0].Detail == "" || runs[0].SessionID != "" {
		t.Fatalf("failed run record incomplete: %+v", runs[0])
	}
}

func TestSchedulerMissedRunsAreNotFired(t *testing.T) {
	// The store-level catch-up policy drives this; here we pin it end to
	// end through the scheduler: a long-closed host must not fire stale
	// prompts on startup.
	fire := &recordingFire{}
	s, st, cursor := newSchedulerTest(t, fire.Fire)
	a := dueAutomation(t, st, cursor)
	setStoredNextRun(t, st, a.ID, cursor.t.Add(-48*time.Hour))

	s.Start(context.Background())
	defer stopScheduler(t, s)

	waitCond(t, 2*time.Second, "missed run recorded", func() bool {
		return len(st.Runs(a.ID)) == 1 && st.Runs(a.ID)[0].Status == RunMissed
	})
	if n := fire.count(); n != 0 {
		t.Fatalf("stale automation fired %d times, want 0", n)
	}
}

// TestSchedulerInterruptedRunsMarked covers the startup sweep closing out
// runs left "running" by a previous host.
func TestSchedulerInterruptedRunsMarked(t *testing.T) {
	fire := &recordingFire{}
	s, st, cursor := newSchedulerTest(t, fire.Fire)
	a := dueAutomation(t, st, cursor)
	runID, err := st.RecordFire(a.ID, cursor.t)
	if err != nil {
		t.Fatal(err)
	}
	_ = runID
	// Not due: this test covers only the startup sweep's reconciliation of
	// runs left "running" by a previous host. Leaving the row due (as
	// dueAutomation does) let that SAME startup sweep claim and fire it, and
	// the fire goroutine outlives the test — Stop deliberately does not wait
	// for in-flight fires — so its RecordFire/RecordFinish writes landed in
	// t.TempDir's RemoveAll window ("directory not empty" on macOS) and its
	// completion log hit t.Logf after the test ended.
	setStoredNextRun(t, st, a.ID, cursor.t.Add(time.Hour))

	s.Start(context.Background())
	defer stopScheduler(t, s)

	waitCond(t, 2*time.Second, "run marked interrupted", func() bool {
		for _, r := range st.Runs(a.ID) {
			if r.Status == RunInterrupted {
				return true
			}
		}
		return false
	})
	if n := fire.count(); n != 0 {
		t.Fatalf("interrupted-run sweep fired %d automation(s), want 0", n)
	}
}

// TestSchedulerStopPreventsFiring verifies Stop() quiesces the sweep.
func TestSchedulerStopPreventsFiring(t *testing.T) {
	fire := &recordingFire{delay: 50 * time.Millisecond}
	s, st, cursor := newSchedulerTest(t, fire.Fire)
	a := dueAutomation(t, st, cursor)

	s.Start(context.Background())
	s.Stop()
	<-s.Done()
	// The in-flight run from the first sweep may still be landing; wait for
	// it, then pin that the sweep is quiescent.
	awaitRunTerminal(t, st, a.ID)
	time.Sleep(50 * time.Millisecond)
	if n := fire.count(); n != 1 {
		t.Fatalf("fires after Stop = %d, want exactly the one in-flight fire", n)
	}
}

// TestSchedulerStopIsIdempotent pins Stop's documented "safe to call more
// than once" contract. The loop goroutine clears the stop channel only when it
// exits, so a second Stop inside that window used to close the same channel
// again and panic ("close of closed channel") — reachable in production by a
// settings toggle-off followed by server shutdown.
func TestSchedulerStopIsIdempotent(t *testing.T) {
	fire := &recordingFire{}
	s, _, _ := newSchedulerTest(t, fire.Fire)

	s.Start(context.Background())
	// Two Stops back to back, before the loop goroutine has had a chance to
	// exit and clear the channel (the window the panic needed).
	s.Stop()
	s.Stop()
	<-s.Done()
	// Stopping again after the loop exited is a no-op too.
	s.Stop()

	// Start remains restartable after any number of Stops.
	s.Start(context.Background())
	s.Stop()
	<-s.Done()
}

// TestSchedulerSaturatedPoolDoesNotStallSweep pins the sweep/spawn-slot
// decoupling: with every slot busy on a long-running fire, the sweep
// goroutine must not block on the spawn semaphore. A stalled sweep would
// stop claiming later-due rows (their next_run_at would not advance) and,
// because Stop() cannot interrupt a blocked channel send, would leave
// Done() open until a slot frees.
func TestSchedulerSaturatedPoolDoesNotStallSweep(t *testing.T) {
	fire := &recordingFire{delay: 600 * time.Millisecond}
	s, st, cursor := newSchedulerTest(t, fire.Fire)

	// One more due row than slots: the extra claim must park on the
	// semaphore inside its own goroutine without stalling the sweep.
	ids := make([]string, 0, maxConcurrentFires+1)
	for i := 0; i < maxConcurrentFires+1; i++ {
		ids = append(ids, dueAutomation(t, st, cursor).ID)
	}

	s.Start(context.Background())
	waitCond(t, 2*time.Second, "spawn slots saturated", func() bool {
		return fire.count() == maxConcurrentFires
	})

	// The sweep has spawned every claim and returned to its select even
	// though the pool is full and one fire is queued: Done must close on
	// Stop, well before the in-flight fires drain.
	s.Stop()
	select {
	case <-s.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("sweep goroutine blocked on the spawn semaphore: Stop did not close Done")
	}

	// Claimed-before-stop rows still fire (the queued fire waits for a
	// slot instead of being dropped).
	for _, id := range ids {
		awaitRunTerminal(t, st, id)
	}
	if n := fire.count(); n != maxConcurrentFires+1 {
		t.Fatalf("fires = %d, want %d (a queued claim must still fire)", n, maxConcurrentFires+1)
	}
}

func TestSchedulerStartIdempotent(t *testing.T) {
	fire := &recordingFire{delay: time.Second}
	s, st, cursor := newSchedulerTest(t, fire.Fire)
	a := dueAutomation(t, st, cursor)

	ctx := context.Background()
	s.Start(ctx)
	s.Start(ctx) // second start is a no-op
	s.Stop()
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	awaitRunTerminal(t, st, a.ID)
}

func TestSchedulerContextCancel(t *testing.T) {
	fire := &recordingFire{delay: time.Second}
	s, st, cursor := newSchedulerTest(t, fire.Fire)
	a := dueAutomation(t, st, cursor)

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	cancel()
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop on ctx cancel")
	}
	awaitRunTerminal(t, st, a.ID)
}

// --- ProcessFire: the real subprocess runner -------------------------------

// TestHelperProcess is the re-exec helper for the ProcessFire tests (the
// standard Go pattern): the test binary runs itself as the "gogen" binary
// with GO_AUTOM_HELPER=1; the mode env picks the behavior.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_AUTOM_HELPER") != "1" {
		return
	}
	idFile := os.Getenv(SessionIDFileEnv)
	outFile := os.Getenv("GO_AUTOM_HELPER_OUT")
	report := func(extra ...string) {
		if outFile == "" {
			return
		}
		wd, _ := os.Getwd()
		lines := append([]string{"wd=" + wd, "args=" + strings.Join(os.Args[3:], " ")}, extra...)
		_ = os.WriteFile(outFile, []byte(strings.Join(lines, "\n")), 0o600)
	}
	switch os.Getenv("GO_AUTOM_HELPER_MODE") {
	case "ok":
		report()
		if idFile != "" {
			_ = os.WriteFile(idFile, []byte("sess-XYZ\n"), 0o600)
		}
		os.Exit(0)
	case "fail":
		report()
		// Emit more than stderrTailCap of leading noise, then the real error:
		// the captured detail keeps the TAIL, so the error line must survive
		// and the earliest noise must be evicted (cappedTail regression).
		for i := 0; i < 64; i++ {
			fmt.Fprintf(os.Stderr, "noise line %03d %s\n", i, strings.Repeat("x", 72))
		}
		fmt.Fprintln(os.Stderr, "Error: model unreachable")
		os.Exit(3)
	case "hang":
		// Report the cwd first, then hang past the test's timeout.
		report()
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "noworkdir":
		// The runner sets cmd.Dir to the (nonexistent) working dir: the
		// spawn itself must fail with a chdir error.
		os.Exit(0)
	default:
		os.Exit(1)
	}
}

func newHelperFire(t *testing.T, mode, outFile string, timeout time.Duration) *ProcessFire {
	t.Helper()
	return &ProcessFire{
		Binary:  os.Args[0],
		Timeout: timeout,
		// The helper binary is the test binary itself, so the -test.run
		// flag rides in front; the payload that lands after the "--"
		// separator is what exec actually passes through (the default
		// --dir/-p shape is pinned separately in TestProcessFireDefaultArgs).
		BuildArgs: func(a *Automation) []string {
			return []string{"-test.run=TestHelperProcess", "--", "--dir", a.WorkingDir, "-p", a.Prompt}
		},
		ExtraEnv: []string{"GO_AUTOM_HELPER=1", "GO_AUTOM_HELPER_MODE=" + mode, "GO_AUTOM_HELPER_OUT=" + outFile},
		idDir:    t.TempDir(),
	}
}

func TestProcessFireSuccess(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "helper.out")
	f := newHelperFire(t, "ok", outFile, DefaultFireTimeout)
	a := &Automation{
		Title: "t", Prompt: "hello world", WorkingDir: t.TempDir(),
		Schedule: Schedule{Kind: KindDaily, Time: "09:00"},
	}
	handle, err := f.Fire(a)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if handle.SessionIDFile == "" {
		t.Fatal("Fire did not set up the session-id link file")
	}
	select {
	case res := <-handle.Result:
		if res.Err != nil {
			t.Fatalf("run failed: %v (detail %q)", res.Err, res.Detail)
		}
		if res.SessionID != "sess-XYZ" {
			t.Fatalf("session id = %q, want sess-XYZ (the child must report via %s)", res.SessionID, SessionIDFileEnv)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	// The child really ran in the automation's working directory with the
	// built args (--dir <wd> -p <prompt>).
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("helper report missing: %v", err)
	}
	body := string(data)
	// The child reports os.Getwd(), which resolves symlinks; the automation's
	// WorkingDir comes from t.TempDir() and on macOS lives behind
	// /var -> /private/var, so compare the resolved forms. The args keep the
	// literal path (checked below), since they are passed through verbatim.
	gotWD := ""
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, "wd="); ok {
			gotWD = v
			break
		}
	}
	wantWD, err := filepath.EvalSymlinks(a.WorkingDir)
	if err != nil {
		t.Fatalf("resolve working dir %q: %v", a.WorkingDir, err)
	}
	rawWD := gotWD
	gotWD, err = filepath.EvalSymlinks(gotWD)
	if err != nil {
		t.Fatalf("resolve child cwd %q: %v", rawWD, err)
	}
	if gotWD != wantWD {
		t.Fatalf("child cwd = %q (resolved %q), want %q (resolved %q)", rawWD, gotWD, a.WorkingDir, wantWD)
	}
	if !strings.Contains(body, "--dir "+a.WorkingDir) || !strings.Contains(body, "-p hello world") {
		t.Fatalf("child args missing the built payload: %q", body)
	}
}

func TestProcessFireFailureCapturesStderr(t *testing.T) {
	f := newHelperFire(t, "fail", "", DefaultFireTimeout)
	a := &Automation{Title: "t", Prompt: "p", WorkingDir: t.TempDir(), Schedule: Schedule{Kind: KindDaily}}
	handle, err := f.Fire(a)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	select {
	case res := <-handle.Result:
		if res.Err == nil {
			t.Fatal("exit-3 run must fail")
		}
		if !strings.Contains(res.Detail, "model unreachable") {
			t.Fatalf("stderr tail lost: %q", res.Detail)
		}
		// The helper writes > stderrTailCap of noise before the error: the
		// earliest line must have been evicted, proving the writer keeps the
		// newest bytes (a head-only cap would return only the noise).
		if strings.Contains(res.Detail, "noise line 000") {
			t.Fatalf("cappedTail kept the head instead of the tail: %q", res.Detail)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
}

func TestProcessFireTimeoutKills(t *testing.T) {
	f := newHelperFire(t, "hang", "", 250*time.Millisecond)
	a := &Automation{Title: "t", Prompt: "p", WorkingDir: t.TempDir(), Schedule: Schedule{Kind: KindDaily}}
	handle, err := f.Fire(a)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	select {
	case res := <-handle.Result:
		if res.Err == nil {
			t.Fatal("timed-out run must fail")
		}
		if !strings.Contains(res.Err.Error(), "timeout") {
			t.Fatalf("timeout not reported: %v", res.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("killed run did not report")
	}
}

func TestProcessFireMissingWorkingDir(t *testing.T) {
	f := newHelperFire(t, "ok", "", DefaultFireTimeout)
	a := &Automation{Title: "t", Prompt: "p", WorkingDir: filepath.Join(t.TempDir(), "gone"), Schedule: Schedule{Kind: KindDaily}}
	handle, err := f.Fire(a)
	if err == nil {
		// Depending on the platform the chdir failure surfaces at Start
		// (returned) or at Wait (result channel) — both must be a failure.
		select {
		case res := <-handle.Result:
			if res.Err == nil {
				t.Fatal("run in a missing working dir must fail")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("run did not report")
		}
	}
}

func TestProcessFireDefaultArgs(t *testing.T) {
	a := &Automation{Title: "t", Prompt: "do things", WorkingDir: "/proj", Schedule: Schedule{Kind: KindDaily}}
	got := defaultFireArgs(a)
	want := []string{"--dir", "/proj", "-p", "do things"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
}

// TestSchedulerPollerDoesNotBlockRunFinalization pins the fire-result
// ownership contract: the session-id poller waits on handle.Done — never on
// handle.Result, whose single value belongs to the fire goroutine — so a
// run whose session-id file is never written (a failed run, a timeout
// kill) still reaches a terminal record with the poller alive. This is a
// contract pin, not a steal trap: the runtime wakes blocked receivers in
// enqueue order and fireOne registers first, so the old direct-Result
// receive passed this test too (a steal needed a freak preemption window);
// the Done-based form makes the outcome impossible instead of merely
// improbable. The double mirrors ProcessFire at completion — a never-written
// session-id file, a Done signal, and one Result delivered ASYNCHRONOUSLY
// (a value already sitting in the buffer would be taken by fireOne
// directly, without exercising the contended path at all).
func TestSchedulerPollerDoesNotBlockRunFinalization(t *testing.T) {
	st, _ := newTestStore(t)
	for i := 0; i < 24; i++ {
		a := Automation{
			Title: "t", Prompt: "p", WorkingDir: t.TempDir(),
			Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true,
		}
		if err := st.Create(&a); err != nil {
			t.Fatal(err)
		}
		s := NewScheduler(st, func(*Automation) (*FireHandle, error) {
			result := make(chan FireResult, 1)
			done := make(chan struct{})
			go func() {
				// Deliver only after both receivers (fireOne + the poller)
				// are blocked, like a real child exiting does.
				time.Sleep(20 * time.Millisecond)
				result <- FireResult{}
				close(done)
			}()
			return &FireHandle{
				SessionIDFile: filepath.Join(t.TempDir(), "sid.txt"),
				Done:          done,
				Result:        result,
			}, nil
		}, WithLogger(schedulerTestLogger(t)))
		s.fireOne(Claim{Automation: a, PlannedAt: time.Now()})
		awaitRunTerminal(t, st, a.ID)
	}
}
