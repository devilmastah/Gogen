package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"

	tea "charm.land/bubbletea/v2"
)

// Steering: a message typed while a turn (or compaction) runs is QUEUED on
// the session and delivered as the next user turn when the in-flight one
// ends — FIFO across user messages and system deliveries, one item per
// turn. The composer stays editable while busy; interrupt (ctrl+c) is
// explicit and clears the user's queued items (system deliveries are
// machine bookkeeping and keep draining). This file owns the queue type,
// the per-session field's accessors, the enqueue/drain paths, and the
// /queue management modal.
//
// The queue is HOST state (TUI/web wrappers own turn starts; the agent has
// no turn lock), so it lives on the liveSession next to streaming/turnSeq —
// the same home as the web server's per-runtime delivery queue it unifies
// with — not in an agent domain group.

// steerItem is one queued turn waiting for the session to go idle.
type steerItem struct {
	// id identifies the item (cancel targeting / debugging); generated.
	id string
	// text is the prompt the drained turn will run.
	text string
	// system marks a machine-generated delivery (job notice, scheduled
	// reminder, subagent report): rendered as a system notice line, not
	// individually cancellable, and never dropped by the user-interrupt
	// clear.
	system bool
}

var (
	steerIDMu  sync.Mutex
	steerIDSeq uint64
	steerIDAdd = uint64(time.Now().UnixNano() & 0xffff)
)

// newSteerItemID generates a queue-item id ("q_<seed>_<seq>").
func newSteerItemID() string {
	steerIDMu.Lock()
	steerIDSeq++
	seq := steerIDSeq
	steerIDMu.Unlock()
	return fmt.Sprintf("q_%x_%x", steerIDAdd, seq)
}

// maxSteerUserQueue caps the user's queued steering messages per session.
// Unlike system deliveries (bounded separately, drop-oldest), user text is
// never silently dropped: overflow refuses the enqueue with a visible
// status message.
const maxSteerUserQueue = 20

// countUserSteer counts queued user items. Update thread only.
func (s *liveSession) countUserSteer() int {
	n := 0
	for _, it := range s.steerQueue {
		if !it.system {
			n++
		}
	}
	return n
}

// clearUserSteer drops every USER queue item (the interrupt semantic:
// "stop what you're doing" removes the user's own queued text) and returns
// how many were removed. SYSTEM deliveries are deliberately kept — job
// notices, scheduled reminders, and subagent reports are machine
// bookkeeping whose loss strands the delivery machinery. Update thread
// only.
func (s *liveSession) clearUserSteer() int {
	kept := s.steerQueue[:0]
	removed := 0
	for _, it := range s.steerQueue {
		if it.system {
			kept = append(kept, it)
			continue
		}
		removed++
	}
	s.steerQueue = kept
	return removed
}

// removeSteerItem drops one USER queue item by id; system deliveries are
// not individually cancellable. Reports whether anything was removed.
func (s *liveSession) removeSteerItem(id string) bool {
	for i, it := range s.steerQueue {
		if !it.system && it.id == id {
			s.steerQueue = append(s.steerQueue[:i], s.steerQueue[i+1:]...)
			return true
		}
	}
	return false
}

// enqueueSteer queues the user's text against the FOCUSED session while it
// is busy (a turn running or a compaction in flight): strictly FIFO with
// system deliveries, delivered as the next user turn at the next terminal
// boundary. Overflow refuses (never silently drops typed text).
func (m *Model) enqueueSteer(text string) tea.Cmd {
	s := m.turnSession()
	if n := s.countUserSteer(); n >= maxSteerUserQueue {
		m.statusMsg = fmt.Sprintf("Too many queued messages (limit %d) — stop the turn or wait for it.", maxSteerUserQueue)
		return nil
	}
	s.steerQueue = append(s.steerQueue, steerItem{id: newSteerItemID(), text: text})
	m.statusMsg = fmt.Sprintf("Queued (%d) — runs after the current turn", s.countUserSteer())
	return nil
}

// startTurnOn runs a turn on sess (focused or background) with the
// per-session wiring shared by every turn entry point (seq, cancel ctx,
// adapter, delete approver). Callers must ensure sess is idle (no
// streaming turn). The FOCUSED-session mirrors (m.streaming, progress
// strip, chat line, composer relayout) are the caller's business — see
// startTurn.
func (m *Model) startTurnOn(sess *liveSession, text string) tea.Cmd {
	sess.streaming = true
	// Bump the turn generation BEFORE the goroutine starts: every message
	// this turn emits (and its terminal) carries the new seq, so a later
	// cancel + resubmit supersedes it cleanly.
	sess.turnSeq++
	seq := sess.turnSeq
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	streamCtx, cancelFn := context.WithCancel(base)
	sess.cancel = cancelFn

	adapter := NewStreamAdapter(sess.id, seq, m.program, sess)
	a := sess.agent
	if a == nil {
		a = m.agent
	}
	approver := m.makeDeleteApprover(sess.id, m.program)
	return func() tea.Msg {
		defer cancelFn()
		_, err := a.StreamProcessInput(
			agent.ContextWithDeleteApprover(streamCtx, approver),
			text,
			adapter.Handlers(),
		)
		if err != nil {
			return failOfStream(sess.id, seq, err)
		}
		// Return streamEndMsg directly so handleStreamEnd refreshes context
		// stats synchronously after Messages are final.
		return endOfStream(sess.id, seq)
	}
}

// drainSessionQueue pops the queue head from s — or, when the head is a
// system delivery, the contiguous run of system deliveries behind it — and
// starts its turn. The queue drains at every terminal boundary
// (handleTurnFinishedMsg, handleCompactResultMsg, drainDeliveries). System
// notices are one-way injections, so a run of them shares ONE turn (one
// provider request); USER items keep their own turn each, and a run stops
// at the first user item so FIFO arrival order is preserved. Returns nil
// when nothing can start (empty queue, busy session). FOCUSED items render
// through startTurn (chat line + mirrors); BACKGROUND items run on their
// session's agent without touching the focused transcript — their events
// buffer in the session's replay queue and their terminal routes back here
// through handleTurnFinishedMsg.
func (m *Model) drainSessionQueue(s *liveSession) tea.Cmd {
	if s == nil || s.streaming || s.compacting || len(s.steerQueue) == 0 {
		return nil
	}
	n := 1
	if s.steerQueue[0].system {
		for n < len(s.steerQueue) && s.steerQueue[n].system {
			n++
		}
	}
	batch := s.steerQueue[:n]
	s.steerQueue = s.steerQueue[n:]
	item := batch[0]
	// text is the turn prompt (blank-line separated); display is the
	// focused transcript's system line (one notice per visual line).
	text, display := item.text, item.text
	if n > 1 {
		texts := make([]string, n)
		for i, it := range batch {
			texts[i] = it.text
		}
		text = strings.Join(texts, "\n\n")
		display = strings.Join(texts, "\n")
	}
	focused := m.lives != nil && m.lives.Active() == s
	if focused {
		if item.system {
			return m.startTurn(text, SystemStyle.Render(noticeLabel+" "+display))
		}
		return m.startTurn(text, UserStyle.Render(userLabel)+" "+text)
	}
	return m.startTurnOn(s, text)
}

// noticeTarget resolves the session a system delivery was raised for: the
// slot whose routing id is sid, or the focused session when sid is empty
// (direct constructions, tests) or names a slot that no longer exists (the
// session was closed between the notice firing and this handler running).
// Never nil while the model has an agent to route through — turnSession
// materializes the registry for bare test Models.
func (m *Model) noticeTarget(sid string) *liveSession {
	if sid != "" && m.lives != nil {
		if s := m.lives.ByID(sid); s != nil {
			return s
		}
	}
	return m.turnSession()
}

// isBusyCommand reports whether input is a slash command or one of the
// bare command words the TUI command table dispatches. Pure: while a turn
// runs, such input is REFUSED with a hint (commands like /new or /resume
// would rebind the session mid-turn) instead of being queued as chat —
// /queue itself is intercepted earlier.
func isBusyCommand(trimmed string) bool {
	if strings.HasPrefix(trimmed, "/") {
		return true
	}
	if trimmed == "plan" || trimmed == "act" || trimmed == "mode" ||
		trimmed == "context" || trimmed == "help" || trimmed == "verbose" ||
		trimmed == "subagents" || trimmed == "save-config" || trimmed == "compact" ||
		trimmed == "new" || trimmed == "sessions" || trimmed == "resume" || trimmed == "fork" ||
		trimmed == "switch" || trimmed == "open" || trimmed == "queue" {
		return true
	}
	for _, p := range []string{"think ", "context ", "models ", "dir ", "switch ", "open ",
		"save-config ", "sessions ", "resume ", "fork ", "switch"} {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

// relayout re-runs the size math so the busy band's progress strip (the
// composer gives it one row while streaming/compacting) applies at the
// moment streaming flips — SetSize is otherwise driven only by
// WindowSizeMsg. Called at turn start/end, cancel, compaction start/end,
// and focus switches.
func (m *Model) relayout() {
	if m.width > 0 && m.height > 0 {
		m.SetSize(m.width, m.height)
	}
}

// ── /queue modal ─────────────────────────────────────────────────────────

// queueCursor is the /queue modal's selected row (state.go field).
// Managed here and by the modal keys below.

// renderQueueModal lists the FOCUSED session's queued items with a cursor;
// `d` deletes the selected USER item, `D` clears every user item, esc
// closes. System deliveries are listed for visibility but are not
// individually cancellable (machine bookkeeping).
func (m *Model) renderQueueModal() string {
	rows := []styleLine{{text: "Queued messages", highlight: true}}
	s := m.focusedSession()
	var items []steerItem
	if s != nil {
		items = s.steerQueue
	}
	if len(items) == 0 {
		rows = append(rows, styleLine{text: "Nothing queued."})
	} else {
		// A LOCAL clamped cursor: the queue can shrink while the modal is
		// open (drain pops at turn boundaries), and the highlight must not
		// sit past the last row — without mutating state from the render
		// path (the stored cursor is re-clamped by handleQueueKey).
		cursor := m.queueCursor
		if cursor >= len(items) {
			cursor = len(items) - 1
		}
		if cursor < 0 {
			cursor = 0
		}
		for i, it := range items {
			label := it.text
			if it.system {
				label = "[delivery] " + label
			}
			label = sliceByRuneCount(label, 58)
			prefix := "  "
			if i == cursor {
				prefix = "> "
			}
			rows = append(rows, styleLine{text: prefix + label, highlight: i == cursor})
		}
	}
	rows = append(rows, styleLine{text: "", highlight: false},
		styleLine{text: "d delete selected   D clear queued   esc close", highlight: false})
	return m.renderBorderedModal(rows)
}

// clampQueueCursor keeps the /queue modal's cursor inside the queue after
// an out-of-modal shrink: a drain pops the head at every turn boundary (the
// modal opens while a turn runs), and an interrupt clears the user's items,
// so the cursor can sit past the end. Without the clamp the "d" handler
// indexes out of range and the highlight silently stops rendering.
// Update-thread only (queueCursor is Update-thread state).
func (m *Model) clampQueueCursor(n int) {
	if m.queueCursor >= n {
		m.queueCursor = n - 1
	}
	if m.queueCursor < 0 {
		m.queueCursor = 0
	}
}

// handleQueueKey dispatches the /queue modal's keys.
func (m *Model) handleQueueKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.focusedSession()
	n := 0
	if s != nil {
		n = len(s.steerQueue)
	}
	m.clampQueueCursor(n)
	switch msg.String() {
	case "esc", "q":
		m.modal = ModalNone
	case "down", "j":
		if m.queueCursor < n-1 {
			m.queueCursor++
		}
	case "up", "k":
		if m.queueCursor > 0 {
			m.queueCursor--
		}
	case "d":
		if s != nil && n > 0 {
			if it := s.steerQueue[m.queueCursor]; it.system {
				// Deliveries are not individually cancellable.
				m.statusMsg = "System deliveries wait for the idle state and cannot be cancelled individually."
			} else if s.removeSteerItem(it.id) {
				if m.queueCursor >= len(s.steerQueue) {
					m.queueCursor = len(s.steerQueue) - 1
				}
				if m.queueCursor < 0 {
					m.queueCursor = 0
				}
			}
		}
	case "D":
		if s != nil && s.clearUserSteer() > 0 {
			m.queueCursor = 0
		}
	}
	return m, nil
}

// cmdQueue opens the /queue management modal (the queued bar's cancel
// surface — the TUI's counterpart of the web's per-bubble ✕).
func cmdQueue(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.queueCursor = 0
	m.modal = ModalQueue
	return true, false, nil
}

// Composer placeholders: while a session is busy, Enter QUEUES instead of
// sending — the placeholder says so before the keypress (web parity: the
// send button's tooltip carries the same message).
const (
	idleComposerPlaceholder   = "Type a message or command..."
	queuedComposerPlaceholder = "Queue a message… (runs after the current turn)"
)

// applyComposerPlaceholder swaps the composer placeholder for the busy
// state (busy ⇒ queueing semantics, regardless of the current queue depth —
// that's what Enter does). Called wherever streaming/compacting flips or
// focus rebinds.
func (m *Model) applyComposerPlaceholder() {
	if m.streaming || m.compacting {
		m.textarea.Placeholder = queuedComposerPlaceholder
		return
	}
	m.textarea.Placeholder = idleComposerPlaceholder
}
