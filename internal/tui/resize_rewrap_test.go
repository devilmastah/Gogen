package tui

import (
	"strings"
	"testing"
)

// TestSetSizeSkipsRewrapWhenWidthUnchanged pins the relayout cost fix: SetSize
// runs at every round and turn boundary where the terminal size has not
// changed, so it must not re-wrap the whole transcript again. The contract that
// makes the skip safe is that every chatLines mutation re-wraps itself
// (buildFromPrefix / commitChatLineWrite / a direct setViewportContent), which
// the test relies on: a mutation that bypasses all of them stays visible only
// until the next width change.
func TestSetSizeSkipsRewrapWhenWidthUnchanged(t *testing.T) {
	m := newQueueTestModel()
	m.chatLines = []string{"one short line"}
	m.SetSize(100, 40)
	lines := len(m.viewport.Lines())
	if lines != 1 {
		t.Fatalf("initial wrap produced %d lines, want 1", lines)
	}

	// Rewrite the line WITHOUT re-wrapping: a same-width SetSize must leave the
	// viewport alone (the old behaviour re-wrapped everything here).
	m.chatLines[0] = strings.Repeat("x", 400)
	m.SetSize(100, 40)
	if got := len(m.viewport.Lines()); got != lines {
		t.Fatalf("same-width SetSize re-wrapped: %d lines, want %d", got, lines)
	}

	// A real width change still re-wraps (the 400-char line needs several rows).
	m.SetSize(40, 40)
	if got := len(m.viewport.Lines()); got <= lines {
		t.Fatalf("narrower SetSize did not re-wrap: %d lines, want more than %d", got, lines)
	}
}
