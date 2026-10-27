package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeTree creates files (path -> content) under dir, creating parents.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFindIndirectDependents pins the structured-search fix: indirect
// dependents are files that reference a direct dependent's PATH. The previous
// implementation parsed SearchCode's context-free output as
// "path:line:content", but that mode renders hits grouped (a bare path line,
// then "line:content" lines), so no line parsed and the list was always empty.
func TestFindIndirectDependents(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"app/main.go":     "package app\n",
		"app/notes.txt":   "see app/main.go for the entry point\n",
		"other/unrelated": "nothing to see\n",
	})
	e := NewExecutor(dir)
	got := e.findIndirectDependents(context.Background(), []string{"app/main.go"})
	if len(got) != 1 || got[0] != "app/notes.txt" {
		t.Fatalf("indirect dependents = %#v, want [app/notes.txt]", got)
	}
}

// TestFindIndirectDependentsSkipsDirectDependents: a file that IS a direct
// dependent is not also reported as an indirect one.
func TestFindIndirectDependentsSkipsDirectDependents(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"app/main.go":  "package app\n// app/main.go\n",
		"app/util.txt": "app/main.go\n",
	})
	e := NewExecutor(dir)
	got := e.findIndirectDependents(context.Background(), []string{"app/main.go"})
	if len(got) != 1 || got[0] != "app/util.txt" {
		t.Fatalf("indirect dependents = %#v, want [app/util.txt]", got)
	}
}

// TestFindDefinitionTextKeepsPaths pins the other half of the same fix: the
// text fallback used to keep only the matching "line:content" lines from the
// grouped rendering, so every hit came back without a file path.
func TestFindDefinitionTextKeepsPaths(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"pkg/a.go": "package pkg\n\nfunc Target() {}\n",
	})
	e := NewExecutor(dir)
	defs, err := e.findDefinitionText(context.Background(), "", "", "Target")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 {
		t.Fatalf("defs = %#v, want one hit", defs)
	}
	if want := "pkg/a.go:3:"; len(defs[0]) < len(want) || defs[0][:len(want)] != want {
		t.Fatalf("definition = %q, want it to start with %q", defs[0], want)
	}
}
