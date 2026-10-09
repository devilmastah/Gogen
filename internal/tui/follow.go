package tui

// Viewport follow policy — the TUI's stickToBottom (web parity, see
// internal/server/web/components/scroll.js). The viewport follows new
// content only while the user has not scrolled away from the live edge: a
// manual scroll above the bottom suspends following (viewDetached), so
// streaming tokens cannot yank a reader back down. Returning to the bottom
// — or an explicit "take me to the newest content" action (submit, session
// switch, transcript rebuild) — resumes following.

// followBottom re-pins the viewport to the live edge while following is
// active. Content growth (streamed tokens, appended transcript lines) goes
// through this instead of a raw GotoBottom, so a detached reader keeps
// their position. Growth past the detach baseline also latches the jump
// button's "New messages" state (the web's refreshNewMessages).
func (m *Model) followBottom() {
	if !m.viewDetached {
		m.viewport.GotoBottom()
		return
	}
	if m.viewport.TotalLines() > m.viewDetachLines {
		m.viewNewContent = true
	}
}

// pinFollow scrolls to the live edge and resumes following. Use it for
// explicit jumps to the newest content (submit, session switch, transcript
// rebuild), where landing at the bottom is the point of the action.
func (m *Model) pinFollow() {
	m.viewDetached = false
	m.viewNewContent = false
	m.viewJumpHover = false
	m.viewport.GotoBottom()
}

// viewportScrolledByUser records the follow state after a manual scroll:
// any position above the live edge suspends auto-follow; scrolling back
// down to the bottom resumes it. A fresh detach snapshots the content
// height (markDetached), so the jump button can tell later growth below
// the fold from plain scrolling.
func (m *Model) viewportScrolledByUser() {
	detached := !m.viewport.AtBottom()
	if detached {
		if !m.viewDetached {
			m.viewDetachLines = m.viewport.TotalLines()
			m.viewNewContent = false
		}
	} else {
		m.viewNewContent = false
		m.viewJumpHover = false // the button is hidden at the live edge
	}
	m.viewDetached = detached
}
