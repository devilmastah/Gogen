package codeindex

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
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
	Meta    metaFile
	Chunks  []diskChunk
	Vectors [][]float32
}

func indexPaths(root string) (meta, chunks, vectors string) {
	return filepath.Join(root, "meta.json"),
		filepath.Join(root, "chunks.json"),
		filepath.Join(root, "vectors.bin")
}

func loadStored(root string) (stored, bool) {
	// A root reached through a link that leaves the working directory is
	// not this folder's index. Reading it would show the other folder's
	// counts as if they belonged here.
	if indexEscapes(root) {
		return stored{}, false
	}
	// A missing meta.json stays missing. Putting meta.json.bak back here
	// races a publisher that has already installed the new meta: on Linux
	// and macOS that rename replaces it, and the sweep then deletes the
	// files the restored meta names. Recovery runs under the file lock.
	metaPath, _, _ := indexPaths(root)
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return stored{}, false
	}
	var meta metaFile
	if json.Unmarshal(mb, &meta) != nil || meta.Chunks < 0 {
		return stored{}, false
	}
	if meta.Chunks == 0 {
		return stored{Meta: meta}, true
	}
	if meta.Dimension <= 0 {
		return stored{}, false
	}
	chunksName, vectorsName, ok := generationNames(meta)
	if !ok {
		return stored{}, false
	}
	cb, err := os.ReadFile(filepath.Join(root, chunksName))
	if err != nil {
		return stored{}, false
	}
	var chunks []diskChunk
	if json.Unmarshal(cb, &chunks) != nil || len(chunks) != meta.Chunks {
		return stored{}, false
	}
	for _, c := range chunks {
		if !safeChunkPath(c.Path) {
			return stored{}, false
		}
	}
	vb, err := os.ReadFile(filepath.Join(root, vectorsName))
	if err != nil {
		return stored{}, false
	}
	width := meta.Dimension
	if len(vb) != len(chunks)*width*4 {
		return stored{}, false
	}
	vectors := make([][]float32, len(chunks))
	for i := range chunks {
		row := make([]float32, width)
		for j := 0; j < width; j++ {
			off := (i*width + j) * 4
			row[j] = math.Float32frombits(binary.LittleEndian.Uint32(vb[off : off+4]))
		}
		vectors[i] = row
	}
	return stored{Meta: meta, Chunks: chunks, Vectors: vectors}, true
}

// publish writes the new chunks and vectors beside the files the current
// meta.json names, then replaces meta.json last. A crash before that
// replacement leaves the previous index in place. The previous generation
// is removed only after the new meta.json is in place.
func publish(root string, meta metaFile, chunks []diskChunk, vectors [][]float32) error {
	if indexEscapes(root) {
		return errors.New("code index path is outside the working directory")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	metaPath, _, _ := indexPaths(root)
	restoreReplaced(metaPath)
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
	chunksPath := filepath.Join(root, meta.ChunksFile)
	vecPath := filepath.Join(root, meta.VectorsFile)
	if err := writeFile(chunksPath, append(cb, '\n')); err != nil {
		return err
	}
	if err := writeFile(vecPath, buf); err != nil {
		_ = os.Remove(chunksPath)
		return err
	}
	mb, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		_ = os.Remove(chunksPath)
		_ = os.Remove(vecPath)
		return err
	}
	tmp := metaPath + ".tmp"
	if err := writeFile(tmp, append(mb, '\n')); err != nil {
		_ = os.Remove(chunksPath)
		_ = os.Remove(vecPath)
		return err
	}
	if err := replaceFile(tmp, metaPath); err != nil {
		_ = os.Remove(chunksPath)
		_ = os.Remove(vecPath)
		_ = os.Remove(tmp)
		return err
	}
	sweepGenerations(root, meta.ChunksFile, meta.VectorsFile)
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

func writeFile(path string, body []byte) error {
	return os.WriteFile(path, body, 0o644)
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
