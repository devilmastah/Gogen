package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestPatchFilePreservesCRLF is the B13 line-ending regression: a one-line
// patch to a CRLF checkout used to rewrite EVERY line ending in the file (the
// pre-image was split after normalizing CRLF to LF and joined back with "\n"),
// turning a 1-line edit into a whole-file diff in the user's VCS.
func TestPatchFilePreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello\r\nbye\r\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	diff := "--- a/hello.txt\n+++ b/hello.txt\n@@ -1,2 +1,3 @@\n hello\n+world\n bye\n"
	e := &Executor{WorkingDir: dir}
	if _, err := e.PatchFile(context.Background(), diff, false, false); err != nil {
		t.Fatalf("PatchFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "hello\r\nworld\r\nbye\r\n"; string(got) != want {
		t.Fatalf("patched content = %q, want %q", string(got), want)
	}
}

// TestPatchFileDoesNotSplitBareCR pins the other half: a bare "\r" inside file
// content is not a line separator. The old splitter turned it into one, so a
// patch could match the "lines" it invented and silently rewrite an embedded
// carriage return (e.g. inside a Go string literal) into a newline.
func TestPatchFileDoesNotSplitBareCR(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(path, []byte("a\rb\nc\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	diff := "--- a/x.txt\n+++ b/x.txt\n@@ -2 +2 @@\n-c\n+d\n"
	e := &Executor{WorkingDir: dir}
	if _, err := e.PatchFile(context.Background(), diff, false, false); err != nil {
		t.Fatalf("PatchFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "a\rb\nd\n"; string(got) != want {
		t.Fatalf("patched content = %q, want %q (the embedded CR must survive)", string(got), want)
	}
}

// TestSplitJoinRoundTripsLineEndings covers the helpers directly, including
// the no-trailing-newline case, so the eol plumbing cannot silently start
// adding or dropping a trailing newline.
func TestSplitJoinRoundTripsLineEndings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		eol   string
		lines []string
	}{
		{"lf", "a\nb\n", "\n", []string{"a", "b"}},
		{"crlf", "a\r\nb\r\n", "\r\n", []string{"a", "b"}},
		{"crlf no trailing newline", "a\r\nb", "\r\n", []string{"a", "b"}},
		{"bare cr is content", "a\rb\n", "\n", []string{"a\rb"}},
		{"empty", "", "\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, eol := splitLinesPreserveTrailing(tc.in)
			if eol != tc.eol {
				t.Fatalf("eol = %q, want %q", eol, tc.eol)
			}
			if len(lines) != len(tc.lines) {
				t.Fatalf("lines = %#v, want %#v", lines, tc.lines)
			}
			for i := range lines {
				if lines[i] != tc.lines[i] {
					t.Fatalf("line %d = %q, want %q", i, lines[i], tc.lines[i])
				}
			}
			got := joinLinesPreserveTrailing(lines, len(tc.in) > 0 && tc.in[len(tc.in)-1] == '\n', eol)
			if got != tc.in {
				t.Fatalf("round trip = %q, want %q", got, tc.in)
			}
		})
	}
}
