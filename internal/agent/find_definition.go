package agent

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/treesitter"
)

// FindDefinition locates the file and line where a symbol is defined.
// Uses tree-sitter AST when available for supported languages; falls back to text search.
func (e *Executor) FindDefinition(ctx context.Context, symbol, subpath, glob string) (string, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", fmt.Errorf("symbol is required")
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	// skips collects unreadable-subtree errors from the AST walk so the
	// result is never silently partial. The text fallback runs through
	// SearchCode, which surfaces its own skipped-path footer.
	skips := &walkSkips{}

	// Use AST fallback pattern: try AST first, then text search
	fallback := &ASTFallback[[]string]{
		ASTFunc: func() ([]string, error) {
			return e.findDefinitionAST(ctx, searchRoot, relPrefix, glob, symbol, skips)
		},
		TextFunc: func() ([]string, error) {
			return e.findDefinitionText(ctx, subpath, glob, symbol)
		},
		HasResult: func(defs []string) bool {
			return len(defs) > 0
		},
	}

	defs, err := fallback.Run()
	if err != nil {
		return "", err
	}

	if len(defs) == 0 {
		return fmt.Sprintf("No definition found for %q", symbol) + skips.footer(), nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Definition(s) for %q:\n", symbol))
	for _, def := range defs {
		b.WriteString(def + "\n")
	}
	b.WriteString(fmt.Sprintf("\n(%d definition(s) found)", len(defs)))
	return b.String() + skips.footer(), nil
}

// findDefinitionText performs text-based search for symbol definitions.
func (e *Executor) findDefinitionText(ctx context.Context, subpath, glob, symbol string) ([]string, error) {
	// Combine all definition patterns into a single alternation to avoid
	// spawning ripgrep (or walking the tree) once per kind. The pattern is
	// generated from the shared definition-keyword table (same source as
	// isDefinitionLine and the list_definitions text outline) covering
	// functions (incl. Go methods with receivers), types, classes and
	// const/var/let bindings.
	pattern := definitionSearchPattern(symbol, "func", "type", "class", "const", "var")

	out, err := e.SearchCode(ctx, pattern, subpath, glob, 0, false)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(out, "No matches") {
		return nil, nil
	}

	var allDefs []string
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, symbol) {
			allDefs = append(allDefs, line)
		}
		if len(allDefs) >= 20 {
			break
		}
	}
	return allDefs, nil
}

func (e *Executor) findDefinitionAST(ctx context.Context, searchRoot, relPrefix, glob, symbol string, skips *walkSkips) ([]string, error) {
	if !treesitter.Enabled() {
		return nil, nil
	}

	var defs []string
	err := e.walkReadableFiles(ctx, searchRoot, relPrefix, glob, skips, true, func(path, rel string, content []byte) error {
		defList, err := treesitter.ListDefinitions(path, content)
		if err != nil {
			return nil
		}
		for _, def := range defList {
			if def.Name == symbol {
				defs = append(defs, fmt.Sprintf("%s:%d (%s)", rel, def.Line, def.Kind))
				if len(defs) >= 20 {
					return nil
				}
			}
		}
		return nil
	})
	return defs, err
}
