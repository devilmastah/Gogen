package tui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// fullExtractDiff is a test oracle: the pre-phase-2 whole-buffer extraction,
// kept here so advanceToolDiffRender can be checked against it at every
// batch boundary. The second return reports whether the value's closing
// quote was found (the legacy function instead reported "content found").
func fullExtractDiff(rawJSON string) (diff string, closed bool) {
	idx := strings.Index(rawJSON, `"diff"`)
	if idx < 0 {
		return "", false
	}
	rest := rawJSON[idx+6:]
	rest = strings.TrimLeft(rest, " \t")
	if len(rest) == 0 || rest[0] != ':' {
		return "", false
	}
	rest = rest[1:]
	rest = strings.TrimLeft(rest, " \t")
	if len(rest) == 0 || rest[0] != '"' {
		return "", false
	}
	rest = rest[1:]
	var buf strings.Builder
	i := 0
	for i < len(rest) {
		if rest[i] == '\\' && i+1 < len(rest) {
			switch rest[i+1] {
			case 'n':
				buf.WriteByte('\n')
			case 't':
				buf.WriteByte('\t')
			case '"':
				buf.WriteByte('"')
			case '\\':
				buf.WriteByte('\\')
			case 'r':
			default:
				buf.WriteByte(rest[i])
				buf.WriteByte(rest[i+1])
			}
			i += 2
		} else if rest[i] == '"' {
			return buf.String(), true
		} else {
			buf.WriteByte(rest[i])
			i++
		}
	}
	return buf.String(), false // value still open
}

// heldBackTrailingBackslash reports whether, for a still-open diff value,
// the raw buffer ends in an unpaired backslash: the oracle writes it as a
// literal, the incremental extractor holds it back until its escape pair
// completes in the next batch.
func heldBackTrailingBackslash(raw string, valueStart int) bool {
	n := 0
	for i := len(raw) - 1; i >= valueStart && raw[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// TestToolDiffIncrementalExtractMatchesFull feeds the raw args buffer one
// byte at a time (hitting every possible batch boundary, including the
// middle of escape sequences) and asserts at each step that the incremental
// state equals the whole-buffer oracle, and that the incremental rendered
// lines equal a full re-render of the current diff.
func TestToolDiffIncrementalExtractMatchesFull(t *testing.T) {
	diffs := []string{
		"--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-old line\n+new line\n",
		"+quote \" inside\n+tab\there\n+back\\\\slash\n+crlf\rline\n+bad\\x esc\n",
		"no trailing newline\n+ends with backslash \\\\",
	}
	for _, diff := range diffs {
		b, err := json.Marshal(diff)
		if err != nil {
			t.Fatal(err)
		}
		raw := `{"dry_run":false,"diff":` + string(b) + `,"fuzzy":true}`
		m := newStreamTestModel()
		for i := 1; i <= len(raw); i++ {
			prefix := raw[:i]
			st := m.advanceToolDiffRender(0, []byte(prefix))
			want, closed := fullExtractDiff(prefix)
			if st.valueStart < 0 {
				if want != "" {
					t.Fatalf("prefix %d: key not located but oracle found %q", i, want)
				}
				continue
			}
			if !closed && heldBackTrailingBackslash(prefix, st.valueStart) {
				want = want[:len(want)-1]
			}
			if string(st.diff) != want {
				t.Fatalf("prefix %d: st.diff=%q want %q (closed=%v)", i, string(st.diff), want, closed)
			}
			if (st.valueEnd < 0) != !closed {
				t.Fatalf("prefix %d: valueEnd open=%v, oracle closed=%v", i, st.valueEnd < 0, closed)
			}
			// Rendered state must equal a full re-render of the current
			// diff (whitespace-only diffs are a degenerate case where the
			// legacy renderDiff returns "" — skip the identity check there).
			if strings.TrimSpace(string(st.diff)) != "" {
				displayed := make([]string, 0, len(st.renderedLines)+1)
				displayed = append(displayed, st.renderedLines...)
				if st.lastRaw != "" {
					displayed = append(displayed, renderDiffLine(st.lastRaw))
				}
				if wantLines := strings.Split(renderDiff(string(st.diff)), "\n"); !slices.Equal(displayed, wantLines) {
					t.Fatalf("prefix %d: rendered=%q want %q", i, displayed, wantLines)
				}
			}
		}
		// At the end the incremental state must hold the fully unescaped
		// value (\r is dropped by design, matching the legacy extractor).
		if st := m.streamToolDiffRender[0]; string(st.diff) != strings.ReplaceAll(diff, "\r", "") {
			t.Fatalf("final st.diff=%q want %q", string(st.diff), strings.ReplaceAll(diff, "\r", ""))
		}
	}
}

// oldRenderDiff is the pre-phase-2 renderer, kept as an oracle for the
// renderDiffLine refactor.
func oldRenderDiff(diff string) string {
	if strings.TrimSpace(diff) == "" {
		return ""
	}
	lines := strings.Split(diff, "\n")
	var out strings.Builder
	for _, line := range lines {
		if len(line) == 0 {
			out.WriteByte('\n')
			continue
		}
		switch line[0] {
		case '+':
			if !strings.HasPrefix(line, "+++ ") {
				out.WriteString(DiffAddStyle.Render(line))
			} else {
				out.WriteString(DiffMetaStyle.Render(line))
			}
		case '-':
			if !strings.HasPrefix(line, "--- ") {
				out.WriteString(DiffDelStyle.Render(line))
			} else {
				out.WriteString(DiffMetaStyle.Render(line))
			}
		case '@':
			if strings.HasPrefix(line, "@@") {
				out.WriteString(DiffHunkStyle.Render(line))
			} else {
				out.WriteString(line)
			}
		default:
			out.WriteString(line)
		}
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}

// TestRenderDiffMatchesLegacy pins the renderDiffLine refactor to the
// legacy whole-diff renderer's output.
func TestRenderDiffMatchesLegacy(t *testing.T) {
	samples := []string{
		"",
		"   ",
		"--- a/f\n+++ b/f\n@@ -1,3 +1,4 @@\n ctx\n+add\n\ndel line\nweird @x\n",
		"+plus\n---dash\n@@hunk\n@single\n",
		"trailing newline\n",
	}
	for _, s := range samples {
		if got, want := renderDiff(s), oldRenderDiff(s); got != want {
			t.Fatalf("renderDiff(%q) = %q, legacy %q", s, got, want)
		}
	}
}

// assertDiffParity fails if the model's incrementally-maintained viewport
// content differs from a full setViewportContent rebuild of the same
// chatLines — the invariant the streaming append funnels must preserve.
func assertDiffParity(t *testing.T, m *Model) {
	t.Helper()
	fresh := newStreamTestModel()
	fresh.width = 200 // let setViewportContent run its full rebuild
	fresh.chatLines = append([]string(nil), m.chatLines...)
	fresh.setViewportContent()
	if got, want := m.transcriptText(), fresh.transcriptText(); got != want {
		t.Fatalf("incremental viewport != full rebuild\ngot:  %q\nwant: %q", got, want)
	}
}

// TestPatchFileProgressiveDiffParity streams a realistic patch_file diff in
// small batches (a stride that splits escapes and lines at arbitrary
// points) and asserts after every batch that the incremental viewport state
// is byte-identical to a full rebuild. The old path forced a full
// setViewportContent per batch; the funnel path must reach the same state.
func TestPatchFileProgressiveDiffParity(t *testing.T) {
	m := newStreamTestModel()
	m.width = 200 // let the safety-valve setViewportContent paths run too

	m.handleStreamToolCall(0, "tc0", "patch_file")

	diff := "diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1,3 +1,4 @@\n package main\n+func add() int {\n+\treturn 1\n }\n"
	b, err := json.Marshal(diff)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"diff":` + string(b) + `}`

	for i := 0; i < len(raw); i += 7 {
		end := i + 7
		if end > len(raw) {
			end = len(raw)
		}
		m.handleStreamToolArgs(0, "tc0", raw[i:end])
		assertDiffParity(t, m)
	}

	m.handleStreamToolCallFinal(0, llm.ToolCall{Index: 0, ID: "tc0", Name: "patch_file", Args: map[string]any{"diff": diff}})
	assertDiffParity(t, m)
	m.handleStreamToolResult("tc0", "patch_file", "Applied 1 hunk.", true)
	assertDiffParity(t, m)

	lines := strings.Split(stripAnsi(m.transcriptText()), "\n")
	// The progressive block must be framed; the tool line is rewritten in
	// final form by handleStreamToolCallFinal, so only check its prefix.
	wantBlock := []string{
		"  ╭─ diff ─",
		"  diff --git a/f.go b/f.go",
		"  --- a/f.go",
		"  +++ b/f.go",
		"  @@ -1,3 +1,4 @@",
		"   package main",
		"  +func add() int {",
		"  +    return 1", // wrapLine expands the tab to 4 spaces in viewport content
		"   }",
		"  ╰───────",
	}
	if len(lines) < len(wantBlock)+1 {
		t.Fatalf("content too short (%d lines):\n%s", len(lines), m.transcriptText())
	}
	if !strings.HasPrefix(lines[0], "  → patch_file") {
		t.Fatalf("tool line = %q, want prefix %q", lines[0], "  → patch_file")
	}
	for i, want := range wantBlock {
		if got := lines[1+i]; got != want {
			t.Fatalf("line %d = %q, want %q", 1+i, got, want)
		}
	}
}

// TestPatchFileDiffLastLineGrowth covers the batch shapes that must NOT
// trigger a full rebuild: a batch that only extends the current last diff
// line, and a batch that completes it while appending a new line.
func TestPatchFileDiffLastLineGrowth(t *testing.T) {
	m := newStreamTestModel()
	m.width = 200

	m.handleStreamToolCall(0, "tc0", "patch_file")

	// Diff value open; last line incomplete.
	m.handleStreamToolArgs(0, "tc0", `{"diff":"--- a/f\n+++ b/f\n@@ -1 +1 @@\n-old`)
	assertDiffParity(t, m)

	// Batch grows only the last line (no newline in the delta).
	m.handleStreamToolArgs(0, "tc0", `line`)
	assertDiffParity(t, m)

	// Batch completes the line, appends a new one, and closes the value.
	m.handleStreamToolArgs(0, "tc0", `\n+new"}`)
	assertDiffParity(t, m)

	want := "  → patch_file\n  ╭─ diff ─\n  --- a/f\n  +++ b/f\n  @@ -1 +1 @@\n  -oldline\n  +new"
	if got := stripAnsi(m.transcriptText()); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestPatchFileNonDiffKeyAfterDiff covers the rare middle-line write: a
// non-diff key arrives after the diff value closed, so the compact args on
// the tool-call line change while diff lines sit below it.
func TestPatchFileNonDiffKeyAfterDiff(t *testing.T) {
	m := newStreamTestModel()
	m.width = 200

	m.handleStreamToolCall(0, "tc0", "patch_file")

	// Diff value closed but the object still open (the brace arrives last).
	m.handleStreamToolArgs(0, "tc0", `{"diff":"--- a/f\n+++ b/f\n@@ -1 +1 @@\n+x"`)
	assertDiffParity(t, m)
	lineIdx := m.streamToolCallLines[0]
	if got := stripAnsi(m.chatLines[lineIdx]); got != "  → patch_file" {
		t.Fatalf("tool line before extra key = %q, want %q", got, "  → patch_file")
	}

	m.handleStreamToolArgs(0, "tc0", `,"dry_run":true}`)
	assertDiffParity(t, m)
	if got := stripAnsi(m.chatLines[lineIdx]); !strings.Contains(got, `dry_run="true"`) {
		t.Fatalf("tool line after extra key = %q, want it to contain dry_run=\"true\"", got)
	}
}
