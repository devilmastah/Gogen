package codeindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchQuiet is how long a burst of writes waits before one refresh.
const watchQuiet = time.Second

// WatchSource resolves the embedder and identity for one refresh. The watch
// calls it when a quiet period ends, so a settings change applies to the
// next refresh. A nil embedder or an error skips that refresh.
type WatchSource func() (Embedder, Identity, error)

// SetWatch starts or stops the filesystem watch for this working directory.
// On does nothing until a complete index already exists: a change never
// creates the first index. A burst of writes inside watchQuiet is one
// refresh, and that refresh is the same one Search uses.
func (ix *Index) SetWatch(on bool, source WatchSource) {
	ix.setWatch(on, source, watchQuiet)
}

// setWatch is SetWatch with a caller-chosen quiet period. Tests use a short
// one. Production goes through SetWatch and waits watchQuiet.
func (ix *Index) setWatch(on bool, source WatchSource, quiet time.Duration) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if quiet <= 0 {
		quiet = watchQuiet
	}
	ix.watchOn = on
	ix.watchSource = source
	ix.watchQuiet = quiet
	ix.restartWatchLocked()
}

// Watching reports whether the watch is armed on the current directory.
func (ix *Index) Watching() bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.watchReady
}

// restartWatchLocked stops any running watch and starts one when watchOn is
// set. The caller holds ix.mu.
func (ix *Index) restartWatchLocked() {
	if ix.watchCancel != nil {
		ix.watchCancel()
		ix.watchCancel = nil
	}
	ix.watchGen++
	ix.watchReady = false
	if !ix.watchOn || ix.watchSource == nil || ix.dir == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ix.watchCancel = cancel
	gen := ix.watchGen
	dir := ix.dir
	source := ix.watchSource
	quiet := ix.watchQuiet
	go ix.runWatch(ctx, gen, dir, source, quiet)
}

func (ix *Index) markWatchReady(gen uint64, ready bool) {
	ix.mu.Lock()
	if ix.watchGen == gen {
		ix.watchReady = ready
	}
	ix.mu.Unlock()
}

func (ix *Index) runWatch(ctx context.Context, gen uint64, dir string, source WatchSource, quiet time.Duration) {
	defer ix.markWatchReady(gen, false)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	defer w.Close()
	if err := addWatchTree(w, dir); err != nil {
		return
	}
	ix.markWatchReady(gen, true)

	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	arm := func() {
		if timer == nil {
			timer = time.NewTimer(quiet)
			timerC = timer.C
			return
		}
		stopTimer()
		timer.Reset(quiet)
		timerC = timer.C
	}
	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Name == "" || ev.Op == fsnotify.Chmod {
				continue
			}
			if skipWatchEvent(dir, ev.Name) {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				addCreatedDir(w, dir, ev.Name)
			}
			arm()
		case <-timerC:
			timerC = nil
			if err := ix.refreshFromWatch(ctx, source); errors.Is(err, ErrBusy) {
				arm()
			}
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}

// refreshFromWatch embeds files that changed. No complete index means it
// returns without creating one.
func (ix *Index) refreshFromWatch(ctx context.Context, source WatchSource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ix.mu.Lock()
	root := ix.root
	ix.mu.Unlock()
	if _, ok := loadStored(root); !ok {
		return nil
	}
	emb, id, err := source()
	if err != nil || emb == nil || strings.TrimSpace(id.Model) == "" {
		return nil
	}
	if !ix.mu.TryLock() {
		return ErrBusy
	}
	defer ix.mu.Unlock()
	if _, ok := loadStored(ix.root); !ok {
		return nil
	}
	_, _, err = ix.refresh(ctx, emb, id, false)
	return err
}

func addWatchTree(w *fsnotify.Watcher, root string) error {
	root = filepath.Clean(root)
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if filepath.Clean(path) != root && skipDir(d.Name()) {
			return filepath.SkipDir
		}
		if outsideLink(root, path) {
			return filepath.SkipDir
		}
		return w.Add(path)
	})
}

// skipWatchEvent reports whether a filesystem event is outside the indexed
// tree or names a path the walk would skip.
func skipWatchEvent(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || !within(root, path) || outsideLink(root, path) {
		return true
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts[:len(parts)-1] {
		if skipDir(part) {
			return true
		}
	}
	base := parts[len(parts)-1]
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return skipDir(base)
	}
	if skipFile(base) {
		return true
	}
	// The path is a file, or it is already gone. skipDir is true for every
	// name that starts with a dot, and that would hide an indexed dotfile
	// such as .gitignore. A gone node_modules or vendor is still skipped.
	if err != nil && !strings.HasPrefix(base, ".") && skipDir(base) {
		return true
	}
	return false
}

// addCreatedDir watches a directory created after the watch started. A
// directory the walk skips is not added, so a new node_modules tree does
// not consume a watch for every nested folder. A link whose target leaves
// the working directory is not added. Lstat is used so the check does not
// follow that link.
func addCreatedDir(w *fsnotify.Watcher, root, path string) {
	if outsideLink(root, path) {
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || skipDir(filepath.Base(path)) {
		return
	}
	_ = w.Add(path)
}
