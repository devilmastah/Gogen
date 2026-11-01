//go:build cgo

package codeindex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/treesitter"
)

func TestChunkMethodIsNotTheWholeClass(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("requires CGO")
	}
	treesitter.Configure(true, "")
	t.Cleanup(func() { treesitter.Configure(true, "") })

	src := "class Greeter {\n  greet(name) {\n    return 1;\n  }\n  part() {\n    return 2;\n  }\n}\n"
	got := ChunkSource("greeter.js", src, "h", 40, 5)
	for _, c := range got {
		if strings.Contains(c.Text, "greet") && !strings.Contains(c.Text, "part") {
			return
		}
	}
	t.Fatalf("method was not its own chunk: %#v", got)
}

func TestChunkFunctions(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("requires CGO")
	}
	treesitter.Configure(true, "")
	t.Cleanup(func() { treesitter.Configure(true, "") })

	src := "package p\n\nfunc One() int {\n\treturn 1\n}\n\nfunc Two() int {\n\treturn 2\n}\n"
	got := ChunkSource("a.go", src, "h", 40, 5)
	if len(got) != 2 {
		t.Fatalf("chunks = %d, want 2\n%#v", len(got), got)
	}
	if !strings.Contains(got[0].Text, "func One") || strings.Contains(got[0].Text, "func Two") {
		t.Fatalf("first chunk = %q", got[0].Text)
	}
	if !strings.Contains(got[1].Text, "func Two") || strings.Contains(got[1].Text, "func One") {
		t.Fatalf("second chunk = %q", got[1].Text)
	}
	if strings.Contains(got[0].Text, "package p") || strings.Contains(got[1].Text, "package p") {
		t.Fatalf("package line was indexed: %#v", got)
	}

	var b strings.Builder
	b.WriteString("package p\n\nfunc Long() {\n")
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "\tprintln(%d)\n", i)
	}
	b.WriteString("}\n\nfunc Short() {\n\treturn\n}\n")
	got = ChunkSource("long.go", b.String(), "h", 10, 2)
	longChunks := 0
	shortChunks := 0
	var prevStart, prevEnd int
	havePrev := false
	overlapped := false
	for _, c := range got {
		if strings.Contains(c.Text, "package p") {
			t.Fatalf("package line was indexed: %q", c.Text)
		}
		if strings.Contains(c.Text, "func Short") && strings.Contains(c.Text, "println") {
			t.Fatalf("short function merged into the long one: %q", c.Text)
		}
		if strings.Contains(c.Text, "println") {
			longChunks++
			if havePrev {
				shared := prevEnd - c.Start + 1
				if shared != 2 {
					t.Fatalf("overlap = %d, want 2 (lines %d-%d then %d-%d)", shared, prevStart, prevEnd, c.Start, c.End)
				}
				overlapped = true
			}
			prevStart, prevEnd = c.Start, c.End
			havePrev = true
		} else {
			havePrev = false
		}
		if strings.Contains(c.Text, "func Short") {
			shortChunks++
		}
	}
	if longChunks < 2 {
		t.Fatalf("long function chunks = %d, want a line split\n%#v", longChunks, got)
	}
	if !overlapped {
		t.Fatal("long function split without the overlap")
	}
	if shortChunks != 1 {
		t.Fatalf("short function chunks = %d, want 1", shortChunks)
	}
}

func TestSymbolBoundariesRebuild(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("requires CGO")
	}
	treesitter.Configure(false, "")
	t.Cleanup(func() { treesitter.Configure(true, "") })

	dir := t.TempDir()
	src := "package p\n\nfunc One() int {\n\treturn 1\n}\n\nfunc Two() int {\n\treturn 2\n}\n"
	writeTree(t, filepath.Join(dir, "a.go"), src)
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(t.Context(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	stripBoundaries(t, dir)
	if _, err := ix.Search(t.Context(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if batches := fileBatches(emb); len(batches) != 1 {
		t.Fatalf("empty boundaries rebuilt a line index: %d file embeds", len(batches))
	}

	treesitter.Configure(true, "")
	if _, err := ix.Search(t.Context(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	batches := fileBatches(emb)
	if len(batches) != 2 {
		t.Fatalf("file embeds = %d, want 2", len(batches))
	}
	if len(batches[1]) != 2 {
		t.Fatalf("symbol chunks = %d, want 2\n%v", len(batches[1]), batches[1])
	}
	if strings.Contains(batches[1][0], "func Two") || strings.Contains(batches[1][1], "func One") {
		t.Fatalf("chunks = %v", batches[1])
	}
	meta := readMeta(t, dir)
	if meta.Boundaries != BoundariesSymbols {
		t.Fatalf("boundaries = %q", meta.Boundaries)
	}
}

func stripBoundaries(t *testing.T, dir string) {
	t.Helper()
	meta := readMeta(t, dir)
	meta.Boundaries = ""
	raw, err := os.ReadFile(filepath.Join(dir, ".gogen", "index", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Chunks == 0 {
		t.Fatal("expected a stored index")
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, `"boundaries"`) {
			continue
		}
		kept = append(kept, line)
	}
	text := strings.Join(kept, "\n")
	if strings.Contains(text, "boundaries") {
		t.Fatalf("boundaries field still present: %s", text)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gogen", "index", "meta.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
