package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// scrollableModel builds a model over a scrollable transcript pinned to the
// live edge, in input focus (the TUI's default).
func scrollableModel(t *testing.T, lines int) *Model {
	t.Helper()
	m := newSidebarFullModel(t)
	m.focus = FocusInput
	for i := 0; i < lines; i++ {
		m.appendChatLine(DimStyle.Render(fmt.Sprintf("line %d", i)))
	}
	m.viewport.GotoBottom()
	m.viewDetached = false
	return m
}

// The transcript scroll keys must work from input focus (the default) —
// no esc-into-viewport dance — with pgup/pgdown paging the chat and
// home/end jumping to the edges while the composer is empty.
func TestInputFocusScrollKeys(t *testing.T) {
	newScrollable := func(t *testing.T) (*Model, int) {
		t.Helper()
		m := scrollableModel(t, 40)
		bottom := m.viewport.YOffset
		if bottom == 0 {
			t.Fatal("test setup: viewport must be scrollable")
		}
		return m, bottom
	}

	t.Run("pgup pages up and detaches", func(t *testing.T) {
		m, bottom := newScrollable(t)
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
		if m.focus != FocusInput {
			t.Fatalf("focus = %v, want input", m.focus)
		}
		if want := max(0, bottom-m.viewport.Height); m.viewport.YOffset != want {
			t.Fatalf("pgup: YOffset = %d, want %d", m.viewport.YOffset, want)
		}
		if !m.viewDetached {
			t.Fatal("pgup must detach auto-follow")
		}
		if got := m.textarea.Value(); got != "" {
			t.Fatalf("pgup typed into the composer: %q", got)
		}
	})

	t.Run("pgdown pages down and re-attaches", func(t *testing.T) {
		m, bottom := newScrollable(t)
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgDown})
		if m.viewport.YOffset != bottom {
			t.Fatalf("pgdown: YOffset = %d, want %d", m.viewport.YOffset, bottom)
		}
		if m.viewDetached {
			t.Fatal("returning to the bottom must re-attach auto-follow")
		}
	})

	t.Run("home jumps to the top on an empty composer", func(t *testing.T) {
		m, _ := newScrollable(t)
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyHome})
		if m.viewport.YOffset != 0 {
			t.Fatalf("home: YOffset = %d, want 0", m.viewport.YOffset)
		}
		if !m.viewDetached {
			t.Fatal("home must detach auto-follow")
		}
	})

	t.Run("end returns to the bottom", func(t *testing.T) {
		m, bottom := newScrollable(t)
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyHome})
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnd})
		if m.viewport.YOffset != bottom {
			t.Fatalf("end: YOffset = %d, want %d", m.viewport.YOffset, bottom)
		}
		if m.viewDetached {
			t.Fatal("end must re-attach auto-follow")
		}
	})

	t.Run("home keeps the textarea line-start with a draft", func(t *testing.T) {
		m, bottom := newScrollable(t)
		m.textarea.SetValue("a draft")
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyHome})
		if m.viewport.YOffset != bottom {
			t.Fatalf("home with a draft scrolled the transcript: %d -> %d", bottom, m.viewport.YOffset)
		}
		if got := m.textarea.Value(); got != "a draft" {
			t.Fatalf("draft clobbered: %q", got)
		}
	})

	t.Run("pgup still scrolls with a draft", func(t *testing.T) {
		m, bottom := newScrollable(t)
		m.textarea.SetValue("a draft")
		m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
		if m.viewport.YOffset == bottom {
			t.Fatal("pgup with a draft must still scroll the transcript")
		}
		if got := m.textarea.Value(); got != "a draft" {
			t.Fatalf("draft clobbered: %q", got)
		}
	})
}

// A manual scroll away from the live edge suspends auto-follow: streaming
// growth must not yank the reader back down until they return to the
// bottom (web stickToBottom parity).
func TestDetachedViewportSurvivesGrowth(t *testing.T) {
	m := scrollableModel(t, 40)
	m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
	parked := m.viewport.YOffset
	if !m.viewDetached {
		t.Fatal("test setup: pgup must detach")
	}

	// Every content-growth funnel must leave a parked viewport alone.
	m.appendChatLine(DimStyle.Render("streamed line"))
	if m.viewport.YOffset != parked {
		t.Fatalf("appendChatLine yanked the parked viewport: %d -> %d", parked, m.viewport.YOffset)
	}
	m.appendToLastLine(" more")
	if m.viewport.YOffset != parked {
		t.Fatalf("appendToLastLine yanked the parked viewport: %d -> %d", parked, m.viewport.YOffset)
	}
	m.commitChatLineWrite(len(m.chatLines)-1, true, "tail") // last-line (prefix) write
	if m.viewport.YOffset != parked {
		t.Fatalf("last-line write yanked the parked viewport: %d -> %d", parked, m.viewport.YOffset)
	}
	m.commitChatLineWrite(0, false, "head") // middle-line write (full rebuild)
	if m.viewport.YOffset != parked {
		t.Fatalf("middle-line write yanked the parked viewport: %d -> %d", parked, m.viewport.YOffset)
	}
	m.appendChatLines([]string{DimStyle.Render("a"), DimStyle.Render("b")})
	if m.viewport.YOffset != parked {
		t.Fatalf("appendChatLines yanked the parked viewport: %d -> %d", parked, m.viewport.YOffset)
	}

	// End re-attaches: growth follows again.
	m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.viewDetached {
		t.Fatal("end must re-attach auto-follow")
	}
	m.appendChatLine(DimStyle.Render("after end"))
	if !m.viewport.AtBottom() {
		t.Fatal("a re-attached viewport must follow new content")
	}
}

// The viewport-focus path (esc) detaches too: growth below must not pull
// the view away from what the user is reading.
func TestViewportFocusScrollDetaches(t *testing.T) {
	m := scrollableModel(t, 40)
	m.focus = FocusViewport
	m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	if !m.viewDetached {
		t.Fatal("viewport-focus scroll must detach auto-follow")
	}
	parked := m.viewport.YOffset
	m.appendChatLine(DimStyle.Render("streamed"))
	if m.viewport.YOffset != parked {
		t.Fatalf("growth yanked the viewport-scrolled reader: %d -> %d", parked, m.viewport.YOffset)
	}
}

// Turn end (handleStreamEnd) and the relayout it triggers must not yank a
// reader who scrolled away mid-turn: the finished reply settles in place
// until they return to the live edge.
func TestTurnEndKeepsDetachedPosition(t *testing.T) {
	m := scrollableModel(t, 40)
	m.SetSize(100, 30) // establish the layout so relayout() is a real pass
	m.viewport.GotoBottom()
	m.viewDetached = false
	m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyPgUp})
	parked := m.viewport.YOffset
	if !m.viewDetached {
		t.Fatal("test setup: pgup must detach")
	}

	m.handleStreamEnd()
	m.relayout()
	if m.viewport.YOffset != parked {
		t.Fatalf("turn end yanked the parked viewport: %d -> %d", parked, m.viewport.YOffset)
	}
	if !m.viewDetached {
		t.Fatal("turn end must not re-attach auto-follow")
	}
}

// The wheel is a manual scroll too: wheel-up detaches (the streamed reply
// cannot yank the reader back), a wheel landing at the bottom re-attaches.
func TestWheelScrollDetachesFollow(t *testing.T) {
	m := scrollableModel(t, 40)
	bottom := m.viewport.YOffset
	chatX := m.sidebarWidth + 5
	m.Update(tea.MouseWheelMsg{X: chatX, Y: 5, Button: tea.MouseWheelUp})
	if m.viewport.YOffset != bottom-1 {
		t.Fatalf("wheel up: YOffset = %d, want %d", m.viewport.YOffset, bottom-1)
	}
	if !m.viewDetached {
		t.Fatal("wheel up must detach auto-follow")
	}
	parked := m.viewport.YOffset
	m.appendChatLine(DimStyle.Render("streamed"))
	if m.viewport.YOffset != parked {
		t.Fatalf("growth yanked the wheel-parked viewport: %d -> %d", parked, m.viewport.YOffset)
	}
	for i := 0; i < 4; i++ {
		m.Update(tea.MouseWheelMsg{X: chatX, Y: 5, Button: tea.MouseWheelDown})
	}
	if !m.viewport.AtBottom() || m.viewDetached {
		t.Fatalf("wheel to the bottom must re-attach: offset=%d detached=%v",
			m.viewport.YOffset, m.viewDetached)
	}
}
