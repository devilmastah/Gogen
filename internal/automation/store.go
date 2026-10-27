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

	"gogen/internal/filelock"
	"gogen/internal/ioutil"
	"gogen/internal/projectfile"
)

// Store file names (under the global config dir, next to config.yaml).
const (
	AutomationsFile = "automations.json"
	RunsFile        = "automation_runs.json"
	// LockFile is the advisory cross-process lock guarding every
	// read-modify-write of the two files above. It is never renamed,
	// truncated or deleted: the JSON files are published by temp-file
	// rename, and deleting a held lock file would let a third process lock a
	// fresh inode while the previous holder still holds the old one.
	LockFile = "automations.lock"
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
	s := &Store{dir: dir, host: hostIdentity(), pid: os.Getpid(), now: time.Now}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Store is the file-backed automation + run-history store.
//
// Concurrency has two layers, and their order is the whole contract:
//
//   - In-process: mu serializes every read and mutation (the CLI mutates
//     records while the in-process scheduler sweeps and appends runs).
//   - Cross-process: an advisory file lock (filelock, see LockFile) held
//     across each read-modify-write of the store files. Lock order is mu,
//     THEN the file lock, and the file lock is released BEFORE mu (defer
//     order gives this naturally) — never the reverse, or two processes could
//     each hold one and wait for the other.
//
// The file lock alone is not enough: this store used to read the JSON once and
// write the whole in-memory map back, so even a locked write clobbered
// whatever another process had written in the meantime. Every mutating method
// therefore RE-READS the affected file inside the lock, applies only its own
// change to that fresh state, and writes it back; the in-memory copy becomes
// the fresh state, so List/Get never serve rows a mutation just made stale.
// Every write is atomic (temp file + rename + fsync).
//
// Run rows also carry the identity of the host process that started them
// (Run.Host/Run.PID) so a starting host can tell its own leftovers from a run
// another LIVE host is executing right now — see MarkInterruptedRuns.
type Store struct {
	mu  sync.Mutex
	dir string
	// host is this process's machine identity (see hostIdentity) and pid its
	// OS process id; both are stamped on the run rows it writes.
	host string
	pid  int

	autos map[string]*Automation
	runs  []Run
	// stamps record the file state the in-memory copies were read from, so a
	// reader can tell that another process wrote the store since (see
	// refreshAutomationsLocked). Zero stamps mean "not read yet".
	autosStamp fileStamp
	runsStamp  fileStamp

	// now is swappable for tests so claim/catch-up math is deterministic.
	now func() time.Time
}

// hostIdentity is the machine identity recorded on run rows. Together with the
// pid it answers "whose run is this?" — this process, another live process on
// this machine (a TUI plus a `--web` host), or another machine.
//
// It is deliberately the MACHINE hostname and not a per-process id: a host
// that restarts must recognize the rows its previous incarnation left
// `running` and close them, which a per-process id would hide from it forever.
//
// Known limitation: hosts are identified by hostname, so two machines sharing
// a hostname look like one machine — the pid check then decides, and a pid
// that is live elsewhere but absent here reads as dead. Hostnames are assumed
// unique per fleet, as they are for every other machine-identity scheme with
// no daemon or shared registry.
func hostIdentity() string {
	name, err := os.Hostname()
	if err != nil {
		// No identity at all is worse than a shared one: an empty Host is the
		// legacy marker for "owner unknown", which MarkInterruptedRuns closes.
		return "unknown-host"
	}
	return name
}

// lockStore takes the cross-process store lock. Callers MUST hold s.mu (lock
// order: mu, then file lock) and must Release the result before returning.
//
// A failure here is returned to error-returning callers and logged by the
// best-effort ones (ClaimDue, RecordLink, MarkInterruptedRuns): a mutation
// that cannot take the lock must not run at all, because applying it to a
// state it could not re-read is exactly the lost-update bug the lock exists to
// prevent.
func (s *Store) lockStore() (*filelock.Lock, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, fmt.Errorf("create automation store dir %s: %w", s.dir, err)
	}
	lock, err := filelock.Acquire(filepath.Join(s.dir, LockFile))
	if err != nil {
		return nil, fmt.Errorf("lock automation store %s: %w", s.dir, err)
	}
	return lock, nil
}

// load reads both files. Missing files are an empty store; unparseable
// files abort the open (see OpenStore).
func (s *Store) load() error {
	if err := s.reloadAutomationsLocked(); err != nil {
		return err
	}
	return s.reloadRunsLocked()
}

// reloadAutomationsLocked replaces the in-memory automations with the current
// contents of automations.json. Callers hold s.mu — and, when they are about to
// write, the store file lock too: re-reading is half of the compare-and-set,
// writing without it is the stale-map overwrite this store used to suffer.
//
// The fresh map replaces the old one only on success, so an unreadable file
// leaves memory (and a subsequent save) untouched.
// fileStamp identifies one version of a store file: a writer publishes it by
// temp-file rename, so size+mtime changes on every write.
type fileStamp struct {
	size int64
	mod  time.Time
}

// stampFor stats a store file, reporting ok=false when it does not exist (a
// store that was never written).
func stampFor(path string) (fileStamp, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, false
	}
	return fileStamp{size: fi.Size(), mod: fi.ModTime()}, true
}

// changedSince reports whether the file at path was written since stamp. A
// missing file counts as changed only when the stamp is non-zero (it was there
// before).
func changedSince(path string, stamp fileStamp) bool {
	cur, ok := stampFor(path)
	if !ok {
		return stamp != fileStamp{}
	}
	return !cur.mod.Equal(stamp.mod) || cur.size != stamp.size
}

// refreshAutomationsLocked re-reads the automations file when another process
// wrote it, so a reader (List/Get, i.e. the web tab or `gogen automation ls`)
// never shows a stale view of a store somebody else just edited. It takes no
// file lock on purpose: writers publish by atomic rename, so a plain read sees
// either the old or the new file, never a torn one — and the read path must not
// queue behind a writer's exclusive lock.
func (s *Store) refreshAutomationsLocked() {
	if !changedSince(filepath.Join(s.dir, AutomationsFile), s.autosStamp) {
		return
	}
	if err := s.reloadAutomationsLocked(); err != nil {
		// Keep serving the last consistent copy: a store another process is
		// mid-write on (or a corrupted one) must not blank the list.
		log.Printf("automation store: refresh skipped: %v", err)
	}
}

// refreshRunsLocked is refreshAutomationsLocked for the run history.
func (s *Store) refreshRunsLocked() {
	if !changedSince(filepath.Join(s.dir, RunsFile), s.runsStamp) {
		return
	}
	if err := s.reloadRunsLocked(); err != nil {
		log.Printf("automation store: run-history refresh skipped: %v", err)
	}
}

func (s *Store) reloadAutomationsLocked() error {
	var m map[string]*Automation
	if err := loadInto(filepath.Join(s.dir, AutomationsFile), &m); err != nil {
		return err
	}
	s.autos = m
	s.autosStamp, _ = stampFor(filepath.Join(s.dir, AutomationsFile))
	return nil
}

// reloadRunsLocked replaces the in-memory run history with the current
// contents of automation_runs.json (same discipline as reloadAutomationsLocked).
func (s *Store) reloadRunsLocked() error {
	var wrapper struct {
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
		s.runs = nil
		s.runsStamp = fileStamp{}
		return nil
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return fmt.Errorf("%s is corrupted (not valid JSON): %w", path, err)
	}
	s.runs = wrapper.Runs
	s.runsStamp, _ = stampFor(path)
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

// saveAutomations rewrites automations.json atomically and records the new
// file stamp, so this process's own readers see no spurious refresh.
//
// Both store files go through ioutil.WriteFileAtomic: a temp file in the target
// directory plus fsync and a rename, so a crash or power loss mid-write can
// never leave a truncated store behind. They stay private at 0600 (unless an
// existing file already carries another mode).
func (s *Store) saveAutomations() error {
	list := make([]Automation, 0, len(s.autos))
	for _, a := range s.autos {
		list = append(list, *a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	path := filepath.Join(s.dir, AutomationsFile)
	err := ioutil.WriteFileAtomic(path, storeFileBody("automations", list), 0o600)
	if err == nil {
		s.autosStamp, _ = stampFor(path)
	}
	return err
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
	path := filepath.Join(s.dir, RunsFile)
	err := ioutil.WriteFileAtomic(path, storeFileBody("runs", runs), 0o600)
	if err == nil {
		s.runsStamp, _ = stampFor(path)
	}
	return err
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

// List returns all automations sorted by creation time (oldest first).
func (s *Store) List() []Automation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshAutomationsLocked()
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
	s.refreshAutomationsLocked()
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
	lock, err := s.lockStore()
	if err != nil {
		return err
	}
	defer lock.Release()
	// Append to the CURRENT file contents: a row the CLI created a moment ago
	// must not disappear because this process still holds the map it read at
	// open time.
	if err := s.reloadAutomationsLocked(); err != nil {
		return err
	}
	now := s.now()
	a.ID = NewID()
	a.CreatedAt = now
	a.UpdatedAt = now
	if a.Enabled {
		a.NextRunAt = initialNextRun(a.Schedule, now)
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
	lock, err := s.lockStore()
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := s.reloadAutomationsLocked(); err != nil {
		return err
	}
	a, ok := s.autos[id]
	if !ok {
		// Includes the row another host deleted since this process read it:
		// the fresh state is the truth.
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
	lock, err := s.lockStore()
	if err != nil {
		// Nothing was removed anywhere, so memory still matches disk.
		return err
	}
	defer lock.Release()
	if err := s.reloadAutomationsLocked(); err != nil {
		return err
	}
	a, ok := s.autos[id]
	if !ok {
		return fmt.Errorf("no automation with id %s", id)
	}
	prev := a.clone()
	prevRuns := s.runs // the filter below builds a NEW slice, so this stays intact
	delete(s.autos, id)
	s.runs = withoutRuns(s.runs, id)
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
	if err := s.reloadRunsLocked(); err != nil {
		// The history could not be re-read, so it must NOT be rewritten from
		// this process's copy (that would drop every run another host
		// appended). Drop the rows from memory instead so Runs() agrees with
		// the committed delete, and report the failure.
		s.runs = withoutRuns(s.runs, id)
		return err
	}
	s.runs = withoutRuns(s.runs, id)
	return s.saveRuns()
}

// withoutRuns returns the run history without the given automation's rows
// (Delete's history cleanup).
func withoutRuns(runs []Run, autoID string) []Run {
	var out []Run
	for _, r := range runs {
		if r.AutomationID != autoID {
			out = append(out, r)
		}
	}
	return out
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
	lock, err := s.lockStore()
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := s.reloadAutomationsLocked(); err != nil {
		return err
	}
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
	s.refreshRunsLocked()
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
	lock, err := s.lockStore()
	if err != nil {
		// Nothing may fire: a claim whose advance could not be persisted
		// would be handed to the scheduler again on the next sweep.
		log.Printf("automation store: claim skipped: %v", err)
		return nil
	}
	defer lock.Release()
	// seen is what this process last read; the re-read below is what is
	// actually on disk. A row whose next_run_at MOVED between the two was
	// claimed (or re-timed) by another process, so this claim skips it: two
	// hosts sweeping the same due row must fire it exactly once between them.
	seen := s.autos
	if err := s.reloadAutomationsLocked(); err != nil {
		log.Printf("automation store: claim skipped: %v", err)
		return nil
	}
	// The claim writes the history too (missed rows, the Last* mirror), so it
	// is re-read as well. A history that cannot be read aborts the claim
	// instead of being clobbered by this process's stale copy.
	if err := s.reloadRunsLocked(); err != nil {
		log.Printf("automation store: claim skipped: %v", err)
		return nil
	}
	var claims []Claim
	changed := false
	for _, a := range s.autos {
		if !a.Enabled || a.NextRunAt == nil || a.NextRunAt.After(now) {
			continue
		}
		if prev, known := seen[a.ID]; known && !sameNextRun(prev.NextRunAt, a.NextRunAt) {
			// Another process owns the row's current time (it claimed this
			// occurrence or the user re-timed it from the CLI). The refreshed
			// in-memory copy makes the NEXT sweep compare the new time with
			// itself, so the occurrence is claimed once — a sweep later —
			// instead of twice.
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

// sameNextRun reports whether two next_run_at stamps denote the same instant
// (a nil on either side counts as a difference, so "unset" never compares
// equal to a real time).
func sameNextRun(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
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
	lock, err := s.lockStore()
	if err != nil {
		return "", err
	}
	defer lock.Release()
	// Append to the current history and mirror onto the current row: the
	// history another host just extended must survive this write.
	if err := s.reloadAutomationsLocked(); err != nil {
		return "", err
	}
	if _, ok := s.autos[autoID]; !ok {
		return "", fmt.Errorf("no automation with id %s", autoID)
	}
	if err := s.reloadRunsLocked(); err != nil {
		return "", err
	}
	now := s.now()
	run := Run{
		ID:           NewRunID(),
		AutomationID: autoID,
		PlannedAt:    plannedAt,
		FiredAt:      &now,
		Status:       RunRunning,
		// Ownership: the starting host stamps itself on the row so another
		// host's startup reconciliation can tell a live run from a dead one
		// (see MarkInterruptedRuns).
		Host: s.host,
		PID:  s.pid,
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
	lock, err := s.lockStore()
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := s.reloadAutomationsLocked(); err != nil {
		return err
	}
	if err := s.reloadRunsLocked(); err != nil {
		return err
	}
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
	lock, err := s.lockStore()
	if err != nil {
		log.Printf("automation store: session link skipped: %v", err)
		return
	}
	defer lock.Release()
	if err := s.reloadRunsLocked(); err != nil {
		log.Printf("automation store: session link skipped: %v", err)
		return
	}
	for i := range s.runs {
		if s.runs[i].ID == runID && s.runs[i].SessionID == "" {
			s.runs[i].SessionID = sessionID
			_ = s.saveRuns()
			return
		}
	}
}

// MarkInterruptedRuns closes out run rows still marked running because the host
// that started them is gone. Returns the number of rows closed. Called once at
// scheduler startup.
//
// Ownership rule (Run.Host/Run.PID; the fields do not exist in files written by
// older binaries, which load as Host=""):
//
//   - Another machine (Host != this host) — left alone. That run belongs to a
//     host this process cannot observe, and guessing "it must be dead" is how
//     a starting host used to kill the run a live sibling was executing.
//   - This machine, another pid, and that pid is still alive (a TUI plus a
//     `--web` host) — left alone, same reason.
//   - This machine and that pid is gone — closed: its host crashed.
//   - This machine, this pid — closed. A run this very process recorded can
//     only still be `running` because the scheduler was stopped and restarted
//     inside it; if the fire goroutine is in fact still alive, its own
//     RecordFinish overwrites this row a moment later.
//   - No owner (Host="") — a legacy row written before host identity existed.
//     There is no way to tell a crashed owner from a live older-binary host, so
//     the row is closed, which is what every host did for every row before this
//     change: leaving it `running` forever would be a worse lie.
//
// A pid that cannot be inspected (permission denied) counts as alive — the
// conservative side, which leaves a row alone rather than closing a live run.
func (s *Store) MarkInterruptedRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.lockStore()
	if err != nil {
		log.Printf("automation store: interrupted-run sweep skipped: %v", err)
		return 0
	}
	defer lock.Release()
	if err := s.reloadRunsLocked(); err != nil {
		log.Printf("automation store: interrupted-run sweep skipped: %v", err)
		return 0
	}
	n := 0
	now := s.now()
	for i := range s.runs {
		run := &s.runs[i]
		if run.Status != RunRunning || ownedByLiveHost(run.Host, run.PID, s.host, s.pid) {
			continue
		}
		run.Status = RunInterrupted
		run.Detail = "host exited before the run finished"
		run.EndedAt = &now
		n++
	}
	if n > 0 {
		if err := s.saveRuns(); err != nil {
			log.Printf("automation store: interrupted-run save failed: %v", err)
		}
	}
	return n
}

// ownedByLiveHost reports whether a running row belongs to a host that is not
// this process and is still alive — the rows MarkInterruptedRuns must not
// touch. host/pid identify the current process.
func ownedByLiveHost(runHost string, runPID int, host string, pid int) bool {
	if runHost == "" {
		return false // legacy row: no owner recorded (see MarkInterruptedRuns)
	}
	if runHost != host {
		return true // another machine's run
	}
	if runPID == 0 || runPID == pid {
		return false // this process (or an unrecorded pid we cannot attribute)
	}
	return processAlive(runPID)
}
