//go:build !cgo

package codeindex

// boundaryMode is the line window. This binary has no tree-sitter, so the
// treesitter setting does not change the split.
func boundaryMode() string {
	return BoundariesLines
}

func symbolSpans(path, text string) []lineSpan {
	return nil
}
