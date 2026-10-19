package tui

import (
	"fmt"
	"strings"

	"gogen/internal/agent"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleInputKey dispatches key events when the input textarea has focus.
func (m *Model) handleInputKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m2, cmd, ok := m.handleInputHelpKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleVerboseKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleCancelOrQuitKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleSubmitKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleHistoryUpKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleHistoryDownKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleInputCompletionKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleDeleteForwardKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleEscapeKey(msg); ok {
		return m2, cmd
	}
	if m2, cmd, ok := m.handleInputScrollKey(msg); ok {
		return m2, cmd
	}
	// Pass all other keys to textarea (it handles editing, word nav, kill, etc.)
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

// handleInputScrollKey scrolls the transcript while the composer has focus,
// so pgup/pgdn/home/end work without the esc-into-viewport dance. pgup/pgdn
// page the transcript unconditionally — the textarea's own pgup/pgdn only
// page the cursor around a ≤8-row composer, a far smaller win. home/end
// jump to the transcript top/bottom only while the composer is EMPTY: with
// a draft they keep their documented textarea meaning (line start/end).
func (m *Model) handleInputScrollKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	switch msg.String() {
	case "pgup":
		m.viewport.PageUp()
	case "pgdown":
		m.viewport.PageDown()
	case "home", "end":
		if strings.TrimSpace(m.textarea.Value()) != "" {
			return m, nil, false
		}
		if msg.String() == "home" {
			m.viewport.GotoTop()
		} else {
			m.viewport.GotoBottom()
		}
	default:
		return m, nil, false
	}
	m.viewportScrolledByUser()
	return m, nil, true
}

// handleInputHelpKey opens the help modal on F1 in any mode (but NOT "?" — that
// is a printable character).
func (m *Model) handleInputHelpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// F1 opens help in any mode (but NOT ? — that's a printable character)
	if key.Matches(msg, m.keys.Help) {
		m.modal = ModalHelp
		return m, nil, true
	}
	// ctrl+q opens the queued-messages modal (per-item cancel — steering),
	// any time input has focus, busy or idle.
	if key.Matches(msg, m.keys.QueueModal) {
		m.queueCursor = 0
		m.modal = ModalQueue
		return m, nil, true
	}
	return m, nil, false
}

func (m *Model) handleVerboseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Global: verbose toggle
	if key.Matches(msg, m.keys.Verbose) {
		m.verbose = !m.verbose
		return m, nil, true
	}
	return m, nil, false
}

// handleCancelKey handles ctrl+c: cancel a running turn, cancel a running
// /compact, or quit when idle. Shared by input, viewport, and sidebar
// focus — the three copy-pasted variants diverged (the input variant
// cleared m.streaming immediately while the others didn't), which is how
// the cancel-then-resubmit race hid. The turn-epoch guard (liveSession.
// turnSeq) makes clearing the mirror immediately safe: the superseded
// turn's stragglers are dropped by seq.
func (m *Model) handleCancelKey() (tea.Model, tea.Cmd, bool) {
	if m.streaming {
		m.streaming = false
		m.clearProgress()
		// Cancel the underlying LLM stream. The session's flags stay set
		// until its attributed terminal arrives; if a newer turn has
		// started by then, the epoch guard drops that terminal.
		m.cancelActiveStream()
		m.resetStreamState(false)
		// Interrupt semantics (documented decision, ticket #78): the
		// user's own queued steering messages are cleared with the turn —
		// "stop what you're doing". SYSTEM deliveries are machine
		// bookkeeping (job notices, subagent reports) whose loss strands
		// the delivery machinery, so they keep draining.
		dropped := 0
		if s := m.focusedSession(); s != nil {
			dropped = s.clearUserSteer()
		}
		if dropped > 0 {
			m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Cancelled — %d queued message(s) removed.", dropped)))
		} else {
			m.appendChatLine(SystemStyle.Render("Cancelled."))
		}
		m.applyComposerPlaceholder()
		m.relayout()
		// refocusInput is a no-op outside input focus (viewport/sidebar
		// stay where they are).
		return m, m.refocusInput(), true
	}
	if m.compacting {
		// Cancel the FOCUSED session's compaction (m.compacting is its
		// mirror); a background session's compaction is left running.
		if s := m.focusedSession(); s != nil {
			s.compactUserCancelled = true
			if s.compactCancel != nil {
				s.compactCancel()
			}
			s.compacting = false
			s.compactCancel = nil
		}
		m.compacting = false
		m.appendChatLine(SystemStyle.Render("Compact cancelled."))
		// This branch clears the mirror itself, so it owns the compaction
		// boundary bookkeeping handleCompactResultMsg performs: hand the
		// strip's reserved row back to the composer and flip the
		// placeholder. Skipping it left the band a row short (SetSize
		// budgeted the strip, nothing renders it) — the frame came out one
		// row SHORTER than the terminal until the next relayout, and the
		// composer kept the queued-turn placeholder.
		m.applyComposerPlaceholder()
		m.relayout()
		return m, nil, true
	}
	m.flushAndQuit()
	return m, tea.Quit, true
}

func (m *Model) handleCancelOrQuitKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Cancel turn (ctrl+c) or quit
	if key.Matches(msg, m.keys.CancelTurn) {
		return m.handleCancelKey()
	}
	return m, nil, false
}

func (m *Model) handleSubmitKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Submit (enter)
	if key.Matches(msg, m.keys.Submit) {
		input := strings.TrimRight(m.textarea.Value(), "\n")
		if strings.TrimSpace(input) == "" {
			return m, nil, true
		}

		// Busy (a turn running or a compaction in flight): the composer is
		// NOT locked — steering. Plain text is QUEUED (FIFO, delivered as
		// the next user turn when the in-flight one ends); /queue opens the
		// management modal; other commands are refused with a hint (they
		// would rebind the session or rewrite history mid-turn). A running
		// /compact also queues: the drain waits for it (drainDeliveries
		// gates on both m.streaming and m.compacting).
		if m.streaming || m.compacting {
			trimmed := strings.TrimSpace(input)
			// History bookkeeping: queued text is typed input.
			m.appendInputHistory(input)

			if trimmed == "queue" || trimmed == "/queue" {
				m.queueCursor = 0
				m.modal = ModalQueue
				m.textarea.Reset()
				return m, m.textarea.Focus(), true
			}
			if isBusyCommand(trimmed) {
				m.statusMsg = "Busy — commands wait for the idle state; Enter queues plain text."
				return m, nil, true
			}
			cmd := m.enqueueSteer(input)
			m.textarea.Reset()
			return m, tea.Batch(cmd, m.textarea.Focus()), true
		}

		// Add to history
		m.appendInputHistory(input)

		// Check if it's a command
		if handled, quit, cmd := m.dispatchCommand(input); handled {
			m.textarea.Reset()
			if quit {
				return m, cmd, true
			}
			return m, tea.Batch(cmd, m.textarea.Focus()), true
		}

		// Send to agent
		cmd := m.submitUserInput(input)
		m.textarea.Reset()
		return m, tea.Batch(cmd, m.textarea.Focus()), true
	}
	return m, nil, false
}

// maxInputHistory bounds the ↑/↓ prompt history. Entries are whole prompts
// (which may be large pastes), so an unbounded slice would grow for the life
// of the process on a long-lived session.
const maxInputHistory = 500

// appendInputHistory records one submitted prompt (or queued steering text)
// in the ↑ history, skipping an immediate duplicate, and keeps the list
// within maxInputHistory by dropping the oldest entries. historyIdx is reset
// to the end so the next ↑ starts from the newest entry.
func (m *Model) appendInputHistory(s string) {
	if n := len(m.inputHistory); n == 0 || m.inputHistory[n-1] != s {
		m.inputHistory = append(m.inputHistory, s)
		if over := len(m.inputHistory) - maxInputHistory; over > 0 {
			// In-place shift, oldest first (dst index is always below src).
			m.inputHistory = append(m.inputHistory[:0], m.inputHistory[over:]...)
		}
	}
	m.historyIdx = len(m.inputHistory)
}

func (m *Model) handleHistoryUpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// History navigation
	if key.Matches(msg, m.keys.HistoryUp) {
		if len(m.inputHistory) == 0 {
			return m, nil, true
		}
		if m.historyIdx == len(m.inputHistory) {
			m.historyDraft = m.textarea.Value()
		}
		if m.historyIdx > 0 {
			m.historyIdx--
			m.textarea.Reset()
			m.textarea.SetValue(m.inputHistory[m.historyIdx])
			m.textarea.CursorEnd()
		}
		return m, m.textarea.Focus(), true
	}
	return m, nil, false
}

func (m *Model) handleHistoryDownKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if key.Matches(msg, m.keys.HistoryDown) {
		if m.historyIdx >= len(m.inputHistory) {
			return m, nil, true
		}
		m.historyIdx++
		if m.historyIdx == len(m.inputHistory) {
			m.textarea.Reset()
			m.textarea.SetValue(m.historyDraft)
		} else {
			m.textarea.Reset()
			m.textarea.SetValue(m.inputHistory[m.historyIdx])
		}
		m.textarea.CursorEnd()
		return m, m.textarea.Focus(), true
	}
	return m, nil, false
}

func (m *Model) handleInputCompletionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Tab completion
	if key.Matches(msg, m.keys.Completion) {
		line := m.textarea.Value()
		if prefix, arg, ok := agent.ResumeLinePrefix(line); ok {
			completions := m.agent.ResumeArgCompletions(arg)
			if len(completions) == 0 {
				return m, nil, true
			}
			if len(completions) == 1 {
				newArg := completions[0]
				if newArg == "del" {
					newArg = "del "
				}
				m.textarea.Reset()
				m.textarea.SetValue(prefix + newArg)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus(), true
			}
			cp := agent.LongestCommonPrefix(completions)
			if len(cp) > len(arg) {
				m.textarea.Reset()
				m.textarea.SetValue(prefix + cp)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus(), true
			}
			// Show completion modal
			m.completions = completions
			m.completionIdx = 0
			m.completionLine = line
			m.modal = ModalCompletion
			return m, nil, true
		}
		if completions := agent.SlashCommandCompletions(line, false, true); len(completions) > 0 {
			if len(completions) == 1 {
				m.textarea.Reset()
				m.textarea.SetValue(completions[0] + " ")
				m.textarea.CursorEnd()
				return m, m.textarea.Focus(), true
			}
			cp := agent.LongestCommonPrefix(completions)
			trimmed := strings.TrimRight(line, " \t")
			if len(cp) > len(trimmed) {
				m.textarea.Reset()
				m.textarea.SetValue(cp)
				m.textarea.CursorEnd()
				return m, m.textarea.Focus(), true
			}
			m.completions = completions
			m.completionIdx = 0
			m.completionLine = line
			m.modal = ModalCompletion
			return m, nil, true
		}
		return m, nil, true
	}
	return m, nil, false
}

func (m *Model) handleDeleteForwardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Ctrl+D on empty line = quit
	if key.Matches(msg, m.keys.DeleteForward) {
		val := strings.TrimSpace(m.textarea.Value())
		if val == "" {
			m.cancelActiveStream()
			m.cancelCompactForQuit()
			m.dismissApproval(false)
			m.flushAndQuit()
			return m, tea.Quit, true
		}
	}
	return m, nil, false
}

func (m *Model) handleEscapeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	// Escape to focus viewport
	if msg.String() == "esc" {
		m.focus = FocusViewport
		m.clearSelection()
		m.textarea.Blur()
		return m, nil, true
	}
	return m, nil, false
}

// handleViewportKey dispatches key events when the viewport has focus.
func (m *Model) handleViewportKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Help: F1 or ?
	if key.Matches(msg, m.keys.Help) || msg.String() == "?" {
		m.modal = ModalHelp
		return m, nil
	}

	// Cancel turn / compact or quit (shared handler; stays on viewport
	// focus — refocusInput is a no-op here).
	if key.Matches(msg, m.keys.CancelTurn) {
		_, cmd, _ := m.handleCancelKey()
		return m, cmd
	}

	// Focus back to input
	switch msg.String() {
	case "i", "enter":
		m.clearSelection()
		m.focus = FocusInput
		return m, m.textarea.Focus()
	case "esc":
		m.clearSelection()
		m.focus = FocusInput
		return m, m.textarea.Focus()
	case "tab":
		// Keyboard access to the sessions panel (web parity is mouse;
		// tab is the keyboard entry point). Tab is unused in viewport
		// focus — in input focus it stays completion.
		m.focus = FocusSidebar
		m.sidebarCursor = m.sidebarFocusedRow()
		return m, nil
	}

	// [ / ] jump to the previous/next user prompt — the prompt rail's
	// keyboard access (the web is mouse-only; the brackets follow the
	// sidebar's bracket-key convention). No target → fall through and
	// type the bracket like any other printable.
	if msg.String() == "[" || msg.String() == "]" {
		if m.tocJumpNext(msg.String() == "]") {
			return m, nil
		}
	}

	// Viewport scrolling — MUST run before the printable fall-through
	// below: j/k/g/G are printable ASCII, and the typing check would
	// steal them (while it came first, the vi-scroll keys were dead code
	// and the documented "j/k scroll" binding typed into the input).
	// Each case records the follow state: scrolling up detaches from the
	// live edge (streaming tokens stop re-pinning), returning to the
	// bottom re-attaches.
	switch msg.String() {
	case "up", "k":
		m.viewport.LineUp(1)
		m.viewportScrolledByUser()
		return m, nil
	case "down", "j":
		m.viewport.LineDown(1)
		m.viewportScrolledByUser()
		return m, nil
	case "pgup":
		m.viewport.PageUp()
		m.viewportScrolledByUser()
		return m, nil
	case "pgdown", "ctrl+f":
		m.viewport.PageDown()
		m.viewportScrolledByUser()
		return m, nil
	case "home", "g":
		m.viewport.GotoTop()
		m.viewportScrolledByUser()
		return m, nil
	case "end", "G":
		m.viewport.GotoBottom()
		m.viewportScrolledByUser()
		return m, nil
	}

	// Ctrl+U half page up (in viewport mode)
	if key.Matches(msg, m.keys.ViewportHalfUp) {
		m.viewport.HalfPageUp()
		m.viewportScrolledByUser()
		return m, nil
	}
	// Ctrl+D half page down (in viewport mode)
	if key.Matches(msg, m.keys.ViewportHalfDown) {
		m.viewport.HalfPageDown()
		m.viewportScrolledByUser()
		return m, nil
	}

	// Any other printable character switches to input and passes the
	// character through.
	// v2: check the key code with no modifiers (v1's Runes field is gone;
	// ctrl/alt-modified letters carry the same Code but must not type text).
	if c := msg.Code; c >= 32 && c < 127 && msg.Mod == 0 {
		m.focus = FocusInput
		focusCmd := m.textarea.Focus()
		var updateCmd tea.Cmd
		m.textarea, updateCmd = m.textarea.Update(msg)
		return m, tea.Batch(focusCmd, updateCmd)
	}

	return m, nil
}
