//go:build cgo

package treesitter

import (
	"bytes"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// refsQueryForLang returns the compiled reference query for langName, sourced
// from the registry's inline refsQuery text and compiled through the shared
// query cache (compileQuery).
func refsQueryForLang(langName string) (*tree_sitter.Query, error) {
	registryOnce.Do(initRegistry)
	spec, ok := langSpecs[langName]
	if !ok || spec.refsQuery == "" {
		return nil, ErrUnsupported
	}
	return compileQuery(langName, "refs", spec.refsQuery)
}

// findSymbolReferences collects identifier matches for symbol, stopping at
// limit hits when limit > 0 (0 = unlimited).
func findSymbolReferences(path string, content []byte, symbol string, limit int) ([]Reference, error) {
	run, err := runQuery(path, content, refsQueryForLang)
	if err != nil {
		return nil, err
	}
	defer run.Close()

	var refs []Reference
	symbolBytes := []byte(symbol)
	for cap := range run.Captures() {
		// Compare the capture's bytes in place: Utf8Text allocated a string for
		// EVERY capture in the file just to throw most of them away (a query
		// captures identifiers, not only the symbol).
		start, end := int(cap.Node.StartByte()), int(cap.Node.EndByte())
		if start < 0 || end < start || end > len(content) {
			continue
		}
		if !bytes.Equal(content[start:end], symbolBytes) {
			continue
		}
		refs = append(refs, Reference{
			Line:  int(cap.Node.StartPosition().Row) + 1,
			Start: start,
			End:   end,
			Text:  lineTextAtOffset(content, start),
		})
		if limit > 0 && len(refs) >= limit {
			sortReferences(refs)
			return dedupeReferences(refs), nil
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	sortReferences(refs)
	return dedupeReferences(refs), nil
}

// ReferenceSearchSupported reports whether AST reference search is available for path.
func ReferenceSearchSupported(path string) bool {
	if !Enabled() {
		return false
	}
	lang, ok := langNameForPath(path)
	if !ok {
		return false
	}
	spec, ok := langSpecs[lang]
	return ok && spec.refsQuery != ""
}
