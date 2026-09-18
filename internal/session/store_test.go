package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gogen/internal/llm"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	snap := SessionSnapshot{
		WorkingDir: dir,
		Model:      "gpt-4o",
		Mode:       "plan",
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}
	id := "test-session"
	if err := store.Save(id, snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "hello" {
		t.Fatalf("messages=%+v", loaded.Messages)
	}
	if loaded.Mode != "plan" {
		t.Fatalf("mode=%q", loaded.Mode)
	}
}

// TestSaveLoadModelProviderRoundTrip pins the routing-key persistence: the
// provider profile owning the model survives a Save/Load round trip, and a
// legacy file without the field loads with an empty ModelProvider (the
// default-profile migration marker).
func TestSaveLoadModelProviderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	snap := SessionSnapshot{
		WorkingDir:    dir,
		Model:         "deepseek-v4-flash",
		ModelProvider: "deepseek",
		Messages:      []llm.Message{{Role: "user", Content: "hello"}},
	}
	id := "model-provider-session"
	if err := store.Save(id, snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model != "deepseek-v4-flash" || loaded.ModelProvider != "deepseek" {
		t.Fatalf("round trip: model=%q provider=%q, want deepseek-v4-flash/deepseek", loaded.Model, loaded.ModelProvider)
	}

	// Legacy file: no modelProvider field at all.
	legacy := filepath.Join(dir, ".gogen", "sessions", "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"version":1,"id":"legacy","created":"2026-01-01T00:00:00Z","updated":"2026-01-01T00:00:00Z","workingDir":"`+dir+`","model":"gpt-4o","mode":"act","messages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyLoaded, err := store.LoadInWorkingDir(dir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if legacyLoaded.Model != "gpt-4o" {
		t.Fatalf("legacy model=%q, want gpt-4o", legacyLoaded.Model)
	}
	if legacyLoaded.ModelProvider != "" {
		t.Fatalf("legacy ModelProvider=%q, want empty (default-profile migration marker)", legacyLoaded.ModelProvider)
	}
}

func TestLatestIDUsesUpdatedNotMtime(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})

	if err := store.Save("older", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "older"}},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := store.Save("newer", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "newer"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Touch the older file so mtime is newer than "newer".
	olderPath := filepath.Join(dir, ".gogen", "sessions", "older.json")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(olderPath, future, future); err != nil {
		t.Fatal(err)
	}

	got, err := store.LatestID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "newer" {
		t.Fatalf("LatestID=%q want %q (should use Updated, not mtime)", got, "newer")
	}
}

func TestGlobalModeListCacheCoherentAcrossWorkingDirs(t *testing.T) {
	// In global mode every working dir shares one session dir. The in-memory
	// list cache must be keyed by that shared dir, or a Delete issued via one
	// working dir would leave the other working dir's cached listing stale for
	// the TTL window (and a Save would appear "missing" from it).
	globalDir := t.TempDir()
	wdA := filepath.Join(t.TempDir(), "projA")
	wdB := filepath.Join(t.TempDir(), "projB")
	if err := os.MkdirAll(wdA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wdB, 0o755); err != nil {
		t.Fatal(err)
	}
	store := NewStoreWithOptions(true, StoreOptions{})
	store.SetGlobalDir(globalDir)

	id := "global-session"
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: wdA,
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}

	// List via B to populate the (shared) cache, then delete via B and make
	// sure a List via A does not resurrect the deleted session from the cache.
	if _, err := store.List(wdB); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(wdB, id); err != nil {
		t.Fatal(err)
	}
	listA, err := store.List(wdA)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range listA {
		if s.ID == id {
			t.Fatalf("List(%s) still returns deleted session %s from the cache", wdA, id)
		}
	}

	// And a Save issued via A must be visible to a List via B.
	id2 := "second-session"
	if err := store.Save(id2, SessionSnapshot{
		WorkingDir: wdB,
		Messages:   []llm.Message{{Role: "user", Content: "hi2"}},
	}); err != nil {
		t.Fatal(err)
	}
	listB, err := store.List(wdA)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range listB {
		if s.ID == id2 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("List(%s) does not show session %s saved via %s", wdA, id2, wdB)
	}
}

func TestSetCreatedCacheEvictsOldest(t *testing.T) {
	store := NewStoreWithOptions(true, StoreOptions{})
	base := time.Now().UTC()
	for i := 0; i < maxCreatedCacheEntries+10; i++ {
		store.setCreatedCache("id"+fmt.Sprintf("%d", i), base.Add(time.Duration(i)*time.Second))
	}
	if len(store.createdCache) != maxCreatedCacheEntries {
		t.Fatalf("cache size=%d want %d", len(store.createdCache), maxCreatedCacheEntries)
	}
	// The evicted entries must be the OLDEST ones (smallest timestamps).
	for i := 0; i < 10; i++ {
		if _, ok := store.createdCache["id"+fmt.Sprintf("%d", i)]; ok {
			t.Fatalf("expected oldest entry id%d to be evicted", i)
		}
	}
}

func TestSavePreservesCreatedOnCacheMiss(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "created-preserve"
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".gogen", "sessions", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var first file
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}
	if first.Created.IsZero() {
		t.Fatal("expected Created to be set")
	}

	// New store instance = empty createdCache (simulates process restart
	// without Load). Save must still keep the original Created.
	time.Sleep(5 * time.Millisecond)
	store2 := NewStoreWithOptions(true, StoreOptions{})
	if err := store2.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "hi again"}},
	}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var second file
	if err := json.Unmarshal(data, &second); err != nil {
		t.Fatal(err)
	}
	if !second.Created.Equal(first.Created) {
		t.Fatalf("Created reset on cache miss: first=%v second=%v", first.Created, second.Created)
	}
}

// TestAppendMessagesUpdatesIndexMessageCount verifies that incremental delta
// saves keep the session index's MessageCount accurate, so List does not
// report a stale count between full snapshots. The delta file is cumulative
// (all messages since the last full snapshot), so each AppendMessages carries
// the agent's total message count.
func TestAppendMessagesUpdatesIndexMessageCount(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "delta-count"
	base := []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	if err := store.Save(id, SessionSnapshot{WorkingDir: dir, Messages: base}); err != nil {
		t.Fatal(err)
	}

	// First incremental save: one new message (total 3).
	if err := store.AppendMessages(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "q2"}},
	}, 3); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].MessageCount != 3 {
		t.Fatalf("after first delta, want 3 messages, got %+v", sessions)
	}

	// Second incremental save: the delta is cumulative (both new messages,
	// matching how doPersist writes a.Messages[lastSavedMsgCount:]); total 4.
	if err := store.AppendMessages(id, SessionSnapshot{
		WorkingDir: dir,
		Messages: []llm.Message{
			{Role: "user", Content: "q2"},
			{Role: "assistant", Content: "a2"},
		},
	}, 4); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].MessageCount != 4 {
		t.Fatalf("after second delta, want 4 messages, got %+v", sessions)
	}

	// A full save refreshes the count from the snapshot itself.
	full := append(append([]llm.Message{}, base...),
		llm.Message{Role: "user", Content: "q2"},
		llm.Message{Role: "assistant", Content: "a2"},
	)
	if err := store.Save(id, SessionSnapshot{WorkingDir: dir, Messages: full}); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].MessageCount != 4 {
		t.Fatalf("after full save, want 4 messages, got %+v", sessions)
	}
}

// TestAppendMessagesMissingIndexEntry verifies a delta save for a session
// absent from the index is a no-op for the index (no entry created), keeping
// the legacy fallback scan as the source of truth.
func TestAppendMessagesMissingIndexEntry(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "no-index-entry"
	// Write a session file without touching the index (simulates a legacy
	// directory before the first full save indexed it).
	path := filepath.Join(dir, ".gogen", "sessions", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"id":"no-index-entry","messages":[{"role":"user","content":"hi"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "q2"}},
	}, 2); err != nil {
		t.Fatal(err)
	}
	// The index file must not be created by AppendMessages; List falls back
	// to the legacy scan.
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", "index.json")); !os.IsNotExist(err) {
		t.Fatalf("index should not be created by AppendMessages: %v", err)
	}
	sessions, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session from legacy scan, got %+v", sessions)
	}
}

// TestLoadInWorkingDirKeepsDeltaUntilFullSave verifies that loading a session
// does not delete its delta file: the merged state is only made durable by the
// next full save, so a crash or early exit between load and save cannot lose
// the delta messages. Regression: LoadInWorkingDir used to delete the delta
// immediately, so the tail of the history existed only in memory until the
// next persist.
func TestLoadInWorkingDirKeepsDeltaUntilFullSave(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "keep-delta"
	base := []llm.Message{{Role: "user", Content: "q1"}}
	if err := store.Save(id, SessionSnapshot{WorkingDir: dir, Messages: base}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "assistant", Content: "a1"}},
	}, 2); err != nil {
		t.Fatal(err)
	}

	// The delta records the snapshot message count it extends (baseCount).
	dpath := filepath.Join(dir, ".gogen", "sessions", id+".delta")
	data, err := os.ReadFile(dpath)
	if err != nil {
		t.Fatal(err)
	}
	var df deltaFile
	if err := json.Unmarshal(data, &df); err != nil {
		t.Fatal(err)
	}
	if df.BaseCount != 1 {
		t.Fatalf("expected baseCount 1, got %d", df.BaseCount)
	}

	loaded, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("expected 2 messages after merge, got %d", len(loaded.Messages))
	}
	// The delta must still exist after load.
	if _, err := os.Stat(dpath); err != nil {
		t.Fatalf("delta file should survive a load: %v", err)
	}
	// Loading again must be idempotent (no double-merge).
	again, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Messages) != 2 {
		t.Fatalf("second load must not double-merge: got %d messages", len(again.Messages))
	}
	// A full save supersedes the delta and removes it.
	full := []llm.Message{{Role: "user", Content: "q1"}, {Role: "assistant", Content: "a1"}}
	if err := store.Save(id, SessionSnapshot{WorkingDir: dir, Messages: full}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dpath); !os.IsNotExist(err) {
		t.Fatalf("full save should remove the delta, err=%v", err)
	}
	loaded, err = store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("expected 2 messages after full save, got %d", len(loaded.Messages))
	}
}

// TestLoadSkipsStaleDeltaWhenSnapshotAlreadyContainsIt simulates the crash
// window between a full snapshot write and the delta removal: the snapshot
// already contains the delta's messages, so loading must not merge them
// again (which would duplicate history).
func TestLoadSkipsStaleDeltaWhenSnapshotAlreadyContainsIt(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "stale-delta"
	base := []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	if err := store.Save(id, SessionSnapshot{WorkingDir: dir, Messages: base}); err != nil {
		t.Fatal(err)
	}
	delta := []llm.Message{
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	if err := store.AppendMessages(id, SessionSnapshot{WorkingDir: dir, Messages: delta}, 4); err != nil {
		t.Fatal(err)
	}
	// Full save: the snapshot now contains all 4 messages and Save removes
	// the delta. Then simulate the crash window by writing the delta back
	// (snapshot write succeeded, delta unlink never happened).
	full := append([]llm.Message{}, base...)
	full = append(full, delta...)
	if err := store.Save(id, SessionSnapshot{WorkingDir: dir, Messages: full}); err != nil {
		t.Fatal(err)
	}
	df := deltaFile{Messages: delta, BaseCount: 2}
	data, err := json.Marshal(df)
	if err != nil {
		t.Fatal(err)
	}
	dpath := filepath.Join(dir, ".gogen", "sessions", id+".delta")
	if err := os.WriteFile(dpath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 4 {
		t.Fatalf("expected 4 messages without duplication, got %d", len(loaded.Messages))
	}
	// The stale delta is removed so a later load cannot double-merge.
	if _, err := os.Stat(dpath); !os.IsNotExist(err) {
		t.Fatalf("stale delta should be removed on load, err=%v", err)
	}
}

// TestLoadDropsDeltaWhenSnapshotTruncated verifies that when the snapshot was
// rewritten with fewer messages than the delta's base (compaction or error
// rollback), the delta's messages are treated as deliberately dropped rather
// than merged back into the history.
func TestLoadDropsDeltaWhenSnapshotTruncated(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "truncated-delta"
	base := []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	if err := store.Save(id, SessionSnapshot{WorkingDir: dir, Messages: base}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(id, SessionSnapshot{
		WorkingDir: dir,
		Messages: []llm.Message{
			{Role: "user", Content: "q2"},
			{Role: "assistant", Content: "a2"},
		},
	}, 4); err != nil {
		t.Fatal(err)
	}
	// Truncated re-save (1 message) — simulates a compacted/rolled-back
	// session whose full save crashed before removing the delta.
	truncated := []llm.Message{{Role: "user", Content: "q1"}}
	if err := store.Save(id, SessionSnapshot{WorkingDir: dir, Messages: truncated}); err != nil {
		t.Fatal(err)
	}
	df := deltaFile{
		Messages: []llm.Message{
			{Role: "user", Content: "q2"},
			{Role: "assistant", Content: "a2"},
		},
		BaseCount: 2,
	}
	data, err := json.Marshal(df)
	if err != nil {
		t.Fatal(err)
	}
	dpath := filepath.Join(dir, ".gogen", "sessions", id+".delta")
	if err := os.WriteFile(dpath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 {
		t.Fatalf("expected 1 message (delta dropped), got %d", len(loaded.Messages))
	}
	if _, err := os.Stat(dpath); !os.IsNotExist(err) {
		t.Fatalf("stale delta should be removed on load, err=%v", err)
	}
}

// TestLegacyDeltaWithoutBaseCountIsMerged verifies backward compatibility:
// delta files written before the baseCount field (absent → 0) are merged
// unconditionally, matching the historic behavior.
func TestLegacyDeltaWithoutBaseCountIsMerged(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "legacy-delta"
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "q1"}},
	}); err != nil {
		t.Fatal(err)
	}
	dpath := filepath.Join(dir, ".gogen", "sessions", id+".delta")
	if err := os.WriteFile(dpath, []byte(`{"messages":[{"role":"assistant","content":"a1"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("expected 2 messages from legacy delta merge, got %d", len(loaded.Messages))
	}
}

// TestDeleteRemovesDelta verifies Delete cleans up both the snapshot file and
// any pending delta for the session.
func TestDeleteRemovesDelta(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	id := "delete-delta"
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "q1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "assistant", Content: "a1"}},
	}, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(dir, id); err != nil {
		t.Fatal(err)
	}
	sessDir := filepath.Join(dir, ".gogen", "sessions")
	if _, err := os.Stat(filepath.Join(sessDir, id+".json")); !os.IsNotExist(err) {
		t.Fatalf("expected session file removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(sessDir, id+".delta")); !os.IsNotExist(err) {
		t.Fatalf("expected delta file removed, err=%v", err)
	}
}

// TestReservedIndexIDRejected pins the reserved-name guard: "index" maps to
// the metadata index file (index.json), so every store operation must reject
// it (case-insensitively — "INDEX" collides on case-insensitive filesystems)
// instead of overwriting or deleting the index. Before the guard, Save
// clobbered index.json with the session payload and its index rewrite then
// replaced that payload, hiding every other session from listings.
func TestReservedIndexIDRejected(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	if err := store.Save("real", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "keep me"}},
	}); err != nil {
		t.Fatal(err)
	}
	indexPath := store.indexFile(dir)
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"index", "INDEX"} {
		if err := store.Save(id, SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: "overwrite attempt"}},
		}); err == nil {
			t.Fatalf("Save(%q) must fail (reserved id)", id)
		}
		if _, err := store.LoadInWorkingDir(dir, id); err == nil {
			t.Fatalf("LoadInWorkingDir(%q) must fail (reserved id)", id)
		}
		if err := store.Delete(dir, id); err == nil {
			t.Fatalf("Delete(%q) must fail (reserved id)", id)
		}
		if err := store.AppendMessages(id, SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: "delta attempt"}},
		}, 1); err == nil {
			t.Fatalf("AppendMessages(%q) must fail (reserved id)", id)
		}
	}

	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("index file must survive the reserved-id attempts: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("index.json was modified by a reserved-id operation:\nbefore %s\nafter %s", before, after)
	}
	if _, err := store.LoadInWorkingDir(dir, "real"); err != nil {
		t.Fatalf("real session no longer loads: %v", err)
	}
}

// TestSavePersistsTodosOnlySession pins the todos exception in Save's
// empty-session skip: the legacy-todos migration imports a project-level
// list into a fresh session before the first turn (renaming the source
// away), so a 0-message session carrying todos must be persisted —
// otherwise the import is lost on quit. An empty-todos session must still
// be skipped like every other anonymous empty session.
func TestSavePersistsTodosOnlySession(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})

	if err := store.Save("todos-only", SessionSnapshot{
		WorkingDir: dir,
		Todos: &TodoList{
			Items:  []TodoItem{{ID: 1, Text: "migrated", Status: "pending"}},
			NextID: 2,
		},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadInWorkingDir(dir, "todos-only")
	if err != nil {
		t.Fatalf("todos-only session must be persisted: %v", err)
	}
	if loaded.Todos == nil || len(loaded.Todos.Items) != 1 || loaded.Todos.Items[0].Text != "migrated" {
		t.Fatalf("todos=%+v, want the migrated item", loaded.Todos)
	}

	// Empty todos are still "no content".
	if err := store.Save("empty-todos", SessionSnapshot{
		WorkingDir: dir,
		Todos:      &TodoList{Items: []TodoItem{}, NextID: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadInWorkingDir(dir, "empty-todos"); err == nil {
		t.Fatal("empty-todos session should not be persisted")
	}
}

// TestLoadRejectsMismatchedFileID pins the loader's identity check: a
// session file copied/renamed to another id must not load as that id.
func TestLoadRejectsMismatchedFileID(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	if err := store.Save("orig", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "secret"}},
	}); err != nil {
		t.Fatal(err)
	}
	sessDir := filepath.Join(dir, ".gogen", "sessions")
	data, err := os.ReadFile(filepath.Join(sessDir, "orig.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "copy.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadInWorkingDir(dir, "copy"); err == nil {
		t.Fatal("LoadInWorkingDir(copy) must reject a file whose id is orig")
	}
}

// TestAppendMessagesWithoutSnapshotIsNoop pins the orphan-delta guard: a
// delta appended after the session file is gone (a late turn flush racing a
// delete/eviction) is unloadable — LoadInWorkingDir fails on the missing
// main file before it looks at the delta — so AppendMessages must not create
// one.
func TestAppendMessagesWithoutSnapshotIsNoop(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})

	if err := store.AppendMessages("gone", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "late"}},
	}, 1); err != nil {
		t.Fatalf("AppendMessages on a missing session: %v", err)
	}
	if _, err := os.Stat(store.deltaPath(dir, "gone")); !os.IsNotExist(err) {
		t.Fatalf("orphan delta was created (stat err=%v)", err)
	}
	if _, err := os.Stat(store.path(dir, "gone")); !os.IsNotExist(err) {
		t.Fatalf("session file should not be created by AppendMessages (stat err=%v)", err)
	}
}
