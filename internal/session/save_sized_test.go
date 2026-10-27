package session

import (
	"os"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestSaveSizedStreamsAndRoundTrips pins the streaming full-snapshot write: the
// payload goes straight through the atomic writer (no whole-history []byte on
// the heap) and it reports the size the agent uses to decide when a delta has
// grown enough to be folded in. The file is still published atomically and
// loads back unchanged.
func TestSaveSizedStreamsAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	big := strings.Repeat("0123456789", 1000)
	snap := SessionSnapshot{
		WorkingDir: dir,
		Model:      "gpt-4o",
		Messages: []llm.Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: big},
		},
	}
	id := "sized-session"
	n, err := store.SaveSized(id, snap)
	if err != nil {
		t.Fatal(err)
	}
	if n < int64(len(big)) {
		t.Fatalf("reported size %d is smaller than the payload content %d", n, len(big))
	}
	// The on-disk file is that size (plus nothing else): the counter measures
	// the real write, not an estimate.
	path := store.path(dir, id)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != n {
		t.Fatalf("file size = %d, reported = %d", info.Size(), n)
	}
	loaded, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != big {
		t.Fatalf("round trip lost the payload: %d message(s)", len(loaded.Messages))
	}
}

// TestSaveSizedEmptyStore: a disabled store still reports no error and no size.
func TestSaveSizedDisabled(t *testing.T) {
	store := NewStoreWithOptions(false, StoreOptions{})
	n, err := store.SaveSized("id", SessionSnapshot{})
	if err != nil || n != 0 {
		t.Fatalf("SaveSized on a disabled store = (%d, %v), want (0, nil)", n, err)
	}
}
