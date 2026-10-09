package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// detachedJumpModel parks a scrollable transcript above the live edge so
// the jump button is shown.
func detachedJumpModel(t *testing.T, lines int) *Model {
	t.Helper()
	m := scrollableModel(t, lines)
	m.SetSize(100, 30)
	m.viewport.GotoBottom()
	m.viewDetached = false
	m.viewNewContent = false
	m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if !m.viewDetached {
		t.Fatal("test setup: pgup must detach")
	}
	return m
}

// The jump button mirrors the web's scroll-bottom button: hidden while
// following, a plain "Jump to latest" label when only reading scrolled-up
// history, and "New messages" once content landed below the fold.
func TestJumpButtonStates(t *testing.T) {
	m := detachedJumpModel(t, 40)

	t.Run("hidden while following", func(t *testing.T) {
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnd})
		if _, _, ok := m.jumpButtonZone(); ok {
			t.Fatal("jump button must be hidden while following")
		}
		if frame := stripANSI(m.View().Content); strings.Contains(frame, "Jump to latest") {
			t.Fatal("following frame must not paint the jump label")
		}
	})

	t.Run("plain label on the divider row when scrolled up", func(t *testing.T) {
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
		x0, x1, ok := m.jumpButtonZone()
		if !ok {
			t.Fatal("jump button must show when detached")
		}
		if m.viewNewContent {
			t.Fatal("a fresh scroll-up must not latch new content")
		}
		rows := strings.Split(stripANSI(m.View().Content), "\n")
		if m.viewport.Height >= len(rows) {
			t.Fatalf("divider row %d outside the frame (%d rows)", m.viewport.Height, len(rows))
		}
		divider := rows[m.viewport.Height]
		if got := strings.TrimSpace(sliceByCells(divider, x0, x1+1)); got != m.jumpButtonText() {
			t.Fatalf("painted label = %q, want %q", got, m.jumpButtonText())
		}
		if !strings.Contains(divider, jumpButtonJumpLabel) {
			t.Fatalf("divider row missing the jump label: %q", divider)
		}
	})

	t.Run("new messages after growth below the fold", func(t *testing.T) {
		m.appendChatLine(DimStyle.Render("streamed line"))
		if !m.viewNewContent {
			t.Fatal("growth past the detach baseline must latch viewNewContent")
		}
		if got := m.jumpButtonText(); got != jumpButtonNewLabel {
			t.Fatalf("label = %q, want %q", got, jumpButtonNewLabel)
		}
		divider := strings.Split(stripANSI(m.View().Content), "\n")[m.viewport.Height]
		if !strings.Contains(divider, jumpButtonNewLabel) {
			t.Fatalf("divider row missing the new-messages label: %q", divider)
		}
	})

	t.Run("end clears the latch and hides", func(t *testing.T) {
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnd})
		if m.viewDetached || m.viewNewContent {
			t.Fatalf("end must re-attach and clear the latch (detached=%v new=%v)",
				m.viewDetached, m.viewNewContent)
		}
		if _, _, ok := m.jumpButtonZone(); ok {
			t.Fatal("jump button must hide at the live edge")
		}
	})

	t.Run("resize does not fake new content", func(t *testing.T) {
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
		if m.viewNewContent {
			t.Fatal("fresh detach must not latch")
		}
		m.SetSize(80, 30) // the narrower re-wrap grows the line count
		if m.viewNewContent {
			t.Fatal("a resize alone must not read as new messages")
		}
		m.SetSize(100, 30)
	})

	t.Run("hidden under a modal", func(t *testing.T) {
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
		if _, _, ok := m.jumpButtonZone(); !ok {
			t.Fatal("jump button must show when detached")
		}
		m.modal = ModalHelp
		if _, _, ok := m.jumpButtonZone(); ok {
			t.Fatal("modal overlay must hide the jump button")
		}
		m.modal = ModalNone
	})
}

// A left click on the label jumps to the live edge (web parity); other
// presses on the divider row do nothing, and the overlay never changes the
// frame's row count.
func TestJumpButtonClickJumps(t *testing.T) {
	m := detachedJumpModel(t, 40)

	base := m.renderMainColumn()
	if got, want := strings.Count(m.applyJumpButtonOverlay(base), "\n"), strings.Count(base, "\n"); got != want {
		t.Fatalf("overlay changed the frame height: %d -> %d rows", want, got)
	}

	x0, x1, ok := m.jumpButtonZone()
	if !ok {
		t.Fatal("test setup: button must be visible")
	}
	row := m.viewport.Height

	// Off-zone press (same row): not consumed, no jump.
	if m.handleJumpButtonMouse(mouseEvent{x: m.sidebarOffsetX(), y: row, button: tea.MouseLeft, kind: mousePress}) {
		t.Fatal("press outside the label must not be consumed")
	}
	if !m.viewDetached {
		t.Fatal("press outside the label must not jump")
	}

	// Full message path: click the middle of the label.
	m.Update(tea.MouseClickMsg{X: (x0 + x1) / 2, Y: row, Button: tea.MouseLeft})
	if m.viewDetached {
		t.Fatal("clicking the label must re-attach auto-follow")
	}
	if !m.viewport.AtBottom() {
		t.Fatal("clicking the label must jump to the live edge")
	}
}

// Hover follows the pointer: the label goes bold while the pointer is on
// it, and the highlight clears when the pointer leaves (the sidebar path
// clears it too, since the button is not under the panel).
func TestJumpButtonHover(t *testing.T) {
	m := detachedJumpModel(t, 40)
	x0, x1, _ := m.jumpButtonZone()
	row := m.viewport.Height

	m.Update(tea.MouseMotionMsg{X: (x0 + x1) / 2, Y: row, Button: tea.MouseNone})
	if !m.viewJumpHover {
		t.Fatal("motion over the label must set the hover highlight")
	}
	if got := m.jumpButtonSegment(); !strings.Contains(got, ansiHighlightOn) {
		t.Fatalf("hovered segment must use the highlight style: %q", got)
	}

	m.Update(tea.MouseMotionMsg{X: m.sidebarOffsetX() + 2, Y: row, Button: tea.MouseNone})
	if m.viewJumpHover {
		t.Fatal("motion off the label must clear the hover highlight")
	}

	// Motion over the panel clears it as well (the label is not there).
	m.Update(tea.MouseMotionMsg{X: (x0 + x1) / 2, Y: row, Button: tea.MouseNone})
	if !m.viewJumpHover {
		t.Fatal("test setup: hover must be set again")
	}
	m.Update(tea.MouseMotionMsg{X: 2, Y: 2, Button: tea.MouseNone})
	if m.viewJumpHover {
		t.Fatal("motion over the panel must clear the hover highlight")
	}
}

// In viewport focus the divider's right end carries the " [SCROLL] …"
// hint: the button yields to it (skipped when the column cannot fit
// both), so the hint is never covered or truncated.
func TestJumpButtonAvoidsFocusHint(t *testing.T) {
	m := detachedJumpModel(t, 40)
	m.focus = FocusViewport
	hint := strings.TrimSpace(viewportFocusIndicator)
	visible := 0

	for w := 60; w <= 160; w += 10 {
		m.SetSize(w, 30)
		rows := strings.Split(stripANSI(m.View().Content), "\n")
		divider := rows[m.viewport.Height]
		if _, _, ok := m.jumpButtonZone(); ok {
			visible++
			if !strings.Contains(divider, jumpButtonJumpLabel) {
				t.Fatalf("width %d: zone reported but label not painted: %q", w, divider)
			}
		}
		if !strings.Contains(divider, hint) {
			t.Fatalf("width %d: the focus hint is covered or truncated: %q", w, divider)
		}
	}
	// The button must be reachable while the hint is up (wide terminals),
	// not silently suppressed everywhere.
	if visible == 0 {
		t.Fatal("button never rendered next to the focus hint")
	}
}
