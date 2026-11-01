package codeindex

import (
	"strings"
	"testing"

	"gogen/internal/treesitter"
)

func TestChunkLinesWhenTreeSitterOff(t *testing.T) {
	treesitter.Configure(false, "")
	t.Cleanup(func() { treesitter.Configure(true, "") })

	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("line\n")
	}
	text := b.String()
	got := ChunkSource("a.go", text, "h", 40, 5)
	want := ChunkText("a.go", text, "h", 40, 5)
	if len(got) != len(want) {
		t.Fatalf("chunks = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Start != want[i].Start || got[i].End != want[i].End {
			t.Fatalf("chunk %d = %d-%d, want %d-%d", i, got[i].Start, got[i].End, want[i].Start, want[i].End)
		}
	}
	if len(got) < 2 {
		t.Fatal("expected the line window to split the file")
	}
	shared := got[0].End - got[1].Start + 1
	if shared != 5 {
		t.Fatalf("overlap = %d, want 5", shared)
	}
}

func fileBatches(f *fakeEmb) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if isQuery(c) {
			continue
		}
		out = append(out, append([]string(nil), c...))
	}
	return out
}
