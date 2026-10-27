package automation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Two-process tests.
//
// The defects these pin are cross-PROCESS defects — the CLI editing the store
// while a host runs, two hosts sweeping the same due row, a starting host
// closing a run a live sibling is executing — so they cannot be reproduced with
// goroutines in one process (the store's sync.Mutex would serialize them and
// hide exactly what is broken). Each test re-executes this test binary as a
// REAL second OS process, the standard Go helper-process idiom: the child runs
// only TestAutomationStoreHelperProcess and picks its operation from
// GO_AUTOM_MP_* env vars, so both processes contend for the same store
// directory and the same lock file.

const (
	mpModeEnv    = "GO_AUTOM_MP_MODE"
	mpDirEnv     = "GO_AUTOM_MP_DIR"
	mpIDEnv      = "GO_AUTOM_MP_ID"
	mpTitleEnv   = "GO_AUTOM_MP_TITLE"
	mpStartEnv   = "GO_AUTOM_MP_START"   // barrier: the helper waits for it
	mpReleaseEnv = "GO_AUTOM_MP_RELEASE" // the hold-run helper waits for it
	mpResultEnv  = "GO_AUTOM_MP_RESULT"  // where the helper writes its outcome
)

// mpClaimWindow is how long each claim contestant sweeps. It only has to
// outlast one ClaimDue in the other process; the loops are read-only once the
// single claim landed, so a loaded box cannot make this flaky.
const mpClaimWindow = 750 * time.Millisecond

// TestAutomationStoreHelperProcess is the second-process helper. In a normal
// run (no GO_AUTOM_MP_MODE) it returns immediately.
func TestAutomationStoreHelperProcess(t *testing.T) {
	mode := os.Getenv(mpModeEnv)
	if mode == "" {
		return
	}
	st, err := OpenStore(os.Getenv(mpDirEnv))
	if err != nil {
		mpFailf("open store: %v", err)
	}
	switch mode {
	case "create":
		a := Automation{
			Title:      os.Getenv(mpTitleEnv),
			Prompt:     "helper prompt",
			WorkingDir: os.Getenv(mpDirEnv),
			Schedule:   Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
			Enabled:    true,
		}
		if err := st.Create(&a); err != nil {
			mpFailf("create: %v", err)
		}
		mpWriteResult("id=" + a.ID)
	case "claim":
		// Announce that this process has the store loaded (its in-memory copy
		// is now a snapshot the other process is about to make stale), THEN
		// wait for the starting gun: both processes must sweep from their own
		// pre-race copy for the contention to be real.
		mpWriteResult("ready")
		mpWaitForFile(os.Getenv(mpStartEnv))
		mpWriteResult("claims=" + strconv.Itoa(mpClaimLoop(st, mpClaimWindow)))
	case "hold-run":
		// Record a run and keep this process alive: it is the "live sibling
		// host" a starting host must not disturb.
		runID, err := st.RecordFire(os.Getenv(mpIDEnv), time.Now())
		if err != nil {
			mpFailf("record fire: %v", err)
		}
		mpWriteResult("run=" + runID)
		mpWaitForFile(os.Getenv(mpReleaseEnv))
	default:
		mpFailf("unknown helper mode %q", mode)
	}
	os.Exit(0)
}

func mpFailf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "helper: "+format+"\n", args...)
	os.Exit(2)
}

func mpWriteResult(body string) {
	path := os.Getenv(mpResultEnv)
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		mpFailf("write result: %v", err)
	}
}

func mpWaitForFile(path string) {
	if path == "" {
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	mpFailf("timed out waiting for %s", path)
}

// mpClaimLoop sweeps ClaimDue like a host scheduler does, with the catch-up
// window wide enough that the past-due once row fires instead of being missed.
func mpClaimLoop(st *Store, d time.Duration) int {
	deadline := time.Now().Add(d)
	claims := 0
	for time.Now().Before(deadline) {
		claims += len(st.ClaimDue(time.Now(), 24*time.Hour))
		time.Sleep(time.Millisecond)
	}
	return claims
}

// mpHelper is a running second process.
type mpHelper struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *bytes.Buffer
	done bool
}

// startMPHelper launches the re-executed test binary with the given
// "K=V" environment entries. The caller must either wait or kill it.
func startMPHelper(t *testing.T, env ...string) *mpHelper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestAutomationStoreHelperProcess")
	cmd.Env = append(os.Environ(), env...)
	out := &bytes.Buffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	h := &mpHelper{t: t, cmd: cmd, out: out}
	t.Cleanup(h.kill)
	return h
}

// wait blocks until the helper exits and fails the test if it exited non-zero.
func (h *mpHelper) wait() {
	h.t.Helper()
	if h.done {
		return
	}
	h.done = true
	if err := h.cmd.Wait(); err != nil {
		h.t.Fatalf("helper process failed: %v\n%s", err, h.out.String())
	}
}

// kill terminates the helper unless it already exited (cleanup for a failing
// assertion, which must not leave a stray sleep behind).
func (h *mpHelper) kill() {
	if h.done {
		return
	}
	h.done = true
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	_ = h.cmd.Wait()
}

func readResult(t *testing.T, path, prefix string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("helper result %s: %v", path, err)
	}
	body := strings.TrimSpace(string(data))
	value, ok := strings.CutPrefix(body, prefix)
	if !ok {
		t.Fatalf("helper result %q does not start with %q", body, prefix)
	}
	return value
}

// waitForResult waits until the helper's result file holds exactly body.
func waitForResult(t *testing.T, path, body string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == body {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s", body, path)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// newTwoProcessAutomation creates a stored automation in dir for the
// cross-process tests.
func newTwoProcessAutomation(t *testing.T, st *Store, dir, title string, schedule Schedule) Automation {
	t.Helper()
	a := Automation{Title: title, Prompt: "p", WorkingDir: dir, Schedule: schedule, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatalf("create automation: %v", err)
	}
	return a
}

// TestStoreMutationSurvivesAnotherProcessesWrite pins defect 1: the store used
// to write its whole in-memory map back, so a row the CLI (or a second host)
// added after this process read the file was silently overwritten by the next
// mutation here. The parent now runs that mutation — a plain --disable of its
// own row — after the second process created a row, and both rows must survive.
func TestStoreMutationSurvivesAnotherProcessesWrite(t *testing.T) {
	dir := t.TempDir()
	parent, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	own := newTwoProcessAutomation(t, parent, dir, "parent job",
		Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"})

	result := filepath.Join(dir, "create.result")
	helper := startMPHelper(t,
		mpModeEnv+"=create", mpDirEnv+"="+dir,
		mpTitleEnv+"=cli job", mpResultEnv+"="+result)
	helper.wait()
	cliID := readResult(t, result, "id=")

	// The running host's next write: with the stale-map overwrite this save
	// dropped the CLI's row from disk.
	if err := parent.SetEnabled(own.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	// The in-memory copy is refreshed by the mutation, so the CLI's row is
	// visible here too (not just after a reopen).
	if got := parent.List(); len(got) != 2 {
		t.Fatalf("parent sees %d automations after its own write, want 2: %+v", len(got), got)
	}
	if _, ok := parent.Get(cliID); !ok {
		t.Fatalf("the other process's automation %s is missing from memory", cliID)
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	autos := reopened.List()
	if len(autos) != 2 {
		t.Fatalf("store holds %d automations after the concurrent write, want 2: %+v", len(autos), autos)
	}
	byID := map[string]Automation{}
	for _, a := range autos {
		byID[a.ID] = a
	}
	if _, ok := byID[cliID]; !ok {
		t.Fatal("the row the second process created was overwritten by the parent's write")
	}
	got, ok := byID[own.ID]
	if !ok {
		t.Fatalf("the parent's own row vanished")
	}
	if got.Enabled {
		t.Fatal("the parent's --disable was lost")
	}
}

// TestClaimDueClaimedOnceAcrossProcesses pins defect 2: two hosts sweeping
// their own copy of the store both saw the same due next_run_at and both fired
// the automation. ClaimDue is now a compare-and-set on next_run_at, so the
// occurrence is claimed exactly once between the two processes.
func TestClaimDueClaimedOnceAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	parent, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	// A once row due ten minutes ago: well inside the (widened) catch-up
	// window, so it must FIRE rather than be recorded as missed.
	at := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	auto := newTwoProcessAutomation(t, parent, dir, "due once",
		Schedule{Kind: KindOnce, At: at})

	start := filepath.Join(dir, "start")
	result := filepath.Join(dir, "claim.result")
	helper := startMPHelper(t,
		mpModeEnv+"=claim", mpDirEnv+"="+dir,
		mpStartEnv+"="+start, mpResultEnv+"="+result)
	// Wait until the second process has the store loaded, so its copy is the
	// same stale snapshot this process holds when both start sweeping.
	waitForResult(t, result, "ready")
	if err := os.WriteFile(start, []byte("go\n"), 0o600); err != nil {
		t.Fatalf("start barrier: %v", err)
	}

	parentClaims := mpClaimLoop(parent, mpClaimWindow)
	helper.wait()
	helperClaims, err := strconv.Atoi(readResult(t, result, "claims="))
	if err != nil {
		t.Fatalf("helper claim count: %v", err)
	}
	if total := parentClaims + helperClaims; total != 1 {
		t.Fatalf("due automation claimed %d times across two processes (parent %d, helper %d), want exactly 1",
			total, parentClaims, helperClaims)
	}

	// One more sweep refreshes this process's copy and must find nothing due.
	if claims := parent.ClaimDue(time.Now(), 24*time.Hour); len(claims) != 0 {
		t.Fatalf("a claimed row was claimed again: %d claims", len(claims))
	}
	got, ok := parent.Get(auto.ID)
	if !ok {
		t.Fatal("claimed automation vanished")
	}
	if got.Enabled || got.NextRunAt != nil {
		t.Fatalf("once row not disarmed by the claim: enabled=%v next=%v", got.Enabled, got.NextRunAt)
	}
	if runs := parent.Runs(auto.ID); len(runs) != 0 {
		t.Fatalf("a within-catchup claim must not record a missed run: %+v", runs)
	}
}

// appendLegacyRun writes a `running` run row WITHOUT host/pid — the shape an
// older binary produced — straight into the history file.
func appendLegacyRun(t *testing.T, dir, autoID string) string {
	t.Helper()
	path := filepath.Join(dir, RunsFile)
	var wrapper struct {
		Version int   `json:"version"`
		Runs    []Run `json:"runs"`
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &wrapper); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", path, err)
	}
	run := Run{
		ID:           NewRunID(),
		AutomationID: autoID,
		PlannedAt:    time.Now().Add(-time.Minute),
		FiredAt:      ptrTime(time.Now().Add(-time.Minute)),
		Status:       RunRunning,
	}
	wrapper.Version = 1
	wrapper.Runs = append(wrapper.Runs, run)
	data, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return run.ID
}

// runStatus returns the status of one run row.
func runStatus(st *Store, autoID, runID string) string {
	for _, r := range st.Runs(autoID) {
		if r.ID == runID {
			return r.Status
		}
	}
	return ""
}

// TestMarkInterruptedRunsLeavesLiveForeignRun pins defect 3: a starting host
// marked EVERY `running` row interrupted, including the run a different, still
// live host was executing. Ownership is now recorded on the row, so only the
// starting host's own leftovers (and unowned legacy rows) are closed.
func TestMarkInterruptedRunsLeavesLiveForeignRun(t *testing.T) {
	dir := t.TempDir()
	parent, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	auto := newTwoProcessAutomation(t, parent, dir, "job",
		Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"})

	// A live sibling host (another process, same machine) records a run and
	// keeps running.
	result := filepath.Join(dir, "hold.result")
	release := filepath.Join(dir, "release")
	helper := startMPHelper(t,
		mpModeEnv+"=hold-run", mpDirEnv+"="+dir,
		mpIDEnv+"="+auto.ID, mpResultEnv+"="+result, mpReleaseEnv+"="+release)
	waitForFile(t, result)
	foreignRun := readResult(t, result, "run=")

	// The starting host also has leftovers of its own and one legacy row.
	ownRun, err := parent.RecordFire(auto.ID, time.Now())
	if err != nil {
		t.Fatalf("RecordFire: %v", err)
	}
	legacyRun := appendLegacyRun(t, dir, auto.ID)

	if n := parent.MarkInterruptedRuns(); n != 2 {
		t.Fatalf("MarkInterruptedRuns closed %d rows, want 2 (this process's run + the legacy row)", n)
	}
	if status := runStatus(parent, auto.ID, foreignRun); status != RunRunning {
		t.Fatalf("a live sibling host's run was marked %q, want it left running", status)
	}
	if status := runStatus(parent, auto.ID, ownRun); status != RunInterrupted {
		t.Fatalf("this process's own stale run is %q, want interrupted", status)
	}
	if status := runStatus(parent, auto.ID, legacyRun); status != RunInterrupted {
		t.Fatalf("the unowned legacy row is %q, want interrupted", status)
	}

	// The sibling exits without finishing: now its row is a dead host's
	// leftover and the next startup sweep closes it.
	if err := os.WriteFile(release, []byte("done\n"), 0o600); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	helper.wait()
	if n := parent.MarkInterruptedRuns(); n != 1 {
		t.Fatalf("MarkInterruptedRuns closed %d rows after the owner exited, want 1", n)
	}
	if status := runStatus(parent, auto.ID, foreignRun); status != RunInterrupted {
		t.Fatalf("the dead host's run is %q, want interrupted", status)
	}
}

// TestRecordFireStampsOwnership pins the on-disk schema: a run row records the
// host identity and pid that started it, which is what MarkInterruptedRuns
// decides on. Legacy rows simply lack the fields (omitempty) and load as
// unowned.
func TestRecordFireStampsOwnership(t *testing.T) {
	st, _ := newTestStore(t)
	a := dueDaily(st, st.now())
	runID, err := st.RecordFire(a.ID, st.now())
	if err != nil {
		t.Fatalf("RecordFire: %v", err)
	}
	var wrapper struct {
		Runs []Run `json:"runs"`
	}
	data, err := os.ReadFile(filepath.Join(st.dir, RunsFile))
	if err != nil {
		t.Fatalf("read %s: %v", RunsFile, err)
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("parse %s: %v", RunsFile, err)
	}
	if len(wrapper.Runs) != 1 {
		t.Fatalf("history has %d rows, want 1", len(wrapper.Runs))
	}
	got := wrapper.Runs[0]
	if got.ID != runID || got.Host != st.host || got.PID != st.pid {
		t.Fatalf("run row ownership = host %q pid %d, want host %q pid %d",
			got.Host, got.PID, st.host, st.pid)
	}
	if st.host == "" || st.pid <= 0 {
		t.Fatalf("store has no host identity: host %q pid %d", st.host, st.pid)
	}
}
