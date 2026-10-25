package automation

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gogen/internal/ioutil"
	"gogen/internal/projectfile"
)

// Store file names (under the global config dir, next to config.yaml).
const (
	AutomationsFile = "automations.json"
	RunsFile        = "automation_runs.json"
)

// History caps: the run file is deliberately small ("a small history").
const (
	maxRunsPerAutomation = 25
	maxTotalRuns         = 1000
)

// DefaultDir returns the store directory: the global config dir
// (~/.config/gogen/ on Linux, honoring XDG_CONFIG_HOME).
func DefaultDir() string {
	return projectfile.GlobalConfigDir()
}

// OpenStore opens (or creates) the automation store rooted at dir.
//
// A store file that exists but cannot be parsed is an error — the store
// never overwrites data it could not read, so a corrupted automations.json
// surfaces as an error message (in the CLI) or a logged warning with
// automations disabled (in the hosts), never as lost data or a panic.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	s := &Store{dir: dir, now: time.Now}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Store is the file-backed automation + run-history store. All state lives
// behind one mutex: the CLI mutates records while the in-process scheduler
// sweeps and appends runs, and every mutation rewrites the affected JSON
// file atomically (temp file + rename). Single-writer across processes is
// NOT guaranteed — see the multi-host note in the package docs.
type Store struct {
	mu  sync.Mutex
	dir string

	autos map[string]*Automation
	runs  []Run

	// now is swappable for tests so claim/catch-up math is deterministic.
	now func() time.Time
}

// load reads both files. Missing files are an empty store; unparseable
// files abort the open (see OpenStore).
func (s *Store) load() error {
	if err := loadInto(filepath.Join(s.dir, AutomationsFile), &s.autos); err != nil {
		return err
	}
	var runs struct {
		Runs []Run `json:"runs"`
	}
	path := filepath.Join(s.dir, RunsFile)
	// ReadFileRetry: the store is published by temp-file rename, so on Windows
	// a reader can be refused with a sharing violation while a writer replaces
	// the file. A plain read turns that momentary window into a hard failure for
	// the CLI, a second host, or the polling regression test.
	data, err := ioutil.ReadFileRetry(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		return nil
	}
	if err := json.Unmarshal(data, &runs); err != nil {
		return fmt.Errorf("%s is corrupted (not valid JSON): %w", path, err)
	}
	s.runs = runs.Runs
	return nil
}

// loadInto decodes the automations file into a map keyed by id.
func loadInto(path string, into *map[string]*Automation) error {
	var wrapper struct {
		Automations []Automation `json:"automations"`
	}
	data, err := ioutil.ReadFileRetry(path)
	if err != nil {
		if os.IsNotExist(err) {
			*into = map[string]*Automation{}
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return fmt.Errorf("%s is corrupted (not valid JSON): %w", path, err)
	}
	m := make(map[string]*Automation, len(wrapper.Automations))
	for i := range wrapper.Automations {
		a := wrapper.Automations[i]
		m[a.ID] = &a
	}
	*into = m
	return nil
}

// saveAutomations rewrites automations.json atomically.
func (s *Store) saveAutomations() error {
	list := make([]Automation, 0, len(s.autos))
	for _, a := range s.autos {
		list = append(list, *a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	return writeFileAtomic(filepath.Join(s.dir, AutomationsFile), storeFileBody("automations", list))
}

// saveRuns rewrites automation_runs.json atomically, keeping the history
// bounded (per-automation and total caps; oldest runs drop first).
func (s *Store) saveRuns() error {
	runs := s.runs
	// Per-automation cap: decide every keep-set first, then compact once —
	// compacting inside the loop would overwrite the backing array the
	// other automations' slices still read.
	perAuto := map[string][]Run{}
	for _, r := range runs {
		perAuto[r.AutomationID] = append(perAuto[r.AutomationID], r)
	}
	keepAll := map[string]map[string]bool{}
	for id, list := range perAuto {
		if len(list) <= maxRunsPerAutomation {
			continue
		}
		excess := len(list) - maxRunsPerAutomation
		keep := map[string]bool{}
		for _, r := range list[excess:] {
			keep[r.ID] = true
		}
		keepAll[id] = keep
	}
	if len(keepAll) > 0 {
		filtered := runs[:0]
		for _, r := range runs {
			keep, capped := keepAll[r.AutomationID]
			if !capped || keep[r.ID] {
				filtered = append(filtered, r)
			}
		}
		runs = filtered
	}
	if len(runs) > maxTotalRuns {
		runs = runs[len(runs)-maxTotalRuns:]
	}
	s.runs = runs
	return writeFileAtomic(filepath.Join(s.dir, RunsFile), storeFileBody("runs", runs))
}

// initialNextRun computes the first next_run_at for a freshly stored
// automation. A once schedule takes its stored At instant verbatim (even in
// the past or exactly now) so the sweep's catch-up policy applies to it
// uniformly; recurrences take their next future occurrence.
func initialNextRun(schedule Schedule, now time.Time) *time.Time {
	if schedule.Kind == KindOnce {
		if at, err := ParseAt(schedule.At, schedule.Timezone); err == nil {
			return &at
		}
		return nil
	}
	next, err := NextRun(schedule, now)
	if err != nil || next.IsZero() {
		return nil
	}
	return &next
}

// storeFileBody marshals items as {"version":1, key:[...]}.
func storeFileBody[T any](key string, items []T) []byte {
	wrapper := map[string]any{"version": 1, key: items}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		// Marshaling plain data structures cannot fail; keep going rather
		// than inventing an error path the callers cannot meaningfully hit.
		data = []byte("{}")
	}
	return append(data, '\n')
}

// writeFileAtomic writes data via a temp file in the target directory and a
// rename, so a crash mid-write can never leave a truncated store behind. It
// goes through the shared atomic writer (which also fsyncs, so a power loss
// cannot revert the store to its previous contents) and keeps the store
// files private at 0600 unless an existing file already carries another
// mode.
func writeFileAtomic(path string, data []byte) error {
	return ioutil.WriteFileAtomic(path, data, 0o600)
}

// List returns all automations sorted by creation time (oldest first).
func (s *Store) List() []Automation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Store) listLocked() []Automation {
	out := make([]Automation, 0, len(s.autos))
	for _, a := range s.autos {
		out = append(out, a.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Get returns a copy of the automation with the given id.
func (s *Store) Get(id string) (Automation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.autos[id]
	if !ok {
		return Automation{}, false
	}
	return a.clone(), true
}

// Create validates and stores a new automation. The id and timestamps are
// assigned here; next_run_at is computed from the schedule (once schedules
// take their stored At instant). The record is disabled-eligible: a next
// run in the past is stored as-is and handled by the first sweep (missed
// per the catch-up policy).
func (s *Store) Create(a *Automation) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(a.WorkingDir) {
		abs, err := filepath.Abs(a.WorkingDir)
		if err != nil {
			return fmt.Errorf("resolve working dir: %w", err)
		}
		a.WorkingDir = abs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	a.ID = NewID()
	a.CreatedAt = now
	a.UpdatedAt = now
	if a.Enabled {
		a.NextRunAt = initialNextRun(a.Schedule, now)
	}
	if s.autos == nil {
		s.autos = map[string]*Automation{}
	}
	// Store a CLONE, not the caller's pointer: Create only assigns the
	// caller's fields (id/timestamps/next run) — after it returns, the
	// caller keeps reading its own struct (the CLI's "Created %s" line, the
	// web handler's "Created automation" notice) while the scheduler or
	// another connection's Update mutates the stored row. Aliasing the
	// caller's struct made those reads race the mutators (seen as a -race
	// failure in TestAutomationsUpdateOpViaWS: the create connection's
	// notice read raced another connection's Update through the same
	// struct). Get/listLocked already hand out clones; Create must respect
	// the same ownership boundary.
	stored := a.clone()
	s.autos[a.ID] = &stored
	if err := s.saveAutomations(); err != nil {
		delete(s.autos, a.ID) // don't leave a phantom row behind a failed write
		return err
	}
	return nil
}

// SetEnabled toggles the enabled flag. Re-enabling recomputes next_run_at
// from now (a stale stored time would otherwise fire immediately or be
// double-missed); disabling keeps the stored time for reference but the
// sweep never claims disabled rows.
func (s *Store) SetEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.autos[id]
	if !ok {
		return fmt.Errorf("no automation with id %s", id)
	}
	prev := a.clone()
	a.Enabled = enabled
	a.UpdatedAt = s.now()
	if enabled {
		if next, err := NextRun(a.Schedule, a.UpdatedAt); err == nil {
			a.NextRunAt = &next
		} else {
			a.NextRunAt = nil
			log.Printf("automation %s: schedule yields no next run, keeping it disabled-pending: %v", id, err)
		}
	}
	if err := s.saveAutomations(); err != nil {
		// Roll the in-memory row back: with the mutation left in place a
		// later successful save would silently persist a toggle that never
		// hit the disk (the same phantom-write discipline as Create).
		*a = prev
		return err
	}
	return nil
}

// Delete removes the automation and its history entries.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.autos[id]
	if !ok {
		return fmt.Errorf("no automation with id %s", id)
	}
	prev := a.clone()
	prevRuns := s.runs // the filter below builds a NEW slice, so this stays intact
	delete(s.autos, id)
	var runs []Run
	for _, r := range s.runs {
		if r.AutomationID != id {
			runs = append(runs, r)
		}
	}
	s.runs = runs
	if err := s.saveAutomations(); err != nil {
		// The automations file is the commit point and nothing was written:
		// restore the row and its history together so memory and disk keep
		// agreeing. Returning with the deletion still applied to memory
		// would let the next successful save silently persist it (the
		// phantom-write discipline of Create applied to both halves).
		stored := prev
		s.autos[id] = &stored
		s.runs = prevRuns
		return err
	}
	// Past the commit point the row is durably gone; the history cleanup is
	// best-effort from here — a failed runs write is retried by the next
	// saveRuns and can never resurrect the automation.
	return s.saveRuns()
}

// Update replaces the user-editable fields of the automation with the given
// id (title, prompt, working dir, schedule, enabled state) and persists it.
// The record is validated like Create; id, timestamps, and the run history
// are preserved.
//
// next_run_at policy: recomputed when the schedule CHANGED (a once row takes
// its new instant; a recurrence takes its next future occurrence from now —
// missed occurrences collapse, same as re-enable), or when a paused row is
// re-enabled without a schedule change (the fresh-enable rule of
// SetEnabled). Otherwise the stored next_run_at is kept — editing the
// prompt must not delay a run that is already minutes away.
func (s *Store) Update(updated *Automation) error {
	if err := updated.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(updated.WorkingDir) {
		abs, err := filepath.Abs(updated.WorkingDir)
		if err != nil {
			return fmt.Errorf("resolve working dir: %w", err)
		}
		updated.WorkingDir = abs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.autos[updated.ID]
	if !ok {
		return fmt.Errorf("no automation with id %s", updated.ID)
	}
	prev := stored.clone()
	scheduleChanged := !schedulesEqual(stored.Schedule, updated.Schedule)
	wasEnabled := stored.Enabled

	stored.Title = updated.Title
	stored.Prompt = updated.Prompt
	stored.WorkingDir = updated.WorkingDir
	stored.Schedule = updated.Schedule
	stored.Enabled = updated.Enabled
	stored.UpdatedAt = s.now()

	switch {
	case scheduleChanged:
		stored.NextRunAt = initialNextRun(stored.Schedule, stored.UpdatedAt)
	case !wasEnabled && stored.Enabled:
		// Fresh enable with an unchanged schedule: next from now, exactly
		// like SetEnabled(true).
		if next, err := NextRun(stored.Schedule, stored.UpdatedAt); err == nil {
			stored.NextRunAt = &next
		} else {
			stored.NextRunAt = nil
			log.Printf("automation %s: schedule yields no next run: %v", stored.ID, err)
		}
		// Unchanged schedule on an already-enabled row keeps its stored
		// next_run_at: a prompt edit must not delay an imminent run.
	}
	if err := s.saveAutomations(); err != nil {
		// Roll the stored row back to its pre-update value: otherwise the
		// new fields stay in memory only and the next successful save
		// silently persists an update that reported failure (Create's
		// phantom-write discipline).
		*stored = prev
		return err
	}
	return nil
}

// schedulesEqual compares two schedules field by field (the weekday lists
// are compared as sets; the store canonicalizes them sorted at create).
func schedulesEqual(a, b Schedule) bool {
	if a.Kind != b.Kind || a.At != b.At || a.Time != b.Time || a.Minute != b.Minute || a.Timezone != b.Timezone {
		return false
	}
	if len(a.Weekdays) != len(b.Weekdays) {
		return false
	}
	for i := range a.Weekdays {
		if a.Weekdays[i] != b.Weekdays[i] {
			return false
		}
	}
	return true
}

// Runs returns the history for one automation, newest first.
func (s *Store) Runs(id string) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, r := range s.runs {
		if r.AutomationID == id {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlannedAt.Equal(out[j].PlannedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].PlannedAt.After(out[j].PlannedAt)
	})
	return out
}

// Claim is one due automation handed to the scheduler after its next_run_at
// was already advanced (the dedup guarantee): firing Claim is safe exactly
// once because the stored schedule has moved on.
type Claim struct {
	Automation Automation
	PlannedAt  time.Time
}

// ClaimDue advances every enabled automation whose next_run_at is due and
// returns the claims to fire, all under one lock.
//
// Catch-up policy (documented on the Scheduler too): a due row whose
// planned time is older than the catch-up window was missed while no host
// was running (or the host slept far past the window) — it is recorded as
// a missed run and the schedule advances without firing. Rows overdue by
// less than the window fire normally.
//
// A row whose schedule yields no next time (a once row that fired, or an
// unrecoverable recurrence) is disabled instead of spinning — same behavior
// as the reference design's "disable instead of loop".
func (s *Store) ClaimDue(now time.Time, catchup time.Duration) []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	var claims []Claim
	changed := false
	for _, a := range s.autos {
		if !a.Enabled || a.NextRunAt == nil || a.NextRunAt.After(now) {
			continue
		}
		planned := *a.NextRunAt
		if now.Sub(planned) > catchup {
			// Missed while GoGen was not running: record and advance.
			s.appendRunLocked(Run{
				ID:           NewRunID(),
				AutomationID: a.ID,
				PlannedAt:    planned,
				FiredAt:      &now,
				EndedAt:      &now,
				Status:       RunMissed,
				Detail: fmt.Sprintf("missed: due %s ago, beyond the %s catch-up window",
					now.Sub(planned).Round(time.Second), catchup),
			})
			a.LastRunAt = &now
			a.LastRunStatus = RunMissed
			changed = true
		} else {
			claims = append(claims, Claim{Automation: a.clone(), PlannedAt: planned})
		}
		// Advance (or disarm) regardless of fired/missed so the row cannot
		// be claimed twice.
		s.advanceLocked(a, now)
		changed = true
	}
	if changed {
		if err := s.saveAutomations(); err != nil {
			log.Printf("automation store: advance save failed: %v", err)
		}
		if err := s.saveRuns(); err != nil {
			log.Printf("automation store: run-history save failed: %v", err)
		}
	}
	return claims
}

// advanceLocked moves the row to its next fire time. A once row (or a
// recurrence that yields no future time) disables itself and clears
// next_run_at.
func (s *Store) advanceLocked(a *Automation, now time.Time) {
	if a.Schedule.Kind == KindOnce {
		a.Enabled = false
		a.NextRunAt = nil
		a.UpdatedAt = now
		return
	}
	next, err := NextRun(a.Schedule, now)
	if err != nil || next.IsZero() {
		// A recurrence that no longer yields a future time disables the row
		// rather than spinning every sweep.
		a.Enabled = false
		a.NextRunAt = nil
		a.UpdatedAt = now
		return
	}
	a.NextRunAt = &next
	a.UpdatedAt = now
}

// appendRunLocked appends a run entry and mirrors it onto the parent row's
// Last* fields.
func (s *Store) appendRunLocked(run Run) {
	s.runs = append(s.runs, run)
	if a, ok := s.autos[run.AutomationID]; ok {
		a.LastRunAt = cloneTime(run.FiredAt)
		a.LastRunStatus = run.Status
	}
}

// RecordFire records that a claim fired: the run row is created in the
// running state and the parent's Last* fields are updated. It returns the
// run id used by RecordFinish.
func (s *Store) RecordFire(autoID string, plannedAt time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.autos[autoID]; !ok {
		return "", fmt.Errorf("no automation with id %s", autoID)
	}
	now := s.now()
	run := Run{
		ID:           NewRunID(),
		AutomationID: autoID,
		PlannedAt:    plannedAt,
		FiredAt:      &now,
		Status:       RunRunning,
	}
	s.appendRunLocked(run)
	if err := s.saveRuns(); err != nil {
		return "", err
	}
	return run.ID, s.saveAutomations()
}

// RecordFinish stores the terminal outcome of a fired run (and the headless
// session it produced).
func (s *Store) RecordFinish(runID, status, detail, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for i := range s.runs {
		if s.runs[i].ID != runID {
			continue
		}
		s.runs[i].Status = status
		s.runs[i].Detail = detail
		s.runs[i].SessionID = sessionID
		s.runs[i].EndedAt = &now
		autoID := s.runs[i].AutomationID
		if a, ok := s.autos[autoID]; ok {
			a.LastRunStatus = status
		}
		if err := s.saveRuns(); err != nil {
			return err
		}
		return s.saveAutomations()
	}
	return fmt.Errorf("no run with id %s", runID)
}

// RecordLink attaches the headless session id to a still-running run as
// soon as the child reports it (best-effort, non-fatal).
func (s *Store) RecordLink(runID, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.runs {
		if s.runs[i].ID == runID && s.runs[i].SessionID == "" {
			s.runs[i].SessionID = sessionID
			_ = s.saveRuns()
			return
		}
	}
}

// MarkInterruptedRuns closes out run rows still marked running (a previous
// host process exited before the run finished). Returns the number of rows
// closed. Called once at scheduler startup.
func (s *Store) MarkInterruptedRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	now := s.now()
	for i := range s.runs {
		if s.runs[i].Status != RunRunning {
			continue
		}
		s.runs[i].Status = RunInterrupted
		s.runs[i].Detail = "host exited before the run finished"
		s.runs[i].EndedAt = &now
		n++
	}
	if n > 0 {
		if err := s.saveRuns(); err != nil {
			log.Printf("automation store: interrupted-run save failed: %v", err)
		}
	}
	return n
}
