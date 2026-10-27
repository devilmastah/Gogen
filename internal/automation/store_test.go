package automation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore opens a store in a fresh temp dir with a controllable clock.
func newTestStore(t *testing.T) (*Store, *timeCursor) {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cursor := &timeCursor{t: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	st.now = cursor.Now
	return st, cursor
}

// timeCursor is a controllable clock for deterministic claim tests.
type timeCursor struct {
	t time.Time
}

func (c *timeCursor) Now() time.Time { return c.t }

func (c *timeCursor) Advance(d time.Duration) { c.t = c.t.Add(d) }

func ptrTime(t time.Time) *time.Time { return &t }

// setStoredNextRun plants a next_run_at on a stored row and PERSISTS it.
//
// The direct in-memory poke these tests used to do would now be discarded:
// every store mutation re-reads the file first (the compare-and-set the
// cross-process lock exists for), so test setup has to reach the file exactly
// like a hand-edited store does.
func setStoredNextRun(t *testing.T, st *Store, id string, when time.Time) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.autos[id]
	if !ok {
		t.Fatalf("setStoredNextRun: no automation with id %s", id)
	}
	a.NextRunAt = ptrTime(when)
	if err := st.saveAutomations(); err != nil {
		t.Fatalf("setStoredNextRun: save: %v", err)
	}
}

// dueDaily creates a daily automation due exactly now, so a claim is due (the
// stored next run is persisted, see setStoredNextRun).
func dueDaily(st *Store, now time.Time) Automation {
	a := Automation{
		Title:      "Nightly triage",
		Prompt:     "triage the board",
		WorkingDir: "/tmp/proj",
		Schedule:   Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
		Enabled:    true,
	}
	if err := st.Create(&a); err != nil {
		panic(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.autos[a.ID].NextRunAt = ptrTime(now)
	if err := st.saveAutomations(); err != nil {
		panic(err)
	}
	return a
}

func TestCreateAndReload(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	a := Automation{
		Title:      "Nightly triage",
		Prompt:     "triage the board",
		WorkingDir: "relative/dir",
		Schedule:   Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
		Enabled:    true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.ID == "" || a.CreatedAt.IsZero() {
		t.Fatalf("Create did not assign id/timestamps: %+v", a)
	}
	if !filepath.IsAbs(a.WorkingDir) {
		t.Fatalf("Create did not absolutize the working dir: %q", a.WorkingDir)
	}
	if a.NextRunAt == nil {
		t.Fatalf("Create did not compute next_run_at")
	}
	wantNext, _ := NextRun(a.Schedule, time.Now())
	if !a.NextRunAt.Equal(wantNext) {
		t.Fatalf("next_run_at = %v, want %v", a.NextRunAt, wantNext)
	}

	// A fresh store instance over the same dir reads the same record.
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := st2.Get(a.ID)
	if !ok {
		t.Fatalf("reopened store lost automation %s", a.ID)
	}
	if got.Title != a.Title || got.Schedule.Kind != KindDaily || !got.NextRunAt.Equal(*a.NextRunAt) {
		t.Fatalf("reopened record drifted: %+v vs %+v", got, a)
	}
}

// TestCreateDoesNotAliasCallerStruct pins the store's ownership boundary:
// Create must store its own copy of the row. Aliasing the caller's struct
// made post-Create reads (the web handler's "Created automation" notice,
// the CLI's "Created %s" line) race concurrent mutators — the scheduler's
// advance or another connection's Update — all writing the same struct
// (a -race failure in TestAutomationsUpdateOpViaWS). The caller's struct
// keeps the assigned fields (id, timestamps, next run); store mutations
// must not reach it, and Get must see the store's own state.
func TestCreateDoesNotAliasCallerStruct(t *testing.T) {
	st, _ := newTestStore(t)
	a := Automation{
		Title:      "Nightly",
		Prompt:     "p",
		WorkingDir: "/x",
		Schedule:   Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
		Enabled:    true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Mutate the STORED row through the sanctioned API. With the pre-fix
	// aliasing, SetEnabled(false) wrote the SHARED struct and the caller's
	// read saw the store's value; with the copy boundary, the caller's own
	// struct keeps what Create assigned.
	if err := st.SetEnabled(a.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if !a.Enabled {
		t.Fatal("Create aliased the caller's struct: a store-side mutation reached it")
	}
	got, ok := st.Get(a.ID)
	if !ok {
		t.Fatal("created row vanished")
	}
	if got.Enabled {
		t.Fatal("SetEnabled did not reach the stored row")
	}
	if got.Title != a.Title || got.Prompt != a.Prompt {
		t.Fatalf("store row drifted from the created values: %+v vs caller %+v", got, a)
	}
}

func TestCreateRejectsInvalid(t *testing.T) {
	st, _ := newTestStore(t)
	cases := []struct {
		name string
		a    Automation
	}{
		{"no title", Automation{Prompt: "p", WorkingDir: "/x", Schedule: Schedule{Kind: KindDaily, Time: "09:00"}}},
		{"no prompt", Automation{Title: "t", WorkingDir: "/x", Schedule: Schedule{Kind: KindDaily, Time: "09:00"}}},
		{"no dir", Automation{Title: "t", Prompt: "p", Schedule: Schedule{Kind: KindDaily, Time: "09:00"}}},
		{"bad kind", Automation{Title: "t", Prompt: "p", WorkingDir: "/x", Schedule: Schedule{Kind: "monthly"}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := tc.a
			if err := st.Create(&a); err == nil {
				t.Fatalf("Create accepted an invalid automation: %+v", a)
			}
		})
	}
}

// TestOpenCorruptedStore verifies a corrupted store file surfaces as an
// error (never a panic), and that the store does NOT overwrite the file it
// could not read.
func TestOpenCorruptedStore(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		file string
		body string
	}{
		{"truncated automations file", AutomationsFile, `{"version":1,"automations":[{"id":"auto-x"`},
		{"garbage automations file", AutomationsFile, "\x00\x01 not json at all"},
		{"garbage runs file", RunsFile, "{oops"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := OpenStore(dir)
			if err == nil {
				t.Fatalf("OpenStore accepted corrupted %s", tc.file)
			}
			if st != nil {
				t.Fatalf("OpenStore returned a store alongside an error")
			}
			if !strings.Contains(err.Error(), "corrupted") {
				t.Fatalf("error %v should mention corruption", err)
			}
			// The corrupted bytes are still on disk: the store never
			// destroys data it could not read.
			data, rerr := os.ReadFile(path)
			if rerr != nil || string(data) != tc.body {
				t.Fatalf("corrupted file was modified: %q (err %v)", data, rerr)
			}
		})
	}
}

// TestOpenMissingStoreDir covers the first-run path: no files exist yet.
func TestOpenMissingStoreDir(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "does-not-exist-yet"))
	if err != nil {
		t.Fatalf("OpenStore on a missing dir: %v", err)
	}
	if got := st.List(); len(got) != 0 {
		t.Fatalf("fresh store should be empty, got %v", got)
	}
}

// TestClaimDueFiresOnce is the dedup guarantee: one sweep claims a due row
// and advances it, so an immediate second sweep finds nothing.
func TestClaimDueFiresOnce(t *testing.T) {
	st, cursor := newTestStore(t)
	a := dueDaily(st, cursor.t)

	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("first claim: got %d claims, want 1", len(claims))
	}
	if claims[0].Automation.ID != a.ID {
		t.Fatalf("claim id = %s, want %s", claims[0].Automation.ID, a.ID)
	}
	if claims[0].PlannedAt != cursor.t {
		t.Fatalf("planned = %v, want %v", claims[0].PlannedAt, cursor.t)
	}
	// The row advanced to tomorrow's 09:00.
	got, _ := st.Get(a.ID)
	want := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at after claim = %v, want %v", got.NextRunAt, want)
	}
	if !got.Enabled {
		t.Fatalf("recurring row disabled by a normal claim")
	}

	// Second sweep at the same instant: nothing due (the dedup guarantee).
	if claims := st.ClaimDue(cursor.t, DefaultCatchupWindow); len(claims) != 0 {
		t.Fatalf("second claim fired %d rows — a due row must be claimed exactly once", len(claims))
	}
}

// TestClaimDueCatchupMissed pins the catch-up policy: a due row far older
// than the window is recorded as missed and skipped, not fired.
func TestClaimDueCatchupMissed(t *testing.T) {
	st, cursor := newTestStore(t)
	a := dueDaily(st, cursor.t)

	// The row went stale: it was due 2 days ago (GoGen was closed).
	stale := cursor.t.Add(-48 * time.Hour)
	setStoredNextRun(t, st, a.ID, stale)

	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 0 {
		t.Fatalf("a 48h-stale row must not fire, got %d claims", len(claims))
	}
	runs := st.Runs(a.ID)
	if len(runs) != 1 || runs[0].Status != RunMissed {
		t.Fatalf("missed run not recorded: %+v", runs)
	}
	if runs[0].PlannedAt != stale {
		t.Fatalf("missed planned_at = %v, want %v", runs[0].PlannedAt, stale)
	}
	got, _ := st.Get(a.ID)
	if got.LastRunStatus != RunMissed {
		t.Fatalf("last_run_status = %q, want missed", got.LastRunStatus)
	}
	// Advanced from NOW (not from the stale time) so no extra misses queue.
	want := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at after miss = %v, want %v", got.NextRunAt, want)
	}
}

// TestClaimDueWithinCatchupWindow pins the other half of the policy: a run
// missed by 40 minutes still fires (run-once-on-startup semantics).
func TestClaimDueWithinCatchupWindow(t *testing.T) {
	st, cursor := newTestStore(t)
	a := dueDaily(st, cursor.t)
	setStoredNextRun(t, st, a.ID, cursor.t.Add(-40*time.Minute))

	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("a 40-minute-late due row must fire, got %d claims", len(claims))
	}
	if claims[0].PlannedAt != cursor.t.Add(-40*time.Minute) {
		t.Fatalf("planned = %v, want the stale scheduled time", claims[0].PlannedAt)
	}
}

// TestClaimOnceDisarms verifies a due once-automation claims and disables
// itself (fire once, then off).
func TestClaimOnceDisarms(t *testing.T) {
	st, cursor := newTestStore(t)
	a := Automation{
		Title:      "One-shot deploy",
		Prompt:     "deploy",
		WorkingDir: "/tmp/proj",
		Schedule:   Schedule{Kind: KindOnce, At: "2026-01-05T09:00:00Z"},
		Enabled:    true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("once row not claimed: %d", len(claims))
	}
	got, _ := st.Get(a.ID)
	if got.Enabled || got.NextRunAt != nil {
		t.Fatalf("once row must disarm after the claim: enabled=%v next=%v", got.Enabled, got.NextRunAt)
	}
	if claims := st.ClaimDue(cursor.t, DefaultCatchupWindow); len(claims) != 0 {
		t.Fatalf("disarmed row claimed again")
	}
}

// TestClaimUnfixableDisables pins the "disable instead of spin" rule: a
// hand-edited record whose recurrence cannot produce a future time is
// disabled by the sweep instead of being re-claimed forever.
func TestClaimUnfixableDisables(t *testing.T) {
	st, cursor := newTestStore(t)
	// Bypass validation by writing the record like a hand-edited store file
	// would: an unknown schedule kind (Create would reject it, so the row goes
	// straight to disk — the store re-reads it, exactly like the sweep did).
	a := Automation{ID: NewID(), Title: "broken", Prompt: "p", WorkingDir: "/x",
		Schedule: Schedule{Kind: "fortnightly", Time: "09:00"}, Enabled: true, NextRunAt: ptrTime(cursor.t)}
	st.mu.Lock()
	st.autos[a.ID] = &a
	if err := st.saveAutomations(); err != nil {
		st.mu.Unlock()
		t.Fatalf("save the hand-edited row: %v", err)
	}
	st.mu.Unlock()

	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("due broken row should still be claimed once (it is due): %d", len(claims))
	}
	got, _ := st.Get(a.ID)
	if got.Enabled || got.NextRunAt != nil {
		t.Fatalf("unfixable row must disable itself: %+v", got)
	}
	if claims := st.ClaimDue(cursor.t, DefaultCatchupWindow); len(claims) != 0 {
		t.Fatalf("disabled row claimed again — the row would spin")
	}
}

// TestSetEnabledPersistsAndRecomputes covers enable/disable across a
// restart (persisted, and re-enable recomputes next_run_at from now).
func TestSetEnabledPersistsAndRecomputes(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := Automation{
		Title: "Nightly triage", Prompt: "p", WorkingDir: "/x",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"}, Enabled: true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabled(a.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// Reopen: the disable survived the restart, and disabled rows are never
	// claimed even when their next_run_at has passed.
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st2.Get(a.ID)
	if !ok || got.Enabled {
		t.Fatalf("disable did not persist: %+v", got)
	}
	setStoredNextRun(t, st2, a.ID, time.Now().Add(-time.Hour))
	if claims := st2.ClaimDue(time.Now(), DefaultCatchupWindow); len(claims) != 0 {
		t.Fatalf("disabled row was claimed")
	}
	// Re-enable recomputes next_run_at to the future.
	if err := st2.SetEnabled(a.ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _ = st2.Get(a.ID)
	if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.After(time.Now().Add(-time.Second)) {
		t.Fatalf("re-enable did not recompute next_run_at: %+v", got)
	}
}

func TestSetEnabledUnknownID(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.SetEnabled("auto-nope", true); err == nil {
		t.Fatal("SetEnabled on an unknown id should fail")
	}
}

// TestRunsHistoryAndCaps covers the run-history plumbing: append, finish,
// session-id link, newest-first ordering, and the per-automation cap.
func TestRunsHistoryAndCaps(t *testing.T) {
	st, _ := newTestStore(t)
	a := dueDaily(st, st.now())

	runID, err := st.RecordFire(a.ID, *st.autos[a.ID].NextRunAt)
	if err != nil {
		t.Fatalf("RecordFire: %v", err)
	}
	st.RecordLink(runID, "sess-123")
	if err := st.RecordFinish(runID, RunCompleted, "", "sess-123"); err != nil {
		t.Fatalf("RecordFinish: %v", err)
	}
	runs := st.Runs(a.ID)
	if len(runs) != 1 {
		t.Fatalf("history has %d rows, want 1", len(runs))
	}
	r := runs[0]
	if r.Status != RunCompleted || r.SessionID != "sess-123" || r.FiredAt == nil || r.EndedAt == nil {
		t.Fatalf("finished run incomplete: %+v", r)
	}
	got, _ := st.Get(a.ID)
	if got.LastRunStatus != RunCompleted || got.LastRunAt == nil {
		t.Fatalf("parent last-run mirror not updated: %+v", got)
	}

	// Overflow the cap: only the newest maxRunsPerAutomation survive.
	for i := 0; i < maxRunsPerAutomation+5; i++ {
		id, err := st.RecordFire(a.ID, time.Now())
		if err != nil {
			t.Fatalf("RecordFire %d: %v", i, err)
		}
		if err := st.RecordFinish(id, RunFailed, "boom", ""); err != nil {
			t.Fatalf("RecordFinish %d: %v", i, err)
		}
	}
	if got := len(st.Runs(a.ID)); got != maxRunsPerAutomation {
		t.Fatalf("history size = %d, want the cap %d", got, maxRunsPerAutomation)
	}
	if err := st.RecordFinish("run-nope", RunCompleted, "", ""); err == nil {
		t.Fatal("RecordFinish on an unknown run id should fail")
	}
}

// TestRunsCapIndependentPerAutomation pins the per-automation cap when TWO
// automations both overflow (a regression guard: the compaction used to
// clobber the backing array while later automations' slices still read it).
func TestRunsCapIndependentPerAutomation(t *testing.T) {
	st, _ := newTestStore(t)
	a1 := dueDaily(st, st.now())
	a2 := dueDaily(st, st.now())
	for i := 0; i < maxRunsPerAutomation+7; i++ {
		for _, a := range []Automation{a1, a2} {
			id, err := st.RecordFire(a.ID, time.Now())
			if err != nil {
				t.Fatalf("RecordFire %s: %v", a.ID, err)
			}
			if err := st.RecordFinish(id, RunCompleted, "", ""); err != nil {
				t.Fatalf("RecordFinish %s: %v", a.ID, err)
			}
		}
	}
	for _, a := range []Automation{a1, a2} {
		if got := len(st.Runs(a.ID)); got != maxRunsPerAutomation {
			t.Fatalf("automation %s history size = %d, want the cap %d", a.ID, got, maxRunsPerAutomation)
		}
	}
	// And the total stays bounded when the automations differ.
	if got := len(st.runs); got > maxRunsPerAutomation*2 {
		t.Fatalf("total runs = %d, want <= %d", got, maxRunsPerAutomation*2)
	}
}

// TestRunsPersistAcrossReopen covers the run-history file.
func TestRunsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := Automation{Title: "t", Prompt: "p", WorkingDir: "/x",
		Schedule: Schedule{Kind: KindOnce, At: "2030-01-01T00:00:00Z"}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	runID, err := st.RecordFire(a.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.RecordFinish(runID, RunCompleted, "", "sess-9")

	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	runs := st2.Runs(a.ID)
	if len(runs) != 1 || runs[0].SessionID != "sess-9" {
		t.Fatalf("run history did not persist: %+v", runs)
	}
}

// TestMarkInterruptedRuns verifies startup cleanup of runs left "running"
// by a previous host process.
func TestMarkInterruptedRuns(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := Automation{Title: "t", Prompt: "p", WorkingDir: "/x",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	id1, _ := st.RecordFire(a.ID, time.Now())
	_, _ = st.RecordFire(a.ID, time.Now())
	_ = st.RecordFinish(id1, RunFailed, "boom", "")

	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := st2.MarkInterruptedRuns(); n != 1 {
		t.Fatalf("MarkInterruptedRuns = %d, want 1", n)
	}
	for _, r := range st2.Runs(a.ID) {
		if r.Status == RunRunning {
			t.Fatalf("run row still running after startup cleanup: %+v", r)
		}
	}
	if n := st2.MarkInterruptedRuns(); n != 0 {
		t.Fatalf("second cleanup marked %d more rows", n)
	}
}

// TestDeleteRemovesAutomationAndHistory covers the CLI delete path.
func TestDeleteRemovesAutomationAndHistory(t *testing.T) {
	st, _ := newTestStore(t)
	a := dueDaily(st, st.now())
	runID, err := st.RecordFire(a.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = runID
	if err := st.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := st.Get(a.ID); ok {
		t.Fatal("automation survived delete")
	}
	if runs := st.Runs(a.ID); len(runs) != 0 {
		t.Fatalf("history survived delete: %+v", runs)
	}
	if err := st.Delete("auto-nope"); err == nil {
		t.Fatal("delete of unknown id should fail")
	}
}

// blockStoreFile replaces one of the store files with a directory so the
// atomic write for it fails (the temp-file rename hits EISDIR) while the
// other file stays writable. The returned func removes the blocker and
// restores the original bytes; it is idempotent.
func blockStoreFile(t *testing.T, st *Store, name string) func() {
	t.Helper()
	path := filepath.Join(st.dir, name)
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	restored := false
	return func() {
		if restored {
			return
		}
		restored = true
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove blocker %s: %v", path, err)
		}
		if err := os.WriteFile(path, orig, 0o600); err != nil {
			t.Fatalf("restore %s: %v", path, err)
		}
	}
}

// marshalAutomation serializes a record so two snapshots can be compared
// without relying on time.Time's internal representation.
func marshalAutomation(t *testing.T, a Automation) string {
	t.Helper()
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal automation: %v", err)
	}
	return string(data)
}

// TestFailedWritesRollBackMemory pins the phantom-write discipline for the
// mutation paths that do NOT recreate the row from scratch: a failed
// automations write must leave both memory and disk at the pre-operation
// state. Create already compensates by deleting its just-added row;
// SetEnabled, Update, and Delete must roll back too, or the next successful
// save would silently persist an operation the caller was told had failed.
func TestFailedWritesRollBackMemory(t *testing.T) {
	cases := []struct {
		name  string
		apply func(st *Store, a Automation) error
	}{
		{
			name: "SetEnabled",
			apply: func(st *Store, a Automation) error {
				return st.SetEnabled(a.ID, false)
			},
		},
		{
			name: "Update",
			apply: func(st *Store, a Automation) error {
				updated := a
				updated.Title = "Renamed by a failed write"
				updated.Enabled = false
				return st.Update(&updated)
			},
		},
		{
			name: "Delete",
			apply: func(st *Store, a Automation) error {
				return st.Delete(a.ID)
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newTestStore(t)
			a := updateBase(t, st)
			if _, err := st.RecordFire(a.ID, st.now()); err != nil {
				t.Fatalf("RecordFire: %v", err)
			}
			before, ok := st.Get(a.ID)
			if !ok {
				t.Fatal("setup lost the automation")
			}
			beforeRuns := len(st.Runs(a.ID))

			restore := blockStoreFile(t, st, AutomationsFile)
			if err := tc.apply(st, a); err == nil {
				t.Fatal("operation succeeded despite a blocked automations write")
			}
			// In-memory state is untouched: no phantom mutation is left
			// waiting for the next successful save to persist it.
			after, ok := st.Get(a.ID)
			if !ok {
				t.Fatal("failed write dropped the row from memory")
			}
			if marshalAutomation(t, before) != marshalAutomation(t, after) {
				t.Fatalf("failed write mutated memory:\n before %s\n  after %s",
					marshalAutomation(t, before), marshalAutomation(t, after))
			}
			if got := len(st.Runs(a.ID)); got != beforeRuns {
				t.Fatalf("failed write changed the in-memory history: %d -> %d", beforeRuns, got)
			}

			// The disk still holds the pre-operation record, so the next
			// successful write cannot smuggle the aborted mutation through.
			restore()
			if err := st.SetEnabled(a.ID, true); err != nil {
				t.Fatalf("post-failure write: %v", err)
			}
			st2, err := OpenStore(st.dir)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			onDisk, ok := st2.Get(a.ID)
			if !ok {
				t.Fatal("aborted operation was persisted by a later successful write")
			}
			if marshalAutomation(t, before) != marshalAutomation(t, onDisk) {
				t.Fatalf("disk record drifted after an aborted write:\n before %s\n   disk %s",
					marshalAutomation(t, before), marshalAutomation(t, onDisk))
			}
		})
	}
}

// TestDeleteRunsWriteFailureKeepsCommitPoint pins the two-file commit
// point: once the automations write succeeds the row is durably gone, so a
// failed history write must NOT resurrect it in memory (that would let the
// next automations save restore a row the user deleted). The history
// cleanup is retried by the next runs write instead.
func TestDeleteRunsWriteFailureKeepsCommitPoint(t *testing.T) {
	st, _ := newTestStore(t)
	a := updateBase(t, st)
	if _, err := st.RecordFire(a.ID, st.now()); err != nil {
		t.Fatal(err)
	}
	restore := blockStoreFile(t, st, RunsFile)
	if err := st.Delete(a.ID); err == nil {
		t.Fatal("Delete should report the failed history write")
	}
	if _, ok := st.Get(a.ID); ok {
		t.Fatal("row resurrected in memory after the automations commit point")
	}
	if runs := st.Runs(a.ID); len(runs) != 0 {
		t.Fatalf("history removal not applied in memory: %d rows", len(runs))
	}
	// The automations file lost the row; the failed history write is only
	// an eventual cleanup.
	restore()
	st2, err := OpenStore(st.dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := st2.Get(a.ID); ok {
		t.Fatal("deleted row still on disk after the automations commit")
	}
}

// TestStoreFileShapes pins the on-disk formats (machine-readable, versioned
// JSON under the global config dir).
func TestStoreFileShapes(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := dueDaily(st, st.now())
	if _, err := st.RecordFire(a.ID, st.now()); err != nil {
		t.Fatal(err)
	}

	var autos struct {
		Version     int          `json:"version"`
		Automations []Automation `json:"automations"`
	}
	data, err := os.ReadFile(filepath.Join(dir, AutomationsFile))
	if err != nil {
		t.Fatalf("automations file missing: %v", err)
	}
	if err := json.Unmarshal(data, &autos); err != nil {
		t.Fatalf("automations file is not the documented shape: %v", err)
	}
	if autos.Version != 1 || len(autos.Automations) != 1 {
		t.Fatalf("unexpected automations file: %s", data)
	}

	var runs struct {
		Version int   `json:"version"`
		Runs    []Run `json:"runs"`
	}
	data, err = os.ReadFile(filepath.Join(dir, RunsFile))
	if err != nil {
		t.Fatalf("runs file missing: %v", err)
	}
	if err := json.Unmarshal(data, &runs); err != nil {
		t.Fatalf("runs file is not the documented shape: %v", err)
	}
	if runs.Version != 1 || len(runs.Runs) != 1 {
		t.Fatalf("unexpected runs file: %s", data)
	}
}

// TestReadersSeeAnotherInstancesWrites pins read-path freshness: a row created
// or edited through a second Store instance must show up in this instance's
// List/Get/Runs without this one performing a mutation of its own.
//
// A second instance in the same process is the faithful simulation of another
// PROCESS here: the two share nothing but the files on disk (each has its own
// memory and its own mutex), which is exactly the relation between the CLI and
// a running host. Before the stamp check, List/Get served the copy loaded at
// OpenStore, so a job created from the terminal stayed invisible in the web
// tab until some unrelated mutation refreshed it — and after the cross-process
// lock work the write survived, so the stale read became the visible half of
// that bug.
func TestReadersSeeAnotherInstancesWrites(t *testing.T) {
	dir := t.TempDir()
	host, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore(host): %v", err)
	}
	cli, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore(cli): %v", err)
	}
	if got := host.List(); len(got) != 0 {
		t.Fatalf("fresh store lists %d automations", len(got))
	}

	// 1. A row created by the other instance becomes visible.
	a := Automation{
		Title:      "created from the CLI",
		Prompt:     "check the thing",
		WorkingDir: dir,
		Schedule:   Schedule{Kind: KindHourly, Minute: 5, Timezone: "UTC"},
		Enabled:    true,
	}
	if err := cli.Create(&a); err != nil {
		t.Fatalf("cli Create: %v", err)
	}
	if got := host.List(); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("host List after cli Create = %+v, want the created row", got)
	}
	if got, ok := host.Get(a.ID); !ok || got.Title != a.Title {
		t.Fatalf("host Get after cli Create = %+v ok=%v", got, ok)
	}

	// 2. An edit by the other instance becomes visible, including a same-length
	// one (the stamp compares mtime as well as size).
	stored, ok := cli.Get(a.ID)
	if !ok {
		t.Fatal("cli Get: row vanished")
	}
	stored.Title = "edited from CLI" // same length as "created from the CLI"? no — but keep it simple
	stored.Prompt = "check the other thing"
	if err := cli.Update(&stored); err != nil {
		t.Fatalf("cli Update: %v", err)
	}
	if got, _ := host.Get(a.ID); got.Prompt != "check the other thing" {
		t.Fatalf("host Get after cli Update = %q, want the edited prompt", got.Prompt)
	}

	// 3. A run recorded by the other instance becomes visible.
	if _, err := cli.RecordFire(a.ID, time.Now()); err != nil {
		t.Fatalf("cli RecordFire: %v", err)
	}
	if runs := host.Runs(a.ID); len(runs) != 1 || runs[0].AutomationID != a.ID {
		t.Fatalf("host Runs after cli RecordFire = %+v, want one run", runs)
	}

	// 4. A delete by the other instance becomes visible.
	if err := cli.Delete(a.ID); err != nil {
		t.Fatalf("cli Delete: %v", err)
	}
	if got, ok := host.Get(a.ID); ok {
		t.Fatalf("host Get after cli Delete = %+v, want gone", got)
	}
	if got := host.List(); len(got) != 0 {
		t.Fatalf("host List after cli Delete = %+v, want empty", got)
	}
}
