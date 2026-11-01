package codeindex

import (
	"fmt"
	"sort"
	"strings"
)

type scoredChunk struct {
	Chunk
	Score float64
}

type fileHit struct {
	Path   string
	Score  float64
	Ranges []scoredChunk
}

// confidence is the one cutoff function. No files is unknown. A top score
// below 0.40 is low. A top score of at least 0.40 that leads the next file
// by 0.15, or a single file at that score, is high. Anything else is medium.
func confidence(files []fileHit) string {
	if len(files) == 0 {
		return "unknown"
	}
	top := files[0].Score
	if top < 0.40 {
		return "low"
	}
	if len(files) == 1 || top-files[1].Score >= 0.15 {
		return "high"
	}
	return "medium"
}

// groupHits merges overlapping or touching ranges in the same file, keeps the
// higher score, and returns at most MaxFiles files, best score first.
func groupHits(chunks []scoredChunk) []fileHit {
	if len(chunks) > KeepChunks {
		sort.Slice(chunks, func(i, j int) bool { return chunks[i].Score > chunks[j].Score })
		chunks = chunks[:KeepChunks]
	}
	byPath := map[string][]scoredChunk{}
	var order []string
	for _, c := range chunks {
		if _, ok := byPath[c.Path]; !ok {
			order = append(order, c.Path)
		}
		byPath[c.Path] = append(byPath[c.Path], c)
	}
	var files []fileHit
	for _, path := range order {
		ranges := mergeRanges(byPath[path])
		score := ranges[0].Score
		for _, r := range ranges[1:] {
			if r.Score > score {
				score = r.Score
			}
		}
		files = append(files, fileHit{Path: path, Score: score, Ranges: ranges})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Score == files[j].Score {
			return files[i].Path < files[j].Path
		}
		return files[i].Score > files[j].Score
	})
	if len(files) > MaxFiles {
		files = files[:MaxFiles]
	}
	return files
}

func mergeRanges(in []scoredChunk) []scoredChunk {
	sort.Slice(in, func(i, j int) bool { return in[i].Start < in[j].Start })
	out := []scoredChunk{in[0]}
	for _, c := range in[1:] {
		last := &out[len(out)-1]
		if c.Start <= last.End+1 {
			if c.End > last.End {
				last.End = c.End
				last.Text = ""
			}
			if c.Score > last.Score {
				last.Score = c.Score
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

// formatResult renders the tool result. linesOf returns the file's lines for
// a range, capped at SnippetLines. A missing file yields an empty body.
func formatResult(files []fileHit, linesOf func(path string, start, end int) string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Confidence: %s\n", confidence(files))
	if len(files) == 0 {
		return b.String()
	}
	for i, f := range files {
		fmt.Fprintf(&b, "\n%d. %s (score: %.2f)\n", i+1, f.Path, f.Score)
		for _, r := range f.Ranges {
			fmt.Fprintf(&b, "%d-%d:\n", r.Start, r.End)
			if text := linesOf(f.Path, r.Start, r.End); text != "" {
				b.WriteString(text)
				if !strings.HasSuffix(text, "\n") {
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}
