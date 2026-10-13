package tui

import (
	"bytes"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// cellWidth returns the number of terminal cells s occupies under the SAME
// width method Bubble Tea's renderer uses, which is ansi.WcWidth: ultraviolet's
// screen buffer is created with that method (NewScreenBuffer) and only
// switches to ansi.GraphemeWidth when the terminal reports Unicode core mode
// 2027 as already set (bubbletea tea.go, on the DECRQM reply). Mode 2027 is
// off by default, so WcWidth is what actually paints the frame.
//
// The TUI must measure the same way it is measured. ansi.StringWidth (and
// lipgloss.Width, and lipgloss.Wrap) all use GraphemeWidth, which disagrees
// with the renderer on multi-codepoint grapheme clusters — a ZWJ emoji
// sequence ("👨‍👩‍👧‍👦") is 2 cells by grapheme but 8 by wcwidth, and a skin-tone
// modifier ("👍🏽") is 2 vs 4. Under-reserving that difference shifts whatever
// follows: a sidebar's right border lands short of its column, the status
// bar's middle gap opens, a prompt-rail tooltip border drifts left.
//
// Plain emoji, CJK and ASCII are unaffected — the two methods agree there —
// so this only changes behaviour for the clusters that were actually broken.
//
// A row whose padding lipgloss computes itself (StatusBarStyle.Width, the
// viewport's padding) still pads by GraphemeWidth; that is internal to
// lipgloss v2.0.6 and cannot be redirected from here. Those rows are neither
// helped nor harmed by this change.
func cellWidth(s string) int {
	return ansi.StringWidthWc(s)
}

// blockWidth returns the width of the widest line in a (possibly
// multi-line) block, measured the way the renderer measures.
//
// It exists because lipgloss.Width and ansi.StringWidth do NOT agree on a
// multi-line string: lipgloss.Width reports the widest line, while
// ansi.StringWidth — and ansi.StringWidthWc, hence cellWidth — SUM every
// line (a 60-cell, 11-line modal measures 660 by summation). So cellWidth is
// only a drop-in for lipgloss.Width on a SINGLE line; a block needs this.
func blockWidth(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		if lw := cellWidth(line); lw > w {
			w = lw
		}
	}
	return w
}

// cutCells truncates a row to the cell budget the layout reserved for it.
//
// It deliberately stays on ansi.Cut (GraphemeWidth) rather than ansi.CutWc:
// CutWc is broken in x/ansi v0.11.8 — cut() wires truncateLeft = TruncateWc
// for the Wc path, but a left-truncator is TruncateLeftWc, so
// CutWc("abcdef", 1, 3) returns "a" instead of "bc" and CutWc(s, 3, 6)
// returns "abc" instead of "def". That silently corrupted the prompt rail's
// pasteCell and the tooltip rows (whole rows came back at double width).
//
// The truncation sites are safe on GraphemeWidth because the budget they cut
// TO is now measured by cellWidth: reserving the renderer's (Wider) width and
// cutting with the (narrower) grapheme measure can only ever cut a little
// early, never past the reserved column.
func cutCells(s string, left, right int) string {
	return ansi.Cut(s, left, right)
}

// wrapCells is lipgloss.Wrap measured the way the renderer measures, for the
// same reason as cellWidth. It mirrors lipgloss.Wrap exactly — ansi.Wrap to
// break the line, then a WrapWriter pass to re-open any SGR style that is
// still active at each inserted newline — with the Wc-width wrapper swapped
// in. Both halves matter: the WrapWriter pass is what keeps a styled chat
// line styled across its wrap points (see wrapLine).
func wrapCells(s string, width int) string {
	if width < 1 {
		return s
	}
	var buf bytes.Buffer
	w := lipgloss.NewWrapWriter(&buf)
	defer w.Close() //nolint:errcheck
	_, _ = io.WriteString(w, ansi.WrapWc(s, width, ""))
	return buf.String()
}
