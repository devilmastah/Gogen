package treesitter

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var (
	ErrDisabled    = errors.New("tree-sitter is disabled (set GOGEN_TREESITTER=on or unset GOGEN_TREESITTER)")
	ErrUnsupported = errors.New("no definition query for this file type")
)

// guardEnabled returns ErrDisabled when tree-sitter is disabled, nil otherwise.
func guardEnabled() error {
	if !Enabled() {
		return ErrDisabled
	}
	return nil
}

// Definition is a named symbol outline entry.
type Definition struct {
	Line int
	Kind string
	Name string
}

// Span is one function, method, or type. Lines are 1-based and inclusive.
// Class, struct, enum, trait, and interface nodes are reported as types.
type Span struct {
	Start int
	End   int
	Kind  string
}

// SymbolSpans returns function, method, and type ranges for a supported file.
// Tree-sitter off returns ErrDisabled. A file type with no definition query,
// and a binary built without tree-sitter, return ErrUnsupported.
func SymbolSpans(path string, content []byte) ([]Span, error) {
	if err := guardEnabled(); err != nil {
		return nil, err
	}
	return symbolSpans(path, content)
}

// ListDefinitions returns structural definitions for a supported source file.
func ListDefinitions(path string, content []byte) ([]Definition, error) {
	if err := guardEnabled(); err != nil {
		return nil, err
	}
	return listDefinitions(path, content)
}

// FormatDefinitions renders definitions for tool output.
func FormatDefinitions(path string, defs []Definition) string {
	if len(defs) == 0 {
		return fmt.Sprintf("No definitions found in %s", path)
	}
	slices.SortFunc(defs, func(a, b Definition) int {
		return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.Name, b.Name))
	})
	var b strings.Builder
	fmt.Fprintf(&b, "Definitions in %s (%d):\n", path, len(defs))
	for _, d := range defs {
		fmt.Fprintf(&b, "L%-4d  %-10s  %s\n", d.Line, d.Kind, d.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}
