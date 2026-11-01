//go:build cgo

package codeindex

import "gogen/internal/treesitter"

// boundaryMode is symbols while tree-sitter is on. The line window is the
// other mode, including a binary that has tree-sitter compiled out.
func boundaryMode() string {
	if treesitter.Enabled() {
		return BoundariesSymbols
	}
	return BoundariesLines
}

func symbolSpans(path, text string) []lineSpan {
	if !treesitter.Enabled() {
		return nil
	}
	spans, err := treesitter.SymbolSpans(path, []byte(text))
	if err != nil || len(spans) == 0 {
		return nil
	}
	out := make([]lineSpan, 0, len(spans))
	for _, sp := range spans {
		if sp.Start < 1 || sp.End < sp.Start {
			continue
		}
		out = append(out, lineSpan{Start: sp.Start, End: sp.End})
	}
	return out
}
