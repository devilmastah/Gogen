package tui

import (
	"fmt"
	"strings"
	"testing"

	"gogen/internal/agent"
)

// The inline renderer owns every row of a terminal-height frame, so the frame
// must be EXACTLY terminal width × terminal height, measured the way the
// renderer paints (wcwidth). A row one cell too wide soft-wraps in the
// terminal, every row below it shifts down, and the renderer's incremental
// cell diff desyncs from the screen — ghost rows, stuck columns, half-missing
// lines — until something forces a full repaint, which is why a resize
// appears to "fix" the corruption.
//
// The width bugs all came from one cause: lipgloss's Width/padding/joins and
// ansi.Cut/Truncate measure by GRAPHEME width, while ultraviolet paints with
// wcwidth. On a multi-codepoint cluster the two disagree — a ZWJ family emoji
// is 2 cells by grapheme and 8 by wcwidth, a skin-tone modifier 2 vs 4 — so
// every lipgloss-computed pad over-fills the row by the difference. Transcript
// lines, session labels, model names and working directories can all carry
// those clusters (they come from model output and from the user).

// wideClusterLines is content that must never widen a frame row: a plain line,
// a wrapping line, ZWJ/skin-tone/flag clusters, styled text and a long token.
func wideClusterLines() []string {
	return []string{
		"plain line",
		strings.Repeat("wide ", 60),
		"family 👨‍👩‍👧‍👦 and 👍🏽 and 你好 with a long tail that wraps around the edge of the screen",
		"\x1b[31mred styled text that is long enough to wrap somewhere\x1b[0m",
		strings.Repeat("x", 300),
		"🏳️‍🌈 flag sequence",
	}
}

// checkFrameShape asserts the frame's row count and per-row cell width — the
// invariant the inline renderer depends on.
func checkFrameShape(t *testing.T, m *Model) {
	t.Helper()
	rows := strings.Split(m.View().Content, "\n")
	if len(rows) != m.height {
		t.Fatalf("frame has %d rows, want %d", len(rows), m.height)
	}
	for i, row := range rows {
		if w := cellWidth(row); w > m.width {
			t.Fatalf("row %d is %d cells wide in a %d-cell terminal (the terminal soft-wraps it and desyncs the renderer): %q",
				i, w, m.width, row)
		}
	}
}

// TestFrameFitsTerminalWithWideClusters walks the states that change the frame
// geometry — terminal size, modal, busy band, sidebar, composer content, live
// sidebar rows, status-bar model/dir — with ZWJ/skin-tone content everywhere.
func TestFrameFitsTerminalWithWideClusters(t *testing.T) {
	sizes := [][2]int{{80, 24}, {100, 30}, {40, 10}, {24, 8}, {12, 6}, {120, 40}}
	modals := []ModalKind{
		ModalNone, ModalApproval, ModalSessions, ModalModels, ModalHelp,
		ModalCompletion, ModalSubagents, ModalLiveSessions, ModalConfirm, ModalQueue,
	}
	for _, size := range sizes {
		for _, modal := range modals {
			for _, streaming := range []bool{false, true} {
				for _, sidebar := range []bool{false, true} {
					w, h := size[0], size[1]
					name := fmt.Sprintf("%dx%d modal=%v streaming=%v sidebar=%v", w, h, modal, streaming, sidebar)
					t.Run(name, func(t *testing.T) {
						m := newSidebarTestModel(w)
						m.lives = newLiveSessions(&agent.Agent{})
						m.SetSize(w, h)
						if sidebar {
							m.toggleSidebar()
						}
						m.streaming = streaming
						m.relayout()
						m.modal = modal
						m.chatLines = wideClusterLines()
						m.setViewportContent()
						checkFrameShape(t, m)

						// The composer (textarea) with wide clusters.
						m.textarea.SetValue("hello 👨‍👩‍👧‍👦 👍🏽 你好 sh" + strings.Repeat("x", 40))
						checkFrameShape(t, m)

						// A live session row whose label holds wide clusters:
						// the sidebar and the live-sessions modal both render it.
						m.lives.Add(&agent.Agent{SessionID: "bg-emoji"}, "family 👨‍👩‍👧‍👦 👍🏽 你好")
						checkFrameShape(t, m)

						// Status bar: a wide cluster in the model name and the
						// working directory.
						a := agent.NewAgent(emojiModelNameProvider{}, agent.NewExecutor("/tmp"), nil)
						a.WorkingDir = "/tmp/dir 👨‍👩‍👧‍👦 👍🏽 你好"
						m.agent = a
						checkFrameShape(t, m)
					})
				}
			}
		}
	}
}

// emojiModelNameProvider reports a model name carrying ZWJ and skin-tone
// clusters, the shape the status bar renders.
type emojiModelNameProvider struct{ fakeStreamProvider }

func (emojiModelNameProvider) ModelName() string {
	return "model 👨‍👩‍👧‍👦 👍🏽 你好 long name"
}

// TestViewportRowWidthIsExactCells pins the viewport's row fitting: lipgloss's
// Width padding measured the grapheme width, so a transcript line with a ZWJ
// cluster was padded as if it were narrower than it paints and the row came
// out six cells wider than the viewport (View used to rely on that padding).
func TestViewportRowWidthIsExactCells(t *testing.T) {
	const width, height = 80, 24
	for _, line := range []string{
		"emoji 👨‍👩‍👧‍👦 done",
		"thumbs 👍🏽 done",
		"flag 🏳️‍🌈 done",
		"你好世界 done",
		"plain line",
	} {
		t.Run(line, func(t *testing.T) {
			m := newSidebarTestModel(width)
			m.SetSize(width, height)
			m.chatLines = []string{line}
			m.setViewportContent()

			rows := strings.Split(m.viewport.View(), "\n")
			if len(rows) != m.viewport.Height {
				t.Fatalf("viewport rendered %d rows, want %d", len(rows), m.viewport.Height)
			}
			for i, row := range rows {
				// Padding cells plus the style's one-cell padding each side.
				if w, want := cellWidth(row), m.viewport.Width; w != want {
					t.Fatalf("row %d is %d cells, want exactly %d: %q", i, w, want, row)
				}
			}
		})
	}
}

// TestStatusBarRowWidthIsExactCells pins the bar's own padding: the bar used
// StatusBarStyle.Width(w), which pads by grapheme width, so a model name or
// working directory with a ZWJ cluster made the bottom frame row up to 16
// cells wider than the terminal (measured live: 96 cells in an 80-cell
// terminal).
func TestStatusBarRowWidthIsExactCells(t *testing.T) {
	for _, width := range []int{40, 80, 120, 8, 3} {
		m := newSidebarTestModel(width)
		m.SetSize(width, 24)
		a := agent.NewAgent(emojiModelNameProvider{}, agent.NewExecutor("/tmp"), nil)
		a.WorkingDir = "/tmp/dir 👨‍👩‍👧‍👦 👍🏽 你好"
		m.agent = a

		for _, bar := range []string{m.renderStatusBar()} {
			if got := len(strings.Split(bar, "\n")); got != 1 {
				t.Fatalf("width %d: status bar rendered %d rows, want 1", width, got)
			}
			if got := cellWidth(bar); got > m.mainWidth() {
				t.Fatalf("width %d: status bar is %d cells, want <= %d: %q", width, got, m.mainWidth(), bar)
			}
		}

		// The transient status message path pads/centers the same way.
		m.statusMsg = "✗ Compact failed: 👨‍👩‍👧‍👦 the provider refused the request"
		if got := cellWidth(m.renderStatusBar()); got > m.mainWidth() {
			t.Fatalf("width %d: status-message bar is %d cells, want <= %d", width, got, m.mainWidth())
		}
	}
}

// TestModalOverlayIsExactCells pins the modal overlay: lipgloss.Place sized the
// box by grapheme width, so a modal row holding a ZWJ session label was padded
// out past the terminal (measured live: 88 cells in an 80-cell terminal) —
// one soft-wrapped row and the whole frame below it shifts.
func TestModalOverlayIsExactCells(t *testing.T) {
	const width, height = 80, 24
	m := newSidebarTestModel(width)
	m.SetSize(width, height)
	m.lives.Add(&agent.Agent{SessionID: "bg-emoji"}, "family 👨‍👩‍👧‍👦 👍🏽 你好")
	m.modal = ModalLiveSessions
	m.liveCursor = 0

	frame := m.View().Content
	rows := strings.Split(frame, "\n")
	if len(rows) != height {
		t.Fatalf("overlay has %d rows, want %d", len(rows), height)
	}
	for i, row := range rows {
		// Exact, not just bounded: the overlay's background must cover every
		// cell of the screen.
		if w := cellWidth(row); w != width {
			t.Fatalf("overlay row %d is %d cells, want exactly %d: %q", i, w, width, row)
		}
	}
}

// TestTruncateCells uses wcwidth: ansi.Truncate/CutWc cannot be used for this
// (their early return compares the GRAPHEME width whatever method is asked
// for, so a ZWJ row never takes the truncating path).
func TestTruncateCells(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{name: "fits unchanged", in: "hello", width: 10, want: "hello"},
		{name: "ascii cut", in: "hello world", width: 5, want: "hello"},
		{name: "zwj cluster is 8 cells", in: "a👨‍👩‍👧‍👦b", width: 9, want: "a👨‍👩‍👧‍👦"},
		{name: "skin tone is 4 cells", in: "👍🏽👍🏽", width: 4, want: "👍🏽"},
		{name: "cjk is 2 cells", in: "你好世界", width: 5, want: "你好"},
		{name: "zero width", in: "hello", width: 0, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateCells(tc.in, tc.width)
			if got != tc.want {
				t.Fatalf("truncateCells(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
			}
			if w := cellWidth(got); w > tc.width {
				t.Fatalf("truncateCells(%q, %d) is %d cells, want <= %d", tc.in, tc.width, w, tc.width)
			}
		})
	}
}

// TestPadCells pads in the renderer's measure and never widens the row.
func TestPadCells(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
	}{
		{in: "emoji 👨‍👩‍👧‍👦", width: 40},
		{in: "thumbs 👍🏽", width: 40},
		{in: strings.Repeat("👨‍👩‍👧‍👦", 8), width: 20},
	} {
		got := padCells(tc.in, tc.width)
		if w := cellWidth(got); w != tc.width {
			t.Fatalf("padCells(%q, %d) is %d cells, want exactly %d", tc.in, tc.width, w, tc.width)
		}
	}
}

// TestDividerHintFitsColumn pins the fitted focus hint: the full sentence on a
// wide column, a shorter form on a narrow one, and never more cells than the
// column (the old code appended the full hint and relied on the row being
// truncated, so the divider itself was over-wide).
func TestDividerHintFitsColumn(t *testing.T) {
	if got := dividerHint(200); got != viewportFocusIndicator {
		t.Fatalf("wide column hint = %q, want the full indicator", got)
	}
	for _, width := range []int{44, 40, 30, 20, 10, 3, 0} {
		hint := dividerHint(width)
		if w := cellWidth(hint); w > width && width >= cellWidth(viewportFocusIndicatorTag) {
			t.Fatalf("dividerHint(%d) is %d cells: %q", width, w, hint)
		}
		if width >= cellWidth(viewportFocusIndicatorTag) && !strings.Contains(hint, "[SCROLL]") {
			t.Fatalf("dividerHint(%d) lost the affordance: %q", width, hint)
		}
	}
}
