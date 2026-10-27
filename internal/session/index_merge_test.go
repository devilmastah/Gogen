package session

import (
	"os"
	"testing"
	"time"
)

// TestMergeIndexEntriesFromDisk is the B27 regression: the session index is
// read-modify-written per process, so two hosts saving at once used to drop
// one entry and leave that session invisible to List and to pruning. The
// upsert path now merges entries another process added.
func TestMergeIndexEntriesFromDisk(t *testing.T) {
	wd := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})

	// Session files are what mergeIndexEntriesFromDisk stats to reject
	// tombstones; their contents do not matter to the merge. (Save skips an
	// empty snapshot, so the files are written directly.)
	if err := os.MkdirAll(s.dir(wd), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, id := range []string{"mine", "theirs"} {
		if err := os.WriteFile(s.path(wd, id), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}

	now := time.Now().UTC()
	// What this process read: only its own entry.
	idx := &sessionIndex{Entries: []sessionIndexEntry{{ID: "mine", Updated: now}}}
	// What is on disk: ours plus another process's entry, plus a ghost whose
	// session file was deleted from under it (cross-process tombstone).
	disk := &sessionIndex{Entries: []sessionIndexEntry{
		{ID: "mine", Updated: now},
		{ID: "theirs", Updated: now.Add(time.Second)},
		{ID: "ghost", Updated: now.Add(2 * time.Second)},
	}}
	if err := s.writeIndex(wd, disk); err != nil {
		t.Fatalf("writeIndex: %v", err)
	}

	s.mergeIndexEntriesFromDisk(wd, idx)

	got := make(map[string]bool, len(idx.Entries))
	for _, e := range idx.Entries {
		got[e.ID] = true
	}
	if !got["mine"] {
		t.Fatal("own entry dropped by the merge")
	}
	if !got["theirs"] {
		t.Fatal("entry added by another process was not merged in (B27)")
	}
	if got["ghost"] {
		t.Fatal("merged an entry whose session file is gone (resurrected tombstone)")
	}
	if len(idx.Entries) != 2 {
		t.Fatalf("entries = %d (%v), want 2", len(idx.Entries), got)
	}
}

// TestRemoveFromIndexDoesNotMergeDeletedEntry pins the other side of the
// merge policy: the delete paths pass mergeOnDisk=false, so a tombstone is
// never resurrected by a stale on-disk copy of the entry it removed.
func TestRemoveFromIndexDoesNotMergeDeletedEntry(t *testing.T) {
	wd := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	now := time.Now().UTC()

	for _, id := range []string{"gone", "kept"} {
		if err := s.Save(id, SessionSnapshot{WorkingDir: wd}); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}
	// A stale index that still lists "gone" (as a previous process wrote it).
	if err := s.writeIndex(wd, &sessionIndex{Entries: []sessionIndexEntry{
		{ID: "gone", Updated: now},
		{ID: "kept", Updated: now},
	}}); err != nil {
		t.Fatalf("writeIndex: %v", err)
	}

	s.removeFromIndex(wd, "gone")

	idx := s.readIndex(wd)
	if idx == nil {
		t.Fatal("index missing after removeFromIndex")
	}
	for _, e := range idx.Entries {
		if e.ID == "gone" {
			t.Fatal("removeFromIndex resurrected the deleted entry by merging the on-disk copy")
		}
	}
}
