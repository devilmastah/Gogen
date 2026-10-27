package treesitter

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Reference is a symbol usage location in a source file. Start and End are
// byte offsets of the identifier within the file, allowing callers to
// replace exactly the identifier span (a string literal or comment on the
// same line must not be renamed). They are zero when the backend cannot
// provide them (stub builds).
type Reference struct {
	Line  int
	Start int
	End   int
	Text  string
}

// maxReferencesPerFile bounds the reference list returned to display-only
// callers (find_symbol, call_graph, dependencies). rename uses the uncapped
// FindSymbolReferencesAll so a rewrite never covers only part of a file.
const maxReferencesPerFile = 200

// FindSymbolReferences locates identifier occurrences matching symbol in a
// source file, capped at maxReferencesPerFile. Use it for display and search
// callers, where a bounded answer is the point.
func FindSymbolReferences(path string, content []byte, symbol string) ([]Reference, error) {
	return findReferences(path, content, symbol, maxReferencesPerFile)
}

// FindSymbolReferencesAll is the uncapped variant. Callers that REWRITE the
// file (rename) must use it: a truncated reference list would silently rename
// only part of the occurrences while reporting success.
func FindSymbolReferencesAll(path string, content []byte, symbol string) ([]Reference, error) {
	return findReferences(path, content, symbol, 0)
}

func findReferences(path string, content []byte, symbol string, limit int) ([]Reference, error) {
	if err := guardEnabled(); err != nil {
		return nil, err
	}
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	return findSymbolReferences(path, content, symbol, limit)
}

// FormatReferenceMatches renders references in search_code-style lines.
func FormatReferenceMatches(relPath string, refs []Reference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s:%d:%s", relPath, r.Line, r.Text))
	}
	return out
}

func sortReferences(refs []Reference) {
	slices.SortFunc(refs, func(a, b Reference) int {
		return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End))
	})
}

func dedupeReferences(refs []Reference) []Reference {
	seen := make(map[[2]int]struct{}, len(refs))
	out := make([]Reference, 0, len(refs))
	for _, r := range refs {
		key := [2]int{r.Start, r.End}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}

// lineTextAtOffset returns the trimmed text of the line containing byte offset
// off. It scans out from the offset, which is O(line): the previous version
// split the WHOLE file for every reference, so a 1 MB file with 200 hits
// allocated ~200 MB of garbage per search.
func lineTextAtOffset(content []byte, off int) string {
	if off < 0 {
		off = 0
	}
	if off > len(content) {
		off = len(content)
	}
	start := bytes.LastIndexByte(content[:off], '\n') + 1
	end := len(content)
	if i := bytes.IndexByte(content[off:], '\n'); i >= 0 {
		end = off + i
	}
	return strings.TrimSpace(string(content[start:end]))
}
