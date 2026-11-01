package codeindex

import (
	"context"
	"strconv"
)

// Chunk sizing defaults. A value outside the accepted range is replaced by
// these in config.NormalizeCodeIndexChunk, which is the one check.
const (
	DefaultChunkLines   = 40
	DefaultChunkOverlap = 5
	MinChunkLines       = 10
	MaxChunkLines       = 200
	// BatchSize is the most chunks sent in one embeddings request.
	BatchSize = 32
	// KeepChunks is how many nearest chunks are grouped into the result.
	KeepChunks = 24
	// MaxFiles is how many files the result includes.
	MaxFiles = 8
	// SnippetLines is how many lines of a working-directory range are shown.
	SnippetLines = 20
	// MaxFileBytes skips a file larger than this. A file of exactly this size
	// is indexed.
	MaxFileBytes = 1 << 20
	// ProbeBytes is how much of a file is read when looking for a NUL.
	ProbeBytes = 8 << 10
	// BoundariesLines is the line window. An older index with no boundaries
	// field is this mode.
	BoundariesLines = "lines"
	// BoundariesSymbols is a function, method, or type. A longer node is
	// split with the line window.
	BoundariesSymbols = "symbols"
)

// Embedder turns texts into vectors. Tests pass a fake. The provider adapter
// is the production implementation.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// RerankHit is one document in a rerank response. Index is into the
// documents that were sent, and Score is that document's relevance.
type RerankHit struct {
	Index int
	Score float64
}

// Reranker reorders the closest chunks. A nil reranker leaves the cosine
// order and is not called.
type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string) ([]RerankHit, error)
}

// Identity is what a stored index was built with. A difference from the
// current identity rebuilds. The API key is not part of it. Boundaries is
// lines or symbols. Empty means lines, which is what an index written
// before that field stored.
type Identity struct {
	Provider     string
	BaseURL      string
	Model        string
	ChunkLines   int
	ChunkOverlap int
	Boundaries   string
}

// canonicalBoundaries treats an empty value as the line window.
func canonicalBoundaries(s string) string {
	if s == BoundariesSymbols {
		return BoundariesSymbols
	}
	return BoundariesLines
}

// Same reports whether both identities would mix vectors.
func (id Identity) Same(other Identity) bool {
	return id.Provider == other.Provider &&
		id.BaseURL == other.BaseURL &&
		id.Model == other.Model &&
		id.ChunkLines == other.ChunkLines &&
		id.ChunkOverlap == other.ChunkOverlap &&
		canonicalBoundaries(id.Boundaries) == canonicalBoundaries(other.Boundaries)
}

// Phase is the progress snapshot's stage.
type Phase string

const (
	PhaseIdle      Phase = "idle"
	PhaseWalking   Phase = "walking"
	PhaseEmbedding Phase = "embedding"
	PhaseWriting   Phase = "writing"
)

// Progress is the shared index's current snapshot. It never holds file contents.
type Progress struct {
	Phase       Phase
	FilesDone   int
	FilesTotal  int
	ChunksDone  int
	ChunksTotal int
	Path        string
	Error       string
	// Indexed is set once a meta.json has been loaded or published.
	// Idle with Indexed false is "Not indexed", including when the counts
	// are zero because nothing has been written yet.
	Indexed bool
	// Seq orders this snapshot against a config frame. A host keeps the
	// line only when the sequence is newer than the one it already showed.
	Seq uint64
}

// Line is the sentence the settings page, the tool card, and /reindex show.
func (p Progress) Line() string {
	if p.Error != "" && p.Phase == PhaseIdle {
		return p.Error
	}
	switch p.Phase {
	case PhaseWalking:
		return "Scanning files"
	case PhaseEmbedding:
		return "Embedding " + strconv.Itoa(p.FilesDone) + "/" + strconv.Itoa(p.FilesTotal) + " files"
	case PhaseWriting:
		return "Saving the index"
	default:
		if !p.Indexed {
			return "Not indexed"
		}
		return strconv.Itoa(p.FilesTotal) + " files, " + strconv.Itoa(p.ChunksTotal) + " chunks"
	}
}
