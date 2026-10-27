package tui

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

const sampleToolResult = "line one\nline two"

// TestToolResultLinesShapes pins the three shapes a finished tool call can
// take (diff block, verbose body, one-line summary) in the shared renderer
// both the live stream and a restored transcript use.
func TestToolResultLinesShapes(t *testing.T) {
	t.Run("summary when not verbose", func(t *testing.T) {
		lines := toolResultLines(toolResultParams{name: "read_file", result: sampleToolResult, success: true})
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want status + summary: %#v", len(lines), lines)
		}
	})

	t.Run("body when verbose", func(t *testing.T) {
		lines := toolResultLines(toolResultParams{name: "read_file", result: sampleToolResult, success: true, verbose: true})
		if len(lines) != 3 {
			t.Fatalf("got %d lines, want status + 2 body lines: %#v", len(lines), lines)
		}
		for _, want := range []string{"line one", "line two"} {
			if !strings.Contains(strings.Join(lines, "\n"), want) {
				t.Fatalf("body missing %q: %#v", want, lines)
			}
		}
	})

	t.Run("show_diff renders the block", func(t *testing.T) {
		diff := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new"
		lines := toolResultLines(toolResultParams{name: "show_diff", result: diff, success: true})
		if !strings.Contains(strings.Join(lines, "\n"), "diff") {
			t.Fatalf("expected a diff block: %#v", lines)
		}
		if len(lines) < 3 {
			t.Fatalf("expected more than a status line: %#v", lines)
		}
	})

	t.Run("patch_file renders the supplied diff", func(t *testing.T) {
		diff := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new"
		lines := toolResultLines(toolResultParams{name: "patch_file", result: "applied", success: true, diff: diff})
		if !strings.Contains(strings.Join(lines, "\n"), "diff") {
			t.Fatalf("expected a diff block: %#v", lines)
		}
	})

	t.Run("patch_file without a diff is never a bare status line", func(t *testing.T) {
		for _, verbose := range []bool{false, true} {
			lines := toolResultLines(toolResultParams{name: "patch_file", result: sampleToolResult, success: true, verbose: verbose})
			if len(lines) < 2 {
				t.Fatalf("verbose=%v: patch_file result rendered only a status line: %#v", verbose, lines)
			}
		}
	})
}

// TestToolResultLiveMatchesHistory is the D1 regression: the live stream
// (handleStreamToolResult → toolResultLines with the explicit success flag and
// the captured diff) and a restored transcript (renderToolResult → tool name
// and diff resolved from the tool call) must produce identical lines,
// including in verbose mode — which the history path used to ignore.
func TestToolResultLiveMatchesHistory(t *testing.T) {
	diff := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new"
	for _, tc := range []struct {
		name    string
		tool    string
		result  string
		diffArg string
	}{
		{"read_file", "read_file", sampleToolResult, ""},
		{"show_diff", "show_diff", diff, ""},
		{"patch_file", "patch_file", "applied", diff},
	} {
		for _, verbose := range []bool{false, true} {
			t.Run(tc.name+"/verbose="+map[bool]string{true: "on", false: "off"}[verbose], func(t *testing.T) {
				call := llm.ToolCall{ID: "c1", Name: tc.tool}
				if tc.diffArg != "" {
					call.Args = map[string]any{"diff": tc.diffArg}
				}
				msg := llm.Message{Role: "tool", ToolCallID: "c1", Content: tc.result}
				fromHistory := renderToolResult(msg, map[string]llm.ToolCall{"c1": call}, verbose)

				// The live path: the runner reports success explicitly, and
				// the captured diff is only re-rendered when it was not
				// already drawn while the arguments streamed.
				fromLive := toolResultLines(toolResultParams{
					name:    tc.tool,
					result:  tc.result,
					success: true,
					diff:    tc.diffArg,
					verbose: verbose,
				})

				if got, want := strings.Join(fromHistory, "\n"), strings.Join(fromLive, "\n"); got != want {
					t.Fatalf("history and live rendering diverged\ngot:\n%s\nwant:\n%s", got, want)
				}
			})
		}
	}
}

// TestToolResultSuccessHeuristic pins the only success signal a restored
// transcript has: the agent prefixes failures with "Error:".
func TestToolResultSuccessHeuristic(t *testing.T) {
	if !toolResultSuccess("applied 2 hunks") {
		t.Fatal("plain result must be a success")
	}
	if !toolResultSuccess("  content that mentions Error: later in the body") {
		t.Fatal("only a leading Error: prefix means failure")
	}
	if toolResultSuccess("Error: file not found") {
		t.Fatal("Error: prefix must be a failure")
	}
	if toolResultSuccess("  Error: file not found") {
		t.Fatal("leading whitespace must not hide the failure")
	}
}
