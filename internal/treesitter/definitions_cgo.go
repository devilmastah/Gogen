//go:build cgo

package treesitter

import (
	"cmp"
	"embed"
	"fmt"
	"slices"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

//go:embed queries/*.scm
var queryFS embed.FS

// queryForLang returns the compiled definition query for langName, sourced
// from the registry's defsQuery embed path and compiled through the shared
// query cache (compileQuery).
func queryForLang(langName string) (*tree_sitter.Query, error) {
	registryOnce.Do(initRegistry)
	spec, ok := langSpecs[langName]
	if !ok || spec.defsQuery == "" {
		return nil, ErrUnsupported
	}
	src, err := queryFS.ReadFile(spec.defsQuery)
	if err != nil {
		return nil, fmt.Errorf("read query %s: %w", spec.defsQuery, err)
	}
	return compileQuery(langName, "defs", string(src))
}

const maxDefinitions = 300

func listDefinitions(path string, content []byte) ([]Definition, error) {
	run, err := runQuery(path, content, queryForLang)
	if err != nil {
		return nil, err
	}
	defer run.Close()

	names := run.query.CaptureNames()

	seen := make(map[string]struct{})
	var defs []Definition
	for cap := range run.Captures() {
		if int(cap.Index) >= len(names) {
			continue
		}
		captureName := names[cap.Index]
		if !strings.HasPrefix(captureName, "name.") {
			continue
		}
		kind := strings.TrimPrefix(captureName, "name.")
		name := strings.TrimSpace(cap.Node.Utf8Text(content))
		if run.langName == "ruby" {
			name = strings.TrimPrefix(name, ":")
		}
		if name == "" {
			continue
		}
		line := int(cap.Node.StartPosition().Row) + 1
		key := fmt.Sprintf("%d:%s:%s", line, kind, name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		defs = append(defs, Definition{Line: line, Kind: kind, Name: name})
		if len(defs) >= maxDefinitions {
			return defs, nil
		}
	}
	return defs, nil
}

// spanKind is the capture names that are a function, method, or type.
// A class, struct, enum, trait, or interface is a type.
func spanKind(captureName string) (string, bool) {
	if !strings.HasPrefix(captureName, "name.") {
		return "", false
	}
	switch strings.TrimPrefix(captureName, "name.") {
	case "function", "method":
		return strings.TrimPrefix(captureName, "name."), true
	case "type", "class", "struct", "enum", "trait", "interface":
		return "type", true
	default:
		return "", false
	}
}

// spanContainer is a declaration node whose range is the whole symbol,
// including the body. Names sit inside a declarator in some grammars, so
// the walk climbs through those to the declaration.
func spanContainer(kind string) bool {
	switch kind {
	case "function_declaration", "function_definition", "function_item",
		"method_declaration", "method_definition", "method", "singleton_method",
		"type_spec", "type_alias_declaration", "type_alias",
		"class_declaration", "class_definition", "class_specifier", "class",
		"struct_item", "struct_specifier", "struct_declaration",
		"enum_item", "enum_specifier", "enum_declaration",
		"trait_item", "trait_definition",
		"interface_declaration",
		"variable_declarator",
		"template_declaration",
		"create_function",
		"declaration":
		return true
	default:
		return false
	}
}

// spanRoot climbs from a name capture to the nearest declaration that owns
// it. A method inside a class is the method. The climb still walks through
// nodes that are not declarations, such as a declarator.
func spanRoot(name tree_sitter.Node) *tree_sitter.Node {
	for p := name.Parent(); p != nil; p = p.Parent() {
		if spanContainer(p.Kind()) {
			return p
		}
	}
	return nil
}

func inclusiveLines(n *tree_sitter.Node) (int, int) {
	start := int(n.StartPosition().Row) + 1
	end := int(n.EndPosition().Row) + 1
	if n.EndPosition().Column == 0 && end > start {
		end--
	}
	if end < start {
		end = start
	}
	return start, end
}

func symbolSpans(path string, content []byte) ([]Span, error) {
	run, err := runQuery(path, content, queryForLang)
	if err != nil {
		return nil, err
	}
	defer run.Close()

	names := run.query.CaptureNames()
	seen := make(map[[2]int]struct{})
	var spans []Span
	for cap := range run.Captures() {
		if int(cap.Index) >= len(names) {
			continue
		}
		kind, ok := spanKind(names[cap.Index])
		if !ok {
			continue
		}
		root := spanRoot(cap.Node)
		if root == nil {
			continue
		}
		start, end := inclusiveLines(root)
		key := [2]int{start, end}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		spans = append(spans, Span{Start: start, End: end, Kind: kind})
	}
	slices.SortFunc(spans, func(a, b Span) int {
		return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End))
	})
	return spans, nil
}
