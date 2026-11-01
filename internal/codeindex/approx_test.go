package codeindex

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withApproxMin(t *testing.T, n int) {
	t.Helper()
	prev := approxMinChunks
	approxMinChunks = n
	t.Cleanup(func() { approxMinChunks = prev })
}

func TestApproxBelowThreshold(t *testing.T) {
	if approxMinChunks != 20000 {
		t.Fatalf("threshold = %d, want 20000", approxMinChunks)
	}
	dir := t.TempDir()
	const n = 300
	const near = 17
	nearName := fmt.Sprintf("f%03d.go", near)
	for i := 0; i < n; i++ {
		body := "far\n"
		if i == near {
			body = "near-hit\n"
		}
		writeTree(t, filepath.Join(dir, fmt.Sprintf("f%03d.go", i)), body)
	}
	emb := &fakeEmb{vec: func(text string) []float32 {
		if text == "q" || strings.Contains(text, "near-hit") {
			return []float32{1, 0}
		}
		return []float32{0, 1}
	}}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	out, err := ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "1. " + nearName
	if !strings.Contains(out, want) {
		t.Fatalf("result = %q, want %s first", out, nearName)
	}
	if ix.approx != nil || ix.approxUses != 0 {
		t.Fatalf("lookup built below the threshold: approx=%v uses=%d", ix.approx != nil, ix.approxUses)
	}
}

func TestApproxKnownNearest(t *testing.T) {
	const n = 400
	const dim = 16
	const planted = 250
	rng := rand.New(rand.NewSource(7))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = approxUnit(rng, dim)
	}
	query := approxUnit(rng, dim)
	vecs[planted] = append([]float32(nil), query...)
	best, bestSim := 0, -2.0
	for i, v := range vecs {
		s := cosine(query, v)
		if s > bestSim {
			best, bestSim = i, s
		}
	}
	if best != planted {
		t.Fatalf("planted index %d is not the nearest (%d)", planted, best)
	}
	h := buildHNSW(vecs, approxGen{chunks: n, dim: dim})
	got := h.search(query, 8)
	for _, i := range got {
		if i == planted {
			return
		}
	}
	t.Fatalf("planted %d not in top 8: %v", planted, got)
}

func TestApproxFollowsFileChange(t *testing.T) {
	withApproxMin(t, 3)
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "aaa-near\n")
	writeTree(t, filepath.Join(dir, "b.go"), "bbb-far\n")
	writeTree(t, filepath.Join(dir, "c.go"), "ccc-stay\n")
	emb := &fakeEmb{vec: approxNearVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	out, err := ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1. a.go") {
		t.Fatalf("result = %q", out)
	}
	if ix.approx == nil {
		t.Fatal("lookup was not built")
	}
	first := ix.approx
	out, err = ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if ix.approx != first {
		t.Fatal("unchanged index rebuilt the lookup")
	}
	if emb.countContaining("ccc-stay") != 1 {
		t.Fatalf("unchanged file embeds = %d", emb.countContaining("ccc-stay"))
	}
	opened := Open(dir)
	if opened.approx == nil {
		t.Fatal("Open did not build the lookup")
	}

	writeTree(t, filepath.Join(dir, "a.go"), "aaa-far\n")
	writeTree(t, filepath.Join(dir, "b.go"), "bbb-near\n")
	out, err = ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1. b.go") || strings.Contains(out, "aaa-near") {
		t.Fatalf("result = %q", out)
	}
	if !strings.Contains(out, "bbb-near") {
		t.Fatalf("result = %q", out)
	}
	if ix.approx == nil || ix.approx == first {
		t.Fatal("edited index kept the previous lookup")
	}
	if emb.countContaining("ccc-stay") != 1 {
		t.Fatal("unchanged file was embedded again")
	}
	if emb.countContaining("bbb-near") != 1 || emb.countContaining("aaa-far") != 1 {
		t.Fatal("edited files were not embedded once")
	}

	rebuilt := ix.approx
	away := t.TempDir()
	ix.SetWorkingDir(away)
	if ix.approx != nil {
		t.Fatal("switching folders kept the lookup")
	}
	ix.SetWorkingDir(dir)
	if ix.approx == nil || ix.approx == rebuilt {
		t.Fatal("returning to the folder did not rebuild the lookup")
	}

	if err := os.Remove(filepath.Join(dir, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "c.go")); err != nil {
		t.Fatal(err)
	}
	out, err = ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if ix.approx != nil {
		t.Fatal("lookup stayed after the index dropped below the cutoff")
	}
	if !strings.Contains(out, "b.go") || strings.Contains(out, "a.go") {
		t.Fatalf("result = %q", out)
	}
}

func TestApproxFailedRefreshKeepsStructure(t *testing.T) {
	withApproxMin(t, 2)
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "aaa-near\n")
	writeTree(t, filepath.Join(dir, "b.go"), "bbb-far\n")
	emb := &fakeEmb{vec: approxNearVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if ix.approx == nil {
		t.Fatal("lookup was not built")
	}
	kept := ix.approx
	writeTree(t, filepath.Join(dir, "c.go"), "ccc-new\n")
	emb.fail = true
	out, err := ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if ix.approx != kept {
		t.Fatal("failed refresh replaced the lookup")
	}
	if !strings.Contains(out, "Refresh failed:") || !strings.Contains(out, "aaa-near") || strings.Contains(out, "ccc-new") {
		t.Fatalf("result = %q", out)
	}
}

func TestApproxPathStaysExact(t *testing.T) {
	withApproxMin(t, 3)
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "near.go"), "root-near\n")
	writeTree(t, filepath.Join(dir, "sub", "mid.go"), "sub-mid\n")
	writeTree(t, filepath.Join(dir, "other.go"), "other-far\n")
	emb := &fakeEmb{vec: func(text string) []float32 {
		if text == "q" || strings.Contains(text, "root-near") {
			return []float32{1, 0}
		}
		return []float32{0, 1}
	}}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if ix.approx == nil {
		t.Fatal("lookup was not built")
	}
	ix.approxUses = 0
	out, err := ix.Search(context.Background(), emb, id, "q", "sub")
	if err != nil {
		t.Fatal(err)
	}
	if ix.approxUses != 0 {
		t.Fatalf("path search used the lookup %d times", ix.approxUses)
	}
	if ix.approx == nil {
		t.Fatal("path search dropped the lookup")
	}
	if !strings.Contains(out, "sub/mid.go") || strings.Contains(out, "near.go") {
		t.Fatalf("result = %q", out)
	}
}

func approxNearVec(text string) []float32 {
	if text == "q" || strings.Contains(text, "near") {
		return []float32{1, 0}
	}
	return []float32{0, 1}
}

func approxUnit(rng *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	var n float64
	for i := range v {
		v[i] = float32(rng.NormFloat64())
		n += float64(v[i]) * float64(v[i])
	}
	n = math.Sqrt(n)
	if n == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] /= float32(n)
	}
	return v
}
