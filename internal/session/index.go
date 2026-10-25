package session

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"gogen/internal/ioutil"
	"gogen/internal/llm"
)

// sessionIndexEntry is a lightweight entry in the session index file.
type sessionIndexEntry struct {
	ID           string    `json:"id"`
	Created      time.Time `json:"created,omitempty"`
	Updated      time.Time `json:"updated"`
	Oneshot      bool      `json:"oneshot,omitempty"`
	MessageCount int       `json:"messageCount"`
	Label        string    `json:"label,omitempty"`
	// LabelRenamed mirrors the session file's marker (see file.LabelRenamed)
	// so List can keep a deliberate rename authoritative without re-reading
	// the session file.
	LabelRenamed bool `json:"labelRenamed,omitempty"`
	// ParentID marks nested (subagent) sessions so the flat list can
	// exclude them without re-reading every session file.
	ParentID string `json:"parentID,omitempty"`
	// SubagentStatus/SubagentSummary mirror the session file's recorded
	// subagent outcome ("" / "success" / "failed") so the sessions payload
	// can render it without re-reading the session file.
	SubagentStatus  string `json:"subagentStatus,omitempty"`
	SubagentSummary string `json:"subagentSummary,omitempty"`
}

// sessionIndex is the on-disk index of session metadata for fast listing.
type sessionIndex struct {
	Entries []sessionIndexEntry `json:"entries"`
}

// Store persists sessions under .gogen/sessions/.
//
// All public operations (Save, AppendMessages, LoadInWorkingDir, Delete,
// TouchSession, LatestID, List, Prune) are serialized internally by mu so
// multiple goroutines (multi-session web server, TUI persist goroutine) can
// write concurrently without corrupting index.json or createdCache. List may
// be called concurrently and is safe under mu; the in-memory list cache keeps
// its own lock so hot reads do not serialize on the store mutex.

// listCacheEntry holds a cached session list for one working directory.
type listCacheEntry struct {
	info []SessionInfo
	time time.Time
}

var (
	listCache   = make(map[string]listCacheEntry) // workingDir → cache
	listCacheMu sync.RWMutex
)

// listCacheSweepThreshold is the entry count above which cacheList sweeps
// expired entries. The cache is keyed by effective store directory and every
// entry is useless once its TTL has passed, so on a long-running process that
// changes working directories (the web working-dir switch) the map would
// otherwise retain one full session listing per directory for the life of the
// process. The sweep is deliberately amortized (only past the threshold), so
// the common single-directory case never pays for it.
const listCacheSweepThreshold = 64

// cacheList stores a freshly built listing for cacheKey and, past the
// threshold, drops entries that are already past the TTL. Expired entries can
// never be served (the read path requires age < 1s), so removing them is
// transparent — it only bounds the map.
func cacheList(cacheKey string, info []SessionInfo) {
	now := time.Now()
	listCacheMu.Lock()
	if len(listCache) >= listCacheSweepThreshold {
		for k, ce := range listCache {
			if now.Sub(ce.time) >= time.Second {
				delete(listCache, k)
			}
		}
	}
	listCache[cacheKey] = listCacheEntry{info: info, time: now}
	listCacheMu.Unlock()
}

// sessionFiles returns the names of the .json session files in the store
// directory for workingDir (excluding index.json). A missing directory
// surfaces as an *os.PathError from os.ReadDir (os.IsNotExist).
func (s *Store) sessionFiles(workingDir string) ([]string, error) {
	entries, err := os.ReadDir(s.dir(workingDir))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// Skip the index file if somehow named .json
		if e.Name() == "index.json" {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// legacySession is one entry from a legacy (no-index) session directory scan.
type legacySession struct {
	id       string
	updated  time.Time
	parentID string // non-empty for nested (subagent) sessions
}

// legacySessionUpdated scans the session files in workingDir's store
// directory (legacy layout without an index) and returns each session id
// with its delta-aware updated timestamp. Sessions whose updated timestamp
// cannot be decoded are omitted. A missing directory surfaces as an
// *os.PathError (os.IsNotExist). Shared by LatestID and prune, whose legacy
// fallbacks need exactly this data.
func (s *Store) legacySessionUpdated(workingDir string) ([]legacySession, error) {
	names, err := s.sessionFiles(workingDir)
	if err != nil {
		return nil, err
	}
	out := make([]legacySession, 0, len(names))
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		data, err := ioutil.ReadFileRetry(s.path(workingDir, id))
		if err != nil {
			continue
		}
		var meta struct {
			Updated  time.Time `json:"updated"`
			ParentID string    `json:"parentID"`
		}
		if err := json.Unmarshal(data, &meta); err != nil || meta.Updated.IsZero() {
			continue
		}
		// Delta-aware timestamp (see sessionUpdatedAt): delta-only updates
		// would otherwise under-rank next to full-save timestamps.
		out = append(out, legacySession{id: id, updated: s.sessionUpdatedAt(workingDir, id, meta.Updated), parentID: meta.ParentID})
	}
	return out, nil
}

// List returns session info for a working directory, ordered by most recently
// updated first. Uses the metadata index when available, falling back to a
// full-file scan for legacy directories. Results are cached briefly in memory
// to avoid repeated disk I/O on reconnects.
func (s *Store) List(workingDir string) ([]SessionInfo, error) {
	if !s.enabled {
		return nil, nil
	}
	// The cache is keyed by the EFFECTIVE store directory, not the working
	// dir: in global mode (SetGlobalDir) every working dir shares one session
	// dir, so keying by workingDir would both duplicate entries and — worse —
	// let a Save/Delete for one working dir leave another working dir's cached
	// listing of the same sessions stale for the TTL window.
	cacheKey := s.dir(workingDir)

	// Check in-memory cache first (1-second TTL).
	listCacheMu.RLock()
	if ce, ok := listCache[cacheKey]; ok && time.Since(ce.time) < time.Second {
		out := make([]SessionInfo, len(ce.info))
		copy(out, ce.info)
		listCacheMu.RUnlock()
		return out, nil
	}
	listCacheMu.RUnlock()

	// The index read/scan/write below must be serialized with Save/Delete/
	// Prune (read-modify-write of index.json). The list cache itself was
	// checked above without the store mutex — concurrent List calls stay
	// parallel on the hot cache path.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Try the metadata index file.
	idx := s.readIndex(workingDir)
	if idx != nil && len(idx.Entries) > 0 {
		// Migration: older sessions may still have a truncated Label (50-char
		// limit from before CSS handled dynamic truncation). Load the session
		// file and refresh with the full untruncated label. Only entries whose
		// stored label is exactly the legacy truncation length are ambiguous,
		// so this is O(1) for the common case instead of re-reading every
		// session file on every list.
		needsRewrite := false
		for i, e := range idx.Entries {
			// Deliberate renames (marker persisted by Save) are always
			// authoritative: never regenerate them from the conversation,
			// even when they are exactly the legacy truncation length.
			if e.LabelRenamed {
				continue
			}
			if len(e.Label) == legacyLabelMaxLen {
				// Only a stored label that is a PREFIX of the derived text
				// is the legacy-truncation shape that must migrate.
				// Anything else at this length is a deliberate name (e.g. a
				// rename from before the marker existed) and must be kept —
				// mirrors sessionLabel's migration rule.
				if raw := s.loadRawLabel(workingDir, e.ID); raw != "" && raw != e.Label && strings.HasPrefix(raw, e.Label) {
					idx.Entries[i].Label = raw
					needsRewrite = true
				}
			}
		}
		if needsRewrite {
			_ = s.writeIndex(workingDir, idx)
		}
		slices.SortFunc(idx.Entries, func(a, b sessionIndexEntry) int { return b.Updated.Compare(a.Updated) })
		out := make([]SessionInfo, len(idx.Entries))
		for i, e := range idx.Entries {
			out[i] = SessionInfo{
				ID:              e.ID,
				Oneshot:         e.Oneshot,
				UpdatedAt:       e.Updated.UTC().Format(time.RFC3339Nano),
				MessageCount:    e.MessageCount,
				Label:           e.Label,
				ParentID:        e.ParentID,
				SubagentStatus:  e.SubagentStatus,
				SubagentSummary: e.SubagentSummary,
			}
		}
		cacheList(cacheKey, out)
		return out, nil
	}

	// Fallback: legacy full-file scan (no index file).
	idx = &sessionIndex{} // collect entries to build the index
	names, err := s.sessionFiles(workingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type item struct {
		info    SessionInfo
		updated time.Time
	}
	var items []item
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		data, err := ioutil.ReadFileRetry(s.path(workingDir, id))
		if err != nil {
			continue
		}
		var f file
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		// Use the delta-aware effective timestamp (AppendMessages bumps only
		// the index and mtimes, not the file's updated field).
		updated := s.sessionUpdatedAt(workingDir, id, f.Updated)
		lbl := sessionLabel(f.Messages, f.Label, f.LabelRenamed)
		items = append(items, item{
			info: SessionInfo{
				ID:              id,
				Oneshot:         f.Oneshot,
				UpdatedAt:       updated.UTC().Format(time.RFC3339Nano),
				MessageCount:    len(f.Messages),
				Label:           lbl,
				ParentID:        f.ParentID,
				SubagentStatus:  f.SubagentStatus,
				SubagentSummary: f.SubagentSummary,
			},
			updated: updated,
		})
		idx.Entries = append(idx.Entries, sessionIndexEntry{
			ID: id, Created: f.Created, Updated: updated, Oneshot: f.Oneshot, MessageCount: len(f.Messages), Label: lbl, LabelRenamed: f.LabelRenamed, ParentID: f.ParentID,
			SubagentStatus: f.SubagentStatus, SubagentSummary: f.SubagentSummary,
		})
	}
	// Persist the index for next time (best-effort).
	if len(idx.Entries) > 0 {
		slices.SortFunc(idx.Entries, func(a, b sessionIndexEntry) int { return b.Updated.Compare(a.Updated) })
		_ = s.writeIndex(workingDir, idx)
	}
	slices.SortFunc(items, func(a, b item) int { return b.updated.Compare(a.updated) })
	out := make([]SessionInfo, len(items))
	for i, it := range items {
		out[i] = it.info
	}

	// Populate in-memory cache.
	cacheList(cacheKey, out)

	return out, nil
}

// LatestID returns the most recently updated session id.
// Uses the Updated field in each session JSON (not file mtime), so copied or
// restored files cannot displace the true latest. Only the updated timestamp
// is decoded — messages and other fields are skipped for a cheap scan.
// Nested (subagent) sessions are excluded: they are not part of the flat
// session list, so "resume latest" / the restart bootstrap must never land
// on a child (D2 — children are reachable only through their parent).
func (s *Store) LatestID(workingDir string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Fast path: use the metadata index file (avoids reading every session file).
	idx := s.readIndex(workingDir)
	if idx != nil {
		if id := latestNonNested(idx.Entries, func(e sessionIndexEntry) (string, string, time.Time) {
			return e.ID, e.ParentID, e.Updated
		}); id != "" {
			return id, nil
		}
	}

	// Fallback: scan all session files (legacy directories without index).
	legacy, err := s.legacySessionUpdated(workingDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Already tried index above; nothing more to do.
			return "", nil
		}
		return "", err
	}
	return latestNonNested(legacy, func(l legacySession) (string, string, time.Time) {
		return l.id, l.parentID, l.updated
	}), nil
}

// latestNonNested returns the id of the most recently updated entry whose
// parentID is empty, or "" when the list is empty or every entry is nested.
// Nested (subagent) sessions are excluded because they are not part of the
// flat session list (see LatestID). Ties keep the first entry seen: only a
// strictly later timestamp displaces the current pick.
func latestNonNested[T any](items []T, meta func(T) (id, parentID string, updated time.Time)) string {
	var latestID string
	var latestUpdated time.Time
	for _, item := range items {
		id, parentID, updated := meta(item)
		if parentID != "" {
			continue
		}
		if updated.After(latestUpdated) {
			latestID, latestUpdated = id, updated
		}
	}
	return latestID
}

// indexFile returns the path to the metadata index for a working directory.
func (s *Store) indexFile(workingDir string) string {
	return filepath.Join(s.dir(workingDir), "index.json")
}

// readIndex loads the session metadata index from disk. Returns nil if the
// file does not exist or is corrupt.
func (s *Store) readIndex(workingDir string) *sessionIndex {
	data, err := ioutil.ReadFileRetry(s.indexFile(workingDir))
	if err != nil {
		return nil
	}
	var idx sessionIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		log.Printf("warning: corrupt session index, rebuilding: %v", err)
		return nil
	}
	return &idx
}

// Info returns the persisted metadata for one session, or nil when the
// session has no index entry (or no index exists). Reads only the metadata
// index — no session file or message payload is touched. Used by the web
// server's delete path to discover a deleted session's parent link without
// loading its transcript.
func (s *Store) Info(workingDir, id string) *SessionInfo {
	if !s.enabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.readIndex(workingDir)
	if idx == nil {
		return nil
	}
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.ID != id {
			continue
		}
		return &SessionInfo{
			ID:              e.ID,
			Oneshot:         e.Oneshot,
			UpdatedAt:       e.Updated.UTC().Format(time.RFC3339Nano),
			MessageCount:    e.MessageCount,
			Label:           e.Label,
			ParentID:        e.ParentID,
			SubagentStatus:  e.SubagentStatus,
			SubagentSummary: e.SubagentSummary,
		}
	}
	return nil
}

// loadRawLabel loads a session file and returns the full first user message.
func (s *Store) loadRawLabel(workingDir, id string) string {
	data, err := ioutil.ReadFileRetry(s.path(workingDir, id))
	if err != nil {
		return ""
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	if len(f.Messages) == 0 {
		return ""
	}
	return llm.SessionLabel(f.Messages)
}

// writeIndex writes the session metadata index atomically.
func (s *Store) writeIndex(workingDir string, idx *sessionIndex) error {
	if idx == nil {
		return nil
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(workingDir), 0o700); err != nil {
		return err
	}
	indexPath := s.indexFile(workingDir)
	if err := ensureUnderSessionsDir(workingDir, indexPath, s.globalDir); err != nil {
		return err
	}
	return ioutil.WriteFileAtomicNoSync(indexPath, data, 0o600)
}

// mutateIndex reads the session metadata index, applies mutate, and writes it
// back when mutate reports a change. When createIfMissing is false and no
// index file exists, the mutation is a no-op; when true, a fresh empty index
// is passed in so callers can seed the first entry. Write errors are returned
// so callers that care (touchIndex) can propagate them; the other index
// mutators swallow them, matching their historical behavior. Callers must hold
// s.mu so the read-modify-write of index.json cannot interleave with another
// mutator.
func (s *Store) mutateIndex(workingDir string, createIfMissing bool, mutate func(idx *sessionIndex) bool) error {
	if !s.enabled {
		return nil
	}
	idx := s.readIndex(workingDir)
	if idx == nil {
		if !createIfMissing {
			return nil
		}
		idx = &sessionIndex{}
	}
	if !mutate(idx) {
		return nil
	}
	if err := s.writeIndex(workingDir, idx); err != nil {
		return err
	}
	s.invalidateListCache(workingDir)
	return nil
}

// updateIndex adds or updates an entry in the session metadata index.
// created is written alongside updated so a later cache-miss Save (e.g. after
// a process restart) can recover the immutable Created timestamp from the
// index instead of re-reading the session file. Existing entries keep their
// Created when created is zero (defensive: Save always passes non-zero).
// updateIndex upserts the full entry for a session in the metadata index.
// Every field of entry is written verbatim except Created: a zero Created on
// an existing entry means "keep the stored Created" (Save recovers it from
// the session file only when the index has no usable value).
func (s *Store) updateIndex(workingDir string, entry sessionIndexEntry) {
	s.mutateIndex(workingDir, true, func(idx *sessionIndex) bool {
		for i, e := range idx.Entries {
			if e.ID != entry.ID {
				continue
			}
			if entry.Created.IsZero() {
				entry.Created = e.Created
			}
			idx.Entries[i] = entry
			return true
		}
		idx.Entries = append(idx.Entries, entry)
		return true
	})
}

// touchIndex updates only the timestamp for a session in the index.
func (s *Store) touchIndex(workingDir, id string, updated time.Time) error {
	return s.mutateIndex(workingDir, false, func(idx *sessionIndex) bool {
		for i, e := range idx.Entries {
			if e.ID == id {
				idx.Entries[i].Updated = updated
				return true
			}
		}
		// Entry not in index; skip. The list fallback will re-scan on next miss.
		return false
	})
}

// updateIndexCount updates the timestamp and message count for a session in
// the metadata index, preserving the label and oneshot flags. Used by
// AppendMessages so listings stay accurate between full snapshots. Entries
// missing from the index are skipped — the list fallback re-scans the
// directory on the next miss.
func (s *Store) updateIndexCount(workingDir string, entry sessionIndexEntry) {
	s.mutateIndex(workingDir, false, func(idx *sessionIndex) bool {
		for i, e := range idx.Entries {
			if e.ID == entry.ID {
				idx.Entries[i].Updated = entry.Updated
				idx.Entries[i].MessageCount = entry.MessageCount
				return true
			}
		}
		return false
	})
}

// removeFromIndex deletes an entry from the session metadata index.
func (s *Store) removeFromIndex(workingDir, id string) {
	s.mutateIndex(workingDir, false, func(idx *sessionIndex) bool {
		filtered := idx.Entries[:0]
		for _, e := range idx.Entries {
			if e.ID != id {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) == len(idx.Entries) {
			return false // nothing removed
		}
		idx.Entries = filtered
		return true
	})
}

// removeFromIndexBatch removes multiple entries from the session metadata index
// in a single pass, rewriting the index file only once.
func (s *Store) removeFromIndexBatch(workingDir string, ids []string) {
	if len(ids) == 0 {
		return
	}
	s.mutateIndex(workingDir, false, func(idx *sessionIndex) bool {
		del := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			del[id] = struct{}{}
		}
		filtered := make([]sessionIndexEntry, 0, len(idx.Entries))
		for _, e := range idx.Entries {
			if _, ok := del[e.ID]; !ok {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) == len(idx.Entries) {
			return false
		}
		idx.Entries = filtered
		return true
	})
}

// invalidateListCache clears the in-memory list cache for a working directory.
func (s *Store) invalidateListCache(workingDir string) {
	// Keyed by the effective store dir (see List) so global-mode invalidations
	// hit every working dir that shares the store.
	listCacheMu.Lock()
	delete(listCache, s.dir(workingDir))
	listCacheMu.Unlock()
}

func sessionLabel(messages []llm.Message, stored string, renamed bool) string {
	// A deliberate rename is always authoritative: the user (or the
	// session_rename tool) chose it, so it must never be regenerated from
	// the conversation. This closes the legacy-migration hole where a
	// rename of exactly legacyLabelMaxLen characters that happens to be a
	// prefix of the derived first-user-message label would be silently
	// replaced by the longer derived label on the next full save.
	if renamed {
		return stored
	}
	derived := llm.SessionLabel(messages)
	// The stored label is authoritative UNLESS it is a legacy 50-char
	// truncation of the derived label. An older version truncated labels
	// when persisting, so a stored label that is exactly the legacy length
	// and a prefix of the full message is stale and must be migrated to the
	// untruncated text. Any other stored label is either already full or a
	// deliberate rename (RenameSession / the session_rename tool) and must
	// NOT be regenerated: the web sidebar shows the live (renamed) label
	// while the session is open, and would otherwise fall back to the first
	// user message the moment the session is closed (the saved-session row
	// reads this index entry).
	if stored != "" && (len(stored) != legacyLabelMaxLen || !strings.HasPrefix(derived, stored)) {
		return stored
	}
	return derived
}
