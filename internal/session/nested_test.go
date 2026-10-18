package session

import (
	"os"
	"testing"
	"time"

	"gogen/internal/llm"
)

func nestedSnap(workingDir, id, parent string) SessionSnapshot {
	return SessionSnapshot{
		WorkingDir: workingDir,
		ParentID:   parent,
		Messages:   []llm.Message{{Role: "user", Content: "job " + id}},
	}
}

// TestNestedCascadeDelete verifies deleting a parent removes its nested
// children (D2) while leaving unrelated sessions alone.
func TestNestedCascadeDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("child-1", nestedSnap(dir, "child-1", "parent")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("child-2", nestedSnap(dir, "child-2", "parent")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("other", nestedSnap(dir, "other", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(dir, "parent"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "other" {
		t.Fatalf("after cascade: %+v, want only 'other'", list)
	}
}

// TestNestedCascadeDeleteGrandchild verifies the cascade is recursive:
// deleting a parent removes its nested children AND their nested children
// (subagent_max_depth >= 2), so no orphaned grandchildren linger — they are
// excluded from the flat list, so nothing else would ever delete them.
func TestNestedCascadeDeleteGrandchild(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("child", nestedSnap(dir, "child", "parent")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("grandchild", nestedSnap(dir, "grandchild", "child")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(dir, "parent"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("after cascade: %+v, want no sessions left", list)
	}
}

// TestNestedChildrenLockedFallbackOrdering verifies the no-index fallback
// orders children most-recently-updated first (the per-parent cap prunes
// from the tail), mirroring the index path — ReadDir's name order would
// prune the wrong (newest) siblings.
func TestNestedChildrenLockedFallbackOrdering(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	// Sequential saves give strictly increasing Updated stamps.
	for _, id := range []string{"old", "mid", "new"} {
		if err := s.Save(id, nestedSnap(dir, id, "parent")); err != nil {
			t.Fatal(err)
		}
	}
	// Drop the index so the fallback (session-file scan) is exercised.
	if err := os.Remove(s.indexFile(dir)); err != nil {
		t.Fatal(err)
	}
	got := s.nestedChildrenLocked(dir, "parent")
	want := []string{"new", "mid", "old"}
	if len(got) != len(want) {
		t.Fatalf("fallback order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fallback order = %v, want %v", got, want)
		}
	}
}

// TestNestedPerParentCap verifies the transcript cap: at most
// maxNestedPerParent children per parent, oldest pruned at child save time
// (D2), while the just-saved child is always kept.
func TestNestedPerParentCap(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	// Save maxNestedPerParent + 2 children (12); each save prunes beyond 10.
	for i := 0; i < maxNestedPerParent+2; i++ {
		id := "child-" + string(rune('a'+i))
		if err := s.Save(id, nestedSnap(dir, id, "parent")); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var children []string
	for _, info := range list {
		if info.ParentID == "parent" {
			children = append(children, info.ID)
		}
	}
	if len(children) != maxNestedPerParent {
		t.Fatalf("children = %v, want %d", children, maxNestedPerParent)
	}
	// The oldest two ('a','b') were pruned; the newest ('k','l') remain.
	for _, gone := range []string{"child-a", "child-b"} {
		for _, c := range children {
			if c == gone {
				t.Fatalf("oldest child %s should have been pruned; remaining: %v", gone, children)
			}
		}
	}
	if _, err := s.LoadInWorkingDir(dir, "parent"); err != nil {
		t.Fatal(err)
	}
}

// TestNestedChildrenTieBreakBySaveOrder pins the tie-break the per-parent cap
// depends on. time.Now has ~15.6 ms granularity on Windows (the coarse
// KUSER_SHARED_DATA tick), so siblings saved inside one tick carry EQUAL
// Updated stamps; with an unstable sort over a comparator that returns 0 the
// prune picked whoever the sort happened to leave at the tail and child-c was
// dropped while the older child-b survived (TestNestedPerParentCap, Windows
// CI). Equal stamps must resolve to save order: the index's insertion order.
func TestNestedChildrenTieBreakBySaveOrder(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	// Saved a, b, c, d in order (the ids are deliberately not alphabetical in
	// save order, so id ordering cannot mask a broken tie-break).
	saved := []string{"z-first", "m-second", "a-third", "b-fourth"}
	for _, id := range saved {
		if err := s.Save(id, nestedSnap(dir, id, "parent")); err != nil {
			t.Fatal(err)
		}
	}
	// Rewrite the index with ONE shared stamp: the coarse-clock shape.
	s.mu.Lock()
	idx := s.readIndex(dir)
	if idx == nil {
		s.mu.Unlock()
		t.Fatal("index missing after saves")
	}
	shared := time.Now().UTC()
	for i := range idx.Entries {
		if idx.Entries[i].ParentID == "parent" {
			idx.Entries[i].Updated = shared
		}
	}
	err := s.writeIndex(dir, idx)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	got := s.nestedChildrenLocked(dir, "parent")
	s.mu.Unlock()
	// Newest first: the reverse of save order.
	want := []string{"b-fourth", "a-third", "m-second", "z-first"}
	if len(got) != len(want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tied children order = %v, want save order reversed %v", got, want)
		}
	}
}

// TestNestedParentIDRoundTrip verifies ParentID survives save → load.
func TestNestedParentIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	snap := nestedSnap(dir, "child", "parent")
	if err := s.Save("child", snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadInWorkingDir(dir, "child")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ParentID != "parent" {
		t.Fatalf("loaded ParentID = %q, want parent", loaded.ParentID)
	}
}

// TestNestedLatestIDExcluded verifies "latest" never resolves to a nested
// (subagent) child: the flat session list excludes children, so "resume
// latest" and the restart bootstrap must land on the most recent TOP-LEVEL
// session instead — a child that finished last must not become the default
// session after a restart.
func TestNestedLatestIDExcluded(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	if err := s.Save("parent", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "p"}},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // child strictly newer
	if err := s.Save("child", SessionSnapshot{
		WorkingDir: dir,
		ParentID:   "parent",
		Messages:   []llm.Message{{Role: "user", Content: "c"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Precondition: the child really is the most recently updated session.
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].ID != "child" {
		t.Fatalf("precondition: newest session = %q, want the child", list[0].ID)
	}
	latest, err := s.LatestID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if latest != "parent" {
		t.Fatalf("LatestID = %q, want the parent (nested children excluded)", latest)
	}
	// Only children on disk: no flat session exists → "" (bootstrap then
	// falls back to a fresh session).
	dir2 := t.TempDir()
	if err := s.Save("child-only", SessionSnapshot{
		WorkingDir: dir2,
		ParentID:   "gone",
		Messages:   []llm.Message{{Role: "user", Content: "c"}},
	}); err != nil {
		t.Fatal(err)
	}
	latest, err = s.LatestID(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if latest != "" {
		t.Fatalf("LatestID with only children = %q, want empty", latest)
	}
}

// TestSubagentOutcomeRoundTrip verifies the recorded subagent outcome
// (status + summary) survives save → load and reaches the metadata index
// (what the sessions payload reads), including across a fresh store over
// the same directory (restart simulation).
func TestSubagentOutcomeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{})
	if err := s.Save("parent", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "p"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("child", SessionSnapshot{
		WorkingDir:      dir,
		ParentID:        "parent",
		SubagentStatus:  "failed",
		SubagentSummary: "boom",
		Messages:        []llm.Message{{Role: "user", Content: "c"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Load round-trip.
	loaded, err := s.LoadInWorkingDir(dir, "child")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SubagentStatus != "failed" || loaded.SubagentSummary != "boom" {
		t.Fatalf("loaded outcome = %q/%q, want failed/boom", loaded.SubagentStatus, loaded.SubagentSummary)
	}
	// Index round-trip: List and Info both carry the outcome.
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, si := range list {
		if si.ID == "child" {
			found = true
			if si.SubagentStatus != "failed" || si.SubagentSummary != "boom" {
				t.Fatalf("List outcome = %q/%q, want failed/boom", si.SubagentStatus, si.SubagentSummary)
			}
		}
	}
	if !found {
		t.Fatal("child missing from List")
	}
	info := s.Info(dir, "child")
	if info == nil || info.SubagentStatus != "failed" || info.SubagentSummary != "boom" {
		t.Fatalf("Info outcome = %+v, want failed/boom", info)
	}
	// Fresh store over the same directory (restart): the index persists.
	s2 := NewStoreWithOptions(true, StoreOptions{})
	info2 := s2.Info(dir, "child")
	if info2 == nil || info2.SubagentStatus != "failed" {
		t.Fatalf("Info after restart = %+v, want the persisted outcome", info2)
	}
}

// TestNestedExemptFromGlobalPrune verifies the retention prune skips nested
// sessions (they are bounded by the per-parent cap, not the global counts)
// and cascades the children of any top-level session it prunes — a pruned
// parent must not orphan its children.
func TestNestedExemptFromGlobalPrune(t *testing.T) {
	dir := t.TempDir()
	// Budget: 2 top-level sessions, no age retention.
	s := NewStoreWithOptions(true, StoreOptions{MaxCount: 2, MaxAgeDays: -1})
	s.SetAutoPrune(false) // drive the prune explicitly below
	// Save order: parent (oldest top-level), other, then the nested children.
	for _, id := range []string{"parent", "extra", "other", "child-1", "child-2"} {
		parent := ""
		if id == "child-1" || id == "child-2" {
			parent = "parent"
		}
		if err := s.Save(id, nestedSnap(dir, id, parent)); err != nil {
			t.Fatal(err)
		}
	}
	s.Prune(dir)
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Budget 2 keeps the newest top-level sessions ("extra", "other");
	// "parent" (oldest) is pruned — with its children (cascade).
	if len(list) != 2 || list[0].ID != "other" || list[1].ID != "extra" {
		t.Fatalf("after prune: %+v, want 'other' + 'extra' (parent and children pruned)", list)
	}
}

// TestNestedExemptFromGlobalPruneFallback covers the no-index fallback path
// (legacySessionUpdated): nested sessions must be skipped there too, or the
// per-parent cap is not the only bound on children.
func TestNestedExemptFromGlobalPruneFallback(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{MaxCount: 2, MaxAgeDays: -1})
	s.SetAutoPrune(false)
	for _, id := range []string{"parent", "extra", "other", "child-1", "child-2"} {
		parent := ""
		if id == "child-1" || id == "child-2" {
			parent = "parent"
		}
		if err := s.Save(id, nestedSnap(dir, id, parent)); err != nil {
			t.Fatal(err)
		}
	}
	// Drop the index so the legacy file-scan fallback is exercised.
	if err := os.Remove(s.indexFile(dir)); err != nil {
		t.Fatal(err)
	}
	s.Prune(dir)
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "other" || list[1].ID != "extra" {
		t.Fatalf("after fallback prune: %+v, want 'other' + 'extra'", list)
	}
}

// TestNestedPruneKeepsRecentChildren verifies the exemption keeps ALL nested
// children of a live parent even when they would exceed the global count.
func TestNestedPruneKeepsRecentChildren(t *testing.T) {
	dir := t.TempDir()
	s := NewStoreWithOptions(true, StoreOptions{MaxCount: 1, MaxAgeDays: -1})
	s.SetAutoPrune(false)
	if err := s.Save("parent", nestedSnap(dir, "parent", "")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Save("child-"+string(rune('a'+i)), nestedSnap(dir, "child-"+string(rune('a'+i)), "parent")); err != nil {
			t.Fatal(err)
		}
	}
	// Protect the parent: with budget 1, a non-exempt child would be
	// pruned — the exemption keeps all of them.
	s.Prune(dir, "parent")
	list, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The parent survives (kept); the children are exempt and survive too.
	if len(list) != 4 {
		t.Fatalf("after prune: %+v, want parent + 3 children (all exempt)", list)
	}
}
