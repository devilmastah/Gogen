package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
)

// stripANSI removes ANSI escape sequences from a string. Uses ansi.Strip
// (parser-based) rather than a regex so non-SGR escapes (OSC, etc.) are
// removed as well.
func stripANSI(s string) string {
	return ansi.Strip(s)
}

// Selection coordinates are rows of the viewport's wrapped content. The rows
// themselves live in exactly one place (the viewport); the PLAIN text of a row
// — ANSI stripped — is derived on demand, for the few rows a mouse event or a
// visible frame actually needs. The previous design kept three extra copies of
// the whole transcript (a joined string, its ANSI-stripped split and its styled
// split) and rebuilt them after every streaming batch.

// plainLine returns row i of the wrapped transcript with ANSI stripped, or ""
// when i is out of range.
func (m *Model) plainLine(i int) string {
	lines := m.viewport.Lines()
	if i < 0 || i >= len(lines) {
		return ""
	}
	return stripANSI(lines[i])
}

// plainLineCount is the number of wrapped transcript rows.
func (m *Model) plainLineCount() int { return len(m.viewport.Lines()) }

// SelectionState tracks the in-progress or finalized text selection.
// StartX/EndX are terminal cell columns (not byte offsets) into the plain line.
type SelectionState struct {
	Active   bool
	Dragging bool // true while the mouse button is held during a drag
	StartX   int  // cell column in plain content (0-based)
	StartY   int  // line in plain content (0-based)
	EndX     int
	EndY     int
}

// mouseEventKind identifies which bubbletea v2 mouse message a mouseEvent
// was normalized from. (v1 carried everything in one MouseMsg with an
// Action field; v2 splits events into distinct message types.)
type mouseEventKind uint8

const (
	mousePress mouseEventKind = iota
	mouseRelease
	mouseMotion
	mouseWheelEvent
)

// mouseEvent normalizes bubbletea v2's typed mouse messages into the single
// shape the selection logic needs (the moral equivalent of v1's MouseMsg).
type mouseEvent struct {
	x, y   int
	button tea.MouseButton
	kind   mouseEventKind
}

// normalizeMouseEvent converts a typed bubbletea v2 mouse message into a
// mouseEvent. Returns ok=false for non-mouse messages.
func normalizeMouseEvent(msg tea.Msg) (mouseEvent, bool) {
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		return mouseEvent{x: ev.X, y: ev.Y, button: ev.Button, kind: mousePress}, true
	case tea.MouseReleaseMsg:
		return mouseEvent{x: ev.X, y: ev.Y, button: ev.Button, kind: mouseRelease}, true
	case tea.MouseMotionMsg:
		return mouseEvent{x: ev.X, y: ev.Y, button: ev.Button, kind: mouseMotion}, true
	case tea.MouseWheelMsg:
		return mouseEvent{x: ev.X, y: ev.Y, button: ev.Button, kind: mouseWheelEvent}, true
	}
	return mouseEvent{}, false
}

// handleMouseSelection processes mouse events for text selection.
// Returns true if the event was consumed (selection handled), false if
// it should be passed through to the viewport for wheel scrolling.
func (m *Model) handleMouseSelection(ev mouseEvent) bool {
	// Block wheel only while dragging so content coordinates stay stable.
	// After release, scrolling is allowed and the highlight tracks content.
	if ev.kind == mouseWheelEvent {
		if m.selection != nil && m.selection.Dragging {
			return true
		}
		return false
	}

	vpHeight := m.viewport.Height
	if m.selection != nil && m.selection.Active {
		return m.handleActiveSelectionMouse(ev, vpHeight)
	}
	return m.startSelectionAt(ev, vpHeight)
}

// handleActiveSelectionMouse processes mouse events while a selection is
// active: drag updates the end point, release finalizes it, right-click
// clears, and left-press restarts. Returns true when the event was consumed.
func (m *Model) handleActiveSelectionMouse(ev mouseEvent, vpHeight int) bool {
	switch {
	case m.selection.Dragging && ev.button == tea.MouseLeft && ev.kind == mouseMotion:
		x, y := m.mouseToContent(ev.x, ev.y)
		if x >= 0 && y >= 0 {
			m.selection.EndX = x
			m.selection.EndY = y
		}
		return true
	case m.selection.Dragging && ev.kind == mouseRelease:
		m.finalizeSelection()
		return true
	case ev.button == tea.MouseRight && (ev.kind == mousePress || ev.kind == mouseRelease):
		m.clearSelection()
		m.statusMsg = ""
		return true
	case !m.selection.Dragging && ev.button == tea.MouseLeft &&
		ev.kind == mousePress && ev.y >= 0 && ev.y < vpHeight:
		// Replace the existing selection; start logic below the if handles it.
		m.clearSelection()
		return m.startSelectionAt(ev, vpHeight)
	case m.selection.Dragging:
		// Ignore other events while dragging (e.g. button-less motion that
		// some terminals emit). Do NOT clear — that was wiping the
		// selection before the user could copy.
		return true
	default:
		return false
	}
}

// startSelectionAt begins a new selection on a left-press inside the
// viewport. Returns true when the event was consumed.
func (m *Model) startSelectionAt(ev mouseEvent, vpHeight int) bool {
	if ev.button == tea.MouseLeft && ev.kind == mousePress &&
		ev.y >= 0 && ev.y < vpHeight {
		x, y := m.mouseToContent(ev.x, ev.y)
		m.statusMsg = ""
		if x >= 0 && y >= 0 {
			m.selectionYOff = m.viewport.YOffset
			m.selection = &SelectionState{
				Active:   true,
				Dragging: true,
				StartX:   x,
				StartY:   y,
				EndX:     x,
				EndY:     y,
			}
		}
		return true
	}
	return false
}

// finalizeSelection ends a drag: unlock scrolling, report the selected
// character count (or clear when nothing was actually selected).
func (m *Model) finalizeSelection() {
	m.selection.Dragging = false
	m.selectionYOff = -1 // unlock scroll; coords are content-absolute
	text := m.getSelectedText()
	if text == "" {
		m.clearSelection()
		m.statusMsg = ""
	} else {
		m.statusMsg = fmt.Sprintf("Selected %d chars — ctrl+shift+c to copy", utf8.RuneCountInString(text))
	}
}

// copySelection copies the current selection to the clipboard. Returns true
// if a selection was present, plus a tea.Cmd when copying had to fall back
// to the terminal (OSC52 via tea.SetClipboard) — the caller must run it.
//
// The local-clipboard path clears the selection on success; the OSC52 path
// keeps it, because OSC52 is fire-and-forget (no delivery ack) and a retry
// should be one keystroke away.
func (m *Model) copySelection() (bool, tea.Cmd) {
	text := m.getSelectedText()
	if text == "" {
		return false, nil
	}
	if err := clipboard.WriteAll(text); err == nil {
		m.statusMsg = fmt.Sprintf("✓ Copied %d chars to clipboard", utf8.RuneCountInString(text))
		m.clearSelection()
		return true, nil
	}
	// Local clipboard unavailable (headless, SSH without xclip/xsel…):
	// fall back to OSC52 through bubbletea's built-in support — most modern
	// terminals (Alacritty, Kitty, iTerm2, GNOME Terminal, Windows
	// Terminal, …) honor it even over SSH.
	m.statusMsg = fmt.Sprintf("Sent %d chars to terminal clipboard", utf8.RuneCountInString(text))
	return true, tea.SetClipboard(text)
}

// runeWidth returns the terminal cell width of r: 2 for wide (CJK/emoji), 1 for
// ordinary, and 0 for zero-width runes — combining marks, ZWJ, variation
// selectors. They must NOT count as their own cell: a selection coordinate is a
// CELL column, and treating "e" + U+0301 as two cells made every cell after it
// one column off (the highlight landed on the wrong characters and the copied
// text was cut short).
func runeWidth(r rune) int {
	if w := cellWidth(string(r)); w > 0 {
		return w
	}
	return 0
}

// sliceByCells returns the substring of s covering terminal cells [start, end).
func sliceByCells(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end <= start {
		return ""
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		rw := runeWidth(r)
		if rw == 0 {
			// Zero-width: it belongs to the cell of the character before it.
			if col-1 >= start && col-1 < end {
				b.WriteRune(r)
			}
			continue
		}
		next := col + rw
		if next > start && col < end {
			b.WriteRune(r)
		}
		col = next
		// Stop only PAST the range: a zero-width rune at col == end belongs to
		// the last included cell, so it must still be visited.
		if col > end {
			break
		}
	}
	return b.String()
}

// cellsToRuneRange maps a half-open cell range to a half-open rune range in s.
func cellsToRuneRange(s string, startCell, endCell int) (startRi, endRi int) {
	if endCell <= startCell {
		return 0, 0
	}
	col := 0
	ri := 0
	startRi = -1
	for _, r := range s {
		rw := runeWidth(r)
		if rw == 0 {
			if col-1 >= startCell && col-1 < endCell {
				if startRi < 0 {
					startRi = ri
				}
				endRi = ri + 1
			}
			ri++
			continue
		}
		next := col + rw
		if next > startCell && col < endCell {
			if startRi < 0 {
				startRi = ri
			}
			endRi = ri + 1
		}
		col = next
		ri++
		if col > endCell {
			// Past the range; a zero-width rune at the boundary was handled
			// above (it belongs to the last included cell).
			break
		}
	}
	if startRi < 0 {
		return 0, 0
	}
	return startRi, endRi
}

// mouseToContent converts terminal-relative mouse coordinates to
// content coordinates (line and cell column in the plain wrapped content).
func (m *Model) mouseToContent(mouseX, mouseY int) (int, int) {
	// Chat column starts right of the sessions panel; presses on the panel
	// itself never select chat text.
	mouseX -= m.sidebarOffsetX()
	if mouseX < 0 {
		return -1, -1
	}
	// Account for viewport scroll position. While dragging, freeze to the
	// YOffset captured at press so motion coordinates stay stable.
	contentY := mouseY + m.viewport.YOffset
	if m.selection != nil && m.selection.Dragging && m.selectionYOff >= 0 {
		contentY = mouseY + m.selectionYOff
	}
	if contentY < 0 || contentY >= m.plainLineCount() {
		return -1, -1
	}

	// Account for left padding (ViewportStyle has PaddingLeft(1)) and the
	// horizontal scroll offset: with xOffset > 0 the first visible content
	// cell IS xOffset, so the content column is shifted right by it (the
	// same mapping the render's [xOffset, xOffset+w) window implies).
	leftPad := m.viewport.Style.GetPaddingLeft()
	contentX := mouseX - leftPad + m.viewport.xOffset
	if contentX < 0 {
		contentX = 0
	}

	line := m.plainLine(contentY)
	if max := cellWidth(line); contentX > max {
		contentX = max
	}

	return contentX, contentY
}

// getSelectedText returns the plain text currently selected, or "" if
// nothing is selected. StartX/EndX are cell columns.
func (m *Model) getSelectedText() string {
	if m.selection == nil || m.plainLineCount() == 0 {
		return ""
	}

	startY, endY := m.selection.StartY, m.selection.EndY
	startX, endX := m.selection.StartX, m.selection.EndX

	// Normalize: ensure start comes before end
	if startY > endY || (startY == endY && startX > endX) {
		startY, endY = endY, startY
		startX, endX = endX, startX
	}

	var b strings.Builder
	for y := startY; y <= endY; y++ {
		if y >= m.plainLineCount() {
			break
		}
		line := m.plainLine(y)
		ls := 0
		le := cellWidth(line)
		if y == startY {
			ls = startX
		}
		if y == endY {
			le = endX
		}
		chunk := sliceByCells(line, ls, le)
		if chunk != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(chunk)
		}
	}
	return b.String()
}

// clearSelection removes any active selection and resets state.
func (m *Model) clearSelection() {
	m.selection = nil
	m.selectionYOff = -1
}

// renderViewportWithSelection renders the viewport content with selection
// highlighting applied. It replicates viewport.View() but injects reverse-video
// ANSI codes on selected text to preserve all original styling while adding
// the selection highlight.
func (m *Model) renderViewportWithSelection() string {
	w := m.viewport.Width
	h := m.viewport.Height
	if sw := m.viewport.Style.GetWidth(); sw != 0 {
		if sw < w {
			w = sw
		}
	}
	if sh := m.viewport.Style.GetHeight(); sh != 0 {
		if sh < h {
			h = sh
		}
	}
	contentWidth := w - m.viewport.Style.GetHorizontalFrameSize()
	contentHeight := h - m.viewport.Style.GetVerticalFrameSize()

	// Freeze scroll offset only while dragging; after release follow viewport.
	yOff := m.viewport.YOffset
	if m.selection != nil && m.selection.Dragging && m.selectionYOff >= 0 {
		yOff = m.selectionYOff
	}
	// The viewport already holds the styled wrapped rows: the selection render
	// reads them directly instead of keeping its own joined+split copy.
	styledLines := m.viewport.Lines()

	// Normalize selection range
	selSY, selEY := m.selection.StartY, m.selection.EndY
	selSX, selEX := m.selection.StartX, m.selection.EndX
	if selSY > selEY || (selSY == selEY && selSX > selEX) {
		selSY, selEY = selEY, selSY
		selSX, selEX = selEX, selSX
	}

	// Match viewport.visibleLines' horizontal window EXACTLY: it cuts every
	// visible line to [xOffset, xOffset+contentWidth) when the viewport is
	// scrolled or when any content line overflows — the same
	// truncate-don't-rewrap rule that keeps lipgloss MaxWidth from
	// soft-wrapping long lines into extra rows and shifting everything
	// below (the selection jump bug). The selection render must show the
	// same cells as the normal render, or the transcript visibly shifts the
	// moment a drag starts.
	cutX := m.viewport.xOffset
	cutAll := cutX > 0 || m.viewport.longestLineWidth > contentWidth

	var lines []string
	for i := 0; i < contentHeight; i++ {
		ci := yOff + i
		if ci < len(styledLines) {
			line := styledLines[ci]
			if ci >= selSY && ci <= selEY {
				plain := stripANSI(line)
				lineWidth := cellWidth(plain)
				hs := 0
				he := lineWidth
				if ci == selSY {
					hs = selSX
				}
				if ci == selEY {
					he = selEX
				}
				if hs > lineWidth {
					hs = lineWidth
				}
				if he > lineWidth {
					he = lineWidth
				}
				if hs < he {
					line = highlightPlainRange(line, plain, hs, he)
				}
			}
			if cutAll && contentWidth > 0 {
				line = cutCells(line, cutX, cutX+contentWidth)
			}
			lines = append(lines, line)
		} else {
			lines = append(lines, "")
		}
	}

	// Pad with fitRows, exactly as Viewport.View does — not with a lipgloss
	// style. It is the same "exactly height rows of width cells, truncate
	// don't re-wrap" contract the normal render uses, measured in wcwidth (so
	// the selection frame cannot disagree with the unselected one about row
	// widths), and it skips a full lipgloss Render pass over the whole screen
	// on every frame of a drag.
	return m.viewport.Style.
		UnsetWidth().UnsetHeight().
		Render(fitRows(lines, contentWidth, contentHeight))
}

// highlightPlainRange inserts reverse-video ANSI codes (\x1b[7m ... \x1b[27m)
// into a styled string over terminal cells [start, end) of the plain text.
// It preserves all existing ANSI styling while adding the selection highlight.
func highlightPlainRange(styled, plain string, start, end int) string {
	startRi, endRi := cellsToRuneRange(plain, start, end)
	if startRi >= endRi {
		return styled
	}

	styledStart := mapPlainToStyled(startRi, styled)
	styledEnd := mapPlainToStyled(endRi, styled)

	if styledStart >= styledEnd {
		return styled
	}

	return styled[:styledStart] + "\x1b[7m" + styled[styledStart:styledEnd] + "\x1b[27m" + styled[styledEnd:]
}

// mapPlainToStyled maps a plain-text rune index to the corresponding
// byte offset in an ANSI-styled string. It skips over ANSI escape sequences
// and handles UTF-8 multi-byte characters.
func mapPlainToStyled(plainIdx int, styled string) int {
	plainPos := 0
	for i := 0; i < len(styled); {
		// Skip ANSI escape sequences. CSI (ESC [) ends at its final byte; OSC
		// (ESC ]) ends at BEL or ST (ESC \). Skipping only CSI counted an OSC
		// payload as plain text, shifting every mapping after a hyperlink.
		if styled[i] == '\x1b' && i+1 < len(styled) {
			switch styled[i+1] {
			case '[':
				end := i + 2
				for end < len(styled) {
					c := styled[end]
					if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
						end++
						break
					}
					end++
				}
				i = end
				continue
			case ']':
				end := i + 2
				for end < len(styled) {
					if styled[end] == '\x07' {
						end++
						break
					}
					if styled[end] == '\x1b' && end+1 < len(styled) && styled[end+1] == '\\' {
						end += 2
						break
					}
					end++
				}
				i = end
				continue
			}
		}
		if plainPos >= plainIdx {
			return i
		}
		_, size := utf8.DecodeRuneInString(styled[i:])
		i += size
		plainPos++
	}
	return len(styled)
}
