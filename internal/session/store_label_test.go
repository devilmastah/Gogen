package session

import (
	"os"
	"strings"
	"testing"

	"gogen/internal/llm"
)

func TestListIncludesLabelAndCount(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	snap := SessionSnapshot{
		WorkingDir: dir,
		Model:      "gpt-4o",
		Mode:       "act",
		Messages: []llm.Message{
			{Role: "user", Content: "implement session commands with labels"},
			{Role: "assistant", Content: "ok"},
		},
	}
	if err := store.Save("sess-1", snap); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sessions", len(list))
	}
	if list[0].MessageCount != 2 {
		t.Fatalf("count=%d", list[0].MessageCount)
	}
	if !strings.Contains(list[0].Label, "implement session") {
		t.Fatalf("label=%q", list[0].Label)
	}
}

// TestListKeepsRenamedLabel verifies the reported bug: a session renamed via
// RenameSession / the session_rename tool keeps its custom label in the
// listing. Pre-fix, sessionLabel always regenerated the index label from the
// messages, so the sidebar showed the rename while the session was open (the
// live pane label) but fell back to the first user message as soon as the
// pane was closed (the saved-session row reads the index entry).
func TestListKeepsRenamedLabel(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	snap := SessionSnapshot{
		WorkingDir: dir,
		Messages: []llm.Message{
			{Role: "user", Content: "implement session commands with labels"},
			{Role: "assistant", Content: "ok"},
		},
		// What RenameSession persists: a custom label distinct from the
		// first user message.
		Label: "My Custom Title",
	}
	if err := store.Save("sess-1", snap); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sessions", len(list))
	}
	if list[0].Label != "My Custom Title" {
		t.Fatalf("label=%q, want the renamed title (not the first user message)", list[0].Label)
	}
}

// TestSavePersistsRenamedEmptySession pins the label exception in Save's
// empty-session skip: an empty session that was explicitly renamed
// (RenameSession / session_rename tool) must still be persisted, or the
// rename would be silently dropped. Plain empty sessions (the /new bloat
// case) must stay skipped.
func TestSavePersistsRenamedEmptySession(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})

	// A renamed empty session: no messages, deliberate label.
	if err := store.Save("sess-renamed", SessionSnapshot{
		WorkingDir: dir,
		Label:      "My Custom Title",
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := store.LoadInWorkingDir(dir, "sess-renamed")
	if err != nil {
		t.Fatalf("renamed empty session must be persisted: %v", err)
	}
	if snap.Label != "My Custom Title" {
		t.Fatalf("label=%q, want %q", snap.Label, "My Custom Title")
	}

	// An anonymous empty session is still skipped (no file, no index entry).
	if err := store.Save("sess-anon", SessionSnapshot{WorkingDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadInWorkingDir(dir, "sess-anon"); err == nil {
		t.Fatal("anonymous empty session should not be persisted")
	}
}

// TestSessionLabelLegacyTruncationStillMigrated pins the other half of the
// trade-off: a legacy 50-char stored label that is a prefix of the full
// message is still migrated to the untruncated text.
func TestSessionLabelLegacyTruncationStillMigrated(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: "implement session commands with labels and keep going"}}
	derived := llm.SessionLabel(msgs)
	legacy := derived[:legacyLabelMaxLen]
	if got := sessionLabel(msgs, legacy, false); got != derived {
		t.Fatalf("legacy truncated label = %q, want migrated to %q", got, derived)
	}
	// A rename shorter than the legacy length must NOT be regenerated.
	if got := sessionLabel(msgs, "Short title", false); got != "Short title" {
		t.Fatalf("rename = %q, want kept, got %q", "Short title", got)
	}
	// Empty stored label falls back to the message-derived label.
	if got := sessionLabel(msgs, "", false); got != derived {
		t.Fatalf("empty stored = %q, want %q", got, derived)
	}
	// A deliberate rename that EXACTLY matches the legacy 50-char truncation
	// shape (a prefix of the derived label) must still be authoritative when
	// the rename marker is set — the hole that the marker closes.
	renamed := derived[:legacyLabelMaxLen]
	if got := sessionLabel(msgs, renamed, true); got != renamed {
		t.Fatalf("renamed 50-char label = %q, want kept verbatim (not migrated to %q)", got, derived)
	}
}

// TestSaveLoadKeepsRenameMarkerAcrossRestart verifies the rename marker
// survives a full save/load round trip (process-restart simulation: a fresh
// Store with an empty createdCache), so a 50-char rename is not migrated by
// the legacy-label rule after a restart.
func TestSaveLoadKeepsRenameMarkerAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	msgs := []llm.Message{
		{Role: "user", Content: "implement session commands with labels and keep going"},
		{Role: "assistant", Content: "ok"},
	}
	derived := llm.SessionLabel(msgs)
	rename := derived[:legacyLabelMaxLen]
	if err := store.Save("sess-renamed", SessionSnapshot{
		WorkingDir:   dir,
		Messages:     msgs,
		Label:        rename,
		LabelRenamed: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Fresh store instance = empty createdCache (simulates a restart).
	store2 := NewStoreWithOptions(true, StoreOptions{})
	loaded, err := store2.LoadInWorkingDir(dir, "sess-renamed")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.LabelRenamed {
		t.Fatalf("LabelRenamed lost across save/load: %+v", loaded)
	}
	if loaded.Label != rename {
		t.Fatalf("label=%q, want the 50-char rename kept verbatim", loaded.Label)
	}
	// A re-save after restart must not migrate the rename either.
	if err := store2.Save("sess-renamed", SessionSnapshot{
		WorkingDir:   dir,
		Messages:     msgs,
		Label:        rename,
		LabelRenamed: true,
	}); err != nil {
		t.Fatal(err)
	}
	list, err := store2.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Label != rename {
		t.Fatalf("list label=%+v, want the rename kept", list)
	}
}

// TestListRebuildKeepsRenamedMarker pins the legacy-scan index rebuild: when
// index.json is missing, the rebuilt entry must carry the session file's
// LabelRenamed marker — otherwise the NEXT List (index path) applies the
// legacy 50-char migration and silently replaces a deliberate rename.
func TestListRebuildKeepsRenamedMarker(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	msgs := []llm.Message{
		{Role: "user", Content: "implement session commands with labels and keep going"},
		{Role: "assistant", Content: "ok"},
	}
	derived := llm.SessionLabel(msgs)
	rename := derived[:legacyLabelMaxLen]
	if err := store.Save("sess-renamed", SessionSnapshot{
		WorkingDir:   dir,
		Messages:     msgs,
		Label:        rename,
		LabelRenamed: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate index loss (deleted/corrupt index, legacy directory).
	if err := os.Remove(store.indexFile(dir)); err != nil {
		t.Fatal(err)
	}
	store.invalidateListCache(dir)

	// First List: legacy scan + index rebuild. The returned label is already
	// correct (sessionLabel honors the marker at scan time).
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Label != rename {
		t.Fatalf("first list label=%+v, want the rename kept", list)
	}

	// The rebuilt index must carry the marker...
	idx := store.readIndex(dir)
	if idx == nil || len(idx.Entries) != 1 {
		t.Fatalf("rebuilt index = %+v, want one entry", idx)
	}
	if !idx.Entries[0].LabelRenamed {
		t.Fatalf("rebuilt index lost LabelRenamed: %+v", idx.Entries[0])
	}

	// ...so the second List (index path, migration loop) keeps the rename.
	store.invalidateListCache(dir)
	list, err = store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Label != rename {
		t.Fatalf("second list label=%+v, want the rename kept (marker dropped?)", list)
	}
}

// TestListRebuildKeepsNonPrefixFiftyCharLabel pins the migration loop's
// prefix rule: a 50-char stored label that is NOT a prefix of the derived
// first-user-message text is not the legacy-truncation shape and must be
// kept even without the rename marker (the marker-less-rename case).
func TestListRebuildKeepsNonPrefixFiftyCharLabel(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	msgs := []llm.Message{{Role: "user", Content: strings.Repeat("a", 80)}}
	stored := strings.Repeat("b", legacyLabelMaxLen)
	if err := store.Save("sess-nonprefix", SessionSnapshot{
		WorkingDir: dir,
		Messages:   msgs,
		Label:      stored,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.indexFile(dir)); err != nil {
		t.Fatal(err)
	}
	store.invalidateListCache(dir)
	if _, err := store.List(dir); err != nil { // legacy scan + rebuild
		t.Fatal(err)
	}
	store.invalidateListCache(dir)
	list, err := store.List(dir) // index path + migration loop
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Label != stored {
		t.Fatalf("label=%+v, want the non-prefix 50-char label kept", list)
	}
}

// TestListRebuildStillMigratesLegacyTruncatedLabel pins the other half of the
// trade-off: a stored label that IS the legacy truncation (50-char prefix of
// the derived text) still migrates on the index path after a rebuild.
func TestListRebuildStillMigratesLegacyTruncatedLabel(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	msgs := []llm.Message{{Role: "user", Content: "implement session commands with labels and keep going"}}
	derived := llm.SessionLabel(msgs)
	legacy := derived[:legacyLabelMaxLen]
	if err := store.Save("sess-legacy", SessionSnapshot{
		WorkingDir: dir,
		Messages:   msgs,
		Label:      legacy,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.indexFile(dir)); err != nil {
		t.Fatal(err)
	}
	store.invalidateListCache(dir)
	if _, err := store.List(dir); err != nil { // legacy scan + rebuild
		t.Fatal(err)
	}
	store.invalidateListCache(dir)
	list, err := store.List(dir) // index path + migration loop
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Label != derived {
		t.Fatalf("label=%+v, want migrated to %q", list, derived)
	}
}
