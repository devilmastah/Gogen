package codeindex

import (
	"bufio"
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
// lock and is never taken while holding an agent lock. Callers take that
// lock only through the methods that touch the store or the watch: Search,
// SearchRerank, SearchIn, Rebuild, RebuildFor, SetWorkingDir, Dir, Watching,
// and SetWatch. Snapshot and SetProgress are atomic. The progress callback
// runs on the index's own goroutine, after the lock is released for the
// callback. deliverMu is taken only to hand the callback the newest snapshot,
// and the callback runs after deliverMu is released. The watch goroutine
// takes the lock only to publish its ready flag and, with TryLock, around
// the same refresh Search uses.
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
	// approxMinChunks. approxGen names the stored generation it was built
	// from, so a cached generation is not hashed again. approxUses counts
	// searches that consulted it. Tests read it.
	approx     *hnsw
	approxGen  string
	approxUses int

	// cached is the last parsed generation, guarded by mu. It keeps a
	// search from re-reading chunks.json and vectors.bin when the meta
	// still names the same generation. A legacy index (no generation
	// names) is not cached.
	cached *stored
}

// Open returns the index for workingDir. It does not create .gogen/index.
// The stored meta is read so the snapshot shows its counts; the vector
// generation is loaded on the first search, or rebuild, not here.
func Open(workingDir string) *Index {
	ix := &Index{dir: workingDir, root: filepath.Join(workingDir, ".gogen", "index")}
	ix.dirSnap.Store(workingDir)
	ix.store(Progress{Phase: PhaseIdle})
	if meta, ok := loadMeta(ix.root); ok {
		ix.store(Progress{
			Phase:       PhaseIdle,
			FilesTotal:  meta.Files,
			ChunksTotal: meta.Chunks,
			Indexed:     true,
		})
	}
	return ix
}

// SetWorkingDir points the index at another folder. It does not create
// .gogen/index. The same folder returns without waiting. A different folder
// waits until a rebuild finishes. The previous folder's cache and lookup are
// dropped. The stored meta at the new folder is read for the snapshot; its
// vectors are loaded on the next search.
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
	ix.cached = nil
	ix.approx = nil
	ix.approxGen = ""
	ix.setProgress(Progress{Phase: PhaseIdle})
	if meta, ok := loadMeta(ix.root); ok {
		ix.setProgress(Progress{Phase: PhaseIdle, FilesTotal: meta.Files, ChunksTotal: meta.Chunks, Indexed: true})
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
	walkRoot, rels, err := walkFiles(ix.dir)
	if err != nil {
		return ix.fail(prev, err)
	}
	ix.setProgress(Progress{Phase: PhaseWalking, FilesTotal: len(rels), ChunksTotal: prev.ChunksTotal})
	if err := ctx.Err(); err != nil {
		ix.setProgress(prev)
		return ix.loaded(false, err)
	}
	old, haveOld := ix.loadStoredLocked()
	reuse := haveOld && !force && old.Meta.identity().Same(id)
	var known map[chunkKey][]float32
	if reuse {
		known = indexVectors(old)
	}
	root, err := os.OpenRoot(walkRoot)
	if err != nil {
		return ix.fail(prev, err)
	}
	defer root.Close()

	var rows []pending
	var pendingRows []int
	var texts []string
	dim := 0
	if reuse {
		dim = old.Meta.Dimension
	}
	filesDone := 0
	seenFile := map[string]bool{}
	didEmbed := false

	// flush embeds the queued batch and fills the rows it belongs to. The
	// queue is at most BatchSize chunks, so the scan never holds every file
	// and chunk text in memory at once.
	flush := func() error {
		if len(texts) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		got, err := emb.Embed(ctx, texts)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(got) != len(texts) {
			return errors.New("embeddings response length mismatch")
		}
		for i, vec := range got {
			if dim == 0 {
				dim = len(vec)
			}
			if len(vec) != dim || dim == 0 {
				return errors.New("embedding dimension changed")
			}
			if dim > maxDimension || int64(len(rows))*int64(dim)*4 > maxVectorsFileBytes {
				return errors.New("code index is too large")
			}
			row := &rows[pendingRows[i]]
			row.vec = vec
			path := row.disk.Path
			if !seenFile[path] {
				seenFile[path] = true
				filesDone++
				ix.setProgress(Progress{
					Phase: PhaseEmbedding, FilesDone: filesDone, FilesTotal: len(rels),
					ChunksDone: len(rows), ChunksTotal: len(rows), Path: path,
				})
			}
		}
		texts = texts[:0]
		pendingRows = pendingRows[:0]
		return nil
	}
	handleFlushError := func(err error) (stored, bool, error) {
		if ctx.Err() != nil {
			ix.setProgress(prev)
			return ix.loaded(false, ctx.Err())
		}
		return ix.fail(prev, err)
	}

	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			ix.setProgress(prev)
			return ix.loaded(false, err)
		}
		text, ok := readIndexable(root, rel)
		if !ok {
			continue
		}
		parts := ChunkSource(rel, text, id.ChunkLines, id.ChunkOverlap)
		for _, c := range parts {
			row := pending{disk: diskChunk{Path: c.Path, Start: c.Start, End: c.End, Hash: c.Hash}}
			if reuse {
				if v, ok := known[chunkKey{Path: c.Path, Hash: c.Hash}]; ok {
					row.vec = v
				}
			}
			rows = append(rows, row)
			if len(rows) > maxChunks {
				return ix.fail(prev, errors.New("code index is too large"))
			}
			if row.vec == nil {
				if !didEmbed {
					didEmbed = true
					ix.setProgress(Progress{Phase: PhaseEmbedding, FilesTotal: len(rels)})
				}
				pendingRows = append(pendingRows, len(rows)-1)
				texts = append(texts, c.Text)
				if len(texts) >= BatchSize {
					if err := flush(); err != nil {
						return handleFlushError(err)
					}
				}
			}
		}
	}
	if err := flush(); err != nil {
		return handleFlushError(err)
	}
	if !didEmbed && reuse && sameChunks(old, rows) {
		// Nothing changed. A legacy index is migrated once to a named
		// generation, so later searches can use the cached generation.
		if old.Gen == "" {
			if err := publish(ix.root, old.Meta, old.Chunks, old.Vectors); err != nil {
				return ix.fail(prev, err)
			}
			published = true
			return ix.notePublished(prev)
		}
		published = true
		ix.setProgress(Progress{Phase: PhaseIdle, FilesTotal: old.Meta.Files, ChunksTotal: old.Meta.Chunks, Indexed: true})
		return old, true, nil
	}
	// Count files whose chunks were reused. The embedding progress above
	// counts only files that had a chunk to embed.
	if didEmbed && filesDone < len(rels) {
		filesDone = len(rels)
		ix.setProgress(Progress{
			Phase: PhaseEmbedding, FilesDone: filesDone, FilesTotal: len(rels),
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
	ix.setProgress(Progress{Phase: PhaseWriting, FilesDone: len(fileSet), FilesTotal: len(rels), ChunksTotal: len(chunks)})
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
	st, ok := ix.loadStoredLocked()
	if published && !ok {
		if err == nil {
			err = errors.New("code index disappeared after it was saved")
		}
		return stored{}, false, err
	}
	return st, ok, err
}

// loadStoredLocked returns the generation the stored meta names, reusing the
// parsed copy when the generation names have not changed. The caller holds
// mu (or is Open, before the index is shared). A legacy index, whose files
// are not generation-named, is read every time.
func (ix *Index) loadStoredLocked() (stored, bool) {
	meta, ok := loadMeta(ix.root)
	if !ok {
		ix.cached = nil
		return stored{}, false
	}
	gen := ""
	if meta.ChunksFile != "" && meta.VectorsFile != "" {
		gen = meta.ChunksFile + "\x00" + meta.VectorsFile
	}
	if gen != "" && ix.cached != nil && ix.cached.Gen == gen {
		return *ix.cached, true
	}
	st, ok := loadStoredWithMeta(ix.root, meta)
	if !ok {
		ix.cached = nil
		return stored{}, false
	}
	if st.Gen != "" {
		cp := st
		ix.cached = &cp
	}
	return st, true
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

// chunkKey identifies chunk content in one file. Lines are not part of it:
// the same text at a new line range keeps its vector, and the new range is
// written with the published row.
type chunkKey struct {
	Path string
	Hash string
}

func indexVectors(old stored) map[chunkKey][]float32 {
	m := make(map[chunkKey][]float32, len(old.Chunks))
	for i, d := range old.Chunks {
		if i >= len(old.Vectors) {
			break
		}
		m[chunkKey{Path: d.Path, Hash: d.Hash}] = old.Vectors[i]
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

// maxSnippetBytes caps one readChunk result. A chunk is at most a few
// hundred lines; this keeps a swapped-in huge file from being loaded.
const maxSnippetBytes = 4 << 20

// readChunk reads a stored chunk through a root, so a link that leaves the
// working directory is rejected rather than followed. A path that is missing,
// not a regular file, or outside the working directory is empty.
func (ix *Index) readChunk(rel string, start, end, limit int) string {
	if !safeChunkPath(rel) {
		return ""
	}
	root, err := os.OpenRoot(ix.dir)
	if err != nil {
		return ""
	}
	defer root.Close()
	name := filepath.FromSlash(rel)
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := root.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	return readSpanFile(f, start, end, limit)
}

// readSpanFile returns lines start..end, capped at limit lines and
// maxSnippetBytes. It reads only the requested lines, so a large file is not
// loaded just to show a snippet.
func readSpanFile(f *os.File, start, end, limit int) string {
	if start < 1 {
		start = 1
	}
	if end < start {
		return ""
	}
	if limit > 0 && end-start+1 > limit {
		end = start + limit - 1
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxSnippetBytes)
	line := 0
	for sc.Scan() {
		line++
		if line < start {
			continue
		}
		if line > end {
			break
		}
		text := sc.Text()
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if b.Len()+len(text) > maxSnippetBytes {
			break
		}
		b.WriteString(text)
	}
	return b.String()
}
