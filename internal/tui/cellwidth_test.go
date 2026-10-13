package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Regression: the TUI measured its text with ansi.StringWidth / lipgloss.Width
// (GraphemeWidth) while Bubble Tea's renderer measures with ansi.WcWidth —
// ultraviolet's screen buffer is created with that method and only switches to
// grapheme width when the terminal reports Unicode core mode 2027 as already
// set, which is off by default. The two disagree on multi-codepoint grapheme
// clusters: a ZWJ emoji ("👨‍👩‍👧‍👦") is 2 cells by grapheme and 8 by wcwidth,
// a skin-tone modifier ("👍🏽") is 2 vs 4. Every padding computation therefore
// under-reserved, and whatever followed the padding — the sidebar's right
// border, the divider's end, the status bar's middle gap — landed short of its
// column.

// The clusters the two measures disagree on. zwj is the zero-width joiner
// that fuses emoji into one grapheme cluster; it is written as an escape so
// the invisible character does not trip staticcheck's ST1018.
const zwj = "\u200d"

const (
	zwjFamily     = "\U0001F468" + zwj + "\U0001F469" + zwj + "\U0001F467" + zwj + "\U0001F466"
	skinToneThumb = "\U0001F44D\U0001F3FD"
)

func TestCellWidthUsesTheRenderersMethod(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"ascii", "hello"},
		{"cjk", "你好世界"},
		{"plain emoji", "🚀🎉"},
		{"zwj emoji", zwjFamily},
		{"skin tone emoji", skinToneThumb},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := cellWidth(tc.in), ansi.StringWidthWc(tc.in); got != want {
				t.Fatalf("cellWidth(%q) = %d, want the renderer's WcWidth %d", tc.in, got, want)
			}
		})
	}
}

// Plain ASCII, CJK and single-codepoint emoji must be untouched by the switch —
// only the clusters that were actually broken may change.
func TestCellWidthAgreesOnOrdinaryText(t *testing.T) {
	for _, s := range []string{"", "hello world", "café", "你好世界", "Привет", "🚀🎉✅"} {
		if cellWidth(s) != ansi.StringWidth(s) {
			t.Fatalf("cellWidth(%q) = %d, differs from the grapheme measure %d", s, cellWidth(s), ansi.StringWidth(s))
		}
	}
}

// blockWidth guards the multi-line case that a blanket cellWidth swap got
// wrong: lipgloss.Width reports the widest line, but ansi.StringWidth AND
// ansi.StringWidthWc SUM every line — a 60-cell, 11-line modal measures 660 by
// summation, which broke the models-modal click origin (TestModelsModalMouse).
func TestBlockWidthTakesWidestLine(t *testing.T) {
	block := strings.Repeat("x", 60) + "\n" + strings.Repeat("y", 12) + "\n" + strings.Repeat("z", 30)
	if got := blockWidth(block); got != 60 {
		t.Fatalf("blockWidth = %d, want the widest line (60)", got)
	}
	// It must agree with lipgloss.Width whenever the two measures agree
	// (i.e. ordinary text), so it is a faithful swap and not a new rule.
	if got, want := blockWidth("abc\ndefgh\nij"), lipgloss.Width("abc\ndefgh\nij"); got != want {
		t.Fatalf("blockWidth = %d, lipgloss.Width = %d", got, want)
	}
}

// wrapCells replaces lipgloss.Wrap so the wrap pass measures like the
// renderer, but it must keep lipgloss's second half: the WrapWriter pass that
// re-opens an SGR style left active at each inserted newline. Losing that is
// what made styled tool lines lose their colour at wrap points.
func TestWrapCellsKeepsLipglossStylePropagation(t *testing.T) {
	styled := "\x1b[36m" + strings.Repeat("styled text ", 6) + "\x1b[0m"
	got := wrapCells(styled, 20)
	want := lipgloss.Wrap(styled, 20, "")
	if got != want {
		t.Fatalf("wrapCells diverged from lipgloss.Wrap:\n got: %q\nwant: %q", got, want)
	}
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected several wrapped lines, got %d", len(lines))
	}
	for i, l := range lines[1:] {
		if !strings.Contains(l, "\x1b[36m") {
			t.Fatalf("continuation line %d lost the open style: %q", i+1, l)
		}
	}
}

// The regression itself: a ZWJ emoji in a session label used to make the
// sidebar's right border land short of the panel edge.
func TestSidebarBorderHoldsWithZWJLabel(t *testing.T) {
	m := newSidebarTestModel(100)
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	inner := m.sidebarWidth - 3

	row := m.renderSidebarTitle(sidebarRow{id: "a", label: zwjFamily + " family"}, false, inner)
	// renderSidebarTitle emits "│ " + content and renderSidebar appends the
	// right border, so the content must fill exactly the inner budget.
	if got := ansi.StringWidthWc(row); got != inner+2 {
		t.Fatalf("title row is %d cells, want %d (the right border must land on the panel edge):\n%q",
			got, inner+2, ansi.Strip(row))
	}
}

// A ZWJ-heavy transcript must not shorten the divider or push the frame's
// columns out of alignment.
func TestFrameColumnsHoldWithZWJTranscript(t *testing.T) {
	m := newSidebarTestModel(80)
	m.sidebarVisible = false
	m.SetSize(80, 24)
	m.chatLines = []string{strings.Repeat(zwjFamily, 3)}
	m.setViewportContent()

	rows := strings.Split(m.renderMainColumn(), "\n")
	divider := rows[len(rows)-3] // viewport, divider, input band, status bar
	if got := ansi.StringWidthWc(divider); got != m.mainWidth() {
		t.Fatalf("divider is %d cells, want the main column's %d", got, m.mainWidth())
	}
}
