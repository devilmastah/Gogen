package codeindex

import "strings"

// lineSpan is a 1-based inclusive range in a file.
type lineSpan struct {
	Start int
	End   int
}

// Chunk is one span of a file. Lines are 1-based and inclusive.
type Chunk struct {
	Path  string
	Start int
	End   int
	Hash  string
	Text  string
}

// ChunkSource splits a file into index chunks. With tree-sitter on, and in
// a binary that includes it, a function, method, or type is one chunk, and
// a node longer than lines is split with the line window, including the
// overlap. With tree-sitter off, or a binary built without it, the whole
// file uses that line window. Lines outside a symbol are not their own chunks.
// A file with no symbols uses the line window.
func ChunkSource(path, text, hash string, lines, overlap int) []Chunk {
	if spans := symbolSpans(path, text); len(spans) > 0 {
		if chunks := chunkSpans(path, text, hash, lines, overlap, spans); len(chunks) > 0 {
			return chunks
		}
	}
	return ChunkText(path, text, hash, lines, overlap)
}

// ChunkText splits text into line windows. A file of at most lines is one
// chunk. An empty text is skipped. lines and overlap are used as given; the
// caller normalizes them.
func ChunkText(path, text, hash string, lines, overlap int) []Chunk {
	all := sourceLines(text)
	if all == nil {
		return nil
	}
	if lines < 1 {
		lines = DefaultChunkLines
	}
	if overlap < 0 || overlap >= lines {
		overlap = 0
	}
	if len(all) <= lines {
		return []Chunk{{
			Path: path, Start: 1, End: len(all), Hash: hash,
			Text: strings.Join(all, "\n"),
		}}
	}
	step := lines - overlap
	var out []Chunk
	for start := 0; start < len(all); start += step {
		end := start + lines
		if end > len(all) {
			end = len(all)
		}
		out = append(out, Chunk{
			Path: path, Start: start + 1, End: end, Hash: hash,
			Text: strings.Join(all[start:end], "\n"),
		})
		if end == len(all) {
			break
		}
	}
	return out
}

// sourceLines splits text into lines. A trailing newline is not an extra
// line, so a file of 40 lines that ends in a newline stays 40 lines.
func sourceLines(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	all := strings.Split(text, "\n")
	if len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	if len(all) == 0 {
		return nil
	}
	return all
}

// chunkSpans cuts one line window per symbol, splitting a long symbol with
// the same overlap as the file window. Line numbers stay file-absolute.
func chunkSpans(path, text, hash string, lines, overlap int, spans []lineSpan) []Chunk {
	all := sourceLines(text)
	if all == nil {
		return nil
	}
	var out []Chunk
	for _, sp := range spans {
		start, end := sp.Start, sp.End
		if start < 1 {
			start = 1
		}
		if end > len(all) {
			end = len(all)
		}
		if start > len(all) || end < start {
			continue
		}
		piece := strings.Join(all[start-1:end], "\n")
		shift := start - 1
		for _, part := range ChunkText(path, piece, hash, lines, overlap) {
			part.Start += shift
			part.End += shift
			out = append(out, part)
		}
	}
	return out
}
