package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestWatchQuietIsOneSecond(t *testing.T) {
	if watchQuiet != time.Second {
		t.Fatalf("quiet = %s, want 1s", watchQuiet)
	}
}

func TestWatchDefault(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	ix := Open(dir)
	t.Cleanup(func() { ix.SetWatch(false, nil) })
	emb := &fakeEmb{vec: unitVec}
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if err := ix.Rebuild(context.Background(), emb, id); err != nil {
		t.Fatal(err)
	}
	ix.SetWatch(true, func() (Embedder, Identity, error) { return emb, id, nil })
	waitWatch(t, ix)
	writeTree(t, filepath.Join(dir, "a.go"), "alpha changed\n")
	time.Sleep(300 * time.Millisecond)
	if emb.countContaining("changed") != 0 {
		t.Fatal("refreshed before the quiet second")
	}
	waitFor(t, 3*time.Second, func() bool { return emb.countContaining("changed") > 0 })
	if got := emb.countContaining("changed"); got != 1 {
		t.Fatalf("embeds of the changed file = %d, want 1", got)
	}
}

func TestWatchOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTree(t, path, "alpha\n")
	ix := Open(dir)
	emb := &fakeEmb{vec: unitVec}
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	ix.SetWatch(false, func() (Embedder, Identity, error) { return emb, id, nil })
	if ix.Watching() {
		t.Fatal("explicit off left the watch running")
	}
	writeTree(t, path, "alpha changed\n")
	time.Sleep(300 * time.Millisecond)
	if emb.countContaining("changed") != 0 {
		t.Fatal("watch off embedded the changed file")
	}
	if _, err := ix.Search(context.Background(), emb, id, "q", ""); err != nil {
		t.Fatal(err)
	}
	if emb.countContaining("changed") == 0 {
		t.Fatal("search did not embed the file the watch left alone")
	}
}

func TestWatchBurst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	writeTree(t, path, "v1\n")
	ix := Open(dir)
	t.Cleanup(func() { ix.SetWatch(false, nil) })
	emb := &fakeEmb{vec: unitVec}
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if err := ix.Rebuild(context.Background(), emb, id); err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	ix.setWatch(true, func() (Embedder, Identity, error) {
		refreshes.Add(1)
		return emb, id, nil
	}, 200*time.Millisecond)
	waitWatch(t, ix)
	writeTree(t, path, "v2\n")
	time.Sleep(40 * time.Millisecond)
	writeTree(t, path, "v3\n")
	time.Sleep(40 * time.Millisecond)
	writeTree(t, path, "v4\n")
	time.Sleep(80 * time.Millisecond)
	if refreshes.Load() != 0 {
		t.Fatalf("burst refreshed %d times before the quiet period", refreshes.Load())
	}
	waitFor(t, time.Second, func() bool { return refreshes.Load() > 0 })
	time.Sleep(300 * time.Millisecond)
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
	if emb.countContaining("v2") != 0 || emb.countContaining("v3") != 0 {
		t.Fatal("burst embedded an intermediate write")
	}
	if emb.countContaining("v4") != 1 {
		t.Fatalf("final embed count = %d, want 1", emb.countContaining("v4"))
	}
}

func TestWatchRefreshesDotfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	writeTree(t, path, "alpha\n")
	ix := Open(dir)
	t.Cleanup(func() { ix.SetWatch(false, nil) })
	emb := &fakeEmb{vec: unitVec}
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	if err := ix.Rebuild(context.Background(), emb, id); err != nil {
		t.Fatal(err)
	}
	ix.SetWatch(true, func() (Embedder, Identity, error) { return emb, id, nil })
	waitWatch(t, ix)
	writeTree(t, path, "alpha changed\n")
	waitFor(t, 3*time.Second, func() bool { return emb.countContaining("changed") > 0 })
}

func TestSkipWatchEvent(t *testing.T) {
	dir := t.TempDir()
	gitignore := filepath.Join(dir, ".gitignore")
	writeTree(t, gitignore, "keep\n")
	if skipWatchEvent(dir, gitignore) {
		t.Fatal(".gitignore was skipped")
	}
	if err := os.Remove(gitignore); err != nil {
		t.Fatal(err)
	}
	if skipWatchEvent(dir, gitignore) {
		t.Fatal("a deleted .gitignore was skipped")
	}
	mods := filepath.Join(dir, "node_modules")
	if err := os.Mkdir(mods, 0o755); err != nil {
		t.Fatal(err)
	}
	if !skipWatchEvent(dir, mods) {
		t.Fatal("node_modules was not skipped")
	}
	if err := os.Remove(mods); err != nil {
		t.Fatal(err)
	}
	if !skipWatchEvent(dir, mods) {
		t.Fatal("a deleted node_modules was not skipped")
	}
	if !skipWatchEvent(dir, filepath.Join(dir, "node_modules", "pkg", "a.go")) {
		t.Fatal("a file inside node_modules was not skipped")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := os.Mkdir(mods, 0o755); err != nil {
		t.Fatal(err)
	}
	addCreatedDir(w, dir, mods)
	writeTree(t, filepath.Join(mods, "a.go"), "pkg\n")
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case ev := <-w.Events:
			if strings.Contains(ev.Name, "node_modules") {
				t.Fatalf("watched a skipped directory: %s", ev.Name)
			}
		case err := <-w.Errors:
			t.Fatal(err)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestWatchDoesNotFollowOutsideLink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(dir, "libs")
	if err := linkToDir(link, outside); err != nil {
		t.Skipf("link: %v", err)
	}
	if !skipWatchEvent(dir, filepath.Join(link, "a.go")) {
		t.Fatal("a file through an outside link was kept")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := addWatchTree(w, dir); err != nil {
		t.Fatal(err)
	}
	addCreatedDir(w, dir, link)
	writeTree(t, filepath.Join(link, "a.go"), "package a\n")
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case ev := <-w.Events:
			if strings.Contains(filepath.Clean(ev.Name), filepath.Clean(link)) {
				t.Fatalf("watched through an outside link: %s", ev.Name)
			}
		case err := <-w.Errors:
			t.Fatal(err)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestWatchNoIndexYet(t *testing.T) {
	dir := t.TempDir()
	ix := Open(dir)
	t.Cleanup(func() { ix.SetWatch(false, nil) })
	emb := &fakeEmb{vec: unitVec}
	id := Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5}
	ix.setWatch(true, func() (Embedder, Identity, error) { return emb, id, nil }, 150*time.Millisecond)
	waitWatch(t, ix)
	writeTree(t, filepath.Join(dir, "a.go"), "alpha\n")
	time.Sleep(400 * time.Millisecond)
	if len(emb.texts()) != 0 {
		t.Fatalf("watch embedded with no index: %v", emb.texts())
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatal("watch created .gogen/index")
	}
}

func waitWatch(t *testing.T, ix *Index) {
	t.Helper()
	waitFor(t, 2*time.Second, ix.Watching)
}

func waitFor(t *testing.T, d time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}
