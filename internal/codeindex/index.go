package codeindex

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBusy is returned when a rebuild is requested while one is already running.
var ErrBusy = errors.New("a code index rebuild is already running")

// ErrNoModel is returned when no embedding model resolved. Nothing is written.
var ErrNoModel = errors.New("code index model is not set")

// testAfterPublish runs after a successful publish and before that
// generation is read back, while the file lock is still held. Tests use
// it to remove the generation and check the caller still receives what
// was published, or an error when that read fails.
var testAfterPublish func(root string)

// Index is the vector store for one working directory. Its mutex is its own
// lock and is never taken while holding an agent lock. Callers take this
// lock only by calling Search, SearchRerank, SearchIn, Rebuild, or SetWatch.
// The progress callback runs on the index's own goroutine, after that lock
// is released for the callback. deliverMu is taken only to hand the callback
// the newest snapshot, and the callback runs after deliverMu is released.
// The watch goroutine takes the lock only to publish its ready flag and,
// with TryLock, around the same refresh Search uses.
type Index struct {
	mu         sync.Mutex
	dir        string
	dirSnap    atomic.Value // string, the same folder as dir, read without mu
	root       string
	lastNotify time.Time
	snap       atomic.Value // Progress
	hook       atomic.Value // *progressHook

	// Progress delivery. progressCh is created on the first update.
	// deliverMu guards progressLatest. It is taken while mu is held and
	// dropped before the callback, so the callback can wait on mu.
	// progressSeq numbers each snapshot. The delivery goroutine never
	// invokes one older than the last one it invoked.
	progressOnce   sync.Once
	progressCh     chan progressNote
	deliverMu      sync.Mutex
	progressLatest *progressNote
	progressSeq    uint64

	// The watch fields are guarded by mu. watchGen keeps a stopped
	// goroutine from clearing a newer watch.
	watchOn     bool
	watchCancel context.CancelFunc
	watchSource WatchSource
	watchQuiet  time.Duration
	watchGen    uint64
	watchReady  bool

	// approx is the in-memory lookup, guarded by mu. It is nil below
	// approxMinChunks. approxUses counts searches that consulted it.
	// Tests read it.
	approx     *hnsw
	approxUses int
}

// Open returns the index for workingDir. It does not create .gogen/index.
// A complete index already on disk is loaded into the snapshot. When that
// index holds 20,000 chunks or more, the in-memory lookup is built from
// its vectors.
func Open(workingDir string) *Index {
	ix := &Index{dir: workingDir, root: filepath.Join(workingDir, ".gogen", "index")}
	ix.dirSnap.Store(workingDir)
	ix.store(Progress{Phase: PhaseIdle})
	if st, ok := loadStored(ix.root); ok {
		ix.store(Progress{
			Phase:       PhaseIdle,
			FilesTotal:  st.Meta.Files,
			ChunksTotal: st.Meta.Chunks,
			Indexed:     true,
		})
		ix.prepareApprox(st)
	}
	return ix
}

// SetWorkingDir points the index at another folder. It does not create
// .gogen/index. The same folder returns without waiting. A different folder
// waits until a rebuild finishes. The previous folder's lookup is dropped.
// A stored index at the new folder is loaded, and the lookup is built again
// when that index is large enough.
func (ix *Index) SetWorkingDir(dir string) {
	if cur, ok := ix.dirSnap.Load().(string); ok && cur == dir {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.dir == dir {
		return
	}
	ix.dir = dir
	ix.dirSnap.Store(dir)
	ix.root = filepath.Join(dir, ".gogen", "index")
	ix.approx = nil
	ix.setProgress(Progress{Phase: PhaseIdle})
	if st, ok := loadStored(ix.root); ok {
		ix.setProgress(Progress{Phase: PhaseIdle, FilesTotal: st.Meta.Files, ChunksTotal: st.Meta.Chunks, Indexed: true})
		ix.prepareApprox(st)
	}
	ix.restartWatchLocked()
}

// Dir is the folder this index covers. A refresh in progress finishes first.
func (ix *Index) Dir() string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.dir
}

// SetProgress installs the callback the host uses to broadcast the snapshot.
// It runs off the index lock, at a phase change, at the end, and at most
// four times a second in between.
func (ix *Index) SetProgress(fn func(Progress)) {
	ix.hook.Store(&progressHook{fn: fn})
}

// Snapshot returns the current progress. It does not wait on a rebuild.
func (ix *Index) Snapshot() Progress {
	v := ix.snap.Load()
	if v == nil {
		return Progress{Phase: PhaseIdle}
	}
	return v.(Progress)
}

// Rebuild embeds every current file again, including files whose hash has
// not changed. A second call while one is running returns ErrBusy.
func (ix *Index) Rebuild(ctx context.Context, emb Embedder, id Identity) error {
	return ix.rebuild(ctx, emb, id, "")
}

// RebuildFor is Rebuild for the session whose folder is folder. When this
// shared index has moved to a different folder, nothing is written.
func (ix *Index) RebuildFor(ctx context.Context, emb Embedder, id Identity, folder string) error {
	return ix.rebuild(ctx, emb, id, folder)
}

func (ix *Index) rebuild(ctx context.Context, emb Embedder, id Identity, folder string) error {
	if !ix.mu.TryLock() {
		return ErrBusy
	}
	defer ix.mu.Unlock()
	if folder != "" && ix.dir != folder {
		return fmt.Errorf("the code index follows a different folder than this session")
	}
	_, _, err := ix.refresh(ctx, emb, id, true)
	return err
}

// Search refreshes files that changed, then returns the closest files in
// cosine order. Below 20,000 stored chunks that order is an exact scan.
// At 20,000 or above it may come from the in-memory lookup, which is
// rebuilt when the stored vectors change. A refresh error with a complete
// index still on disk returns that index and says the refresh failed.
func (ix *Index) Search(ctx context.Context, emb Embedder, id Identity, query, sub string) (string, error) {
	return ix.SearchRerank(ctx, emb, id, query, sub, nil)
}

// SearchRerank is Search, then a rerank of the closest KeepChunks chunks
// when rr is non-nil. A nil rr keeps the cosine order and does not call a
// reranker. A rerank error keeps that cosine order and says the rerank failed.
func (ix *Index) SearchRerank(ctx context.Context, emb Embedder, id Identity, query, sub string, rr Reranker) (string, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.searchLocked(ctx, emb, id, query, sub, rr)
}

// SearchIn is SearchRerank for the session whose folder is folder. When this
// shared index has moved to a different folder, the result says so and the
// other folder is not read.
func (ix *Index) SearchIn(ctx context.Context, emb Embedder, id Identity, query, sub, folder string, rr Reranker) (string, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.dir != folder {
		return "The code index follows a different folder than this session.", nil
	}
	return ix.searchLocked(ctx, emb, id, query, sub, rr)
}

func (ix *Index) searchLocked(ctx context.Context, emb Embedder, id Identity, query, sub string, rr Reranker) (string, error) {
	if _, outside := relInside(ix.dir, sub); outside {
		return "", fmt.Errorf("path %s is outside the working directory", sub)
	}
	if strings.TrimSpace(id.Model) == "" {
		return "", ErrNoModel
	}
	st, ok, refreshErr := ix.refresh(ctx, emb, id, false)
	if !ok {
		if refreshErr != nil {
			return "", refreshErr
		}
		return formatResult(nil, nil), nil
	}
	rel, _ := relInside(ix.dir, sub)
	hits, rerankErr, err := ix.rank(ctx, emb, st, query, rel, rr)
	if err != nil {
		return "", err
	}
	text := formatResult(hits, func(path string, start, end int) string {
		return ix.readChunk(path, start, end, SnippetLines)
	})
	var b strings.Builder
	if refreshErr != nil {
		fmt.Fprintf(&b, "Refresh failed: %s\n\n", refreshErr.Error())
	}
	if rerankErr != nil {
		fmt.Fprintf(&b, "Rerank failed: %s\n\n", rerankErr.Error())
	}
	b.WriteString(text)
	return b.String(), nil
}

func (ix *Index) refresh(ctx context.Context, emb Embedder, id Identity, force bool) (stored, bool, error) {
	if strings.TrimSpace(id.Model) == "" {
		return stored{}, false, ErrNoModel
	}
	id.Boundaries = boundaryMode()
	if err := ctx.Err(); err != nil {
		return stored{}, false, err
	}
	unlock, created, err := ix.holdFileLock()
	if err != nil {
		return stored{}, false, err
	}
	published := false
	defer func() {
		// Delete this refresh's files while the lock is still held. When
		// the lock file is the only thing left, unlink it before releasing
		// the lock. A waiter on Unix then holds a lock on the deleted file
		// and takes the lock again. The directory is removed only when it
		// is empty, so a process that already published is left alone.
		if created && !published {
			discardUnpublished(ix.root)
			if onlyLockFile(ix.root) {
				_ = os.Remove(filepath.Join(ix.root, "index.lock"))
			}
		}
		unlock()
		// The lock file is not removed after the unlock. The directory
		// goes away only when that unlink already left it empty.
		if created && !published && dirEmpty(ix.root) {
			_ = os.Remove(ix.root)
		}
	}()

	prev := ix.Snapshot()
	metaPath, _, _ := indexPaths(ix.root)
	restoreReplaced(metaPath)
	ix.setProgress(Progress{Phase: PhaseWalking, FilesTotal: prev.FilesTotal, ChunksTotal: prev.ChunksTotal})
	files, err := walkFiles(ix.dir, "")
	if err != nil {
		return ix.fail(prev, err)
	}
	if err := ctx.Err(); err != nil {
		ix.setProgress(prev)
		return ix.loaded(false, err)
	}
	old, haveOld := loadStored(ix.root)
	reuse := haveOld && !force && old.Meta.identity().Same(id)
	var known map[chunkKey][]float32
	if reuse {
		known = indexVectors(old)
	}
	var rows []pending
	var need []int
	var texts []string
	for _, f := range files {
		parts := ChunkSource(f.Rel, f.Text, f.Hash, id.ChunkLines, id.ChunkOverlap)
		for _, c := range parts {
			row := pending{disk: diskChunk{Path: c.Path, Start: c.Start, End: c.End, Hash: c.Hash}}
			if reuse {
				if v, ok := known[chunkKey{c.Path, c.Hash, c.Start, c.End}]; ok {
					row.vec = v
				}
			}
			rows = append(rows, row)
			if row.vec == nil {
				need = append(need, len(rows)-1)
				texts = append(texts, c.Text)
			}
		}
	}
	if len(texts) == 0 && reuse && sameChunks(old, rows) {
		published = true
		return ix.notePublished(prev)
	}
	dim := 0
	if reuse {
		dim = old.Meta.Dimension
	}
	filesDone := 0
	seenFile := map[string]bool{}
	embeddedUpTo := 0
	didEmbed := len(texts) > 0
	if didEmbed {
		ix.setProgress(Progress{Phase: PhaseEmbedding, FilesTotal: len(files), ChunksTotal: len(rows)})
	}
	for len(texts) > 0 {
		n := BatchSize
		if n > len(texts) {
			n = len(texts)
		}
		if err := ctx.Err(); err != nil {
			ix.setProgress(prev)
			return ix.loaded(false, err)
		}
		got, err := emb.Embed(ctx, texts[:n])
		if err != nil {
			if ctx.Err() != nil {
				ix.setProgress(prev)
				return ix.loaded(false, ctx.Err())
			}
			return ix.fail(prev, err)
		}
		if err := ctx.Err(); err != nil {
			ix.setProgress(prev)
			return ix.loaded(false, err)
		}
		if len(got) != n {
			return ix.fail(prev, errors.New("embeddings response length mismatch"))
		}
		for i, vec := range got {
			if dim == 0 {
				dim = len(vec)
			}
			if len(vec) != dim || dim == 0 {
				return ix.fail(prev, errors.New("embedding dimension changed"))
			}
			rows[need[embeddedUpTo+i]].vec = vec
			path := rows[need[embeddedUpTo+i]].disk.Path
			if !seenFile[path] {
				seenFile[path] = true
				filesDone++
				ix.setProgress(Progress{
					Phase: PhaseEmbedding, FilesDone: filesDone, FilesTotal: len(files),
					ChunksDone: embeddedUpTo + i + 1, ChunksTotal: len(rows), Path: path,
				})
			}
		}
		texts = texts[n:]
		embeddedUpTo += n
	}
	// Count files that were reused so the snapshot reaches the total.
	if didEmbed && filesDone < len(files) {
		filesDone = len(files)
		ix.setProgress(Progress{
			Phase: PhaseEmbedding, FilesDone: filesDone, FilesTotal: len(files),
			ChunksDone: len(rows), ChunksTotal: len(rows),
		})
	}
	chunks := make([]diskChunk, len(rows))
	vectors := make([][]float32, len(rows))
	fileSet := map[string]struct{}{}
	for i, row := range rows {
		if !safeChunkPath(row.disk.Path) {
			return ix.fail(prev, fmt.Errorf("chunk path %s is outside the working directory", row.disk.Path))
		}
		if row.vec == nil {
			return ix.fail(prev, errors.New("missing embedding"))
		}
		chunks[i] = row.disk
		vectors[i] = row.vec
		fileSet[row.disk.Path] = struct{}{}
	}
	meta := metaFile{
		Provider: id.Provider, BaseURL: id.BaseURL, Model: id.Model,
		Dimension: dim, ChunkLines: id.ChunkLines, ChunkOverlap: id.ChunkOverlap,
		Boundaries: id.Boundaries,
		Files:      len(fileSet), Chunks: len(chunks),
	}
	if dim == 0 {
		meta.Dimension = 0
		meta.Files = 0
		meta.Chunks = 0
	}
	if err := ctx.Err(); err != nil {
		ix.setProgress(prev)
		return ix.loaded(false, err)
	}
	ix.setProgress(Progress{Phase: PhaseWriting, FilesDone: len(fileSet), FilesTotal: len(files), ChunksTotal: len(chunks)})
	if err := publish(ix.root, meta, chunks, vectors); err != nil {
		return ix.fail(prev, err)
	}
	published = true
	return ix.notePublished(prev)
}

// notePublished reads the generation this refresh just committed and stores
// the idle snapshot only after that read succeeds. A missing generation is
// an error on the idle line, not a success count.
func (ix *Index) notePublished(prev Progress) (stored, bool, error) {
	st, ok, err := ix.finishPublish()
	if err != nil || !ok {
		if err == nil {
			err = errors.New("code index disappeared after it was saved")
		}
		return ix.fail(prev, err)
	}
	ix.setProgress(Progress{Phase: PhaseIdle, FilesTotal: st.Meta.Files, ChunksTotal: st.Meta.Chunks, Indexed: true})
	return st, true, nil
}

// finishPublish reads the generation this refresh just committed, while the
// file lock is still held. A later sweep in another process cannot remove
// that generation out from under the caller.
func (ix *Index) finishPublish() (stored, bool, error) {
	if testAfterPublish != nil {
		testAfterPublish(ix.root)
	}
	return ix.loaded(true, nil)
}

func (ix *Index) fail(prev Progress, err error) (stored, bool, error) {
	prev.Phase = PhaseIdle
	prev.Error = err.Error()
	ix.setProgress(prev)
	return ix.loaded(false, err)
}

// loaded reads the index while the file lock is held. A successful publish
// whose files are gone is an error. A failed refresh still returns the
// previous index when one is there.
func (ix *Index) loaded(published bool, err error) (stored, bool, error) {
	st, ok := loadStored(ix.root)
	if published && !ok {
		if err == nil {
			err = errors.New("code index disappeared after it was saved")
		}
		return stored{}, false, err
	}
	return st, ok, err
}

type pending struct {
	disk diskChunk
	vec  []float32
}

func sameChunks(old stored, rows []pending) bool {
	if len(old.Chunks) != len(rows) {
		return false
	}
	for i, row := range rows {
		d := old.Chunks[i]
		if d.Path != row.disk.Path || d.Hash != row.disk.Hash || d.Start != row.disk.Start || d.End != row.disk.End {
			return false
		}
	}
	return true
}

type chunkKey struct {
	path  string
	hash  string
	start int
	end   int
}

func indexVectors(old stored) map[chunkKey][]float32 {
	m := make(map[chunkKey][]float32, len(old.Chunks))
	for i, d := range old.Chunks {
		if i >= len(old.Vectors) {
			break
		}
		m[chunkKey{d.Path, d.Hash, d.Start, d.End}] = old.Vectors[i]
	}
	return m
}

func (ix *Index) rank(ctx context.Context, emb Embedder, st stored, query, sub string, rr Reranker) ([]fileHit, error, error) {
	if len(st.Chunks) == 0 {
		return nil, nil, nil
	}
	qv, err := emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, nil, err
	}
	if len(qv) != 1 || len(qv[0]) != st.Meta.Dimension {
		return nil, nil, errors.New("query embedding dimension does not match the index")
	}
	scored := ix.scoreChunks(st, qv[0], sub)
	ordered, rerankErr := ix.rerankClosest(ctx, rr, query, scored)
	if rerankErr != nil && ctx.Err() != nil {
		return nil, nil, rerankErr
	}
	return groupHits(ordered), rerankErr, nil
}

// rerankClosest asks rr to reorder the closest KeepChunks chunks. A nil
// reranker returns the cosine list untouched. An error returns that same
// cosine list so the caller can keep it and report the failure. The request
// never includes more than KeepChunks documents.
func (ix *Index) rerankClosest(ctx context.Context, rr Reranker, query string, scored []scoredChunk) ([]scoredChunk, error) {
	if rr == nil || len(scored) == 0 {
		return scored, nil
	}
	closest := append([]scoredChunk(nil), scored...)
	sort.Slice(closest, func(i, j int) bool { return closest[i].Score > closest[j].Score })
	if len(closest) > KeepChunks {
		closest = closest[:KeepChunks]
	}
	docs := make([]string, len(closest))
	for i, c := range closest {
		docs[i] = ix.readChunk(c.Path, c.Start, c.End, 0)
	}
	hits, err := rr.Rerank(ctx, query, docs)
	if err != nil {
		return scored, err
	}
	ordered, err := orderRerank(closest, hits)
	if err != nil {
		return scored, err
	}
	return ordered, nil
}

// orderRerank applies a rerank response. The response has to name each
// document once. Anything else cannot be read, and the cosine order stands.
func orderRerank(chunks []scoredChunk, hits []RerankHit) ([]scoredChunk, error) {
	if len(hits) != len(chunks) {
		return nil, errors.New("rerank response could not be read")
	}
	seen := make([]bool, len(chunks))
	out := make([]scoredChunk, len(hits))
	for i, h := range hits {
		if h.Index < 0 || h.Index >= len(chunks) || seen[h.Index] {
			return nil, errors.New("rerank response could not be read")
		}
		seen[h.Index] = true
		c := chunks[h.Index]
		c.Score = h.Score
		out[i] = c
	}
	return out, nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		af := float64(a[i])
		bf := float64(b[i])
		dot += af * bf
		na += af * af
		nb += bf * bf
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func (ix *Index) store(p Progress) Progress {
	ix.progressSeq++
	p.Seq = ix.progressSeq
	ix.snap.Store(p)
	return p
}

func (ix *Index) setProgress(p Progress) {
	prev := PhaseIdle
	if v := ix.snap.Load(); v != nil {
		prev = v.(Progress).Phase
	}
	p = ix.store(p)
	now := time.Now()
	force := prev != p.Phase || p.Phase != PhaseEmbedding || ix.lastNotify.IsZero() || p.Error != ""
	if !force && now.Sub(ix.lastNotify) < 250*time.Millisecond {
		return
	}
	ix.lastNotify = now
	ix.kickProgress(p)
}

type progressHook struct {
	fn func(Progress)
}

// kickProgress hands p to the progress goroutine. It does not wait for the
// callback, so a host that reads the message on the same thread that is
// waiting for this index cannot deadlock the refresh.
type progressNote struct {
	seq uint64
	p   Progress
}

func (ix *Index) kickProgress(p Progress) {
	ix.progressOnce.Do(func() {
		ix.progressCh = make(chan progressNote, 64)
		go ix.deliverProgress()
	})
	note := progressNote{seq: p.Seq, p: p}
	select {
	case ix.progressCh <- note:
	default:
		cp := note
		ix.deliverMu.Lock()
		ix.progressLatest = &cp
		ix.deliverMu.Unlock()
	}
}

func (ix *Index) deliverProgress() {
	var last uint64
	for note := range ix.progressCh {
		last = ix.deliverNote(note, last)
		for {
			ix.deliverMu.Lock()
			extra := ix.progressLatest
			ix.progressLatest = nil
			ix.deliverMu.Unlock()
			if extra == nil {
				break
			}
			last = ix.deliverNote(*extra, last)
		}
	}
}

func (ix *Index) deliverNote(note progressNote, last uint64) uint64 {
	if note.seq <= last {
		return last
	}
	ix.invokeProgress(note.p)
	return note.seq
}

func (ix *Index) invokeProgress(p Progress) {
	v := ix.hook.Load()
	if v == nil {
		return
	}
	if h := v.(*progressHook); h.fn != nil {
		h.fn(p)
	}
}

func (ix *Index) holdFileLock() (func(), bool, error) {
	if !indexWithin(ix.dir, ix.root) {
		return nil, false, fmt.Errorf("code index path is outside the working directory")
	}
	if err := os.MkdirAll(filepath.Dir(ix.root), 0o755); err != nil {
		return nil, false, err
	}
	created := false
	for {
		if err := os.Mkdir(ix.root, 0o755); err != nil {
			if !os.IsExist(err) {
				return nil, false, err
			}
		} else {
			created = true
		}
		lockPath := filepath.Join(ix.root, "index.lock")
		f, err := openLockFile(lockPath)
		if err != nil {
			if created {
				removeEmptyIndex(ix.root)
			}
			return nil, false, err
		}
		unlock, err := acquireLock(f)
		if err != nil {
			f.Close()
			if created {
				removeEmptyIndex(ix.root)
			}
			return nil, false, err
		}
		// The lock is on this open file. If the path now names a different
		// file, the one we locked was removed and replaced. Release it and
		// take the path again so two processes cannot both publish.
		if lockStillAt(f, lockPath) {
			// Mkdir succeeding does not mean this directory is still ours.
			// Another process can publish while this one waits for the lock.
			created = onlyLockFile(ix.root)
			return func() {
				unlock()
				_ = f.Close()
			}, created, nil
		}
		unlock()
		_ = f.Close()
	}
}

// lockStillAt reports whether f is the file currently at path.
func lockStillAt(f *os.File, path string) bool {
	opened, err := f.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(opened, current)
}

// discardUnpublished removes everything in root except the lock file. The
// caller holds that lock. Another process waiting on it cannot enter until
// the lock is released, so these files are still this refresh's.
func discardUnpublished(root string) {
	if indexEscapes(root) {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == "index.lock" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// onlyLockFile reports whether root contains nothing except the lock file.
func onlyLockFile(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		return false
	}
	return entries[0].Name() == "index.lock"
}

// dirEmpty reports whether root has no entries. A lock file another process
// created is not empty, so the directory is left in place.
func dirEmpty(root string) bool {
	entries, err := os.ReadDir(root)
	return err == nil && len(entries) == 0
}

// removeEmptyIndex removes root when it is empty. It does not delete a lock
// file: this process does not hold the lock. Used when the directory was
// created and the lock was never acquired.
func removeEmptyIndex(root string) {
	if dirEmpty(root) {
		_ = os.Remove(root)
	}
}

// readChunk reads a stored chunk. A path that leaves the working directory
// is empty and is not opened.
func (ix *Index) readChunk(rel string, start, end, limit int) string {
	if !safeChunkPath(rel) {
		return ""
	}
	abs := filepath.Join(ix.dir, filepath.FromSlash(rel))
	if !within(ix.dir, abs) || outsideLink(ix.dir, abs) {
		return ""
	}
	return readSpan(abs, start, end, limit)
}

func readSpan(path string, start, end, limit int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end < start {
		return ""
	}
	if limit > 0 && end-start+1 > limit {
		end = start + limit - 1
	}
	return strings.Join(lines[start-1:end], "\n")
}
