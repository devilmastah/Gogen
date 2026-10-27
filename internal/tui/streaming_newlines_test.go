package tui

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

func newStreamTestModel() *Model {
	m := Model{
		chatLines:           nil,
		streamAssistantLine: -1,
		streamThinkingLine:  -1,
	}
	// Wide viewport so wrapping does not split lines; the test isolates the
	// newline/separator logic from wrapping behaviour.
	m.viewport.Width = 200
	// m.width stays 0, so setViewportContent() early-returns and the test
	// exercises the incremental buildFromPrefix() path used during streaming.
	m.resetStreamState(false)
	return &m
}

// hasBlankRenderedLine reports whether the viewport content (as produced by
// the incremental streaming path) contains a fully blank visual line that
// is not part of the intended layout.
func hasBlankRenderedLine(m *Model) bool {
	for _, l := range strings.Split(m.transcriptText(), "\n") {
		if stripAnsi(l) == "" {
			return true
		}
	}
	return false
}

func stripAnsi(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		if r == '\x1b' {
			esc = true
			continue
		}
		if esc {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				esc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// TestNoExtraNewlineBeforeToolCall reproduces the symptom where a model emits
// content ending in a newline right before streaming a tool call and a stray
// blank visual line appears between the assistant text and the tool call.
func TestNoExtraNewlineBeforeToolCall(t *testing.T) {
	m := newStreamTestModel()

	m.handleStreamToken("Let me check.")
	m.handleStreamToken("\n")
	m.handleStreamToolCall(0, "tc0", "read_file")

	if hasBlankRenderedLine(m) {
		t.Fatalf("blank line present between assistant text and tool call\n%s", m.transcriptText())
	}
}

// TestNoExtraNewlineBetweenRounds reproduces the symptom where, after a tool
// result, the next round's assistant text is separated from the tool result
// by a stray blank line carried over from the previous assistant line. This
// exercises the multi-line appendChatLine/appendChatLines prefix accumulation
// that double-counted the separator newline.
func TestNoExtraNewlineBetweenRounds(t *testing.T) {
	m := newStreamTestModel()

	// Round 1: assistant text (with trailing newline) then a tool call.
	m.handleStreamToken("I'll read a file.\n")
	m.handleStreamToolCall(0, "tc0", "read_file")
	m.handleStreamToolCallFinal(0, llm.ToolCall{Index: 0, ID: "tc0", Name: "read_file", Args: map[string]any{"path": "x"}})
	m.handleStreamToolResult("tc0", "read_file", "contents", true)
	// OnStreamEnd fires handleStreamRoundEnd.
	m.handleStreamRoundEnd()

	// Round 2: new assistant content arrives (OnRoundStart reset state).
	m.handleStreamRoundStart()
	m.handleStreamToken("Done.")

	if hasBlankRenderedLine(m) {
		t.Fatalf("blank line present between rounds\n%s", m.transcriptText())
	}
}

// TestNoExtraNewlineAcrossManyAppends covers the general case that produced
// double separators once the incremental prefix became non-empty: every appendChatLine
// after the first must add exactly one separator newline, not two.
func TestNoExtraNewlineAcrossManyAppends(t *testing.T) {
	m := newStreamTestModel()
	m.appendChatLine("line one")
	m.appendChatLine("line two")
	m.appendChatLine("line three")

	if hasBlankRenderedLine(m) {
		t.Fatalf("blank line present across appends\n%s", m.transcriptText())
	}

	want := "line one\nline two\nline three"
	if stripAnsi(m.transcriptText()) != want {
		t.Fatalf("wrappedContent = %q, want %q", m.transcriptText(), want)
	}
}

// TestNoBlankLinesFromWrappedThinking reproduces the symptom where a
// thinking block wider than the viewport word-wraps: lipgloss pads every
// line (except the widest) with trailing spaces when a multi-line string is
// rendered in one call, and reflow's wrap turns each padding run followed by
// a newline into an extra blank visual line. The wide-viewport models used
// elsewhere never wrap, so this test forces a narrow width to exercise the
// wrap path.
func TestNoBlankLinesFromWrappedThinking(t *testing.T) {
	m := newStreamTestModel()
	m.viewport.Width = 24 // force word-wrap

	for _, tok := range []string{
		"Let me think about this.\n",
		"First, check the repo layout.\n",
		"Second, look at config.\n",
		"Third.\n",
	} {
		m.handleStreamThinking(tok)
		if hasBlankRenderedLine(m) {
			t.Fatalf("blank line while streaming %q\n%s", tok, m.transcriptText())
		}
	}
	m.closeThinkingBlock()
	if hasBlankRenderedLine(m) {
		t.Fatalf("blank line after closing thinking block\n%s", m.transcriptText())
	}
}
