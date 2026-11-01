package codeindex

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProgressLine(t *testing.T) {
	cases := []struct {
		name string
		p    Progress
		want string
	}{
		{name: "not indexed", p: Progress{Phase: PhaseIdle}, want: "Not indexed"},
		{name: "empty index", p: Progress{Phase: PhaseIdle, Indexed: true}, want: "0 files, 0 chunks"},
		{name: "counts", p: Progress{Phase: PhaseIdle, Indexed: true, FilesTotal: 3, ChunksTotal: 9}, want: "3 files, 9 chunks"},
		{name: "scanning", p: Progress{Phase: PhaseWalking}, want: "Scanning files"},
		{name: "embedding", p: Progress{Phase: PhaseEmbedding, FilesDone: 12, FilesTotal: 48}, want: "Embedding 12/48 files"},
		{name: "saving", p: Progress{Phase: PhaseWriting}, want: "Saving the index"},
		{name: "error", p: Progress{Phase: PhaseIdle, Error: "embed failed", FilesTotal: 3}, want: "embed failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Line(); got != tc.want {
				t.Fatalf("Line() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfidence(t *testing.T) {
	cases := []struct {
		name  string
		files []fileHit
		want  string
	}{
		{name: "none", want: "unknown"},
		{name: "low", files: []fileHit{{Score: 0.30}}, want: "low"},
		{name: "medium", files: []fileHit{{Score: 0.80}, {Score: 0.79}}, want: "medium"},
		{name: "high lead", files: []fileHit{{Score: 0.80}, {Score: 0.50}}, want: "high"},
		{name: "high single", files: []fileHit{{Score: 0.80}}, want: "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := confidence(tc.files); got != tc.want {
				t.Fatalf("confidence = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestChunkSplit(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		if i > 1 {
			b.WriteByte('\n')
		}
		b.WriteString("line")
	}
	got := ChunkText("a.go", b.String(), "h", DefaultChunkLines, DefaultChunkOverlap)
	if len(got) != 2 {
		t.Fatalf("chunks = %d, want 2", len(got))
	}
	if got[0].Start != 1 || got[0].End != 40 {
		t.Fatalf("first range = %d-%d", got[0].Start, got[0].End)
	}
	if got[1].Start != 36 || got[1].End != 50 {
		t.Fatalf("second range = %d-%d, want 36-50", got[1].Start, got[1].End)
	}
	shared := 40 - 36 + 1
	if shared != 5 {
		t.Fatalf("overlap = %d, want 5", shared)
	}
}

func TestChunkCustom(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		if i > 1 {
			b.WriteByte('\n')
		}
		b.WriteString("line")
	}
	got := ChunkText("a.go", b.String(), "h", 20, 3)
	if len(got) < 2 {
		t.Fatalf("chunks = %d, want at least 2", len(got))
	}
	if got[0].End-got[0].Start+1 != 20 {
		t.Fatalf("first chunk lines = %d, want 20", got[0].End-got[0].Start+1)
	}
	if got[0].End-got[1].Start+1 != 3 {
		t.Fatalf("shared lines = %d, want 3", got[0].End-got[1].Start+1)
	}
}

func TestChunkOverlapZero(t *testing.T) {
	text := strings.Repeat("x\n", 20)
	got := ChunkText("a.go", text, "h", 10, 0)
	if len(got) != 2 || got[1].Start != 11 {
		t.Fatalf("ranges = %+v, want two touching chunks", got)
	}
}

func TestSearchOneChunkAndMatch(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("alpha line\n", 25)
	writeTree(t, filepath.Join(dir, "alpha.go"), body)
	for i := 0; i < 9; i++ {
		writeTree(t, filepath.Join(dir, "other"+string(rune('a'+i))+".go"), "beta\n")
	}
	emb := &fakeEmb{vec: func(text string) []float32 {
		if strings.Contains(text, "alpha") {
			return []float32{1, 0}
		}
		return []float32{0, 1}
	}}
	ix := Open(dir)
	id := Identity{Provider: "embed", BaseURL: "http://embed", Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	out, err := ix.Search(context.Background(), emb, id, "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Confidence: high\n") {
		t.Fatalf("result = %q", out)
	}
	if !strings.Contains(out, "1. alpha.go (score: 1.00)\n1-25:\n") {
		t.Fatalf("result = %q", out)
	}
	n := strings.Count(out, "alpha line\n")
	if n != 20 {
		t.Fatalf("snippet lines = %d, want 20", n)
	}
	if strings.Contains(out, "otheri.go") {
		t.Fatalf("ninth other file was returned:\n%s", out)
	}
	files := 0
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 2 && line[1] == '.' && line[0] >= '1' && line[0] <= '9' {
			files++
		}
	}
	if files != 8 {
		t.Fatalf("files = %d, want 8\n%s", files, out)
	}
}

func TestSearchGroup(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 15; i++ {
		b.WriteString("row ")
		b.WriteString(strings.Repeat("x", i))
		b.WriteByte('\n')
	}
	writeTree(t, filepath.Join(dir, "a.go"), b.String())
	emb := &fakeEmb{vec: func(text string) []float32 {
		if strings.Contains(text, "row 15") {
			return []float32{0, 1}
		}
		return []float32{1, 0}
	}}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 10, ChunkOverlap: 5}
	out, err := ix.Search(context.Background(), emb, id, "tail", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, ":\n") < 1 {
		t.Fatal(out)
	}
	ranges := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "-") && strings.HasSuffix(line, ":") && !strings.HasPrefix(line, "Confidence") {
			ranges++
			if line != "1-15:" {
				t.Fatalf("range = %q, want 1-15:", line)
			}
		}
	}
	if ranges != 1 {
		t.Fatalf("ranges = %d\n%s", ranges, out)
	}
	if !strings.Contains(out, "score: 1.00") {
		t.Fatalf("higher score was not kept:\n%s", out)
	}
}

func TestBoundary(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "in.go"), "inside\n")
	outside := filepath.Join(filepath.Dir(dir), "outside-"+filepath.Base(dir)+".go")
	writeTree(t, outside, "OUTSIDE\n")
	t.Cleanup(func() { os.Remove(outside) })
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", outside); err == nil {
		t.Fatal("expected an error for a path outside the working directory")
	}
	if len(emb.texts()) != 0 {
		t.Fatalf("embedder was called for an outside path: %v", emb.texts())
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatal(".gogen/index was created for an outside path")
	}
}

func TestSkipSecretsAndJunk(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "main.go"), "package main\n")
	writeTree(t, filepath.Join(dir, ".env"), "SECRETENV=1\n")
	writeTree(t, filepath.Join(dir, ".env.local"), "SECRETLOCAL=1\n")
	writeTree(t, filepath.Join(dir, "gogen.conf"), "secret conf\n")
	writeTree(t, filepath.Join(dir, "key.pem"), "SECRETPEM\n")
	writeTree(t, filepath.Join(dir, "id_rsa"), "SECRETRSA\n")
	writeTree(t, filepath.Join(dir, ".git", "config"), "SECRETGIT\n")
	writeTree(t, filepath.Join(dir, "node_modules", "pkg", "a.js"), "SECRETNODE\n")
	writeTree(t, filepath.Join(dir, "vendor", "lib.go"), "SECRETVENDOR\n")
	writeTree(t, filepath.Join(dir, ".hidden", "x.go"), "SECRETDOT\n")
	writeTree(t, filepath.Join(dir, "nul.go"), "ok\x00no\n")
	big := make([]byte, MaxFileBytes+1)
	for i := range big {
		big[i] = 'B'
	}
	writeTree(t, filepath.Join(dir, "big.go"), string(big))
	exact := make([]byte, MaxFileBytes)
	for i := range exact {
		exact[i] = 'E'
	}
	writeTree(t, filepath.Join(dir, "exact.go"), string(exact))
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(emb.texts(), "\n")
	for _, secret := range []string{"SECRETENV", "SECRETLOCAL", "secret conf", "SECRETPEM", "SECRETRSA", "SECRETGIT", "SECRETNODE", "SECRETVENDOR", "SECRETDOT", "ok\x00no"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("embedded skipped content %q", secret)
		}
	}
	if strings.Contains(joined, strings.Repeat("B", 64)) {
		t.Fatal("embedded a file over 1 MiB")
	}
	if !strings.Contains(joined, "package main") {
		t.Fatal("normal source file was not embedded")
	}
	if !strings.Contains(joined, strings.Repeat("E", 64)) {
		t.Fatal("a file of exactly 1 MiB was skipped")
	}
}

func TestUnchangedEditDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTree(t, path, "alpha old\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	fileCalls := emb.countContaining("alpha old")
	if fileCalls != 1 {
		t.Fatalf("first embed count = %d", fileCalls)
	}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if emb.countContaining("alpha old") != 1 {
		t.Fatal("unchanged file was embedded again")
	}
	writeTree(t, path, "alpha new\n")
	out, err := ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha new") || strings.Contains(out, "alpha old") {
		t.Fatalf("result = %q", out)
	}
	if emb.countContaining("alpha new") != 1 {
		t.Fatal("edited file was not re-embedded")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out, err = ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "a.go") {
		t.Fatalf("deleted file still returned: %s", out)
	}
}

func TestModelAndProviderChange(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Provider: "embed", BaseURL: "http://one", Model: "m1", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	id.Model = "m2"
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	meta := readMeta(t, dir)
	if meta.Model != "m2" {
		t.Fatalf("model = %s", meta.Model)
	}
	if emb.countContaining("alpha") != 2 {
		t.Fatalf("model change did not rebuild: %d", emb.countContaining("alpha"))
	}
	id.Provider = "other"
	id.BaseURL = "http://two"
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	meta = readMeta(t, dir)
	if meta.Provider != "other" || meta.BaseURL != "http://two" {
		t.Fatalf("meta = %+v", meta)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".gogen", "index", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-secret") || strings.Contains(string(raw), "apiKey") {
		t.Fatalf("meta contains a key: %s", raw)
	}
}

func TestSameIdentitySkipsFileEmbed(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Provider: "embed", BaseURL: "http://one", Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	// A different chat model is not part of the identity. The next search
	// keeps the stored provider and does not re-embed the file.
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if emb.countContaining("alpha") != 1 {
		t.Fatal("unchanged identity re-embedded the file")
	}
}

func TestDimensionChangeKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(dir, "b.go"), "beta\n")
	emb.badDim = true
	out, err := ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Refresh failed:") || !strings.Contains(out, "alpha") {
		t.Fatalf("result = %q", out)
	}
	if readMeta(t, dir).Dimension != 2 {
		t.Fatalf("dimension changed: %+v", readMeta(t, dir))
	}
}

func TestRefreshErrorKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(dir, "b.go"), "beta\n")
	emb.fail = true
	out, err := ix.Search(context.Background(), emb, id, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Refresh failed:") || !strings.Contains(out, "a.go") || !strings.Contains(out, "alpha") {
		t.Fatalf("result = %q", out)
	}
}

func TestCancelLeavesMeta(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, ".gogen", "index", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(dir, "b.go"), "beta\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emb.cancel = cancel
	_, _ = ix.Search(ctx, emb, id, "q", "")
	after, err := os.ReadFile(filepath.Join(dir, ".gogen", "index", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("meta changed:\n%s\n%s", before, after)
	}
}

func TestCancelAfterSuccessfulEmbedDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, ".gogen", "index", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(dir, "b.go"), "beta\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emb.cancel = cancel
	emb.cancelKeep = true
	_, _ = ix.Search(ctx, emb, id, "q", "")
	after, err := os.ReadFile(filepath.Join(dir, ".gogen", "index", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("meta changed:\n%s\n%s", before, after)
	}
}

func TestUnlockedLoadLeavesMissingMeta(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), &fakeEmb{vec: unitVec}, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, ".gogen", "index")
	meta := filepath.Join(root, "meta.json")
	if err := os.Rename(meta, meta+".bak"); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadStored(root); ok {
		t.Fatal("unlocked load treated a missing meta as present")
	}
	if _, err := os.Stat(meta); !os.IsNotExist(err) {
		t.Fatal("unlocked load put meta.json back")
	}
	if _, err := ix.Search(context.Background(), &fakeEmb{vec: unitVec}, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(meta); err != nil {
		t.Fatal(err)
	}
}

func TestLinkedIndexDirIsNotWritten(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(outside, "index", "chunks-deadbeef.json")
	if err := os.WriteFile(kept, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	link := filepath.Join(dir, ".gogen")
	if err := linkToDir(link, outside); err != nil {
		t.Skipf("link: %v", err)
	}
	ix := Open(dir)
	_, err := ix.Search(context.Background(), &fakeEmb{vec: unitVec}, Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}, "q", "")
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "index", "meta.json")); !os.IsNotExist(statErr) {
		t.Fatal("index was written through the link")
	}
	body, err := os.ReadFile(kept)
	if err != nil || string(body) != "keep" {
		t.Fatalf("outside file = %q err=%v", body, err)
	}
}

func TestReadChunkSkipsOutsideLink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "a.go")
	if err := os.Symlink(secret, target); err != nil {
		t.Skipf("symlink: %v", err)
	}
	ix := Open(dir)
	if got := ix.readChunk("a.go", 1, 5, 20); strings.Contains(got, "TOPSECRET") {
		t.Fatalf("snippet = %q", got)
	}
}

func linkToDir(link, target string) error {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
	return os.Symlink(target, link)
}

func TestEmptyModelWritesNothing(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	_, err := ix.Search(context.Background(), emb, Identity{ChunkLines: 40, ChunkOverlap: 5}, "q", "")
	if err != ErrNoModel {
		t.Fatalf("err = %v", err)
	}
	if len(emb.texts()) != 0 {
		t.Fatal("embedder was called")
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(statErr) {
		t.Fatal(".gogen/index was created")
	}
	if err := ix.Rebuild(context.Background(), emb, Identity{}); err != ErrNoModel {
		t.Fatalf("rebuild err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(statErr) {
		t.Fatal("rebuild created .gogen/index")
	}
}

func TestFailedEmbedRemovesNewIndexDir(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "gamma line\n")
	emb := &fakeEmb{vec: unitVec, fail: true}
	ix := Open(dir)
	_, err := ix.Search(context.Background(), emb, Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}, "q", "")
	if err == nil {
		t.Fatal("expected embed error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(statErr) {
		t.Fatal("failed embed left .gogen/index")
	}
}

func TestCancelRefreshReturnsIdle(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(dir, "b.go"), "beta\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emb.cancel = cancel
	var mu sync.Mutex
	var idle bool
	ix.SetProgress(func(p Progress) {
		if p.Phase == PhaseIdle && p.Error == "" {
			mu.Lock()
			idle = true
			mu.Unlock()
		}
	})
	_, _ = ix.Search(ctx, emb, id, "q", "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := idle
		mu.Unlock()
		if got {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel left the progress off idle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProgressCallbackDoesNotHoldTheLock(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	started := make(chan struct{})
	var once sync.Once
	ix.SetProgress(func(p Progress) {
		if p.Phase != PhaseWalking {
			return
		}
		once.Do(func() { close(started) })
		ix.SetWorkingDir(other)
	})
	done := make(chan error, 1)
	go func() {
		done <- ix.Rebuild(context.Background(), emb, id)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("progress callback did not run")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rebuild waited for the progress callback")
	}
}

func TestReplaceFileKeepsDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(dst, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := replaceFile(filepath.Join(dir, "missing", "meta.json.tmp"), dst)
	if err == nil {
		t.Fatal("missing temp renamed")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old\n" {
		t.Fatalf("destination = %q", got)
	}
}

func TestLoadRejectsEscapingChunkPath(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, ".gogen", "index")
	st, ok := loadStored(root)
	if !ok || st.Meta.ChunksFile == "" {
		t.Fatal("published index has no generation")
	}
	raw, err := os.ReadFile(filepath.Join(root, st.Meta.ChunksFile))
	if err != nil {
		t.Fatal(err)
	}
	var chunks []diskChunk
	if err := json.Unmarshal(raw, &chunks); err != nil {
		t.Fatal(err)
	}
	chunks[0].Path = "../secret.go"
	body, err := json.Marshal(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, st.Meta.ChunksFile), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadStored(root); ok {
		t.Fatal("escaping chunk path was loaded")
	}
}

func TestReadChunkStaysInside(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "codeindex-secret.txt")
	if err := os.WriteFile(outside, []byte("TOPSECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	ix := Open(dir)
	if got := ix.readChunk("../codeindex-secret.txt", 1, 1, 0); got != "" {
		t.Fatalf("read outside = %q", got)
	}
	if strings.Contains(ix.readChunk("a.go", 1, 1, 0), "TOPSECRET") {
		t.Fatal("ordinary path read the outside file")
	}
}

func TestLegacyIndexLoads(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".gogen", "index")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := metaFile{Model: "m", Dimension: 1, ChunkLines: 40, ChunkOverlap: 5, Files: 1, Chunks: 1}
	mb, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meta.json"), append(mb, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	cb, err := json.Marshal([]diskChunk{{Path: "a.go", Start: 1, End: 1, Hash: "h"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "chunks.json"), append(cb, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(1))
	if err := os.WriteFile(filepath.Join(root, "vectors.bin"), b[:], 0o644); err != nil {
		t.Fatal(err)
	}
	st, ok := loadStored(root)
	if !ok || len(st.Chunks) != 1 || st.Chunks[0].Path != "a.go" {
		t.Fatalf("legacy load = %+v ok=%v", st, ok)
	}
}

func TestSecondPublishDropsOldGeneration(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, ".gogen", "index")
	st, ok := loadStored(root)
	if !ok {
		t.Fatal("no index")
	}
	first := st.Meta.ChunksFile
	writeTree(t, filepath.Join(dir, "b.go"), "beta\n")
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, first)); !os.IsNotExist(err) {
		t.Fatal("old generation still on disk")
	}
	st, ok = loadStored(root)
	if !ok || st.Meta.Files != 2 {
		t.Fatalf("reloaded = %+v ok=%v", st.Meta, ok)
	}
}

func TestSearchInStaysOnSessionFolder(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	writeTree(t, filepath.Join(other, "secret.go"), "TOPSECRET\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	ix.SetWorkingDir(other)
	out, err := ix.SearchIn(context.Background(), emb, Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}, "q", "", dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "different folder") || strings.Contains(out, "TOPSECRET") {
		t.Fatalf("result = %q", out)
	}
	if len(emb.texts()) != 0 {
		t.Fatal("embedder was called for the other folder")
	}
}

func TestRebuildAndBusy(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if err := ix.Rebuild(context.Background(), emb, id); err != nil {
		t.Fatal(err)
	}
	if emb.countContaining("alpha") != 2 {
		t.Fatalf("reindex embed count = %d, want 2", emb.countContaining("alpha"))
	}
	snap := ix.Snapshot()
	if snap.Phase != PhaseIdle || snap.FilesTotal != 1 || snap.ChunksTotal != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if strings.Contains(snap.Line(), "alpha") {
		t.Fatal("snapshot contains file contents")
	}

	emb.block = make(chan struct{})
	emb.release = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ix.Rebuild(context.Background(), emb, id)
	}()
	select {
	case <-emb.block:
	case <-time.After(5 * time.Second):
		t.Fatal("rebuild did not start")
	}
	if err := ix.Rebuild(context.Background(), emb, id); err != ErrBusy {
		t.Fatalf("second rebuild = %v, want busy", err)
	}
	close(emb.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if emb.countContaining("alpha") != 3 {
		t.Fatalf("busy rebuild embedded again: %d", emb.countContaining("alpha"))
	}
}

func TestProgressDuringRebuild(t *testing.T) {
	dir := t.TempDir()
	const n = 33
	for i := 0; i < n; i++ {
		writeTree(t, filepath.Join(dir, "f"+strconv.Itoa(i)+".go"), "body "+strconv.Itoa(i)+" UNIQUEBODY\n")
	}
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	var mu sync.Mutex
	var seen []int
	ix.SetProgress(func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(p.Path+p.Error, "UNIQUEBODY") {
			t.Errorf("snapshot contains file contents: %+v", p)
		}
		if p.Phase == PhaseEmbedding || p.Phase == PhaseWriting {
			seen = append(seen, p.FilesDone)
		}
	})
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if err := ix.Rebuild(context.Background(), emb, id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var got []int
	for {
		mu.Lock()
		got = append([]int(nil), seen...)
		mu.Unlock()
		if len(got) >= 2 && got[len(got)-1] == n && got[0] < got[len(got)-1] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("files-done = %v, want an increase ending at %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap := ix.Snapshot()
	if snap.Phase != PhaseIdle || snap.FilesTotal != n || snap.ChunksTotal != n {
		t.Fatalf("snapshot = %+v", snap)
	}
	if strings.Contains(snap.Line()+snap.Path+snap.Error, "UNIQUEBODY") {
		t.Fatalf("idle snapshot contains file contents: %+v", snap)
	}
}

func TestProgressIdle(t *testing.T) {
	dir := t.TempDir()
	ix := Open(dir)
	snap := ix.Snapshot()
	if snap.Phase != PhaseIdle || snap.FilesTotal != 0 || snap.ChunksTotal != 0 {
		t.Fatalf("empty snapshot = %+v", snap)
	}
	if snap.Line() != "Not indexed" {
		t.Fatalf("line = %q", snap.Line())
	}
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	snap = ix.Snapshot()
	if snap.Phase != PhaseIdle || snap.FilesTotal != 1 || snap.ChunksTotal != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Line() != "1 files, 1 chunks" {
		t.Fatalf("line = %q", snap.Line())
	}
}

func TestSnapshotSeqIncreases(t *testing.T) {
	dir := t.TempDir()
	ix := Open(dir)
	first := ix.Snapshot().Seq
	if first == 0 {
		t.Fatal("open left the index sequence at 0")
	}
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	if err := ix.Rebuild(context.Background(), &fakeEmb{vec: unitVec}, Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}); err != nil {
		t.Fatal(err)
	}
	if got := ix.Snapshot().Seq; got <= first {
		t.Fatalf("seq = %d, open seq = %d", got, first)
	}
}

func TestOpenDoesNotCreateDir(t *testing.T) {
	dir := t.TempDir()
	_ = Open(dir)
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatal("Open created .gogen/index")
	}
}

type fakeEmb struct {
	mu         sync.Mutex
	calls      [][]string
	vec        func(string) []float32
	fail       bool
	badDim     bool
	cancel     context.CancelFunc
	cancelKeep bool
	block      chan struct{}
	release    chan struct{}
	onCall     func(*Index)
	ix         *Index
	once       sync.Once
}

func (f *fakeEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	cp := append([]string(nil), texts...)
	f.calls = append(f.calls, cp)
	fail := f.fail
	bad := f.badDim
	cancel := f.cancel
	keep := f.cancelKeep
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(f.ix)
	}
	if f.block != nil {
		f.once.Do(func() { close(f.block) })
		<-f.release
	}
	if cancel != nil {
		cancel()
		if !keep {
			return nil, ctx.Err()
		}
	}
	if fail && !isQuery(texts) {
		return nil, os.ErrInvalid
	}
	if bad && !isQuery(texts) {
		return [][]float32{make([]float32, 3)}, nil
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = f.vec(text)
	}
	return out, nil
}

func (f *fakeEmb) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, c := range f.calls {
		all = append(all, c...)
	}
	return all
}

func (f *fakeEmb) countContaining(s string) int {
	n := 0
	for _, t := range f.texts() {
		if strings.Contains(t, s) {
			n++
		}
	}
	return n
}

func isQuery(texts []string) bool {
	return len(texts) == 1 && (texts[0] == "q" || texts[0] == "alpha" || texts[0] == "tail")
}

func unitVec(string) []float32 { return []float32{1, 0} }

func writeTree(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSafeChunkPath(t *testing.T) {
	abs := "/etc/passwd"
	if runtime.GOOS == "windows" {
		abs = `C:\Windows\notepad.exe`
	}
	if safeChunkPath("C:foo") || safeChunkPath(`..\x`) || safeChunkPath(abs) || safeChunkPath("") {
		t.Fatal("unsafe path accepted")
	}
	if runtime.GOOS == "windows" {
		if safeChunkPath("foo:bar.txt") {
			t.Fatal("windows accepted a colon in a file name")
		}
		return
	}
	if !safeChunkPath("foo:bar.txt") {
		t.Fatal("a colon in a file name was rejected")
	}
}

func TestColonFileNameStaysLoadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a colon is not a file name on Windows")
	}
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "keep.go"), "alpha\n")
	emb := &fakeEmb{vec: unitVec}
	ix := Open(dir)
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(dir, "foo:bar.txt"), "gamma line\n")
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	st, ok := loadStored(filepath.Join(dir, ".gogen", "index"))
	if !ok {
		t.Fatal("index became unloadable")
	}
	for _, c := range st.Chunks {
		if c.Path == "foo:bar.txt" {
			return
		}
	}
	t.Fatalf("colon file was not indexed: %+v", st.Chunks)
}

func TestOnlyLockFileIgnoresAPublishedIndex(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !onlyLockFile(dir) {
		t.Fatal("a lock file alone was not empty")
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if onlyLockFile(dir) {
		t.Fatal("a published index looked empty")
	}
}

func TestRemoveEmptyIndexLeavesALock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "index.lock")
	if err := os.WriteFile(lock, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeEmptyIndex(dir)
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("lock file was removed before the lock was held")
	}
}

func TestLockStillAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !lockStillAt(f, path) {
		t.Fatal("open lock file did not match the path")
	}
	other := filepath.Join(dir, "other.lock")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lockStillAt(f, other) {
		t.Fatal("a different file matched the open lock")
	}
	if err := os.Remove(path); err == nil {
		if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		if lockStillAt(f, path) {
			t.Fatal("a replaced lock file still matched the open one")
		}
	}
}

func TestSetWorkingDirSameFolderDoesNotWait(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "package a\n")
	ix := Open(dir)
	release := make(chan struct{})
	emb := &fakeEmb{vec: unitVec, block: make(chan struct{}), release: release}
	rebuildDone := make(chan struct{})
	go func() {
		_ = ix.Rebuild(context.Background(), emb, Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5})
		close(rebuildDone)
	}()
	select {
	case <-emb.block:
	case <-time.After(2 * time.Second):
		t.Fatal("rebuild did not start")
	}
	done := make(chan struct{})
	go func() {
		ix.SetWorkingDir(dir)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("same folder waited on the rebuild")
	}
	close(release)
	select {
	case <-rebuildDone:
	case <-time.After(2 * time.Second):
		t.Fatal("rebuild did not finish")
	}
}

func TestEmptyIndexSaysZero(t *testing.T) {
	dir := t.TempDir()
	ix := Open(dir)
	if err := ix.Rebuild(context.Background(), &fakeEmb{vec: unitVec}, Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}); err != nil {
		t.Fatal(err)
	}
	if got := ix.Snapshot().Line(); got != "0 files, 0 chunks" {
		t.Fatalf("line = %q", got)
	}
}

func TestUnpublishedCleanupLeavesALaterWritersFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	discardUnpublished(dir)
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); !os.IsNotExist(err) {
		t.Fatalf("unpublished meta still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeEmptyIndex(dir)
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || string(b) != "theirs" {
		t.Fatalf("later writer lost: %v %q", err, b)
	}
}

func TestSearchErrorsWhenPublishedIndexDisappears(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.txt"), "gamma line\n")
	ix := Open(dir)
	testAfterPublish = func(root string) {
		_ = os.Remove(filepath.Join(root, "meta.json"))
	}
	t.Cleanup(func() { testAfterPublish = nil })
	_, err := ix.Search(context.Background(), &fakeEmb{vec: unitVec}, Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}, "q", "")
	if err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("err = %v", err)
	}
	if snap := ix.Snapshot(); !strings.Contains(snap.Line(), "disappeared") {
		t.Fatalf("line = %q", snap.Line())
	}
}

func TestOpenDoesNotLoadLinkedIndex(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"provider":"p","baseUrl":"http://example","model":"m","dimension":0,"chunkLines":40,"chunkOverlap":5,"files":3,"chunks":0}`)
	kept := filepath.Join(outside, "index", "meta.json")
	if err := os.WriteFile(kept, meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkToDir(filepath.Join(dir, ".gogen"), outside); err != nil {
		t.Skipf("link: %v", err)
	}
	ix := Open(dir)
	if ix.Snapshot().Indexed {
		t.Fatal("startup loaded an index through a link")
	}
	other := t.TempDir()
	ix.SetWorkingDir(other)
	ix.SetWorkingDir(dir)
	if ix.Snapshot().Indexed {
		t.Fatal("a folder change loaded an index through a link")
	}
	body, err := os.ReadFile(kept)
	if err != nil || string(body) != string(meta) {
		t.Fatalf("outside meta = %q err=%v", body, err)
	}
}

func TestProgressKeepsNewest(t *testing.T) {
	ix := Open(t.TempDir())
	release := make(chan struct{})
	started := make(chan struct{})
	var mu sync.Mutex
	var last Phase
	var n int
	ix.SetProgress(func(p Progress) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
		}
		mu.Lock()
		last = p.Phase
		mu.Unlock()
	})
	ix.setProgress(Progress{Phase: PhaseWalking})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not start")
	}
	for i := 0; i < 80; i++ {
		ix.setProgress(Progress{Phase: PhaseEmbedding, Error: strconv.Itoa(i), FilesDone: i})
	}
	ix.setProgress(Progress{Phase: PhaseIdle, FilesTotal: 1, ChunksTotal: 1})
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		phase := last
		mu.Unlock()
		if phase == PhaseIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("last phase = %q, want idle", last)
}

func readMeta(t *testing.T, dir string) metaFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".gogen", "index", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m metaFile
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
