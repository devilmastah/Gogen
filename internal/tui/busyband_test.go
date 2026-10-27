package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Regression: the busy progress strip's row was unbounded — long MCP tool
// names ("running mcp__server__very_long_tool…"), stream-retry reasons, the
// token rate, and the queued-messages suffix all concatenated into one
// line. An over-wide row made JoinVertical pad every frame row to its
// width: the frame grew past the terminal, soft-wrapped, and the composer
// and status bar ended up below the bottom edge ("input field outside of
// the border").

func TestFitStripRowCutsToMainWidth(t *testing.T) {
	t.Run("long line is cut to the column width", func(t *testing.T) {
		m := newSidebarTestModel(100)
		m.sidebarVisible = true
		m.sidebarWidth = defaultSidebarWidth
		w := m.mainWidth()
		if w >= 100 {
			t.Fatalf("test setup: mainWidth = %d, want narrower than the terminal", w)
		}
		got := m.fitStripRow(strings.Repeat("x", 200))
		if gotW := ansi.StringWidth(got); gotW != w {
			t.Fatalf("strip row width = %d, want %d", gotW, w)
		}
	})

	t.Run("styled cut does not leak into the next row", func(t *testing.T) {
		m := newSidebarTestModel(100)
		got := m.fitStripRow(DimStyle.Render(strings.Repeat("x", 200)))
		if gotW := ansi.StringWidth(got); gotW != 100 {
			t.Fatalf("strip row width = %d, want 100", gotW)
		}
		// SGR carries across newlines: an open tail would gray out the
		// composer row below the strip.
		if openSGRAt(got, m.mainWidth()) != "" {
			t.Fatalf("cut strip row ends with an open style:\n%q", got)
		}
	})

	t.Run("short line untouched", func(t *testing.T) {
		m := newSidebarTestModel(100)
		if got := m.fitStripRow("  ⣟ thinking"); got != "  ⣟ thinking" {
			t.Fatalf("short strip row changed: %q", got)
		}
	})

	t.Run("zero width model (literal test construction) untouched", func(t *testing.T) {
		m := &Model{}
		line := DimStyle.Render(strings.Repeat("x", 300))
		if got := m.fitStripRow(line); got != line {
			t.Fatal("width-0 model must not cut")
		}
	})
}

// TestBusyFrameStaysInsideTerminal pins the reported symptom end to end:
// with an over-long strip label, rate, and queued suffix, the rendered
// main column must stay within the terminal in BOTH dimensions — no row
// wider than the terminal (that is what pushed the composer below the
// bottom edge).
func TestBusyFrameStaysInsideTerminal(t *testing.T) {
	m := newSidebarTestModel(100)
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	m.SetSize(100, 24)
	m.streaming = true
	m.relayout()
	m.progressPhase = progressTool
	m.progressLabel = "running " + strings.Repeat("mcp__server__tool_", 8) // far wider than the column
	m.spinner = newProgressSpinner()
	m.streamSpeedLine = DimStyle.Render("42 tok/s")
	s := m.focusedSession()
	if s != nil {
		s.steerQ = testQueue(userItem("u1", "one"), userItem("u2", "two"))
	}

	frame := m.renderMainColumn()
	rows := strings.Split(frame, "\n")
	if len(rows) != 24 {
		t.Fatalf("frame rows = %d, want exactly the terminal's 24", len(rows))
	}
	for i, r := range rows {
		if w := ansi.StringWidth(r); w > 100 {
			t.Fatalf("row %d is %d cells wide (> terminal 100):\n%q", i, w, r)
		}
	}
	// The strip stays one row: the label is truncated in place, not wrapped
	// into extra band rows.
	if plain := ansi.Strip(frame); !strings.Contains(plain, "running m") {
		t.Fatalf("strip label missing from the band:\n%s", plain)
	}
}
