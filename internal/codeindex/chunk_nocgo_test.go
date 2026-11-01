//go:build !cgo

package codeindex

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/treesitter"
)

// TestChunkNoCGOUsesLineRule pins that the treesitter setting does not
// grow a second chunker in a binary built without tree-sitter.
func TestChunkNoCGOUsesLineRule(t *testing.T) {
	treesitter.Configure(true, "")
	t.Cleanup(func() { treesitter.Configure(true, "") })

	text := twoLongFuncs()
	got := ChunkSource("a.go", text, "h", 40, 5)
	want := ChunkText("a.go", text, "h", 40, 5)
	if len(got) != len(want) {
		t.Fatalf("chunks = %d, want %d (the line window)", len(got), len(want))
	}
	for i := range want {
		if got[i].Start != want[i].Start || got[i].End != want[i].End {
			t.Fatalf("chunk %d = %d-%d, want %d-%d", i, got[i].Start, got[i].End, want[i].Start, want[i].End)
		}
	}
	if got[0].Start != 1 {
		t.Fatalf("first line = %d, want 1", got[0].Start)
	}
	shared := got[0].End - got[1].Start + 1
	if shared != 5 {
		t.Fatalf("overlap = %d, want 5", shared)
	}

	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), text)
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(t.Context(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	batches := fileBatches(emb)
	if len(batches) != 1 || len(batches[0]) != len(want) {
		t.Fatalf("indexed chunks = %v, want %d line windows", batches, len(want))
	}
	if !strings.HasPrefix(batches[0][0], "package p\n") {
		t.Fatalf("first indexed chunk = %q", batches[0][0])
	}
}

func twoLongFuncs() string {
	var b strings.Builder
	b.WriteString("package p\n\n")
	for _, name := range []string{"One", "Two"} {
		fmt.Fprintf(&b, "func %s() {\n", name)
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&b, "\tprintln(%d)\n", i)
		}
		b.WriteString("}\n\n")
	}
	return b.String()
}
