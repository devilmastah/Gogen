package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"gogen/internal/treesitter"
)

// SymbolRef represents a reference to a symbol found in a file.
type SymbolRef struct {
	File    string // Relative file path
	Line    int    // Line number (1-based)
	Content string // Line content
}

// ErrNoResults is returned by ASTFallback.Run() when both AST and text search
// fail to produce results.
var ErrNoResults = errors.New("no results found in AST or text search")

// ASTFallback is a generic helper that tries AST-based search first,
// then falls back to text-based search if AST returns no results.
// This eliminates code duplication across rename, call_graph, impact
// analysis, and find_symbol.
type ASTFallback[T any] struct {
	ASTFunc   func() (T, error) // AST-based search function
	TextFunc  func() (T, error) // Text-based fallback function
	HasResult func(T) bool      // Check if result has content
}

// Run executes AST search first, then falls back to text search if needed.
func (a *ASTFallback[T]) Run() (T, error) {
	result, err := a.ASTFunc()
	if err == nil && a.HasResult(result) {
		return result, nil
	}
	// AST failed or returned no results, try text fallback
	textResult, textErr := a.TextFunc()
	if textErr != nil {
		// Both paths failed. Return the more informative error.
		if err != nil {
			return result, err
		}
		return result, fmt.Errorf("%w: %v", ErrNoResults, textErr)
	}
	return textResult, nil
}

// walkReadableFiles walks searchRoot (optionally filtered by glob) and hands
// every readable file's path, workspace-relative path, and content to fn. A
// file that cannot be read is skipped rather than aborting the walk, so one
// unreadable file cannot hide every match (skips records them for the result
// footer). parsedOnly restricts the walk to files tree-sitter can parse — the
// AST passes — while the text passes scan every file.
//
// It owns the walk options and the read/skip policy every symbol search shares
// (find_symbol, call_graph, rename, call_graph impact, dependencies); the
// callers differ only in what they do with the content. Deliberately does NOT
// check treesitter.Enabled(): each caller decides when the AST pass applies.
func (e *Executor) walkReadableFiles(ctx context.Context, searchRoot, relPrefix, glob string, skips *walkSkips,
	parsedOnly bool, fn func(path, rel string, content []byte) error) error {

	return walkTree(ctx, searchRoot, relPrefix, walkOpts{glob: glob, checkReadable: true, onSkip: skips.observe}, func(path, rel string, _ os.DirEntry) error {
		if parsedOnly && !treesitter.ReferenceSearchSupported(path) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		return fn(path, rel, content)
	})
}

// walkSymbolReferences is a shared helper that walks the filesystem, finds symbol references
// using tree-sitter AST when available, and calls the visitor for each file with references.
// This eliminates code duplication across find_symbol and call_graph.
func (e *Executor) walkSymbolReferences(ctx context.Context, searchRoot, relPrefix, glob, symbol string, skips *walkSkips,
	visitor func(filePath string, refs []treesitter.Reference, content []byte) error) error {

	if !treesitter.Enabled() {
		return nil
	}

	return e.walkReadableFiles(ctx, searchRoot, relPrefix, glob, skips, true, func(path, rel string, content []byte) error {
		refs, err := treesitter.FindSymbolReferences(path, content, symbol)
		if err != nil {
			return nil
		}
		if len(refs) == 0 {
			return nil
		}

		// Call the visitor with the file path and references
		return visitor(rel, refs, content)
	})
}

// walkSymbolReferencesText is a shared helper for text-based symbol search.
// It walks the filesystem and finds symbol references using regex patterns.
// The pattern parameter should be a pre-compiled regex pattern (callers use regexp.QuoteMeta).
func (e *Executor) walkSymbolReferencesText(ctx context.Context, searchRoot, relPrefix, glob, pattern string, skips *walkSkips,
	visitor func(filePath string, lineNum int, line string) error) error {

	re, err := compileSearchPattern(pattern, false)
	if err != nil {
		return err
	}

	return e.walkReadableFiles(ctx, searchRoot, relPrefix, glob, skips, false, func(path, rel string, content []byte) error {
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				if err := visitor(rel, i+1, line); err != nil {
					return err
				}
			}
		}

		return nil
	})
}
