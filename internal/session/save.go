package session

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"gogen/internal/ioutil"
	"gogen/internal/spill"
)

// maxCreatedCacheEntries limits the in-memory created-timestamp cache so it
// cannot grow unboundedly on long-running processes with many sessions.
const maxCreatedCacheEntries = 200

// savePayloadHook, when non-nil, runs inside Save at the boundary between its
// first critical section (path/Created resolution) and the unlocked payload
// serialization+write. It is a test-only seam for Save's lock contract: the
// store mutex must be free when it runs, so the hook may re-enter the store.
// Production leaves it nil.
var savePayloadHook func()

// loadDecodeHook, when non-nil, runs inside LoadInWorkingDir at the boundary
// between the locked raw-file capture and the lock-free JSON decode. It is a
// test-only seam for Load's lock contract, mirroring savePayloadHook: the
// store mutex must be free when it runs, so the hook may re-enter the store.
// Production leaves it nil.
var loadDecodeHook func()

// setCreatedCache adds an entry to the created-timestamp cache, evicting the
// entry with the oldest Created timestamp if the cache exceeds
// maxCreatedCacheEntries to prevent unbounded memory growth on long-running
// processes. (Go map iteration is unordered, so the oldest must be found by
// comparing timestamps rather than "first entry".)
//
// Callers must hold s.mu (all callers are Store methods that already guard
// against a nil receiver).
func (s *Store) setCreatedCache(id string, created time.Time) {
	if len(s.createdCache) >= maxCreatedCacheEntries {
		var oldestID string
		var oldest time.Time
		for k, v := range s.createdCache {
			if oldestID == "" || v.Before(oldest) {
				oldestID, oldest = k, v
			}
		}
		if oldestID != "" {
			delete(s.createdCache, oldestID)
		}
	}
	s.createdCache[id] = created
}

// Save writes a session snapshot.
//
// Save deliberately does NOT hold the store-wide mutex while it serializes and
// writes the (potentially large) message payload. Holding it across
// json.Marshal of the whole history plus the atomic write meant a turn-end full
// save of a big session blocked every other store operation — List/Info/
// LatestID, i.e. the web sidebar, which is served synchronously on the WS read
// loop — for the whole marshal+write. The payload write is per-session and made
// visible atomically by the temp-file rename, so it needs no shared lock; only
// the shared metadata (createdCache, saveCount, index.json) is mutated, in two
// short critical sections that bracket the unlocked write.
func (s *Store) Save(id string, snap SessionSnapshot) error {
	_, err := s.SaveSized(id, snap)
	return err
}

// SaveSized is Save plus the size of the payload written, so the agent can
// decide when a delta has grown enough to be folded into a full snapshot (see
// Agent.doPersist) without a second stat or a full serialization.
//
// The payload is STREAMED through the atomic writer instead of being marshaled
// into one []byte first: a full snapshot of a large session was a second full
// copy of the whole history on the heap, on top of the copy the agent already
// took under its lock.
func (s *Store) SaveSized(id string, snap SessionSnapshot) (int64, error) {
	if !s.enabled || id == "" {
		return 0, nil
	}
	if err := validateSessionID(id); err != nil {
		return 0, err
	}

	// First critical section: resolve the paths and the immutable Created
	// timestamp from shared state (createdCache, else the index, else a
	// legacy file decode). Cheap in steady state and free of payload
	// serialization. The dir/globalDir reads also belong here — globalDir is
	// guarded by s.mu.
	s.mu.Lock()
	dir := s.dir(snap.WorkingDir)
	path := s.path(snap.WorkingDir, id)
	if err := ensureUnderSessionsDir(snap.WorkingDir, path, s.globalDir); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	if s.skipEmptySave(id, snap, path) {
		s.mu.Unlock()
		return 0, nil
	}
	created := s.recoverCreated(id, path, snap.WorkingDir)
	s.mu.Unlock()

	// Unlocked: serialize and atomically write the snapshot. The rename means
	// readers never observe a torn file; LoadInWorkingDir's delta baseCount
	// check tolerates the (now wider) gap between this write and the delta
	// unlink below — it is the same crash window the store already handles.
	if savePayloadHook != nil {
		savePayloadHook()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	out := file{
		Version:         version,
		ID:              id,
		Created:         created,
		Updated:         time.Now().UTC(),
		WorkingDir:      snap.WorkingDir,
		Model:           snap.Model,
		ModelProvider:   snap.ModelProvider,
		Mode:            snap.Mode,
		ThinkingLevel:   snap.ThinkingLevel,
		Label:           snap.Label,
		LabelRenamed:    snap.LabelRenamed,
		ProjectProfile:  snap.ProjectProfile,
		Todos:           snap.Todos,
		Messages:        snap.Messages,
		Oneshot:         snap.Oneshot,
		TokenCounts:     snap.TokenCounts,
		ContextLimit:    snap.ContextLimit,
		ParentID:        snap.ParentID,
		SubagentStatus:  snap.SubagentStatus,
		SubagentSummary: snap.SubagentSummary,
	}
	n, err := s.writeSnapshot(path, out)
	if err != nil {
		return 0, err
	}

	// Second critical section: publish the write to the shared metadata.
	// The full snapshot now supersedes the delta. Remove it only after the
	// snapshot write succeeded: clearing it first meant a crash or a write
	// failure between the two operations permanently lost the messages that
	// existed only in the delta. A stale delta that survives into a later
	// load is detected and dropped by LoadInWorkingDir's baseCount check.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.clearDeltaFile(snap.WorkingDir, id); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: failed to remove delta for session %s: %v", id, err)
	}
	s.setCreatedCache(id, created)
	s.saveCount++
	// Update index and invalidate in-memory cache. Runs before prune (order
	// swap): prune always keeps the current session id, so its later index
	// rewrite cannot clobber this entry. The index is re-read here — the
	// preload recoverCreated may have taken is NOT reused, because the lock
	// was released across the payload write and another Save may have
	// updated index.json in the meantime; reusing the stale snapshot would
	// drop that concurrent entry. The extra read is a small index.json
	// decode, dwarfed by the payload work the split moved out of the lock.
	label := sessionLabel(snap.Messages, snap.Label, snap.LabelRenamed)
	s.updateIndex(snap.WorkingDir, sessionIndexEntry{
		ID:              id,
		Created:         out.Created,
		Updated:         out.Updated,
		Oneshot:         snap.Oneshot,
		MessageCount:    len(snap.Messages),
		Label:           label,
		LabelRenamed:    snap.LabelRenamed,
		ParentID:        snap.ParentID,
		SubagentStatus:  snap.SubagentStatus,
		SubagentSummary: snap.SubagentSummary,
	})
	// Per-parent transcript cap (D2): nested sessions are exempt from the
	// global retention counts but capped per parent (keep the most recent
	// 10 children; oldest pruned at child save time).
	if snap.ParentID != "" {
		s.pruneNestedSiblings(snap.WorkingDir, id, snap.ParentID)
	}
	// Prune every 3 saves to avoid repeated directory scans on every write.
	if s.autoPrune && s.saveCount%3 == 0 {
		s.prune(snap.WorkingDir, id)
	}
	return n, nil
}

// writeSnapshot streams the encoded snapshot through the atomic writer and
// returns the number of payload bytes written. Encoding straight to the temp
// file avoids materializing the whole session as a second []byte on the heap;
// the rename still makes the publish atomic for readers.
func (s *Store) writeSnapshot(path string, out file) (int64, error) {
	w, err := ioutil.NewAtomicWriterNoSync(path, 0o600)
	if err != nil {
		return 0, err
	}
	cw := &countingWriter{w: w}
	if err := json.NewEncoder(cw).Encode(out); err != nil {
		w.Abort()
		return 0, err
	}
	if err := w.Commit(); err != nil {
		return 0, err
	}
	return cw.n, nil
}

// countingWriter counts the bytes written through to w.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// skipEmptySave reports whether a session with no content should not be
// persisted: /new panes that are never used are flushed on server quit
// (ShutdownSessions flushes every registered runtime), and writing a
// 0-message file + index entry for each one would bloat the saved-session
// list with empty sessions after every restart (the sidebar renders a row
// per index entry; a 0-message row shows no count — messageCount is
// omitempty, so zero is absent from the payload). A session that WAS saved
// before and was rolled back to empty must still update its file — the old
// content was deliberately dropped — so the skip applies only when neither
// the snapshot nor a pending delta exists on disk. Two kinds of content are
// exceptions because they can exist without any message:
//   - A label: it can only be set deliberately (RenameSession / the
//     session_rename tool), so an empty session that was explicitly named
//     must be persisted — otherwise the rename is silently dropped.
//   - Non-empty todos: the legacy-todos migration adopts a project-level
//     todos.json into a fresh session before the first turn, and the source
//     file is renamed away — skipping the write would lose the imported
//     list permanently if the user quits before sending a message.
//
// (Oneshot single-prompt sessions are NOT an exception: their pre-prompt
// flush must stay skipped or every `-p` run would leave an empty session
// entry behind. ParentID/outcome-only children are not exceptions either: a
// refused spawn must not leave a failed session file.) Caller must hold
// s.mu.
func (s *Store) skipEmptySave(id string, snap SessionSnapshot, path string) bool {
	if len(snap.Messages) == 0 && snap.Label == "" && !snapshotHasTodos(snap) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if _, err := os.Stat(s.deltaPath(snap.WorkingDir, id)); os.IsNotExist(err) {
				return true
			}
		}
	}
	return false
}

// snapshotHasTodos reports whether the snapshot carries session-scoped todo
// content. The agent always passes a non-nil (possibly empty) list via
// todoSnapshot, so the item count is what distinguishes "nothing to persist"
// from a todos-only session.
func snapshotHasTodos(snap SessionSnapshot) bool {
	return snap.Todos != nil && len(snap.Todos.Items) > 0
}

// recoverCreated returns the immutable Created timestamp for a session,
// preserving it across saves: the in-memory createdCache when warm, else
// the metadata index (Created is written there on every save, so the common
// restart case avoids re-reading the potentially large session file), else a
// minimal field-only decode of the session file for legacy indexes that
// predate the created field (index-loss recovery; the minimal target skips
// allocating the previous session's messages just to recover one timestamp).
// Caller must hold s.mu.
func (s *Store) recoverCreated(id, path, workingDir string) time.Time {
	created := time.Now().UTC()
	if cached, ok := s.createdCache[id]; ok {
		return cached
	}
	found := false
	if idx := s.readIndex(workingDir); idx != nil {
		for _, e := range idx.Entries {
			if e.ID == id && !e.Created.IsZero() {
				created = e.Created
				found = true
				break
			}
		}
	}
	if !found {
		if data, err := ioutil.ReadFileRetry(path); err == nil {
			var prevMeta struct {
				Created time.Time `json:"created"`
			}
			if err := json.Unmarshal(data, &prevMeta); err == nil && !prevMeta.Created.IsZero() {
				created = prevMeta.Created
			}
		}
	}
	return created
}

// TouchSession updates only the session's timestamp metadata without
// rewriting the full message payload. Uses file mtime plus the index file.
func (s *Store) TouchSession(workingDir, id string) error {
	if !s.enabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return err
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := os.Chtimes(path, now, now); err != nil {
		return err
	}
	return s.touchIndex(workingDir, id, now)
}

// UpdatedAt returns the session's persisted Updated timestamp for workingDir:
// the metadata index entry when present, falling back to the session file's
// updated field (index-loss recovery). Returns the zero time when the session
// has no persisted state in that directory (a never-saved /new pane).
func (s *Store) UpdatedAt(workingDir, id string) time.Time {
	if !s.enabled || id == "" || workingDir == "" {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return time.Time{}
	}
	if idx := s.readIndex(workingDir); idx != nil {
		for _, e := range idx.Entries {
			if e.ID == id {
				return e.Updated
			}
		}
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		return time.Time{}
	}
	if data, err := ioutil.ReadFileRetry(path); err == nil {
		var f file
		if json.Unmarshal(data, &f) == nil {
			// Delta-aware fallback (see sessionUpdatedAt): a delta written
			// after the last full save means the file field is stale.
			return s.sessionUpdatedAt(workingDir, id, f.Updated)
		}
	}
	return time.Time{}
}

// SetUpdatedAt rewrites the session's Updated timestamp in the metadata index
// (index file only — the session file's updated field is left as-is, the same
// accepted divergence AppendMessages/TouchSession have). Used to preserve a
// session's recency ranking across a working-directory change, where the
// relocation flush would otherwise stamp Updated=now on a session the user
// did not touch. No-op when the session has no index entry in workingDir.
func (s *Store) SetUpdatedAt(workingDir, id string, updated time.Time) error {
	if !s.enabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return err
	}
	return s.touchIndex(workingDir, id, updated)
}

// LoadInWorkingDir loads a session from a working directory.
//
// The main file's JSON decode runs OUTSIDE the store-wide mutex: a first
// critical section validates, resolves the paths, and captures the raw file
// state (main bytes, delta bytes, the delta's mtime/existence); the
// potentially multi-MB decode then happens lock-free; a second critical
// section merges the captured delta (same decisions and cleanups as before)
// and publishes the Created cache entry. Holding s.mu across the whole
// decode blocked List/Info/LatestID/Save for every session — the web sidebar
// is served synchronously on the WS read loop, and attach/resume loads run
// there too. The captured (bytes, mtime) pair is consistent: the merge sees
// exactly the file state read under the lock.
func (s *Store) LoadInWorkingDir(workingDir, id string) (SessionSnapshot, error) {
	if !s.enabled {
		return SessionSnapshot{}, fmt.Errorf("session persistence disabled")
	}
	// First critical section: validate, resolve paths, capture raw file
	// state. The delta is read here too so the main/delta pair is captured
	// consistently and the merge's mtime comparison sees the same file.
	s.mu.Lock()
	if err := validateSessionID(id); err != nil {
		s.mu.Unlock()
		return SessionSnapshot{}, err
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		s.mu.Unlock()
		return SessionSnapshot{}, err
	}
	data, err := ioutil.ReadFileRetry(path)
	if err != nil {
		s.mu.Unlock()
		return SessionSnapshot{}, err
	}
	var deltaData []byte
	var deltaMod time.Time
	deltaFound := false
	if fi, statErr := os.Stat(s.deltaPath(workingDir, id)); statErr == nil {
		deltaFound = true
		deltaMod = fi.ModTime().UTC()
		// ReadFileRetry: this read must not lose a Windows replace window —
		// a transient sharing violation here reads as "no delta" and
		// silently drops the messages that exist only in the delta file.
		if raw, readErr := ioutil.ReadFileRetry(s.deltaPath(workingDir, id)); readErr == nil {
			deltaData = raw
		}
	}
	s.mu.Unlock()

	// Lock-free: decode the session file (the dominant cost on large
	// sessions). Test seam for the lock contract, mirroring savePayloadHook.
	if loadDecodeHook != nil {
		loadDecodeHook()
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return SessionSnapshot{}, err
	}
	// Identity check: a renamed/copied file must not be loaded under a
	// different id (the copied index.json case that validateSessionID now
	// rejects is also caught here). Legacy files without an id field load
	// under the requested id, as before.
	if f.ID != "" && f.ID != id {
		return SessionSnapshot{}, fmt.Errorf("session file %s contains id %q", id, f.ID)
	}

	// Second critical section: merge the captured delta and publish the
	// Created cache entry, matching a single-lock load exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	if !f.Created.IsZero() {
		s.setCreatedCache(id, f.Created)
	}

	// Merge any delta file (messages appended since the last full snapshot).
	// The delta is deliberately NOT removed after a successful merge: it
	// stays on disk until the next full save replaces it, so a crash or an
	// early exit after restore cannot lose messages that only exist in the
	// delta. The baseCount check below also detects stale deltas (the
	// snapshot already contains them) and truncated sessions (the delta's
	// messages were deliberately dropped).
	if deltaData != nil {
		snap, deltaErr := decodeDeltaFile(deltaData)
		if deltaErr == nil && snap.Messages != nil {
			// mergeFrom is the index into snap.Messages where the merge starts:
			// 0 merges the whole delta, k>0 merges only the delta tail (the
			// snapshot already absorbed the delta's first k messages), and -1
			// drops the delta entirely.
			mergeFrom := -1
			switch {
			case snap.BaseCount == 0:
				// Legacy delta written before baseCount was recorded. Merging
				// unconditionally (the historic behavior) double-appends when a
				// later full save already included the delta's messages — the
				// crash window Save has between the snapshot write and the delta
				// unlink, which baseCount deltas survive but legacy deltas do
				// not. The snapshot's Updated timestamp versus the delta file's
				// mtime disambiguates: a snapshot saved AFTER the delta was
				// written already contains its messages. When the timestamps
				// are ambiguous (coarse filesystems), prefer merging — the
				// historic behavior — rather than risk dropping real messages.
				if deltaFound && f.Updated.After(deltaMod) {
					_ = s.clearDeltaFile(workingDir, id)
				} else {
					mergeFrom = 0
				}
			case len(f.Messages) == snap.BaseCount:
				// Snapshot is exactly the base the delta was written against:
				// the delta extends it.
				mergeFrom = 0
			case len(f.Messages) >= snap.BaseCount+len(snap.Messages):
				// Snapshot already contains the delta's messages (a full save
				// wrote the snapshot but crashed before removing the delta).
				_ = s.clearDeltaFile(workingDir, id)
			case len(f.Messages) > snap.BaseCount:
				// Snapshot is LONGER than the delta's base but shorter than
				// base+delta. This is the two-writer race shape: a concurrent
				// full save (another process, or an attach whose restore flush
				// landed while the registered agent still appends deltas against
				// its older base) absorbed a PREFIX of the delta into the
				// snapshot. When the snapshot's extra messages are exactly the
				// delta's leading messages, the delta's TAIL is real history
				// that exists nowhere else — merge just the tail instead of
				// dropping the delta (which previously lost those messages). A
				// content mismatch means the snapshot diverged (another writer
				// truncated/rolled back) — keep the snapshot and drop the delta,
				// the same conservative choice as the truncation case below.
				overlap := len(f.Messages) - snap.BaseCount
				if deltaPrefixMatches(f.Messages[snap.BaseCount:], snap.Messages[:overlap]) {
					mergeFrom = overlap
				} else {
					_ = s.clearDeltaFile(workingDir, id)
				}
			default:
				// Snapshot is shorter than the delta's base: the conversation
				// was truncated (compaction/rollback) and re-saved, so the
				// delta's messages were deliberately dropped.
				_ = s.clearDeltaFile(workingDir, id)
			}
			if mergeFrom >= 0 {
				if mergeFrom > 0 {
					// The snapshot already contains the delta's first
					// mergeFrom messages; append only the tail.
					f.Messages = append(f.Messages, snap.Messages[mergeFrom:]...)
					if len(snap.TokenCounts) > 0 {
						f.TokenCounts = append(f.TokenCounts, snap.TokenCounts[mergeFrom:]...)
					}
				} else {
					f.Messages = append(f.Messages, snap.Messages...)
					if len(snap.TokenCounts) > 0 {
						f.TokenCounts = append(f.TokenCounts, snap.TokenCounts...)
					}
				}
			}
		}
	}

	return SessionSnapshot{
		WorkingDir:      f.WorkingDir,
		Model:           f.Model,
		ModelProvider:   f.ModelProvider,
		Mode:            f.Mode,
		ThinkingLevel:   f.ThinkingLevel,
		Oneshot:         f.Oneshot,
		Label:           f.Label,
		LabelRenamed:    f.LabelRenamed,
		ProjectProfile:  f.ProjectProfile,
		Todos:           f.Todos,
		Messages:        f.Messages,
		TokenCounts:     f.TokenCounts,
		ContextLimit:    f.ContextLimit,
		ParentID:        f.ParentID,
		SubagentStatus:  f.SubagentStatus,
		SubagentSummary: f.SubagentSummary,
	}, nil
}

// AppendMessages writes new messages to the delta file for incremental saves.
// The delta file is a lightweight JSON file that holds messages appended since
// the last full snapshot. On restore, the delta is merged into the full snapshot.
// This avoids rewriting the entire (potentially large) message list on every save.
// totalMsgCount is the agent's total message count at save time; because the
// delta is cumulative, it is the correct index count between full snapshots.
// BaseCount (totalMsgCount minus the delta length) records the message count
// of the snapshot the delta extends, letting loads detect stale deltas.
func (s *Store) AppendMessages(id string, snap SessionSnapshot, totalMsgCount int) error {
	if !s.enabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return err
	}
	// A delta without its base snapshot is unloadable: LoadInWorkingDir
	// fails on the missing main file before it ever looks at the delta, so
	// writing one would only leave an orphan sidecar behind (the accepted
	// window after a delete/eviction, where a late turn flush races the
	// file removal). No-op — including the directory creation — when the
	// session file is gone.
	mainPath := s.path(snap.WorkingDir, id)
	if _, err := os.Stat(mainPath); os.IsNotExist(err) {
		return nil
	}
	dir := s.dir(snap.WorkingDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := s.deltaPath(snap.WorkingDir, id)
	if err := ensureUnderSessionsDir(snap.WorkingDir, path, s.globalDir); err != nil {
		return err
	}

	df := deltaFile{
		Messages:    snap.Messages,
		TokenCounts: snap.TokenCounts,
	}
	df.BaseCount = totalMsgCount - len(snap.Messages)
	if df.BaseCount < 0 {
		df.BaseCount = 0
	}
	data, err := json.Marshal(df)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFileAtomicNoSync(path, data, 0o600); err != nil {
		return err
	}

	// Update the main file's mtime so the session appears recent in listings.
	_ = os.Chtimes(mainPath, time.Now(), time.Now())

	// Update the index so the session appears recent in listings. The delta
	// file always holds every message since the last full snapshot, so the
	// agent's total count is authoritative. Label and oneshot flags are
	// preserved — appends do not change them.
	s.updateIndexCount(snap.WorkingDir, sessionIndexEntry{ID: id, Updated: time.Now().UTC(), MessageCount: totalMsgCount})
	return nil
}

// Delete removes a saved session file, its delta, archive, index entry,
// and any nested (subagent) children. A session that exists only in memory
// — a /new pane that was never used is not persisted on quit (see Save's
// empty-session skip) — can still be deleted (sidebar ✕ / resume del);
// deleting it is a success, not a "session not found" error, so a missing
// file is tolerated and any leftover delta/index/archive state is cleaned
// up (a stale Created cache entry would otherwise be re-used by a later
// Save of a new session with the same id).
func (s *Store) Delete(workingDir, id string) error {
	if !s.enabled {
		return fmt.Errorf("session persistence disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return err
	}
	if err := s.deleteSessionFile(workingDir, id); err != nil {
		return err
	}
	// Cascade: deleting a parent removes its nested (subagent) children
	// (D2). Children are not in the flat list, so this is the only way they
	// are ever deleted as a group.
	s.deleteNestedChildren(workingDir, id)
	return nil
}

// maxNestedPerParent is the transcript cap for nested (subagent) sessions
// per parent (D2): children are exempt from the global session retention
// counts but capped here, oldest first.
const maxNestedPerParent = 10

// nestedChildrenLocked returns the ids of saved sessions whose ParentID is
// parentID, most-recently-updated first. Callers must hold s.mu. Falls back
// to scanning session files when no index exists.
func (s *Store) nestedChildrenLocked(workingDir, parentID string) []string {
	idx := s.readIndex(workingDir)
	if idx != nil {
		// The index keeps entries in append order (updateIndex replaces in
		// place), so a higher position means "saved later" — the tie-break
		// the coarse clock needs (see the sort below).
		pos := make(map[string]int, len(idx.Entries))
		for i, e := range idx.Entries {
			pos[e.ID] = i
		}
		var entries []sessionIndexEntry
		for _, e := range idx.Entries {
			if e.ParentID == parentID {
				entries = append(entries, e)
			}
		}
		// Most-recently-updated first, with the per-parent cap pruning the
		// tail. Ties break on the index's insertion order (later position =
		// saved later = newer): time.Now has ~15.6 ms granularity on Windows
		// (the coarse KUSER_SHARED_DATA tick), so siblings saved in one tick
		// compare EQUAL, and the sort — unstable, with Compare returning 0 —
		// then left them in an arbitrary order, which pruned 'child-c' while
		// keeping the older 'child-b' (TestNestedPerParentCap on Windows CI).
		slices.SortFunc(entries, func(a, b sessionIndexEntry) int {
			if c := b.Updated.Compare(a.Updated); c != 0 {
				return c
			}
			return pos[b.ID] - pos[a.ID]
		})
		ids := make([]string, len(entries))
		for i, e := range entries {
			ids[i] = e.ID
		}
		return ids
	}
	names, err := s.sessionFiles(workingDir)
	if err != nil {
		return nil
	}
	type childMeta struct {
		id      string
		updated time.Time
	}
	var metas []childMeta
	var ids []string
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		data, err := ioutil.ReadFileRetry(s.path(workingDir, id))
		if err != nil {
			continue
		}
		var meta struct {
			ParentID string    `json:"parentID"`
			Updated  time.Time `json:"updated"`
		}
		if err := json.Unmarshal(data, &meta); err != nil || meta.ParentID != parentID {
			continue
		}
		metas = append(metas, childMeta{id: id, updated: meta.Updated})
	}
	// Mirror the index path's ordering: most-recently-updated first, so the
	// per-parent cap prunes the OLDEST children (name order from ReadDir is
	// id-sorted, which would prune the wrong siblings). No save order exists
	// in this layout, so equal timestamps (the coarse Windows clock) break on
	// the id instead: arbitrary but deterministic.
	slices.SortFunc(metas, func(a, b childMeta) int {
		if c := b.updated.Compare(a.updated); c != 0 {
			return c
		}
		return strings.Compare(b.id, a.id)
	})
	for _, meta := range metas {
		ids = append(ids, meta.id)
	}
	return ids
}

// deleteNestedChildren removes every saved session whose ParentID is id —
// recursively, so grandchildren (subagent_max_depth >= 2) are not orphaned
// when their parent child is deleted. Callers must hold s.mu (Delete does).
func (s *Store) deleteNestedChildren(workingDir, parentID string) {
	s.deleteNestedChildrenRec(workingDir, parentID, map[string]bool{})
}

func (s *Store) deleteNestedChildrenRec(workingDir, parentID string, visited map[string]bool) {
	for _, id := range s.nestedChildrenLocked(workingDir, parentID) {
		if visited[id] {
			continue // corrupt ParentID cycle — never loop
		}
		visited[id] = true
		if err := s.deleteSessionFile(workingDir, id); err != nil {
			log.Printf("warning: failed to delete nested session %s: %v", id, err)
		}
		s.deleteNestedChildrenRec(workingDir, id, visited)
	}
}

// pruneNestedSiblings keeps at most maxNestedPerParent children for a
// parent, deleting the oldest siblings beyond the cap. Called at child save
// time (Save holds s.mu); the just-saved child (keepID) is always kept.
func (s *Store) pruneNestedSiblings(workingDir, keepID, parentID string) {
	siblings := s.nestedChildrenLocked(workingDir, parentID)
	var victims []string
	for _, id := range siblings {
		if id == keepID {
			continue
		}
		victims = append(victims, id)
	}
	// Index order is most-recently-updated first; the oldest are the tail.
	for len(victims) >= maxNestedPerParent {
		victim := victims[len(victims)-1]
		victims = victims[:len(victims)-1]
		if err := s.deleteSessionFile(workingDir, victim); err != nil {
			log.Printf("warning: failed to prune nested session %s: %v", victim, err)
		}
	}
}

// deleteSessionFile removes one session's file, delta, archive file, spill
// dir, index entry, created-cache entry, and invalidates the list cache. A
// missing session file is not an error (the session may exist only in
// memory or have been deleted already); any other removal error aborts
// before the cleanup runs. Callers must hold s.mu.
func (s *Store) deleteSessionFile(workingDir, id string) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		return err
	}
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s.removeSessionSidecars(workingDir, id)
	delete(s.createdCache, id)
	s.removeFromIndex(workingDir, id)
	s.invalidateListCache(workingDir)
	return nil
}

// removeSessionSidecars removes the per-session on-disk state that lives
// alongside a session's snapshot file: its delta, its archive sidecar, and
// its spill dir (oversized tool output). The snapshot file itself is not
// touched. Extracted so the two destroy paths — deleteSessionFile (user
// delete / nested cascade / sibling cap) and prune's batch deletion
// (count/age retention) — cannot drift on the sidecar list; prune used to
// inline this cleanup and silently orphaned pruned sessions' spill trees.
// Best-effort: removal failures are logged, never returned — a leftover
// sidecar must not fail the session's destruction. Callers must hold s.mu.
func (s *Store) removeSessionSidecars(workingDir, id string) {
	if cerr := s.clearDeltaFile(workingDir, id); cerr != nil && !os.IsNotExist(cerr) {
		log.Printf("warning: failed to remove delta for session %s: %v", id, cerr)
	}
	s.removeArchiveFile(workingDir, id)
	// Spill dir (oversized tool output, .gogen/spill/session-<id>): per-
	// session auxiliary state like the archive sidecar, removed with the
	// session wherever this helper runs — user delete, prune, nested
	// cascade. Best-effort: a failed removal must not fail the delete.
	if serr := spill.RemoveSessionDir(workingDir, id); serr != nil {
		log.Printf("warning: failed to remove spill dir for session %s: %v", id, serr)
	}
}
