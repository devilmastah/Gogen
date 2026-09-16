package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/llm"
)

// TestStoreConcurrentSaveLoadDelete hammers the store from many goroutines
// with distinct session IDs — the multi-session web server pattern. Before
// the internal mutex, concurrent Save/AppendMessages/Load/Delete raced on
// index.json read-modify-write and the createdCache map. Run with -race.
func TestStoreConcurrentSaveLoadDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	store.SetAutoPrune(false) // registry owns pruning; exercise Save without it

	const n = 12
	const ops = 8
	var wg sync.WaitGroup
	errs := make(chan error, n*ops*2)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sess-%d", i)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			snap := SessionSnapshot{
				WorkingDir: dir,
				Messages:   []llm.Message{{Role: "user", Content: "hello " + id}},
			}
			for j := 0; j < ops; j++ {
				if err := store.Save(id, snap); err != nil {
					errs <- fmt.Errorf("Save %s: %w", id, err)
					return
				}
				if _, err := store.LoadInWorkingDir(dir, id); err != nil {
					errs <- fmt.Errorf("Load %s: %w", id, err)
					return
				}
				if err := store.AppendMessages(id, snap, 1); err != nil {
					errs <- fmt.Errorf("Append %s: %w", id, err)
					return
				}
				if _, err := store.List(dir); err != nil {
					errs <- fmt.Errorf("List: %w", err)
					return
				}
			}
			// TouchSession + LatestID also mutate the index/cache.
			if err := store.TouchSession(dir, id); err != nil {
				errs <- fmt.Errorf("Touch %s: %w", id, err)
				return
			}
			if _, err := store.LatestID(dir); err != nil {
				errs <- fmt.Errorf("LatestID: %w", err)
				return
			}
			if err := store.Delete(dir, id); err != nil {
				errs <- fmt.Errorf("Delete %s: %w", id, err)
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestPruneProtectsMultipleActiveIDs verifies that Prune retains every
// protected ID (all active in-memory sessions, E2) while still dropping
// over-capacity sessions, and that SetAutoPrune(false) suppresses the
// internal Save-time prune.
func TestPruneProtectsMultipleActiveIDs(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{MaxCount: 3, MaxAgeDays: 365})
	store.SetAutoPrune(false)

	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if err := store.Save(id, SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: id}},
		}); err != nil {
			t.Fatal(err)
		}
		// Stagger Updated timestamps so the least-recently-updated sessions
		// are deterministic (a < b < c < d < e).
		time.Sleep(5 * time.Millisecond)
	}

	// Auto-prune is off, so the five saves above must NOT have dropped any
	// session — even though MaxCount is 3.
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if _, err := store.LoadInWorkingDir(dir, id); err != nil {
			t.Fatalf("session %s dropped before explicit Prune (auto-prune should be off): %v", id, err)
		}
	}

	// Protect the two "active" sessions b and d; capacity 3 keeps one more
	// (the most recently updated non-protected session, e).
	store.Prune(dir, "b", "d")

	wantGone := map[string]bool{"a": true, "c": true}
	wantKeep := map[string]bool{"b": true, "d": true, "e": true}
	for id := range wantKeep {
		if _, err := store.LoadInWorkingDir(dir, id); err != nil {
			t.Errorf("protected session %s was pruned: %v", id, err)
		}
	}
	for id := range wantGone {
		path := filepath.Join(dir, ".gogen", "sessions", id+".json")
		if _, err := os.Stat(path); err == nil {
			t.Errorf("over-capacity session %s should have been pruned", id)
		}
	}
}

// TestSaveReleasesStoreMutexDuringPayloadWrite pins Save's lock contract: the
// store-wide mutex is released before the (potentially large) payload is
// serialized and written, so a turn-end full save of a big session cannot block
// List/Info/LatestID — the web sidebar, served synchronously on the WS read
// loop. The seam re-enters the store (Info takes s.mu unconditionally); a
// regression that holds s.mu across the marshal+write deadlocks here and is
// caught by the timeout.
func TestSaveReleasesStoreMutexDuringPayloadWrite(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	store.SetAutoPrune(false)

	var fired atomic.Bool
	savePayloadHook = func() {
		fired.Store(true)
		// If Save still held s.mu here, this re-entrant call blocks forever.
		store.Info(dir, "big")
	}
	t.Cleanup(func() { savePayloadHook = nil })

	done := make(chan error, 1)
	go func() {
		done <- store.Save("big", SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}},
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Save hung — store mutex held across the payload write?")
	}
	if !fired.Load() {
		t.Fatal("savePayloadHook did not run; test seam wired wrong")
	}
}

// TestLoadDecodesOutsideStoreMutex pins LoadInWorkingDir's lock contract: the
// store-wide mutex is released before the (potentially large) session JSON is
// decoded, so attaching a big session cannot block List/Info/LatestID — the
// web sidebar, served synchronously on the WS read loop. The seam re-enters
// the store (Info takes s.mu unconditionally); a regression that holds s.mu
// across the decode deadlocks here and is caught by the timeout.
func TestLoadDecodesOutsideStoreMutex(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithOptions(true, StoreOptions{})
	store.SetAutoPrune(false)
	if err := store.Save("big", SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}},
	}); err != nil {
		t.Fatal(err)
	}

	var fired atomic.Bool
	loadDecodeHook = func() {
		fired.Store(true)
		// If LoadInWorkingDir still held s.mu here, this re-entrant call
		// blocks forever.
		store.Info(dir, "big")
	}
	t.Cleanup(func() { loadDecodeHook = nil })

	done := make(chan error, 1)
	go func() {
		_, err := store.LoadInWorkingDir(dir, "big")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("LoadInWorkingDir: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadInWorkingDir hung — store mutex held across the decode?")
	}
	if !fired.Load() {
		t.Fatal("loadDecodeHook did not run; test seam wired wrong")
	}
}
