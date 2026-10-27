//go:build cgo

package treesitter

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	tree_sitter_dockerfile "github.com/gortexhq/tree-sitter-dockerfile/bindings/go"
	tree_sitter_hcl "github.com/tree-sitter-grammars/tree-sitter-hcl/bindings/go"
	tree_sitter_kotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
	tree_sitter_lua "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	tree_sitter_make "github.com/tree-sitter-grammars/tree-sitter-make/bindings/go"
	tree_sitter_toml "github.com/tree-sitter-grammars/tree-sitter-toml/bindings/go"
	tree_sitter_yaml "github.com/tree-sitter-grammars/tree-sitter-yaml/bindings/go"
	tree_sitter_zig "github.com/tree-sitter-grammars/tree-sitter-zig/bindings/go"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	tree_sitter_c_sharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_css "github.com/tree-sitter/tree-sitter-css/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_html "github.com/tree-sitter/tree-sitter-html/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_json "github.com/tree-sitter/tree-sitter-json/bindings/go"
	tree_sitter_php "github.com/tree-sitter/tree-sitter-php/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_scala "github.com/tree-sitter/tree-sitter-scala/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	tree_sitter_sql "github.com/wippyai/tree-sitter-sql/bindings/go"
)

type langSpec struct {
	name      string
	exts      []string
	ptrFn     func() unsafe.Pointer
	defsQuery string // embed path under queries/, "" = no definition query
	refsQuery string // inline query text, "" = no reference query
}

var (
	registryOnce sync.Once
	extToLang    map[string]string
	langSpecs    map[string]langSpec
	langCache    sync.Map // string -> *tree_sitter.Language

	// queryCache caches compiled queries per language and kind ("defs" or
	// "refs") so each query is compiled once per process. Both the
	// definition-query and reference-query paths funnel through compileQuery.
	queryCache sync.Map // "<kind>:<lang>" -> *tree_sitter.Query
)

func bundledSpecs() []langSpec {
	return []langSpec{
		{name: "go", exts: []string{"go"}, ptrFn: tree_sitter_go.Language,
			defsQuery: "queries/go.scm",
			refsQuery: "(identifier) @ref\n(field_identifier) @ref\n(type_identifier) @ref"},
		{name: "python", exts: []string{"py", "pyi"}, ptrFn: tree_sitter_python.Language,
			defsQuery: "queries/python.scm",
			refsQuery: "(identifier) @ref"},
		{name: "javascript", exts: []string{"js", "mjs", "cjs"}, ptrFn: tree_sitter_javascript.Language,
			defsQuery: "queries/javascript.scm",
			refsQuery: "(identifier) @ref\n(property_identifier) @ref\n(shorthand_property_identifier) @ref"},
		{name: "typescript", exts: []string{"ts", "mts", "cts"}, ptrFn: tree_sitter_typescript.LanguageTypescript,
			defsQuery: "queries/typescript.scm",
			refsQuery: "(identifier) @ref\n(property_identifier) @ref\n(shorthand_property_identifier) @ref\n(type_identifier) @ref"},
		{name: "tsx", exts: []string{"tsx"}, ptrFn: tree_sitter_typescript.LanguageTSX,
			defsQuery: "queries/tsx.scm",
			refsQuery: "(identifier) @ref\n(property_identifier) @ref\n(shorthand_property_identifier) @ref\n(type_identifier) @ref"},
		{name: "json", exts: []string{"json"}, ptrFn: tree_sitter_json.Language},
		{name: "rust", exts: []string{"rs"}, ptrFn: tree_sitter_rust.Language,
			defsQuery: "queries/rust.scm",
			refsQuery: "(identifier) @ref\n(field_identifier) @ref\n(type_identifier) @ref"},
		{name: "java", exts: []string{"java"}, ptrFn: tree_sitter_java.Language,
			defsQuery: "queries/java.scm",
			refsQuery: "(identifier) @ref"},
		{name: "kotlin", exts: []string{"kt", "kts"}, ptrFn: tree_sitter_kotlin.Language,
			defsQuery: "queries/kotlin.scm",
			refsQuery: "(identifier) @ref"},
		{name: "c", exts: []string{"c", "h"}, ptrFn: tree_sitter_c.Language,
			defsQuery: "queries/c.scm",
			refsQuery: "(identifier) @ref\n(field_identifier) @ref\n(type_identifier) @ref"},
		{name: "cpp", exts: []string{"cpp", "cc", "cxx", "hpp", "hh", "hxx"}, ptrFn: tree_sitter_cpp.Language,
			defsQuery: "queries/cpp.scm",
			refsQuery: "(identifier) @ref\n(field_identifier) @ref\n(type_identifier) @ref"},
		{name: "csharp", exts: []string{"cs"}, ptrFn: tree_sitter_c_sharp.Language,
			defsQuery: "queries/csharp.scm",
			refsQuery: "(identifier) @ref"},
		{name: "php", exts: []string{"php", "phtml"}, ptrFn: tree_sitter_php.LanguagePHP,
			defsQuery: "queries/php.scm",
			refsQuery: "(name) @ref\n(variable_name) @ref"},
		{name: "ruby", exts: []string{"rb", "rake"}, ptrFn: tree_sitter_ruby.Language,
			defsQuery: "queries/ruby.scm",
			refsQuery: "(identifier) @ref\n(constant) @ref"},
		{name: "scala", exts: []string{"scala"}, ptrFn: tree_sitter_scala.Language,
			defsQuery: "queries/scala.scm",
			refsQuery: "(identifier) @ref"},
		{name: "sql", exts: []string{"sql"}, ptrFn: tree_sitter_sql.Language,
			defsQuery: "queries/sql.scm",
			refsQuery: "(object_reference) @ref"},
		{name: "html", exts: []string{"html", "htm"}, ptrFn: tree_sitter_html.Language},
		{name: "css", exts: []string{"css"}, ptrFn: tree_sitter_css.Language},
		{name: "bash", exts: []string{"sh", "bash"}, ptrFn: tree_sitter_bash.Language,
			defsQuery: "queries/bash.scm",
			refsQuery: "(word) @ref"},
		{name: "dockerfile", exts: nil, ptrFn: tree_sitter_dockerfile.Language},
		{name: "yaml", exts: []string{"yaml", "yml"}, ptrFn: tree_sitter_yaml.Language},
		{name: "toml", exts: []string{"toml"}, ptrFn: tree_sitter_toml.Language},
		{name: "zig", exts: []string{"zig"}, ptrFn: tree_sitter_zig.Language,
			defsQuery: "queries/zig.scm",
			refsQuery: "(identifier) @ref"},
		{name: "lua", exts: []string{"lua"}, ptrFn: tree_sitter_lua.Language,
			defsQuery: "queries/lua.scm",
			refsQuery: "(identifier) @ref"},
		{name: "make", exts: []string{"mk"}, ptrFn: tree_sitter_make.Language},
		{name: "hcl", exts: []string{"hcl", "tf"}, ptrFn: tree_sitter_hcl.Language,
			defsQuery: "queries/hcl.scm",
			refsQuery: "(identifier) @ref"},
	}
}

func initRegistry() {
	extToLang = make(map[string]string)
	langSpecs = make(map[string]langSpec)
	for _, spec := range bundledSpecs() {
		langSpecs[spec.name] = spec
		for _, ext := range spec.exts {
			if ext != "" {
				extToLang[ext] = spec.name
			}
		}
	}
}

func BundledLanguages() []string {
	registryOnce.Do(initRegistry)
	allowed := allowedLangs()
	names := make([]string, 0, len(langSpecs))
	for name := range langSpecs {
		if langAllowed(name, allowed) {
			names = append(names, name)
		}
	}
	return names
}

func languageFor(name string) *tree_sitter.Language {
	if v, ok := langCache.Load(name); ok {
		return v.(*tree_sitter.Language)
	}
	spec, ok := langSpecs[name]
	if !ok || spec.ptrFn == nil {
		return nil
	}
	lang := tree_sitter.NewLanguage(spec.ptrFn())
	langCache.Store(name, lang)
	return lang
}

// compileQuery compiles src for langName and caches the *tree_sitter.Query
// under kind ("defs" or "refs") so each query is compiled once per process.
// Both the definition-query and reference-query paths funnel through here,
// keeping the compile+cache policy in one place.
func compileQuery(langName, kind, src string) (*tree_sitter.Query, error) {
	registryOnce.Do(initRegistry)
	key := kind + ":" + langName
	if v, ok := queryCache.Load(key); ok {
		return v.(*tree_sitter.Query), nil
	}
	lang := languageFor(langName)
	if lang == nil {
		return nil, ErrUnsupported
	}
	q, err := tree_sitter.NewQuery(lang, src)
	if err != nil {
		return nil, fmt.Errorf("compile %s query for %s: %w", kind, langName, *err)
	}
	if existing, loaded := queryCache.LoadOrStore(key, q); loaded {
		// A concurrent caller compiled the same query first. Keep the cached
		// one and release ours: go-tree-sitter has no finalizers, so a losing
		// query left unclosed is leaked C memory for the process lifetime.
		q.Close()
		return existing.(*tree_sitter.Query), nil
	}
	return q, nil
}

func langNameForPath(path string) (string, bool) {
	registryOnce.Do(initRegistry)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		// Special case: filenames without extension (e.g. Dockerfile, Makefile)
		base := strings.ToLower(filepath.Base(path))
		switch base {
		case "dockerfile":
			if !langAllowed("dockerfile", allowedLangs()) {
				return "", false
			}
			return "dockerfile", true
		case "makefile", "gnumakefile":
			if !langAllowed("make", allowedLangs()) {
				return "", false
			}
			return "make", true
		}
		return "", false
	}
	name, ok := extToLang[ext]
	if !ok || !langAllowed(name, allowedLangs()) {
		return "", false
	}
	return name, true
}

func checkSupported(path string, content []byte) []Issue {
	registryOnce.Do(initRegistry)

	langName, ok := langNameForPath(path)
	if !ok {
		return nil
	}
	lang := languageFor(langName)
	if lang == nil {
		return nil
	}
	return parseIssues(lang, content)
}

func parseIssues(lang *tree_sitter.Language, content []byte) []Issue {
	if len(content) == 0 {
		return nil
	}

	p := acquireParser()
	defer releaseParser(p)
	parser := p
	if err := parser.SetLanguage(lang); err != nil {
		return []Issue{{Line: 1, Message: fmt.Sprintf("set language: %v", err)}}
	}

	tree := parser.Parse(content, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	root := tree.RootNode()
	if !root.HasError() {
		return nil
	}

	seen := make(map[int]struct{})
	var issues []Issue
	collectIssues(root, &issues, seen)
	return issues
}

func collectIssues(n *tree_sitter.Node, issues *[]Issue, seen map[int]struct{}) {
	if n == nil {
		return
	}
	// Prune subtrees that cannot contain a problem: HasError() is true when the
	// node OR any descendant is an error, and a missing node has no children.
	// The walk used to descend into every node through cgo even though only a
	// handful of subtrees can produce an issue.
	if !n.HasError() && !n.IsMissing() {
		return
	}
	if n.IsError() || n.IsMissing() {
		line := int(n.StartPosition().Row) + 1
		if _, dup := seen[line]; !dup {
			seen[line] = struct{}{}
			msg := "syntax error"
			if n.IsMissing() {
				msg = "missing token"
			}
			*issues = append(*issues, Issue{Line: line, Message: msg})
		}
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		collectIssues(n.Child(i), issues, seen)
	}
}
