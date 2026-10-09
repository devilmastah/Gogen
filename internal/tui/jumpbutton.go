package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Jump button — the TUI's port of the web's scroll-bottom button
// (#scroll-bottom-btn, components/scroll.js): a label set into the divider
// row right below the transcript, shown while the reader is parked above
// the live edge. It names the state — plain "Jump to latest" while reading
// scrolled-up history, "New messages" once content landed below the fold
// (viewNewContent) — and a left click jumps to the live edge. The keyboard
// equivalents (end / G) are documented in the help modal.
const (
	jumpButtonJumpLabel = "↓ Jump to latest"
	jumpButtonNewLabel  = "↓ New messages"
)

// jumpButtonText returns the button's plain label for the current state.
func (m *Model) jumpButtonText() string {
	if m.viewNewContent {
		return jumpButtonNewLabel
	}
	return jumpButtonJumpLabel
}

// jumpButtonSegment renders the label as a pill: one padding cell on each
// side, cyan — the sidebar footer's clickable-button color — and bold
// while hovered.
func (m *Model) jumpButtonSegment() string {
	style := ansiCyanOn
	if m.viewJumpHover {
		style = ansiHighlightOn
	}
	return style + " " + m.jumpButtonText() + " " + ansiReset
}

// jumpButtonZone returns the button's inclusive horizontal cell range on
// the divider row (frame row m.viewport.Height). ok=false when the button
// is not rendered: following the live edge, a modal is up, or the chat
// column is too narrow for the label (plus the viewport-focus indicator,
// which owns the same row's right end).
func (m *Model) jumpButtonZone() (x0, x1 int, ok bool) {
	if !m.viewDetached || m.modal != ModalNone {
		return 0, 0, false
	}
	chatW := m.mainWidth()
	left := m.sidebarOffsetX()
	w := ansi.StringWidth(m.jumpButtonText()) + 2 // one padding cell each side
	if chatW < w+2 {
		return 0, 0, false
	}
	// Centered like the web pill, then shifted clear of the viewport-focus
	// indicator, which owns the divider's right end (it is appended after
	// the dash fill).
	x0 = left + (chatW-w)/2
	if m.focus == FocusViewport {
		hintStart := left + max(0, chatW-len(viewportFocusIndicator))
		if x0+w > hintStart {
			x0 = hintStart - w
		}
	}
	if x0 < left || x0+w-1 > left+chatW-1 {
		return 0, 0, false
	}
	return x0, x0 + w - 1, true
}

// applyJumpButtonOverlay pastes the jump button into the divider row of
// the rendered frame. The frame is returned unchanged when the button is
// not shown (see jumpButtonZone).
func (m *Model) applyJumpButtonOverlay(frame string) string {
	x0, _, ok := m.jumpButtonZone()
	if !ok {
		return frame
	}
	lines := strings.Split(frame, "\n")
	pasteCell(&lines, m.viewport.Height, x0, m.jumpButtonSegment())
	return strings.Join(lines, "\n")
}

// handleJumpButtonMouse processes pointer events over the jump button.
// Button-less motion tracks the hover highlight and is never consumed, so
// an in-flight selection drag keeps flowing; a left press jumps to the
// live edge and is consumed. Returns true when consumed.
func (m *Model) handleJumpButtonMouse(ev mouseEvent) bool {
	x0, x1, ok := m.jumpButtonZone()
	if !ok {
		m.viewJumpHover = false
		return false
	}
	inZone := ev.y == m.viewport.Height && ev.x >= x0 && ev.x <= x1
	switch {
	case ev.kind == mouseMotion && ev.button == tea.MouseNone:
		m.viewJumpHover = inZone
	case ev.kind == mousePress && ev.button == tea.MouseLeft && inZone:
		m.pinFollow()
		return true
	}
	return false
}
