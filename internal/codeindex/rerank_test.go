package codeindex

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

type recRerank struct {
	calls   int
	docs    []string
	err     error
	bad     bool
	reverse bool
}

func (r *recRerank) Rerank(_ context.Context, _ string, documents []string) ([]RerankHit, error) {
	r.calls++
	r.docs = append([]string(nil), documents...)
	if r.err != nil {
		return nil, r.err
	}
	if r.bad {
		return []RerankHit{{Index: 0, Score: 1}}, nil
	}
	out := make([]RerankHit, len(documents))
	for i := range documents {
		from := i
		if r.reverse {
			from = len(documents) - 1 - i
		}
		out[i] = RerankHit{Index: from, Score: float64(len(documents) - i)}
	}
	return out, nil
}

func TestRerankOff(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "alpha.go"), "alpha\n")
	writeTree(t, filepath.Join(dir, "beta.go"), "beta\n")
	emb := &fakeEmb{vec: func(text string) []float32 {
		if strings.Contains(text, "alpha") {
			return []float32{1, 0}
		}
		if strings.Contains(text, "beta") {
			return []float32{0, 1}
		}
		return []float32{1, 0}
	}}
	rr := &recRerank{reverse: true}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	out, err := ix.SearchRerank(context.Background(), emb, id, "q", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rr.calls != 0 {
		t.Fatal("rerank endpoint was called")
	}
	if !strings.Contains(out, "1. alpha.go") || strings.Contains(out, "Rerank failed") {
		t.Fatalf("result = %q", out)
	}
	alpha := strings.Index(out, "alpha.go")
	beta := strings.Index(out, "beta.go")
	if alpha < 0 || beta < 0 || alpha > beta {
		t.Fatalf("cosine order lost:\n%s", out)
	}
}

func TestRerankOn(t *testing.T) {
	dir := t.TempDir()
	const n = 30
	for i := 0; i < n; i++ {
		writeTree(t, filepath.Join(dir, fmt.Sprintf("f%02d.go", i)), fmt.Sprintf("n-%02d\n", i))
	}
	emb := &fakeEmb{vec: rankVec}
	rr := &recRerank{reverse: true}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	out, err := ix.SearchRerank(context.Background(), emb, id, "q", "", rr)
	if err != nil {
		t.Fatal(err)
	}
	if rr.calls != 1 {
		t.Fatalf("rerank calls = %d", rr.calls)
	}
	if len(rr.docs) != KeepChunks {
		t.Fatalf("documents = %d, want %d", len(rr.docs), KeepChunks)
	}
	joined := strings.Join(rr.docs, "\n")
	if !strings.Contains(joined, "n-00") || strings.Contains(joined, "n-29") {
		t.Fatalf("rerank documents = %v", rr.docs)
	}
	if !strings.Contains(out, "1. f23.go") {
		t.Fatalf("result = %q", out)
	}
	if strings.Contains(out, "f00.go") || strings.Contains(out, "f29.go") {
		t.Fatalf("result kept a file outside the reranked eight:\n%s", out)
	}
	if files := resultFiles(out); files != MaxFiles {
		t.Fatalf("files = %d, want %d\n%s", files, MaxFiles, out)
	}
}

func TestRerankFailure(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "alpha.go"), "alpha\n")
	writeTree(t, filepath.Join(dir, "beta.go"), "beta\n")
	emb := &fakeEmb{vec: func(text string) []float32 {
		if strings.Contains(text, "beta") {
			return []float32{0, 1}
		}
		return []float32{1, 0}
	}}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}

	t.Run("endpoint error", func(t *testing.T) {
		rr := &recRerank{err: errors.New("endpoint down"), reverse: true}
		out, err := ix.SearchRerank(context.Background(), emb, id, "q", "", rr)
		if err != nil {
			t.Fatal(err)
		}
		if rr.calls != 1 {
			t.Fatalf("calls = %d", rr.calls)
		}
		if !strings.Contains(out, "Rerank failed: endpoint down") {
			t.Fatalf("result = %q", out)
		}
		if !cosineAlphaFirst(out) {
			t.Fatalf("cosine order lost:\n%s", out)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		rr := &recRerank{bad: true, reverse: true}
		out, err := ix.SearchRerank(context.Background(), emb, id, "q", "", rr)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Rerank failed: rerank response could not be read") {
			t.Fatalf("result = %q", out)
		}
		if !cosineAlphaFirst(out) {
			t.Fatalf("cosine order lost:\n%s", out)
		}
	})
}

func rankVec(text string) []float32 {
	if text == "q" {
		return []float32{1, 0}
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(text), "n-%d", &n)
	return []float32{float32(1000 - n), float32(n)}
}

func cosineAlphaFirst(out string) bool {
	alpha := strings.Index(out, "alpha.go")
	beta := strings.Index(out, "beta.go")
	return alpha >= 0 && beta >= 0 && alpha < beta
}

func resultFiles(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 2 && line[1] == '.' && line[0] >= '1' && line[0] <= '9' {
			n++
		}
	}
	return n
}
