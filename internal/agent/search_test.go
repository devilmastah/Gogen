package agent

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestSplitSearchLine(t *testing.T) {
	file, rest, ok := splitSearchLine("pkg/a.go:3:var Target = 1")
	if !ok || file != "pkg/a.go" || rest != "3:var Target = 1" {
		t.Fatalf("unexpected result: %q, %q, %v", file, rest, ok)
	}
	file, rest, ok = splitSearchLine("file:with:colons.txt:12:a:b:c")
	if !ok || file != "file:with:colons.txt" || rest != "12:a:b:c" {
		t.Fatalf("unexpected result: %q, %q, %v", file, rest, ok)
	}
	_, _, ok = splitSearchLine("No matches found")
	if ok {
		t.Fatal("should reject non-match lines")
	}
}

func TestSearchCodeMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\n\nfunc hello() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(dir)
	matches, _, err := executor.SearchCodeMatches(context.Background(), "func hello", "", "*.go", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
	found := false
	for _, m := range matches {
		if strings.Contains(m.Path, "hello.go") && strings.Contains(m.Text, "func hello") && m.Line > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("unexpected matches: %+v", matches)
	}
}

// TestSearchCodeMatchesIgnoreCase pins the case-sensitivity flag plumbed from
// fs_search: the same pattern must miss when case-sensitive and hit when
// ignoreCase is set.
func TestSearchCodeMatchesIgnoreCase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mixed.go"), []byte("package main\n\nfunc HelloWorld() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(dir)

	sensitive, _, err := executor.SearchCodeMatches(context.Background(), "helloworld", "", "*.go", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sensitive) != 0 {
		t.Fatalf("case-sensitive search matched %d, want 0: %+v", len(sensitive), sensitive)
	}

	insensitive, _, err := executor.SearchCodeMatches(context.Background(), "helloworld", "", "*.go", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(insensitive) == 0 {
		t.Fatal("case-insensitive search found no matches for helloworld")
	}
}

// TestParseRgMatchesNormalizesWindowsPaths pins the Windows shape of
// ripgrep output: rg prints backslash paths (".\internal\target.go") for a
// root search on Windows, and parseRgMatches must normalize them to the same
// forward-slash workspace-relative paths the Go fallback emits — otherwise
// search results (and consumers like find-definition) see OS-specific paths.
// The leading "./" is dropped too: a root search prints it on every OS, and
// the Go fallback never does, so keeping it made the two engines disagree
// about the same file (and broke path comparisons in impact analysis).
// The input is built with the platform's native separator so the test
// exercises the real rg shape on every OS (on Unix the path passes through
// otherwise unchanged; on Windows the separators must be converted).
func TestParseRgMatchesNormalizesWindowsPaths(t *testing.T) {
	in := filepath.FromSlash("./internal/target.go") + ":3:var TargetNeedle = 2"
	got := parseRgMatches(in, "")
	want := []SearchMatch{{Path: "internal/target.go", Line: 3, Text: "var TargetNeedle = 2"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("parseRgMatches = %+v, want %+v", got, want)
	}
	// With a relPrefix the path is joined and normalized as before.
	got = parseRgMatches(filepath.FromSlash("./a.go")+":1:var A = 1", "pkg")
	want = []SearchMatch{{Path: "pkg/a.go", Line: 1, Text: "var A = 1"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("parseRgMatches with prefix = %+v, want %+v", got, want)
	}
}

func TestSearchCodeRejectsDashPattern(t *testing.T) {
	dir := t.TempDir()
	executor := NewExecutor(dir)
	_, err := executor.SearchCode(context.Background(), "--pre=/tmp/evil.sh", "", "", 0, false)
	if err == nil {
		t.Fatal("expected dash pattern to be rejected")
	}
	if !strings.Contains(err.Error(), "must not start with") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSearchCodeGoFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\n\nfunc hello() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("not in go files\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "func hello", "", "*.go", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello.go") || !strings.Contains(out, "func hello") {
		t.Fatalf("unexpected output: %q", out)
	}
	if _, err := exec.LookPath("rg"); err != nil {
		if !strings.Contains(out, "go fallback") {
			t.Fatalf("expected go fallback note: %q", out)
		}
	}
}

func TestSearchCodeNoMatches(t *testing.T) {
	dir := t.TempDir()
	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "missing-pattern-xyz", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No matches found") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestSearchCodeSubpath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("package pkg\n\nvar Target = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "Target", "pkg", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/a.go") || !strings.Contains(out, "3:var Target = 1") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestSearchCodeUsesRipgrepWhenAvailable(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "findme.go"), []byte("const Needle = \"unique-needle-42\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "unique-needle-42", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "findme.go") || !strings.Contains(out, "unique-needle-42") {
		t.Fatalf("unexpected output: %q", out)
	}
	if strings.Contains(out, "go fallback") {
		t.Fatalf("expected rg path, got go fallback: %q", out)
	}
}

func TestPrefixRelPaths(t *testing.T) {
	got := prefixRelPaths("a.go:1:line\nb.go:2:other", "internal/agent")
	want := "internal/agent/a.go:1:line\ninternal/agent/b.go:2:other"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPrefixRelPathsPreservesEmptyLines(t *testing.T) {
	got := prefixRelPaths("a.go:1:line\n\nb.go:2:other", "pkg")
	if got != "pkg/a.go:1:line\n\npkg/b.go:2:other" {
		t.Fatalf("got %q want %q", got, "pkg/a.go:1:line\n\npkg/b.go:2:other")
	}
}

// TestPrefixRelPathsContextLines pins the context-line ("path-NUM-content")
// handling: the relPrefix joins ONLY the path portion. The old code split on
// the first ':' alone, so a context line without a ':' fed the whole line
// into filepath.Join and filepath.Clean collapsed "//" in the content to
// "/" — Go comments reached the model as "/ comment".
func TestPrefixRelPathsContextLines(t *testing.T) {
	got := prefixRelPaths("view.go-52-// stream handlers for the round\nview.go:57:func prepareMessages()\nview.go-58-\tvar view []llm.Message", "internal/agent")
	want := "internal/agent/view.go-52-// stream handlers for the round\n" +
		"internal/agent/view.go:57:func prepareMessages()\n" +
		"internal/agent/view.go-58-\tvar view []llm.Message"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestPrefixRelPathsContextLineColonInContent covers a context line whose
// content contains a ':' after the line number: the path is still everything
// before the "NUM-" separator, and the content (including the colon) is
// preserved verbatim.
func TestPrefixRelPathsContextLineColonInContent(t *testing.T) {
	got := prefixRelPaths("view.go-54-// \"error\" (compact_last_resort=error): the diagnostic", "internal/agent")
	want := "internal/agent/view.go-54-// \"error\" (compact_last_resort=error): the diagnostic"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestSearchCodeContextLinesUnclamped pins the removal of the
// context_lines pre-clamp: a request far above the old cap of 20 must be
// honored as-is (the searchMaxOutputBytes cap with its truncation notice is
// the output backstop). The silent clamp returned byte-identical output for
// every larger request and drove retry loops incrementing context_lines.
func TestSearchCodeContextLinesUnclamped(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed (context-line separators differ in the go fallback)")
	}

	dir := t.TempDir()
	var sb strings.Builder
	sb.WriteString("package a\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "line%02d filler\n", i)
	}
	sb.WriteString("func needleTarget() {}\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "tail%02d filler\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)

	// 50 context lines on each side of the match (line 62): the window is
	// lines 12-112. The old cap of 20 would have shown only lines 42-82, so
	// the first/last context lines below prove the request was honored.
	out, err := exec.SearchCode(context.Background(), "needleTarget", "", "", 50, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"12-line10 filler", "112-tail49 filler"} {
		if !strings.Contains(out, want) {
			t.Fatalf("context_lines=50 not honored (missing %q):\n%s", want, out)
		}
	}
	if strings.Contains(out, "1:package a") {
		t.Fatalf("context window exceeded the request (line 1 is 61 lines above the match):\n%s", out)
	}
	if strings.Contains(out, "clamped") {
		t.Fatalf("unexpected clamp notice: %q", out)
	}
}

func TestSearchCodeDotDirRequiresSubpath(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gh, "ci.yml"), []byte("name: unique-workflow-marker-99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("unique-workflow-marker-99=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)

	out, err := executor.SearchCode(context.Background(), "unique-workflow-marker-99", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No matches found") {
		t.Fatalf("root search should skip hidden paths, got %q", out)
	}

	out, err = executor.SearchCode(context.Background(), "unique-workflow-marker-99", ".github", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ".github/workflows/ci.yml") {
		t.Fatalf("expected subpath search in .github, got %q", out)
	}
}

func TestSearchCodeGlobPathPatternGoFallback(t *testing.T) {
	dir := t.TempDir()
	internal := filepath.Join(dir, "internal")
	if err := os.MkdirAll(internal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("var RootNeedle = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(internal, "target.go"), []byte("var TargetNeedle = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "TargetNeedle", "", "internal/*.go", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "internal/target.go") {
		t.Fatalf("expected path-scoped glob match, got %q", out)
	}
	if strings.Contains(out, "root.go") {
		t.Fatalf("root.go should not match internal/*.go, got %q", out)
	}
}

func TestSearchCodeContextLinesGoFallback(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nline2\nneedle here\nline4\nline5\n"
	if err := os.WriteFile(filepath.Join(dir, "ctx.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "needle", "", "", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "line2") || !strings.Contains(out, "needle here") || !strings.Contains(out, "line4") {
		t.Fatalf("expected context lines around match, got %q", out)
	}
}

func TestSearchCodeContextLinesRipgrep(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}

	dir := t.TempDir()
	content := "alpha\nbeta\nneedle here\ndelta\nepsilon\n"
	if err := os.WriteFile(filepath.Join(dir, "ctx.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "needle", "", "", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "beta") || !strings.Contains(out, "needle here") || !strings.Contains(out, "delta") {
		t.Fatalf("expected rg context output, got %q", out)
	}
}

func TestReplaceInTreeRegexSemantics(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("fooBar and fooX\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	replaced, fileCount, files, err := exec.ReplaceInTree(context.Background(), `foo\w+`, "XX", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if replaced != 2 || fileCount != 1 {
		t.Fatalf("replaced=%d fileCount=%d, want 2/1", replaced, fileCount)
	}
	if len(files) != 1 || files[0] != "a.txt" {
		t.Fatalf("files=%v, want [a.txt]", files)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "XX and XX\n" {
		t.Fatalf("content = %q, want %q", got, "XX and XX\n")
	}
}

func TestReplaceInTreeSingleFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	replaced, fileCount, files, err := exec.ReplaceInTree(context.Background(), "old", "new", "edit.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if replaced != 1 || fileCount != 1 {
		t.Fatalf("replaced=%d fileCount=%d, want 1/1", replaced, fileCount)
	}
	if len(files) != 1 || files[0] != "edit.txt" {
		t.Fatalf("files=%v, want [edit.txt]", files)
	}
	keep, _ := os.ReadFile(filepath.Join(dir, "keep.txt"))
	edit, _ := os.ReadFile(filepath.Join(dir, "edit.txt"))
	if string(keep) != "old\n" {
		t.Fatalf("keep.txt should be unchanged, got %q", keep)
	}
	if string(edit) != "new\n" {
		t.Fatalf("edit.txt = %q, want new", edit)
	}
}

// TestReplaceInTreeEmptyPatternRejected locks in the up-front empty-pattern
// guard: an empty regex matches at every position (zero-width), which would
// rewrite every scanned file. It must error and leave files untouched.
func TestReplaceInTreeEmptyPatternRejected(t *testing.T) {
	dir := t.TempDir()
	content := "foo bar\n"
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	if _, _, _, err := exec.ReplaceInTree(context.Background(), "", "X", "", ""); err == nil {
		t.Fatal("expected error for empty search pattern")
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatalf("file was modified: got %q, want %q", data, content)
	}
}

// TestScanFileSinglePassMatchesBufferedReference is a differential test: the
// streaming scanner must produce byte-identical output to the old two-pass
// buffered algorithm (store all lines, mark windows, emit) for every
// combination of matches, context widths, and match limits.
func TestScanFileSinglePassMatchesBufferedReference(t *testing.T) {
	// Reference implementation of the two-pass buffered algorithm.
	reference := func(lines []string, re *regexp.Regexp, contextLines, matchLimit int) []string {
		var matchNums []int
		for i, line := range lines {
			if re.MatchString(line) {
				matchNums = append(matchNums, i+1)
				if len(matchNums) >= matchLimit {
					break
				}
			}
		}
		if len(matchNums) == 0 {
			return nil
		}
		want := make([]byte, len(lines)+1)
		for _, n := range matchNums {
			start := n - contextLines
			if start < 1 {
				start = 1
			}
			end := n + contextLines
			if end > len(lines) {
				end = len(lines)
			}
			for i := start; i <= end; i++ {
				if i == n || want[i] == 0 {
					want[i] = ':'
				} else if want[i] != ':' {
					want[i] = '-'
				}
			}
		}
		var out []string
		for i := 1; i <= len(lines); i++ {
			if want[i] != 0 {
				out = append(out, fmt.Sprintf("%s%c%d%c%s", "f.txt", want[i], i, want[i], lines[i-1]))
			}
		}
		return out
	}

	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 500; trial++ {
		n := rng.Intn(50) + 1
		lines := make([]string, n)
		for i := range lines {
			lines[i] = fmt.Sprintf("x%d", rng.Intn(6)) // x0..x5, so x1/x3 match
		}
		re := regexp.MustCompile(`x[13]`)
		c := rng.Intn(4)
		limit := rng.Intn(5) + 1

		input := strings.Join(lines, "\n") + "\n"
		got, err := scanFileSinglePass(strings.NewReader(input), "f.txt", re, c, limit)
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		want := reference(lines, re, c, limit)
		if !reflect.DeepEqual(got, want) {
			detail := ""
			max := len(got)
			if len(want) > max {
				max = len(want)
			}
			for i := 0; i < max; i++ {
				var g, w string
				if i < len(got) {
					g = got[i]
				}
				if i < len(want) {
					w = want[i]
				}
				detail += fmt.Sprintf("  [%d] got=%q want=%q\n", i, g, w)
			}
			t.Fatalf("trial %d (lines=%d, context=%d, limit=%d):\n got: %q\nwant: %q\n%s",
				trial, n, c, limit, got, want, detail)
		}
	}
}

func TestPrefixRelPathsPassesSeparatorsThrough(t *testing.T) {
	got := prefixRelPaths("a.go:1:line\n--\nb.go:2:other", "sub")
	want := "sub/a.go:1:line\n--\nsub/b.go:2:other"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestSearchCodeGoFallbackSkipsBinaryFiles pins the binary-file handling of
// the Go fallback: a binary file in the tree must be skipped without
// crashing (openSearchableFile returns a nil reader for binaries, and the
// walker must not close a nil closer).
func TestSearchCodeGoFallbackSkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.dat"), []byte("abc\x00def\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("needle beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/nonexistent") // hide rg so the Go fallback runs
	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "needle", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "c.txt") {
		t.Fatalf("expected text-file matches, got %q", out)
	}
	if strings.Contains(out, "b.dat") {
		t.Fatalf("binary file must not be reported as a match: %q", out)
	}
}

// TestSearchCodeGoFallbackTruncatesAtMatchCap pins the cap behavior of the
// Go fallback: once searchMaxMatches accumulate, the walk stops and the
// result carries the truncation footer (previously the walk kept going and
// the footer was missing).
func TestSearchCodeGoFallbackTruncatesAtMatchCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		var b strings.Builder
		for j := 0; j < 100; j++ {
			fmt.Fprintf(&b, "needle %d-%d\n", i, j)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", "/nonexistent") // hide rg so the Go fallback runs
	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "needle", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "needle"); got != searchMaxMatches {
		t.Fatalf("expected exactly %d matches, got %d", searchMaxMatches, got)
	}
	if !strings.Contains(out, "… truncated") {
		t.Fatalf("expected truncation footer, got %q", out)
	}
}

// TestSearchCodeGoFallbackConcurrent hammers the Go fallback from several
// goroutines so the shared binary-probe buffer pool is exercised under the
// race detector (make test runs go test -race). It catches use-after-Put
// races in openSearchableFile, where a pooled buffer was returned before the
// NUL scan and the copy-out finished.
func TestSearchCodeGoFallbackConcurrent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 500; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), []byte("alpha beta gamma\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", "/nonexistent") // hide rg so the Go fallback runs
	executor := NewExecutor(dir)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := executor.SearchCode(context.Background(), "alpha", "", "*.txt", 0, false); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestCapRgMatches applies the Go fallback's budgets to rg-parsed matches:
// the total-match cap (searchMaxMatches) and the byte cap
// (searchMaxOutputBytes), with the truncation flag set when either binds.
func TestCapRgMatches(t *testing.T) {
	mk := func(n int) []SearchMatch {
		out := make([]SearchMatch, n)
		for i := range out {
			out[i] = SearchMatch{Path: "pkg/file.go", Line: i + 1, Text: "needle"}
		}
		return out
	}
	// Under the caps: no truncation, all matches kept.
	small := mk(10)
	got, trunc := capRgMatches(small)
	if trunc || len(got) != 10 {
		t.Fatalf("small: truncated=%v len=%d want false/10", trunc, len(got))
	}
	// Over the match cap: exactly searchMaxMatches, truncated.
	big := mk(searchMaxMatches + 50)
	got, trunc = capRgMatches(big)
	if !trunc || len(got) != searchMaxMatches {
		t.Fatalf("big: truncated=%v len=%d want true/%d", trunc, len(got), searchMaxMatches)
	}
	// Over the byte cap: truncated with a prefix of the matches. The match
	// cap (200) binds first for short lines, so the byte cap is exercised
	// with long lines: 180 matches x ~3KB of text ≈ 540KB > 512KB.
	long := make([]SearchMatch, 180)
	for i := range long {
		long[i] = SearchMatch{Path: "pkg/file.go", Line: i + 1, Text: strings.Repeat("x", 3000)}
	}
	got, trunc = capRgMatches(long)
	if !trunc {
		t.Fatal("byte cap: expected truncated=true")
	}
	if len(got) >= len(long) {
		t.Fatalf("byte cap: expected fewer than %d matches, got %d", len(long), len(got))
	}
}

// TestAppendRgTruncationNotice pins the truncation footer for capped rg
// output (context-lines mode).
func TestAppendRgTruncationNotice(t *testing.T) {
	got := appendRgTruncationNotice("some output", false)
	if got != "some output" {
		t.Fatalf("no-overflow must be unchanged, got %q", got)
	}
	got = appendRgTruncationNotice("some output", true)
	if !strings.Contains(got, "truncated (output exceeds") {
		t.Fatalf("expected truncation notice, got %q", got)
	}
}

// TestSearchCodeIgnoreCase verifies the ignore_case option end to end:
// with ignoreCase=true a literal pattern matches lines whose casing differs,
// and with the default (false) it does not. The search runs on whichever
// engine is installed (rg or the Go fallback), so both engines honor the
// flag; the Go fallback is additionally exercised directly below so its
// behavior is pinned even where rg is present.
func TestSearchCodeIgnoreCase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mixed.go"), []byte("package main\n\nvar ErrorCode = 1\nvar errorCode = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(dir)
	out, err := executor.SearchCode(context.Background(), "errorcode", "", "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ErrorCode") || !strings.Contains(out, "errorCode") {
		t.Fatalf("ignore_case should match both casings, got: %q", out)
	}

	out, err = executor.SearchCode(context.Background(), "errorcode", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "ErrorCode") || strings.Contains(out, "errorCode") {
		t.Fatalf("case-sensitive search must not match either casing, got: %q", out)
	}
}

// TestSearchWithGoIgnoreCase pins case-insensitive matching on the Go
// fallback engine specifically (independent of rg availability), for both
// regex and literal patterns.
func TestSearchWithGoIgnoreCase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("Hello World\nhello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(dir)

	cases := []struct {
		name       string
		pattern    string // regex-interpreted first, quoted-literal fallback otherwise
		ignoreCase bool
		wantLines  []int // 1-based file lines expected to match
	}{
		{"regex sensitive", `hello \w+`, false, []int{2}},
		{"regex insensitive", `hello \w+`, true, []int{1, 2}},
		{"literal sensitive", "Hello World", false, []int{1}},
		{"literal insensitive", "Hello World", true, []int{1, 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches, truncated, err := executor.searchWithGoMatches(context.Background(), dir, "", tc.pattern, "", tc.ignoreCase, nil)
			if err != nil {
				t.Fatal(err)
			}
			if truncated {
				t.Fatal("unexpected truncation")
			}
			if len(matches) != len(tc.wantLines) {
				t.Fatalf("got %d matches (%+v), want %d", len(matches), matches, len(tc.wantLines))
			}
			for i, want := range tc.wantLines {
				if matches[i].Line != want {
					t.Fatalf("match %d: got line %d, want %d", i, matches[i].Line, want)
				}
			}

			// The rendered form agrees: compacted layout shows the file
			// header once, then "line:content" per match.
			out, err := executor.searchWithGo(context.Background(), dir, "", tc.pattern, "", 0, tc.ignoreCase, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.wantLines {
				if !strings.Contains(out, fmt.Sprintf("\n%d:", want)) {
					t.Fatalf("output missing line %d: %q", want, out)
				}
			}
		})
	}
}

// TestCompileSearchPatternIgnoreCase pins the (?i) plumbing in
// compileSearchPattern: valid regexes get the flag prepended, invalid regexes
// fall back to a quoted literal that still matches case-insensitively (the
// flag must not end up escaped inside the literal), and the memo keeps
// sensitive/insensitive compiles of one pattern distinct.
func TestCompileSearchPatternIgnoreCase(t *testing.T) {
	re, err := compileSearchPattern(`[Ee]rror`, true)
	if err != nil {
		t.Fatal(err)
	}
	if re.String() != `(?i)[Ee]rror` {
		t.Fatalf("re.String() = %q, want %q", re.String(), `(?i)[Ee]rror`)
	}

	// Invalid regex falls back to QuoteMeta; (?i) stays a live flag rather
	// than being escaped into the literal.
	re, err = compileSearchPattern("[invalid", true)
	if err != nil {
		t.Fatal(err)
	}
	if re.String() != `(?i)\[invalid` {
		t.Fatalf("re.String() = %q, want %q", re.String(), `(?i)\[invalid`)
	}
	if !re.MatchString("[INVALID]") || re.MatchString("[valid") {
		t.Fatalf("quoted-literal fallback broken: %q", re.String())
	}

	sens, err := compileSearchPattern("abc", false)
	if err != nil {
		t.Fatal(err)
	}
	insens, err := compileSearchPattern("abc", true)
	if err != nil {
		t.Fatal(err)
	}
	if sens.String() == insens.String() {
		t.Fatal("sensitive and insensitive compiles must be distinct cache entries")
	}
}

// TestRGAvailableFollowsPath pins the memo: the ripgrep probe is cached per PATH
// value, so a PATH change (a test, or rg installed mid-session) is still seen.
func TestRGAvailableFollowsPath(t *testing.T) {
	dir := t.TempDir()
	rg := filepath.Join(dir, "rg")
	if err := os.WriteFile(rg, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir)
	if !rgAvailable() {
		t.Fatal("rg on PATH not detected")
	}
	t.Setenv("PATH", t.TempDir()) // no rg here
	if rgAvailable() {
		t.Fatal("rg reported present after PATH changed to a dir without it")
	}
	t.Setenv("PATH", dir)
	if !rgAvailable() {
		t.Fatal("rg not re-detected after PATH was restored")
	}
}
