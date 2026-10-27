package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/debuglog"
	"gogen/internal/steerq"

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
	return s.steerQ.Count(steerq.User)
}

// clearUserSteer drops every USER queue item (the interrupt semantic:
// "stop what you're doing" removes the user's own queued text) and returns
// how many were removed. SYSTEM deliveries are deliberately kept — job
// notices, scheduled reminders, and subagent reports are machine
// bookkeeping whose loss strands the delivery machinery. Update thread
// only.
func (s *liveSession) clearUserSteer() int {
	if s.steerQ == nil {
		return 0
	}
	return len(s.steerQ.DropKind(steerq.User))
}

// removeSteerItem drops one USER queue item by id; system deliveries are
// not individually cancellable. Reports whether anything was removed.
func (s *liveSession) removeSteerItem(id string) bool {
	_, ok := s.steerQ.Remove(id, steerq.User)
	return ok
}

// enqueueSteer queues the user's text against the FOCUSED session while it
// is busy (a turn running or a compaction in flight): strictly FIFO with
// system deliveries, delivered as the next user turn at the next terminal
// boundary. Overflow refuses (never silently drops typed text).
func (m *Model) enqueueSteer(text string) tea.Cmd {
	s := m.turnSession()
	if outcome, _ := s.queue().Append(steerq.Item{ID: newSteerItemID(), Text: text, Kind: steerq.User}); outcome != steerq.Accepted {
		m.statusMsg = fmt.Sprintf("Too many queued messages (limit %d) — stop the turn or wait for it.", maxSteerUserQueue)
		return nil
	}
	m.statusMsg = fmt.Sprintf("Queued (%d) — runs after the current turn", s.countUserSteer())
	return nil
}

// handleDeliveryRequestMsg queues a system delivery (a job notice, a scheduled
// reminder, a subagent report) on the session it names.
//
// Deliveries are per-session steering-queue items, and the notice names its
// session, so it is queued THERE: a job notice fired from a background
// goroutine must not be re-attributed to whichever session happens to be
// focused when the message reaches the Update thread. Overflow drops the
// OLDEST SYSTEM delivery (freshness wins — a stale job notice is worse than
// none), reported at the next drain; user steering items are never dropped
// here.
//
// It lives here rather than inline in the Update dispatch because it is
// steering-queue policy, not message routing (C1): the whole queue contract is
// in this file.
func (m *Model) handleDeliveryRequestMsg(msg deliveryRequestMsg) (tea.Model, tea.Cmd) {
	s := m.noticeTarget(msg.sid)
	if s == nil {
		return m, nil
	}
	// The cap/eviction policy is the shared queue's (internal/steerq): at the
	// system cap the OLDEST NOTICE is evicted, and when every system slot
	// holds a targeted send_message delivery — whose reply the parent is
	// waiting on — this notice is dropped instead, exactly like the web host's
	// enqueueSystem.
	outcome, evicted := s.queue().Append(steerq.Item{ID: newSteerItemID(), Text: msg.text, Kind: steerq.Notice})
	switch {
	case outcome == steerq.Dropped:
		// Every system slot holds a targeted reply: this notice is dropped
		// rather than evicting one.
		m.deliveryDrops++
		return m, nil
	case evicted != nil:
		m.deliveryDrops++
	}
	if m.lives != nil && m.lives.Active() == s {
		return m, m.drainDeliveries()
	}
	// A background session's notice drains on ITS idle boundary — now, if it
	// is idle (startTurnOn buffers the events for replay), or at its next turn
	// end (handleTurnFinishedMsg).
	return m, m.drainSessionQueue(s)
}

// startTurnOnWithResult runs a turn on sess (focused or background) with the
// per-session wiring shared by every turn entry point (seq, cancel ctx,
// adapter, delete approver). Callers must ensure sess is idle (no streaming
// turn). The FOCUSED-session mirrors (m.streaming, progress strip, chat line,
// composer relayout) are the caller's business — see startTurnWithResult.
//
// A still-streaming sess is REFUSED: a turn's goroutine keeps appending to the
// session's *agent.Agent until its terminal arrives, so starting a second turn
// in that window interleaves the two turns in the persisted history. The
// submit gate queues instead (see Model.submitBusy), so this is a safety net
// for a caller that ever forgets to check.
//
// onResult is optional and runs on the turn's own goroutine when
// StreamProcessInput returns, so a caller that needs the authoritative output
// (a subagent's main run, or the exact reply to a send_message) never has to
// infer it from the transcript. onResult must not touch Update-thread state.
func (m *Model) startTurnOnWithResult(sess *liveSession, text string, onResult func(string, error)) tea.Cmd {
	if sess == nil {
		return nil
	}
	if sess.streaming {
		m.statusMsg = "Busy — the previous turn is still stopping; press Enter again"
		debuglog.Write("tui/turn", "startTurnOnWithResult refused: session still streaming", "turn-double-start", nil)
		return nil
	}
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
	// Mirror the cancel handle onto the child handle so interrupt_agent (the
	// parent's tool goroutine) can cancel this exact turn without touching
	// Update-thread state.
	child := sess.child
	if child != nil {
		child.setTurn(cancelFn)
	}

	// A nil program means no renderer (headless host, tests): pass a TRUE nil
	// sender rather than a typed-nil *tea.Program, so the adapter's send path
	// can drop rendering events instead of dereferencing a nil receiver.
	var sender programSender
	if m.program != nil {
		sender = m.program
	}
	adapter := NewStreamAdapter(sess.id, seq, sender, sess)
	a := sess.agent
	if a == nil {
		a = m.agent
	}
	approver := m.makeDeleteApprover(sess.id, sender)
	commandApprover := m.makeCommandApprover(sess.id, sender)
	return func() tea.Msg {
		defer cancelFn()
		turnCtx := agent.ContextWithDeleteApprover(streamCtx, approver)
		turnCtx = agent.ContextWithCommandApprover(turnCtx, commandApprover)
		out, err := a.StreamProcessInput(
			turnCtx,
			text,
			adapter.Handlers(),
		)
		// Clear the child's in-flight turn as soon as the turn goroutine is
		// done producing output; the sink below may resolve a waiter that
		// reads Busy immediately afterwards.
		if child != nil {
			child.turnDone()
		}
		if onResult != nil {
			onResult(out, err)
		}
		if err != nil {
			return failOfStream(sess.id, seq, err)
		}
		// Return streamEndMsg directly so handleStreamEnd refreshes context
		// stats synchronously after Messages are final.
		return endOfStream(sess.id, seq)
	}
}

// submitBusy reports whether a submitted prompt must be QUEUED instead of
// started as a new turn. m.streaming/m.compacting are mirrors of the focused
// session, and handleCancelKey clears m.streaming immediately (the UI must not
// stick on a stream that was just cancelled). The cancelled turn's goroutine,
// however, keeps appending to the session's *agent.Agent until its terminal
// message arrives, so the session's own flag is checked as well: submitting
// into that window used to start a SECOND turn on the same agent and
// interleave the persisted history.
func (m *Model) submitBusy() bool {
	if m.streaming || m.compacting {
		return true
	}
	if s := m.turnSession(); s != nil && (s.streaming || s.compacting) {
		return true
	}
	return false
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
	if s == nil || s.streaming || s.compacting || s.steerQ.Len() == 0 {
		return nil
	}
	// A notice run shares ONE turn (one provider request); a TARGETED
	// send_message delivery always gets its own turn, because its reply
	// capture is armed per delivery and must stay 1:1 with the tool call.
	// The head-run rule itself is the shared queue's (steerq.Batch): only a
	// contiguous run of Notice items is ever batched.
	batch := s.steerQ.Take(len(s.steerQ.Batch()))
	n := len(batch)
	item := batch[0]
	// text is the turn prompt (blank-line separated); display is the
	// focused transcript's system line (one notice per visual line).
	text, display := item.Text, item.Text
	if n > 1 {
		texts := make([]string, n)
		for i, it := range batch {
			texts[i] = it.Text
		}
		text = strings.Join(texts, "\n\n")
		display = strings.Join(texts, "\n")
	}
	var onResult func(string, error)
	if h := queuedChild(item); h != nil {
		// Targeted delivery: arm the exact-reply capture for THIS turn and
		// resolve it into the parent conversation when the turn ends. The
		// queue depth is re-mirrored so Busy sees the remaining backlog.
		if child := h.childOf(); child != nil {
			child.BeginReply()
		}
		h.setQueueDepth(s.steerQ.CountTag(h))
		onResult = childTurnResultSink(m.spawner, h, false)
	}
	focused := m.lives != nil && m.lives.Active() == s
	if focused {
		if item.Kind != steerq.User {
			return m.startTurnWithResult(text, SystemStyle.Render(noticeLabel+" "+display), onResult)
		}
		return m.startTurnWithResult(text, UserStyle.Render(userLabel)+" "+text, onResult)
	}
	return m.startTurnOnWithResult(s, text, onResult)
}

// queuedChild returns the child handle of a TARGETED delivery item (nil for
// notices and user items).
func queuedChild(it steerq.Item) *childHandle {
	h, _ := it.Tag.(*childHandle)
	return h
}

// countQueuedChildItems counts the queued targeted deliveries still waiting for
// handle h (its own backlog), so the handle's busy mirror stays honest.
func countQueuedChildItems(q *steerq.Queue, h *childHandle) int {
	return q.CountTag(h)
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
		// Subagent notices name the parent by SESSION id (the lifecycle only
		// knows session ids); slots that do not carry their session id as the
		// routing id (the root slot "s1") resolve here.
		if i := m.lives.indexBySessionID(sid); i >= 0 {
			return m.lives.ByIndex(i)
		}
	}
	return m.turnSession()
}

// isBusyCommand reports whether input is command-shaped (a slash command or a
// bare command word) and must therefore be REFUSED with a hint while a turn
// runs — commands like /new or /resume would rebind the session mid-turn —
// instead of being queued as chat. /queue is intercepted earlier.
//
// The classification itself lives in agent.IsCommandInput, shared with the
// web's busy gate: the two surfaces used to keep separate word lists and
// disagreed about bare "think"/"models" (commands on the web) versus
// "verbose"/"compact"/"queue" (commands in the TUI).
func isBusyCommand(trimmed string) bool {
	return agent.IsCommandInput(trimmed)
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
	var items []steerq.Item
	if s != nil {
		items = s.steerQ.Items()
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
			label := it.Text
			if it.Kind != steerq.User {
				label = "[delivery] " + label
			}
			label = truncateRunes(label, 58)
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
		n = s.steerQ.Len()
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
			if it := s.steerQ.Items()[m.queueCursor]; it.Kind != steerq.User {
				// Deliveries are not individually cancellable.
				m.statusMsg = "System deliveries wait for the idle state and cannot be cancelled individually."
			} else if s.removeSteerItem(it.ID) {
				if m.queueCursor >= s.steerQ.Len() {
					m.queueCursor = s.steerQ.Len() - 1
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
