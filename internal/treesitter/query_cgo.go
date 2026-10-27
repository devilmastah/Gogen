//go:build cgo

package treesitter

import (
	"fmt"
	"iter"
	"runtime"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// parserPool reuses tree-sitter parsers to avoid C FFI allocation overhead on
// every parse call. Parsers are safe to reuse after SetLanguage.
//
// It is a bounded free list rather than a sync.Pool because sync.Pool drops
// idle items on every GC cycle WITHOUT running any destructor, and
// go-tree-sitter has no finalizers (its README: "you must always call Close").
// Every dropped parser therefore leaked its TSParser allocation for the life
// of the process. The list holds at most one parser per P so a parallel walk
// never allocates, and hands the overflow back to Close.
var parserPool = make(chan *tree_sitter.Parser, runtime.GOMAXPROCS(0))

// acquireParser returns a parser from the free list, or a fresh one when the
// list is empty.
func acquireParser() *tree_sitter.Parser {
	select {
	case p := <-parserPool:
		return p
	default:
		return tree_sitter.NewParser()
	}
}

// releaseParser returns a parser to the free list, closing it when the list is
// already full (the C memory must be freed; sync.Pool's silent drop cannot be
// allowed here).
func releaseParser(p *tree_sitter.Parser) {
	if p == nil {
		return
	}
	select {
	case parserPool <- p:
	default:
		p.Close()
	}
}

// queryRun is one compiled query iterated over a freshly parsed file. It
// owns every resource the shared parse preamble acquires — the pooled
// parser, the parsed tree, and the query cursor backing matches — so
// callers release all of them with a single Close. Close order (cursor,
// then tree, then the parser back to the pool) matches the old inline
// defer order: the cursor is released before the tree it walks.
type queryRun struct {
	langName string
	query    *tree_sitter.Query
	parser   *tree_sitter.Parser
	tree     *tree_sitter.Tree
	cursor   *tree_sitter.QueryCursor
	matches  tree_sitter.QueryMatches
}

// Close releases the run's resources. It must be called exactly once, and
// no Captures iteration may be in flight or resumed afterwards.
func (r *queryRun) Close() {
	r.cursor.Close()
	r.tree.Close()
	releaseParser(r.parser)
}

// Captures iterates every capture of every query match, in match order.
// The enclosing range loop may return early (e.g. a per-file cap was
// reached); that stops the iteration cleanly. Capture nodes stay valid
// until Close.
func (r *queryRun) Captures() iter.Seq[tree_sitter.QueryCapture] {
	return func(yield func(tree_sitter.QueryCapture) bool) {
		for {
			match := r.matches.Next()
			if match == nil {
				return
			}
			for _, cap := range match.Captures {
				if !yield(cap) {
					return
				}
			}
		}
	}
}

// runQuery is the shared parse preamble for every query kind (definitions,
// references, and any future kind): resolve the language for path, fetch the
// kind's compiled query through queryFor, parse content with a pooled
// parser, and open a cursor over the matches. A new query kind therefore
// only supplies its query fetcher and its per-capture loop. The caller must
// Close the returned run when done iterating.
func runQuery(path string, content []byte, queryFor func(langName string) (*tree_sitter.Query, error)) (*queryRun, error) {
	langName, ok := langNameForPath(path)
	if !ok {
		return nil, ErrUnsupported
	}
	query, err := queryFor(langName)
	if err != nil {
		return nil, err
	}

	lang := languageFor(langName)
	p := acquireParser()
	if err := p.SetLanguage(lang); err != nil {
		releaseParser(p)
		return nil, fmt.Errorf("set language %s: %w", langName, err)
	}

	tree := p.Parse(content, nil)
	if tree == nil {
		releaseParser(p)
		return nil, fmt.Errorf("failed to parse %s", path)
	}

	cursor := tree_sitter.NewQueryCursor()
	return &queryRun{
		langName: langName,
		query:    query,
		parser:   p,
		tree:     tree,
		cursor:   cursor,
		matches:  cursor.Matches(query, tree.RootNode(), content),
	}, nil
}
