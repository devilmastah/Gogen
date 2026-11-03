package codeindex

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Index file limits. They are far above a normal index and keep a corrupt,
// crafted, or swapped file from exhausting memory or blocking a read.
const (
	// maxMetaBytes caps meta.json.
	maxMetaBytes = 1 << 20
	// maxChunksFileBytes caps chunks.json.
	maxChunksFileBytes = 256 << 20
	// maxVectorsFileBytes caps vectors.bin (1 GiB).
	maxVectorsFileBytes = 1 << 30
	// maxChunks caps the chunk count so the size product stays in int64.
	maxChunks = 2_000_000
	// maxDimension caps one vector. Embedding models are far below this.
	maxDimension = 65536
)

// metaFile is .gogen/index/meta.json. It does not record the API key.
type metaFile struct {
	Provider     string `json:"provider"`
	BaseURL      string `json:"baseUrl"`
	Model        string `json:"model"`
	Dimension    int    `json:"dimension"`
	ChunkLines   int    `json:"chunkLines"`
	ChunkOverlap int    `json:"chunkOverlap"`
	Boundaries   string `json:"boundaries,omitempty"`
	Files        int    `json:"files"`
	Chunks       int    `json:"chunks"`
	// ChunksFile and VectorsFile name the generation this meta commits.
	// Empty means the original chunks.json and vectors.bin.
	ChunksFile  string `json:"chunksFile,omitempty"`
	VectorsFile string `json:"vectorsFile,omitempty"`
}

func (m metaFile) identity() Identity {
	return Identity{
		Provider:     m.Provider,
		BaseURL:      m.BaseURL,
		Model:        m.Model,
		ChunkLines:   m.ChunkLines,
		ChunkOverlap: m.ChunkOverlap,
		Boundaries:   m.Boundaries,
	}
}

// diskChunk is one row of chunks.json. Text is not stored.
type diskChunk struct {
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Hash  string `json:"hash"`
}

type stored struct {
	Meta   metaFile
	Chunks []diskChunk
	// Vectors is indexed in the same order as Chunks.
	Vectors [][]float32
	// Gen names the on-disk generation (the chunk file plus the vector
	// file). It is empty for a legacy index that still uses chunks.json
	// and vectors.bin, which cannot be cached safely by name.
	Gen string
}

func indexPaths(root string) (meta, chunks, vectors string) {
	return filepath.Join(root, "meta.json"),
		filepath.Join(root, "chunks.json"),
		filepath.Join(root, "vectors.bin")
}

// loadMeta reads and validates meta.json only. It is what Open uses to
// show the stored counts without reading the vector generation.
func loadMeta(root string) (metaFile, bool) {
	// A root reached through a link that leaves the working directory is
	// not this folder's index.
	if indexEscapes(root) {
		return metaFile{}, false
	}
	// A missing meta.json stays missing. Putting meta.json.bak back here
	// races a publisher that has already installed the new meta: on Linux
	// and macOS that rename replaces it, and the sweep then deletes the
	// files the restored meta names. Recovery runs under the file lock.
	r, err := os.OpenRoot(root)
	if err != nil {
		return metaFile{}, false
	}
	defer r.Close()
	mb, err := readIndexFile(r, "meta.json", maxMetaBytes)
	if err != nil {
		return metaFile{}, false
	}
	var meta metaFile
	if json.Unmarshal(mb, &meta) != nil || !saneMeta(meta) {
		return metaFile{}, false
	}
	return meta, true
}

// saneMeta bounds every number the loader multiplies or allocates from, so a
// corrupt or crafted meta.json cannot overflow int64 or ask for a huge slice.
func saneMeta(meta metaFile) bool {
	if meta.Files < 0 || meta.Chunks < 0 || meta.Chunks > maxChunks {
		return false
	}
	if meta.Dimension < 0 || meta.Dimension > maxDimension {
		return false
	}
	if meta.Chunks > 0 && meta.Dimension <= 0 {
		return false
	}
	_, _, ok := generationNames(meta)
	return ok
}

func loadStored(root string) (stored, bool) {
	meta, ok := loadMeta(root)
	if !ok {
		return stored{}, false
	}
	return loadStoredWithMeta(root, meta)
}

// loadStoredWithMeta reads the generation meta names. Every file is opened
// through an os.Root, so a symlink that leaves the working directory is
// rejected instead of followed, and every read is bounded.
func loadStoredWithMeta(root string, meta metaFile) (stored, bool) {
	if meta.Chunks == 0 {
		return stored{Meta: meta}, true
	}
	chunksName, vectorsName, ok := generationNames(meta)
	if !ok {
		return stored{}, false
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return stored{}, false
	}
	defer r.Close()
	cb, err := readIndexFile(r, chunksName, maxChunksFileBytes)
	if err != nil {
		return stored{}, false
	}
	var chunks []diskChunk
	if json.Unmarshal(cb, &chunks) != nil || len(chunks) != meta.Chunks {
		return stored{}, false
	}
	for _, c := range chunks {
		if !safeChunkPath(c.Path) || c.Start < 0 || c.End < 0 {
			return stored{}, false
		}
	}
	want := int64(len(chunks)) * int64(meta.Dimension) * 4
	if want <= 0 || want > maxVectorsFileBytes {
		return stored{}, false
	}
	vb, err := readIndexFile(r, vectorsName, want)
	if err != nil || int64(len(vb)) != want {
		return stored{}, false
	}
	width := meta.Dimension
	vectors := make([][]float32, len(chunks))
	for i := range chunks {
		row := make([]float32, width)
		for j := 0; j < width; j++ {
			off := (i*width + j) * 4
			row[j] = math.Float32frombits(binary.LittleEndian.Uint32(vb[off : off+4]))
		}
		vectors[i] = row
	}
	gen := ""
	if meta.ChunksFile != "" && meta.VectorsFile != "" {
		gen = meta.ChunksFile + "\x00" + meta.VectorsFile
	}
	return stored{Meta: meta, Chunks: chunks, Vectors: vectors, Gen: gen}, true
}

// readIndexFile reads one regular file in the index, bounded by limit. A
// symlink (to a file inside or outside the root) is not a regular file and
// is rejected. Root never follows a link that leaves its directory.
func readIndexFile(r *os.Root, name string, limit int64) ([]byte, error) {
	info, err := r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("%s is too large", name)
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is too large", name)
	}
	return body, nil
}

// publish writes the new chunks and vectors beside the files the current
// meta.json names, then replaces meta.json last. A crash before that
// replacement leaves the previous index in place. The previous generation
// is removed only after the new meta.json is in place.
func publish(root string, meta metaFile, chunks []diskChunk, vectors [][]float32) error {
	if indexEscapes(root) {
		return errors.New("code index path is outside the working directory")
	}
	if len(chunks) != len(vectors) || len(chunks) > maxChunks || meta.Chunks != len(chunks) || meta.Files < 0 ||
		meta.Dimension < 0 || meta.Dimension > maxDimension ||
		int64(len(chunks))*int64(meta.Dimension)*4 > maxVectorsFileBytes {
		return errors.New("code index is too large")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// Every write goes through an os.Root. A symlink planted at a fixed
	// name (meta.json.tmp) is removed instead of followed, and Root refuses
	// a link that leaves the index directory.
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := restoreReplacedRoot(r); err != nil {
		return err
	}
	gen, err := newGeneration()
	if err != nil {
		return err
	}
	meta.ChunksFile = "chunks-" + gen + ".json"
	meta.VectorsFile = "vectors-" + gen + ".bin"
	cb, err := json.MarshalIndent(chunks, "", "  ")
	if err != nil {
		return err
	}
	var buf []byte
	for _, row := range vectors {
		if len(row) != meta.Dimension {
			return errors.New("vector dimension does not match the index")
		}
		for _, v := range row {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
			buf = append(buf, b[:]...)
		}
	}
	if err := writeNewFile(r, meta.ChunksFile, append(cb, '\n')); err != nil {
		return err
	}
	if err := writeNewFile(r, meta.VectorsFile, buf); err != nil {
		_ = r.Remove(meta.ChunksFile)
		return err
	}
	mb, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		_ = r.Remove(meta.ChunksFile)
		_ = r.Remove(meta.VectorsFile)
		return err
	}
	tmp := "meta.json.tmp"
	if err := writeNewFile(r, tmp, append(mb, '\n')); err != nil {
		_ = r.Remove(meta.ChunksFile)
		_ = r.Remove(meta.VectorsFile)
		return err
	}
	if err := replaceFileRoot(r, tmp, "meta.json"); err != nil {
		_ = r.Remove(meta.ChunksFile)
		_ = r.Remove(meta.VectorsFile)
		_ = r.Remove(tmp)
		return err
	}
	sweepGenerations(root, meta.ChunksFile, meta.VectorsFile)
	return nil
}

// writeNewFile creates a generation file. An existing name (a leftover or a
// planted symlink) is removed first, so the write never follows a link. The
// generation names are random, so an existing regular file is not expected.
func writeNewFile(r *os.Root, name string, body []byte) error {
	if info, err := r.Lstat(name); err == nil {
		if info.IsDir() {
			return fmt.Errorf("%s is a directory", name)
		}
		if err := r.Remove(name); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := r.Create(name)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = r.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		_ = r.Remove(name)
		return err
	}
	return nil
}

// restoreReplacedRoot is restoreReplaced for a Root: a crash between the two
// renames of replaceFileRoot leaves only meta.json.bak.
func restoreReplacedRoot(r *os.Root) error {
	if _, err := r.Lstat("meta.json"); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := r.Lstat("meta.json.bak"); err == nil {
		return r.Rename("meta.json.bak", "meta.json")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// replaceFileRoot puts tmp in dst's place through a Root. On Windows a rename
// does not replace, so dst is renamed aside first and put back if the new
// name cannot be moved in.
func replaceFileRoot(r *os.Root, tmp, dst string) error {
	bak := dst + ".bak"
	if _, err := r.Lstat(dst); err == nil {
		_ = r.Remove(bak)
		if err := r.Rename(dst, bak); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.Rename(tmp, dst); err != nil {
		if _, statErr := r.Lstat(bak); statErr == nil {
			_ = r.Rename(bak, dst)
		}
		return err
	}
	_ = r.Remove(bak)
	return nil
}

// generationNames resolves the chunk and vector files meta commits.
// An older index names neither and uses chunks.json and vectors.bin.
func generationNames(meta metaFile) (chunks, vectors string, ok bool) {
	chunks = meta.ChunksFile
	if chunks == "" {
		chunks = "chunks.json"
	}
	vectors = meta.VectorsFile
	if vectors == "" {
		vectors = "vectors.bin"
	}
	if !indexFileName(chunks) || !indexFileName(vectors) {
		return "", "", false
	}
	return chunks, vectors, true
}

func indexFileName(name string) bool {
	return name != "" && name == filepath.Base(name) && !strings.Contains(name, "..")
}

// safeChunkPath reports whether p is a relative path inside the working
// directory. An absolute path, or one that climbs out, is not.
func safeChunkPath(p string) bool {
	if p == "" || strings.Contains(p, "\x00") || volumePath(p) {
		return false
	}
	if filepath.IsAbs(p) {
		return false
	}
	// A stored path is relative. A backslash separates names on Windows, so
	// it counts as a separator here too. "..\x" then climbs out on every
	// system, including one where a backslash is otherwise just a character.
	slashed := strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
	if path.IsAbs(slashed) {
		return false
	}
	c := path.Clean(slashed)
	return c != "." && c != ".." && !strings.HasPrefix(c, "../")
}

// volumePath reports a Windows drive path. On Windows a colon is never a
// file name. Elsewhere only a drive letter followed by a colon is rejected,
// so a file named foo:bar.txt stays inside the working directory.
func volumePath(p string) bool {
	if runtime.GOOS == "windows" {
		return strings.Contains(p, ":")
	}
	return len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0])
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func newGeneration() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func sweepGenerations(root, chunksName, vectorsName string) {
	if indexEscapes(root) {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == chunksName || name == vectorsName || name == "meta.json" || name == "index.lock" {
			continue
		}
		if name == "chunks.json" || name == "vectors.bin" || name == "meta.json.bak" || name == "meta.json.tmp" ||
			strings.HasPrefix(name, "chunks-") || strings.HasPrefix(name, "vectors-") {
			_ = os.Remove(filepath.Join(root, name))
		}
	}
}

// restoreReplaced puts a meta.json back when a crash left only its .bak.
func restoreReplaced(dst string) {
	if _, err := os.Stat(dst); err == nil {
		return
	}
	bak := dst + ".bak"
	if _, err := os.Stat(bak); err == nil {
		_ = os.Rename(bak, dst)
	}
}

// replaceFile puts tmp in dst's place. The previous dst is renamed aside
// first and put back if the new name cannot be moved in, so a failure does
// not delete the only good copy. On Windows a rename does not replace.
func replaceFile(tmp, dst string) error {
	bak := dst + ".bak"
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		restoreReplaced(dst)
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(bak)
		if err := os.Rename(dst, bak); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		if _, statErr := os.Stat(bak); statErr == nil {
			_ = os.Rename(bak, dst)
		}
		return err
	}
	_ = os.Remove(bak)
	return nil
}
