package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/subagents"

	tea "charm.land/bubbletea/v2"
)

// renderModal renders the currently active modal.
func (m *Model) renderModal() string {
	switch m.modal {
	case ModalApproval:
		return m.renderApprovalModal()
	case ModalSessions:
		return m.renderSessionsModal()
	case ModalLiveSessions:
		return m.renderLiveSessionsModal()
	case ModalModels:
		return m.renderModelsModal()
	case ModalHelp:
		return m.renderHelpModal()
	case ModalCompletion:
		return m.renderCompletionModal()
	case ModalSubagents:
		return m.renderSubagentsModal()
	case ModalConfirm:
		return m.renderConfirmModal()
	case ModalQueue:
		return m.renderQueueModal()
	}
	return ""
}

// handleModalKey dispatches keys when a modal is active.
func (m *Model) handleModalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.modal {
	case ModalApproval:
		return m.handleApprovalKey(msg)
	case ModalSessions:
		return m.handleSessionsKey(msg)
	case ModalModels:
		return m.handleModelsKey(msg)
	case ModalHelp:
		return m.handleHelpKey(msg)
	case ModalCompletion:
		return m.handleCompletionKey(msg)
	case ModalSubagents:
		return m.handleSubagentsKey(msg)
	case ModalLiveSessions:
		return m.handleLiveSessionsKey(msg)
	case ModalConfirm:
		return m.handleConfirmKey(msg)
	case ModalQueue:
		return m.handleQueueKey(msg)
	}
	return m, nil
}

// renderConfirmModal renders the generic yes/no confirmation (sidebar
// session delete — the web's delete-confirm dialog).
func (m *Model) renderConfirmModal() string {
	rows := []styleLine{{text: "Confirm", highlight: true}, {text: "", highlight: false}}
	for _, l := range strings.Split(m.confirmText, "\n") {
		rows = append(rows, styleLine{text: l})
	}
	rows = append(rows, styleLine{text: "", highlight: false},
		styleLine{text: "y confirm   n/esc cancel", highlight: false})
	return m.renderBorderedModal(rows)
}

// handleConfirmKey dispatches the confirm modal keys. On exit (confirm or
// cancel) the modal returns to confirmRestore — ModalNone for the sidebar
// path, or the modal the dialog was opened from (the sessions list).
func (m *Model) handleConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		fn := m.confirmAction
		m.confirmAction = nil
		m.modal = m.confirmRestore
		m.confirmRestore = ModalNone
		if fn != nil {
			return fn()
		}
	case "n", "esc", "q":
		m.confirmAction = nil
		m.modal = m.confirmRestore
		m.confirmRestore = ModalNone
	}
	return m, nil
}

// renderLiveSessionsModal lists the actively hosted sessions (the registry
// behind /open and the future sidebar): focused marker, per-session label,
// and a streaming dot for background turns. enter focuses the selection
// via switchToLive — the same rebind-and-rebuild contract as /resume.
func (m *Model) renderLiveSessionsModal() string {
	rows := []styleLine{{text: "Active Sessions", highlight: true}, {text: "", highlight: false}}
	for i, s := range m.lives.sessions {
		marker := "  "
		if i == m.lives.active {
			marker = "* "
		}
		state := "idle"
		if s.streaming {
			state = "● streaming"
		}
		cursor := " "
		if i == m.liveCursor {
			cursor = ">"
		}
		line := cursor + marker + " " + padCells(s.label, 12) + " " + padCells("("+s.id+")", 14) + " " + state
		rows = append(rows, styleLine{text: line, highlight: i == m.liveCursor})
	}
	rows = append(rows, styleLine{text: "", highlight: false},
		styleLine{text: "↑↓/jk move  enter focus  c cancel  d close  esc close", highlight: false})
	return m.renderBorderedModal(rows)
}

// handleLiveSessionsKey navigates and focuses live sessions.
func (m *Model) handleLiveSessionsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.modal = ModalNone
	case "up", "k":
		if m.liveCursor > 0 {
			m.liveCursor--
		}
	case "down", "j":
		if m.liveCursor < len(m.lives.sessions)-1 {
			m.liveCursor++
		}
	case "enter":
		var cmds []tea.Cmd
		if i := m.liveCursor; i >= 0 && i < len(m.lives.sessions) && i != m.lives.active {
			cmds = append(cmds, m.switchToLive(i))
			m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Switched to session: %s", m.lives.ByIndex(i).label)))
		}
		m.modal = ModalNone
		return m, tea.Batch(cmds...)
	case "c":
		// Cancel the SELECTED session's in-flight turn (cancelActiveStream
		// only reaches the focused one, so a background turn was otherwise
		// uncancelable). The attributed streamErrorMsg lands later via
		// handleTurnFinishedMsg and clears the flags — "d" works once it
		// does.
		if s := m.lives.ByIndex(m.liveCursor); s != nil {
			if s.cancel != nil {
				s.cancel()
				m.statusMsg = "Cancelling " + s.label + "…"
			} else {
				m.statusMsg = "Cancel: " + s.label + " is idle"
			}
		}
	case "d":
		// Close a background session: flush + detach; it stays resumable
		// under /resume.
		if err := m.closeLiveSession(m.liveCursor); err != nil {
			m.statusMsg = "Close: " + err.Error()
			return m, nil
		}
		if m.liveCursor >= len(m.lives.sessions) {
			m.liveCursor = len(m.lives.sessions) - 1
		}
	}
	return m, nil
}

// renderSubagentsModal lists the nested (subagent) sessions finished in
// this process, with their final reports. LIVE children come first, read from
// the shared lifecycle registry (id, label, status, depth): the modal is the
// tool-visible surface (list_agents parity), while the sidebar rows are the
// pane/presentation surface.
func (m *Model) renderSubagentsModal() string {
	m.subagentMu.Lock()
	list := append([]subagentRecord(nil), m.subagents...)
	m.subagentMu.Unlock()
	var live []*subagents.Child[*childHandle]
	if m.spawner != nil && m.agent != nil {
		live = m.spawner.ChildrenOf(m.agent.SessionID)
	}
	if len(list) == 0 && len(live) == 0 {
		return m.renderBorderedModal([]styleLine{
			{text: "Subagents", highlight: true},
			{text: "", highlight: false},
			{text: "No subagents have run in this session.", highlight: false},
			{text: "", highlight: false},
			{text: "Press esc to close", highlight: false},
		})
	}
	lines := []styleLine{{text: "Subagents", highlight: true}, {text: "", highlight: false}}
	for _, c := range live {
		status := c.StatusOf(m.spawner)
		lines = append(lines, styleLine{
			text:      "● " + c.Label + " — " + status + fmt.Sprintf(" (depth %d, %s)", c.Depth, c.ID),
			highlight: false,
		})
		if line := c.LatestProgress(); line != "" {
			lines = append(lines, styleLine{text: "  " + line, highlight: false})
		}
	}
	if len(live) > 0 && len(list) > 0 {
		lines = append(lines, styleLine{text: "", highlight: false})
	}
	for i, r := range list {
		status := "✅"
		if r.err != nil {
			status = "❌"
		}
		lines = append(lines, styleLine{text: status + " " + r.label, highlight: false})
		summary := r.report
		if r.err != nil {
			summary = r.err.Error()
		}
		// Rune-safe truncation: byte slicing split multi-byte runes
		// mid-sequence, emitting invalid UTF-8 into the modal (mojibake,
		// wrong visible-width math downstream).
		if r := []rune(summary); len(r) > 120 {
			summary = string(r[:120]) + "…"
		}
		if summary != "" {
			lines = append(lines, styleLine{text: "  " + summary, highlight: false})
		}
		if i < len(list)-1 {
			lines = append(lines, styleLine{text: "", highlight: false})
		}
	}
	lines = append(lines, styleLine{text: "", highlight: false}, styleLine{text: "Press esc to close", highlight: false})
	return m.renderBorderedModal(lines)
}

// handleSubagentsKey closes the subagents modal on esc.
func (m *Model) handleSubagentsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.modal = ModalNone
	}
	return m, nil
}

// --- Approval Modal ---

// approvalRequest is one delete-approval exchange. Each request owns its
// reply channel — there is no shared channel or in-flight flag — so the
// approver (which runs on the stream goroutine during tool execution)
// never touches Model state: it sends the request to the Update thread
// (thread-safe program.Send) and blocks on its own reply. Concurrent
// approvals (focused + background sessions) cannot steal each other's
// answers.
type approvalRequest struct {
	// kind is "delete" (default) or "command". command is set for command
	// approvals (a blocked shell command awaiting a decision); req is set
	// for delete approvals.
	kind    string
	command string
	req     agent.DeleteRequest
	sid     string // requesting live session id ("" = unknown)
	// turnSeq is the requesting turn's generation, stamped on FIRST
	// delivery (handleApprovalRequestMsg, Update thread) so turn-end
	// pruning can tell a finished turn's queued request from a resubmitted
	// successor's without the stream goroutine ever reading liveSession
	// state. 0 = unstamped (no registry, or unknown session).
	turnSeq uint64
	// reply is buffered 1: a reply after the approver left is dropped.
	reply chan bool
}

// kindOrDelete normalizes an empty kind to the historical delete approval
// (existing tests and callers construct approvalRequest with only req).
func (ar *approvalRequest) kindOrDelete() string {
	if ar == nil || ar.kind == "" {
		return "delete"
	}
	return ar.kind
}

// pruneQueuedApprovals drops queued delete-approvals requested by sid's
// generation seq or OLDER — that turn has terminated, so its queued
// requests are dead (each approver has left via its reply or ctx.Done) and
// promoting one later would show a modal whose answer goes nowhere.
// Requests stamped with a newer generation belong to a resubmitted
// successor turn and survive a stale terminal. Update-thread only.
func (m *Model) pruneQueuedApprovals(sid string, seq uint64) {
	if len(m.pendingApprovals) == 0 {
		return
	}
	kept := m.pendingApprovals[:0] // reused; pendingApprovals is Update-thread-only
	for _, ar := range m.pendingApprovals {
		if ar.sid == sid && ar.turnSeq <= seq {
			continue
		}
		kept = append(kept, ar)
	}
	m.pendingApprovals = kept
}

type approvalUIState struct {
	kind    string // "delete" or "command"
	command string
	paths   []string
	reason  string
	cursor  int // 0 = No, 1 = Yes
	ar      *approvalRequest
}

// approvalSessionLabel names the requesting session when it is NOT the
// focused one (a background turn's delete request); "" for the focused
// session or unknown ids (keeps the classic title).
func (m *Model) approvalSessionLabel() string {
	if m.approvalUI == nil || m.approvalUI.ar == nil || m.lives == nil {
		return ""
	}
	sid := m.approvalUI.ar.sid
	if sid == "" || sid == m.lives.Active().id {
		return ""
	}
	if s := m.lives.ByID(sid); s != nil {
		return s.label
	}
	return ""
}

func (m *Model) renderApprovalModal() string {
	if m.approvalUI == nil {
		return ""
	}

	kind := m.approvalUI.kind
	if kind == "" {
		kind = "delete"
	}

	var rows []styleLine

	// Title — a background session's request names its session so the
	// answer is not misattributed to the focused transcript.
	title := "Delete Approval Required"
	allowLabel := "Allow delete?"
	if kind == "command" {
		title = "Command Approval Required"
		allowLabel = "Allow command?"
	}
	if label := m.approvalSessionLabel(); label != "" {
		title += " — " + label
	}
	rows = append(rows, styleLine{text: title, highlight: true})
	rows = append(rows, styleLine{text: "", highlight: false})

	if kind == "command" {
		// Blocked shell command the guard escalated for a decision.
		rows = append(rows, styleLine{text: "Command:", preStyled: true})
		for _, l := range strings.Split(m.approvalUI.command, "\n") {
			rows = append(rows, styleLine{text: "  " + l, preStyled: true})
		}
		rows = append(rows, styleLine{text: "", preStyled: true})
		reason := fmt.Sprintf("Reason: matched safety pattern %s", m.approvalUI.reason)
		rows = append(rows, styleLine{text: reason, highlight: false})
		rows = append(rows, styleLine{text: "", highlight: false})
	} else {
		// Reason
		reason := fmt.Sprintf("Reason: %s", m.approvalUI.reason)
		rows = append(rows, styleLine{text: reason, highlight: false})
		rows = append(rows, styleLine{text: "", highlight: false})

		// File list
		rows = append(rows, styleLine{text: "Files to delete:", preStyled: true})
		for _, p := range m.approvalUI.paths {
			rows = append(rows, styleLine{text: "  • " + p, preStyled: true})
		}
		rows = append(rows, styleLine{text: "", preStyled: true})
	}

	// Prompt + buttons (pre-styled line with inline highlights)
	noStyle := ansiDimOn
	yesStyle := ansiDimOn
	if m.approvalUI.cursor == 0 {
		noStyle = ansiHighlightOn
	}
	if m.approvalUI.cursor == 1 {
		yesStyle = ansiHighlightOn
	}
	promptLine := ansiPromptOn + allowLabel + ansiReset +
		"  [" + noStyle + "No" + ansiReset +
		"]  [" + yesStyle + "Yes" + ansiReset + "]"
	rows = append(rows, styleLine{text: promptLine, preStyled: true})

	return m.renderBorderedModal(rows)
}

func (m *Model) handleApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "left", "h":
		if m.approvalUI.cursor > 0 {
			m.approvalUI.cursor--
		}
	case "right", "l":
		if m.approvalUI.cursor < 1 {
			m.approvalUI.cursor++
		}
	case "enter", "y":
		m.dismissApproval(true)
		return m, nil
	case "n", "esc":
		m.dismissApproval(false)
		return m, nil
	case "ctrl+c":
		m.cancelActiveStream()
		m.dismissApproval(false)
		return m, nil
	}
	return m, nil
}

// dismissApproval replies to the on-screen approval request, then promotes
// the next queued one (keeping the modal up) or restores the modal that
// was open before the approval took over.
func (m *Model) dismissApproval(approved bool) {
	// Only honour a dismissal if a request is currently on screen. Without
	// this guard a stale dismissal (e.g. user pressed ctrl+c after the
	// stream was cancelled) would reply to nothing and close a modal that
	// is not the approval's.
	if m.approvalUI == nil {
		return
	}
	ar := m.approvalUI.ar
	m.approvalUI = nil
	if ar != nil {
		select {
		case ar.reply <- approved:
		default:
			// Approver already left (turn cancelled) — drop rather than
			// block the UI thread.
		}
	}
	if len(m.pendingApprovals) > 0 {
		next := m.pendingApprovals[0]
		m.pendingApprovals = m.pendingApprovals[1:]
		m.approvalUI = &approvalUIState{
			kind:    next.kindOrDelete(),
			command: next.command,
			paths:   next.req.Paths,
			reason:  next.req.Reason,
			cursor:  1, // default to Yes
			ar:      next,
		}
		m.bellIfBlurred()
		return
	}
	m.modal = m.modalBeforeApproval
	m.modalBeforeApproval = ModalNone
}

// clampCursorTo clamps a list cursor into [0, total-1], and to 0 for an empty
// list (so every consumer can test the cursor without a separate empty check).
func clampCursorTo(cursor, total int) int {
	return clamp(cursor, 0, max(0, total-1))
}

// visibleWindow returns the [start,end) slice of a total-entry list that keeps
// cursor centered, so a scrolling modal moves the list by whole windows instead
// of sliding it under the cursor. The cursor must already be inside the list
// (see clampCursorTo); maxVisible is the caller's own row budget, since the
// modals reserve different amounts for their headers and footers.
func visibleWindow(cursor, total, maxVisible int) (start, end int) {
	if total > maxVisible {
		start = cursor - maxVisible/2
		if start < 0 {
			start = 0
		}
		if start > total-maxVisible {
			start = total - maxVisible
		}
	}
	end = start + maxVisible
	if end > total {
		end = total
	}
	return start, end
}

// --- Sessions Modal ---

func (m *Model) renderSessionsModal() string {
	if len(m.sessionList) == 0 {
		return m.renderBorderedModal([]styleLine{
			{text: "Saved Sessions", highlight: true},
			{text: "", highlight: false},
			{text: "No saved sessions.", highlight: false},
			{text: "", highlight: false},
			{text: "Press esc to close", highlight: false},
		})
	}

	// Constrain visible area to fit the terminal.
	reserved := 13 // border(2) + title(2) + footer(4) + top/bottom margin(5)
	maxVisible := max(3, m.height-reserved)

	// Clamp cursor, then compute the scroll window that keeps it visible.
	m.sessionCursor = clampCursorTo(m.sessionCursor, len(m.sessionList))
	start, end := visibleWindow(m.sessionCursor, len(m.sessionList), maxVisible)

	var rows []styleLine

	// Title
	rows = append(rows, styleLine{text: "Saved Sessions", highlight: true})
	rows = append(rows, styleLine{text: "", highlight: false})

	// Overflow indicator (top)
	if start > 0 {
		rows = append(rows, styleLine{
			text: fmt.Sprintf("  ↑ %d more", start), highlight: false,
		})
	}

	// Session entries
	for i := start; i < end; i++ {
		s := m.sessionList[i]
		marker := "  "
		if s.Oneshot {
			marker = "⚡ "
		}
		line := fmt.Sprintf("%s%s  (%d msgs)", marker, s.ID, s.MessageCount)
		if s.Label != "" {
			line += fmt.Sprintf("  %q", s.Label)
		}
		if s.ID == m.sessionID {
			line += "  ← current"
		}
		rows = append(rows, styleLine{
			text:      line,
			highlight: i == m.sessionCursor,
		})
	}

	// Overflow indicator (bottom)
	if end < len(m.sessionList) {
		rows = append(rows, styleLine{
			text: fmt.Sprintf("  ↓ %d more", len(m.sessionList)-end), highlight: false,
		})
	}

	// Footer
	rows = append(rows, styleLine{text: "", highlight: false})
	rows = append(rows, styleLine{
		text: "↑↓/jk navigate  pgup/pgdn  enter resume  d delete  esc close", highlight: false,
	})

	return m.renderBorderedModal(rows)
}

func (m *Model) handleSessionsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.modal = ModalNone
		return m, nil
	case "up", "k":
		if m.sessionCursor > 0 {
			m.sessionCursor--
		}
	case "down", "j":
		if m.sessionCursor < len(m.sessionList)-1 {
			m.sessionCursor++
		}
	case "pgup":
		page := max(1, m.height-20)
		m.sessionCursor -= page
		if m.sessionCursor < 0 {
			m.sessionCursor = 0
		}
	case "pgdown":
		page := max(1, m.height-20)
		m.sessionCursor += page
		if m.sessionCursor >= len(m.sessionList) {
			m.sessionCursor = len(m.sessionList) - 1
		}
	case "home", "g":
		m.sessionCursor = 0
	case "end", "G":
		m.sessionCursor = len(m.sessionList) - 1
	case "enter":
		if len(m.sessionList) > 0 && m.sessionCursor >= 0 && m.sessionCursor < len(m.sessionList) {
			return m.resumeSelectedSession()
		}
	case "d":
		if len(m.sessionList) > 0 && m.sessionCursor >= 0 && m.sessionCursor < len(m.sessionList) {
			return m.deleteSelectedSession()
		}
	}
	return m, nil
}

// --- Bordered modal helpers ---

type styleLine struct {
	text      string
	highlight bool // if true, apply ansiHighlightOn; if false, ansiDimOn
	preStyled bool // if true, text already contains ANSI; don't add prefix
}

// renderBorderedModal draws a plain border box around styled text lines,
// sized to the longest row. See renderBorderedModalWidth for the
// preferred-width variant and the shared clamping contract.
func (m *Model) renderBorderedModal(rows []styleLine) string {
	return m.renderBorderedModalWidth(rows, 0)
}

// modelsModalInnerWidth is the models picker's default inner width
// (content + padding; total box = inner + 2 border cells). It is the
// FLOOR for modelsModalInner: short catalogs keep this stable, roomy
// geometry instead of hugging their longest row ("  3. gpt-4o" would
// otherwise give a ~30-cell box), while long entries grow the box up to
// the terminal clamp so the biggest model name fits when the screen can
// show it. See modelsModalInner.
const modelsModalInnerWidth = 58

// renderBorderedModalWidth is renderBorderedModal with a fixed inner
// width: minInner > 0 PINS the box — shorter rows get trailing padding,
// longer rows are ANSI-aware truncated, so the modal never resizes with
// its content. minInner == 0 keeps the hug-the-longest-row sizing. Uses
// ansiHighlightOn / ansiDimOn + ansiReset (foreground-only reset, never
// touches background) so the overlay's #1a1a1a background survives through
// the entire line, including right-hand padding. When preStyled is true
// the line is emitted as-is (caller embeds ANSI).
//
// The box is clamped to the terminal in both dimensions: a modal wider or
// taller than the screen makes the inline renderer scroll and desync
// (lipgloss.Place passes oversized content through untouched). Rows are
// ANSI-aware truncated to the clamped width, and clampModalRows caps the
// row count. Models built without SetSize (width/height zero — tests)
// keep the unclamped layout.
func (m *Model) renderBorderedModalWidth(rows []styleLine, minInner int) string {
	rows = m.clampModalRows(rows)

	// Compute max visible width of plain text (strip ANSI for measurement).
	maxW := 0
	for _, r := range rows {
		w := cellWidth(r.text)
		if w > maxW {
			maxW = w
		}
	}
	innerW := maxW + 4
	if minInner > 0 {
		// Pinned width: content wider than this truncates below rather
		// than resizing the box (the terminal clamp still applies).
		innerW = minInner
	}
	if m != nil && m.width > 0 {
		// Border(2) plus one cell of margin on each side(4).
		limit := m.width - 6
		if limit < 6 {
			limit = 6 // degenerate terminals: bounded, not unbounded
		}
		if innerW > limit {
			innerW = limit
		}
	}
	contentW := innerW - 4

	top := "╭" + strings.Repeat("─", innerW) + "╮"
	bot := "╰" + strings.Repeat("─", innerW) + "╯"

	var b strings.Builder
	b.WriteString(top)
	b.WriteByte('\n')
	for _, r := range rows {
		text := r.text
		// Truncate ANSI-aware to the clamped content width before any
		// styling or padding so every row fits the box budget. truncateCells
		// (wcwidth), not cutCells: a row holding a multi-codepoint cluster can
		// be WIDER than the budget after a grapheme-measured cut, and the
		// negative padding that follows then leaves the box row over-wide —
		// the terminal soft-wraps it and the frame desyncs (a session label
		// with a ZWJ emoji did exactly that).
		if cellWidth(text) > contentW {
			text = truncateCells(text, contentW)
		}
		b.WriteString("│  ")
		if r.preStyled {
			// Caller already embedded ANSI — emit verbatim.
			b.WriteString(text)
		} else {
			if r.highlight {
				b.WriteString(ansiHighlightOn)
			} else {
				b.WriteString(ansiDimOn)
			}
			b.WriteString(text)
			b.WriteString(ansiReset)
		}
		// Fill remaining content width after text.
		pad := contentW - cellWidth(text)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString("  │")
		b.WriteByte('\n')
	}
	b.WriteString(bot)
	return b.String()
}

// clampModalRows caps a modal's row list so the bordered box (rows + top
// and bottom border) never outgrows the terminal height. Everything past
// the first maxRows-2 rows collapses into one overflow indicator, and the
// LAST row is always preserved: every current caller ends its list with a
// footer/hint/prompt line (nav hints, "Press esc to close", the approval
// Yes/No prompt) that must stay visible for the modal to make sense. A
// zero or unset height (literal-built models in tests) skips the clamp,
// preserving the historical unclamped output there.
func (m *Model) clampModalRows(rows []styleLine) []styleLine {
	if m == nil || m.height <= 0 || len(rows) == 0 {
		return rows
	}
	maxRows := m.height - 4 // border(2) + one row of margin top and bottom
	const minRows = 4       // some content + indicator + footer survive even on tiny terminals
	if maxRows < minRows {
		maxRows = minRows
	}
	if maxRows >= len(rows) {
		return rows
	}
	kept := rows[:maxRows-2]
	hidden := len(rows) - len(kept) - 1 // minus the preserved footer row
	out := make([]styleLine, 0, maxRows)
	out = append(out, kept...)
	out = append(out, styleLine{
		text: fmt.Sprintf("  ⋯ %d more lines (terminal too short)", hidden),
	})
	out = append(out, rows[len(rows)-1])
	return out
}

// --- Models Modal ---

func (m *Model) renderModelsModal() string {
	rows, _ := m.modelsModalContent()
	return m.renderBorderedModalWidth(rows, m.modelsModalInner())
}

// modelsModalContent builds the models modal's content rows (title,
// thinking section, model entries, footer) and maps each model entry's
// content-row index to its list index (mouse hit-testing). The renderer
// wraps the rows in the border box; the hit-tester uses the map, so the
// two can't drift.
func (m *Model) modelsModalContent() ([]styleLine, map[int]int) {
	modelRowAt := make(map[int]int)

	// Title
	rows := []styleLine{{text: "Available Models", highlight: true}}

	// Thinking section (web toolbar popover parity): a heading plus the
	// staged chip row, hidden when the model under the cursor has no
	// reasoning-effort control.
	if levels, supported := m.modelsModalThinkingLevels(); supported {
		rows = append(rows, styleLine{text: "  Thinking level", highlight: false})
		chipText, _ := buildThinkingChipRow(levels, thinkingChipCursor(levels, m.thinkingSel))
		rows = append(rows, styleLine{text: chipText, preStyled: true})
	}

	rows = append(rows, styleLine{text: "", highlight: false})

	if len(m.modelList) == 0 {
		rows = append(rows,
			styleLine{text: "No models available.", highlight: false},
			styleLine{text: "", highlight: false},
			styleLine{text: "Press esc to close", highlight: false},
		)
		return rows, modelRowAt
	}

	// Constrain visible area to fit the terminal.
	reserved := 15 // border(2) + title(1) + thinking(2) + footer(2) + margins(5) + slack(3)
	maxVisible := max(3, m.height-reserved)

	// Clamp cursor, then compute the scroll window that keeps it visible.
	m.modelCursor = clampCursorTo(m.modelCursor, len(m.modelList))
	start, end := visibleWindow(m.modelCursor, len(m.modelList), maxVisible)

	// Overflow indicator (top)
	if start > 0 {
		rows = append(rows, styleLine{
			text: fmt.Sprintf("  ↑ %d more", start), highlight: false,
		})
	}

	// Model entries
	entryRows, rowToModel := buildModelRows(m.modelList, start, end, m.modelCursor)
	for i, r := range entryRows {
		rows = append(rows, r)
		if idx, ok := rowToModel[i]; ok {
			modelRowAt[len(rows)-1] = idx
		}
	}

	// Overflow indicator (bottom)
	if end < len(m.modelList) {
		rows = append(rows, styleLine{
			text: fmt.Sprintf("  ↓ %d more", len(m.modelList)-end), highlight: false,
		})
	}

	// Footer
	rows = append(rows, styleLine{text: "", highlight: false})
	rows = append(rows, styleLine{
		text: "↑↓/jk models  ←→/h/l thinking  enter apply  esc close", highlight: false,
	})

	return rows, modelRowAt
}

// modelsModalThinkingLevelsFor returns the thinking levels the models
// modal's chip row offers for one catalog row: "off" (omit) plus the
// model's accepted reasoning-effort values. The row's OWNING profile
// travels with the lookup (ref), because model IDs are not unique across
// providers: a bare-ID lookup would resolve the owner by first-listing and
// offer another profile's effort set. supported=false means the model has
// no reasoning-effort control (a known toggle-only entry, or a llama.cpp
// /props probe reporting no support): the section is hidden, mirroring
// the web's reasoningEffortsUnsupported handling.
func (m *Model) modelsModalThinkingLevelsFor(ref llm.ModelRef) (levels []agent.ThinkingLevel, supported bool) {
	if m.agent == nil {
		// Literal-built models (tests): the default closed set.
		return []agent.ThinkingLevel{agent.ThinkingOff, agent.ThinkingLow, agent.ThinkingMedium, agent.ThinkingHigh}, true
	}
	levels = m.agent.ThinkingLevelsForRef(ref)
	return levels, len(levels) > 1
}

// modelsModalRowRef is the routing ref of one catalog row: the model ID plus
// the profile its group names (see modelProviderName).
func modelsModalRowRef(mdl llm.ModelInfo) llm.ModelRef {
	return llm.ModelRef{Profile: modelProviderName(mdl), ID: mdl.ID}
}

// modelsModalThinkingLevels is the chip row's level set for the model
// under the cursor; with an empty catalog it falls back to the current
// model's levels (web parity: the toolbar chips are anchored to the
// active model, not the catalog).
func (m *Model) modelsModalThinkingLevels() ([]agent.ThinkingLevel, bool) {
	if m.modelCursor >= 0 && m.modelCursor < len(m.modelList) {
		return m.modelsModalThinkingLevelsFor(modelsModalRowRef(m.modelList[m.modelCursor]))
	}
	if m.agent != nil {
		return m.modelsModalThinkingLevelsFor(m.agent.CurrentModelRef())
	}
	return m.modelsModalThinkingLevelsFor(llm.ModelRef{})
}

// thinkingChipLabel is a chip's display text, following the web
// toolbar's rule: "Off" for the omit state, short forms (L/M/H) for the
// closed default set, the full title-cased label for model-specific
// values ("Max").
func thinkingChipLabel(l agent.ThinkingLevel) string {
	if l == agent.ThinkingOff {
		return "Off"
	}
	switch l {
	case agent.ThinkingLow, agent.ThinkingMedium, agent.ThinkingHigh:
		return l.ShortLabel()
	}
	return l.Label()
}

// thinkingChipSpan is one chip's column range on the chip row, relative
// to the content area (0 = first column after the box border and the
// renderer's two-cell content padding).
type thinkingChipSpan struct {
	value agent.ThinkingLevel
	x0    int
	x1    int
}

// buildThinkingChipRow renders the chip row — "    [Off]  [L]  [M]  [H]"
// with the chip at cursor highlighted — and returns each chip's column
// span for mouse hit-testing. The hit-tester calls this with the same
// state, so rendering and hit-testing can't drift.
func buildThinkingChipRow(levels []agent.ThinkingLevel, cursor int) (string, []thinkingChipSpan) {
	const indent = "    "
	var b strings.Builder
	var spans []thinkingChipSpan
	b.WriteString(indent)
	w := cellWidth(indent)
	for i, l := range levels {
		if i > 0 {
			b.WriteString("  ")
			w += 2
		}
		label := "[" + thinkingChipLabel(l) + "]"
		spans = append(spans, thinkingChipSpan{value: l, x0: w, x1: w + cellWidth(label)})
		if i == cursor {
			b.WriteString(ansiHighlightOn + label + ansiReset)
		} else {
			b.WriteString(ansiDimOn + label + ansiReset)
		}
		w += cellWidth(label)
	}
	return b.String(), spans
}

// thinkingChipCursor finds the staged level's chip index; -1 when the
// staged level is not among the offered levels (policy B: no chip
// highlighted = nothing would be sent).
func thinkingChipCursor(levels []agent.ThinkingLevel, sel agent.ThinkingLevel) int {
	for i, l := range levels {
		if l == sel {
			return i
		}
	}
	return -1
}

// modelsModalSyncThinking keeps the staged thinking selection valid for
// the model under the cursor after the cursor moves: a staged level the
// model does not accept resets to off (the web subagent picker's
// resetStaleThinking), so the highlighted chip is always what enter
// would apply.
func (m *Model) modelsModalSyncThinking() {
	if m.thinkingSel == "" || m.thinkingSel == agent.ThinkingOff {
		return
	}
	levels, _ := m.modelsModalThinkingLevels()
	for _, l := range levels {
		if l == m.thinkingSel {
			return
		}
	}
	m.thinkingSel = agent.ThinkingOff
}

// modelsModalStepThinking moves the staged thinking selection by dir
// chips (clamped at the ends).
func (m *Model) modelsModalStepThinking(dir int) {
	levels, supported := m.modelsModalThinkingLevels()
	if !supported {
		return
	}
	idx := thinkingChipCursor(levels, m.thinkingSel)
	if idx < 0 {
		idx = 0
	}
	next := idx + dir
	if next < 0 {
		next = 0
	}
	if next >= len(levels) {
		next = len(levels) - 1
	}
	m.thinkingSel = levels[next]
}

// modelsModalEnter applies the staged selection: the model under the
// cursor plus the staged thinking level. Same model + same level just
// closes; same model + different level applies the level inline; a
// different model runs the async switch and applies the level after it
// lands (validation then runs against the new model's accepted set).
func (m *Model) modelsModalEnter() (tea.Model, tea.Cmd) {
	if len(m.modelList) == 0 || m.modelCursor < 0 || m.modelCursor >= len(m.modelList) {
		m.modal = ModalNone
		return m, nil
	}
	mdl := m.modelList[m.modelCursor]
	sel := m.thinkingSel
	if sel == "" {
		sel = agent.ThinkingOff
	}
	if mdl.Current {
		if m.agent != nil && string(sel) != string(m.agent.ThinkingLevel) {
			if out, _ := m.agent.HandleThinkingCommand("/think " + string(sel)); out != "" {
				m.appendChatLine(SystemStyle.Render(out))
			}
		}
		m.modal = ModalNone
		return m, nil
	}
	return m.loadSelectedModel(sel)
}

// handleModelsModalMouse handles mouse events over the models modal: a
// left click on a thinking chip stages that level, a left click on a
// model entry moves the cursor there (enter still confirms — click
// stages, enter applies). Returns true when the event was consumed.
func (m *Model) handleModelsModalMouse(ev mouseEvent) bool {
	if m.modal != ModalModels || ev.kind != mousePress || ev.button != tea.MouseLeft {
		return false
	}
	if m.width <= 0 || m.height <= 0 {
		return false
	}
	// The overlay centers the box in the terminal (renderModalOverlay);
	// measure the rendered box to recover its origin.
	modal := m.renderModelsModal()
	boxW := blockWidth(modal)
	boxH := lipgloss.Height(modal)
	ox := (m.width - boxW) / 2
	oy := (m.height - boxH) / 2
	if ev.x < ox || ev.x >= ox+boxW || ev.y < oy || ev.y >= oy+boxH {
		return false
	}
	relX := ev.x - ox - 3 // border(1) + content padding(2)
	relY := ev.y - oy - 1 // top border
	if levels, supported := m.modelsModalThinkingLevels(); supported {
		// Content rows: 0 title, 1 thinking heading, 2 chip row.
		if relY == 2 {
			_, spans := buildThinkingChipRow(levels, thinkingChipCursor(levels, m.thinkingSel))
			for _, s := range spans {
				if relX >= s.x0 && relX < s.x1 {
					m.thinkingSel = s.value
					return true
				}
			}
		}
	}
	_, modelRowAt := m.modelsModalContent()
	if idx, ok := modelRowAt[relY]; ok {
		m.modelCursor = idx
		m.modelsModalSyncThinking()
		return true
	}
	return false
}

// modelProviderName returns the registered provider profile that serves the
// model, falling back to "default" for models served by the implicit legacy
// single-endpoint profile. Matches the web model picker's grouping fallback.
func modelProviderName(mi llm.ModelInfo) string {
	if mi.Provider == "" {
		return "default"
	}
	return mi.Provider
}

// modelsModalInner is the picker's inner width: the fixed preferred
// width as the floor, grown to fit the longest row in the WHOLE list
// (provider headers, entries — numbering, id, context suffix, current
// marker — and every model's thinking chip row) so the box neither hugs
// short entries nor resizes while the user scrolls through windows that
// happen to exclude the longest one or moves the cursor between models
// with different accepted efforts. renderBorderedModalWidth still clamps
// the result to the terminal and truncates rows only when even that
// width cannot fit them.
func (m *Model) modelsModalInner() int {
	inner := modelsModalInnerWidth
	for i, mdl := range m.modelList {
		// Provider header: two-space prefix (buildModelRows's "  "+name),
		// plus the renderer's four content-padding cells.
		if w := cellWidth("  "+modelProviderName(mdl)) + 4; w > inner {
			inner = w
		}
		// Entry row at its real index: the %2d numbering width depends on
		// the position, so the probe must use it.
		if w := cellWidth(modelEntryText(i, mdl)) + 4; w > inner {
			inner = w
		}
		// Thinking chip row for this model's accepted efforts.
		if levels, supported := m.modelsModalThinkingLevelsFor(modelsModalRowRef(mdl)); supported {
			text, _ := buildThinkingChipRow(levels, -1)
			if w := cellWidth(text) + 4; w > inner {
				inner = w
			}
		}
	}
	if len(m.modelList) == 0 {
		// Empty catalog: the chip row still renders (the current
		// model's levels) — probe it too.
		if levels, supported := m.modelsModalThinkingLevels(); supported {
			text, _ := buildThinkingChipRow(levels, -1)
			if w := cellWidth(text) + 4; w > inner {
				inner = w
			}
		}
	}
	return inner
}

// modelEntryText composes one picker entry's plain text — the exact string
// buildModelRows styles and emits. Shared with modelsModalInner so the
// measured width always matches what is rendered.
func modelEntryText(i int, mdl llm.ModelInfo) string {
	line := fmt.Sprintf("  %2d. %s", i+1, mdl.ID)
	if mdl.ContextLimit > 0 {
		line += fmt.Sprintf("  (context: %d tokens)", mdl.ContextLimit)
	}
	if mdl.Current {
		line += " *"
	}
	return line
}

// buildModelRows renders the model entries for the visible scroll window
// [start, end), grouped by serving provider: a header line precedes each run
// of models from one profile (models arrive in profile order — default
// first — so consecutive grouping matches the catalog, mirroring the web
// model picker). Headers are decorative only: they are not cursor targets
// and the 1-based numbering stays aligned with the full list so /models <n>
// selects the same model as the modal. The returned map links each rendered
// row's index to its model list index (mouse hit-testing); header rows are
// not in the map.
func buildModelRows(list []llm.ModelInfo, start, end, cursor int) ([]styleLine, map[int]int) {
	rows := make([]styleLine, 0, end-start+2)
	modelRowAt := make(map[int]int)
	lastProvider := ""
	if start > 0 && start <= len(list) {
		// Resuming mid-list: seed with the previous entry's provider so a
		// group that began above the window emits no duplicate header.
		lastProvider = modelProviderName(list[start-1])
	}
	for i := start; i < end && i < len(list); i++ {
		mdl := list[i]
		if prov := modelProviderName(mdl); prov != lastProvider {
			rows = append(rows, styleLine{
				text:      "  " + ansiCyanOn + prov + ansiReset,
				preStyled: true,
			})
			lastProvider = prov
		}
		rows = append(rows, styleLine{
			text:      modelEntryText(i, mdl),
			highlight: i == cursor,
		})
		modelRowAt[len(rows)-1] = i
	}
	return rows, modelRowAt
}

func (m *Model) handleModelsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.modal = ModalNone
		return m, nil
	case "up", "k":
		if m.modelCursor > 0 {
			m.modelCursor--
			m.modelsModalSyncThinking()
		}
	case "down", "j":
		if m.modelCursor < len(m.modelList)-1 {
			m.modelCursor++
			m.modelsModalSyncThinking()
		}
	case "pgup":
		page := max(1, m.height-20)
		m.modelCursor -= page
		if m.modelCursor < 0 {
			m.modelCursor = 0
		}
		m.modelsModalSyncThinking()
	case "pgdown":
		page := max(1, m.height-20)
		m.modelCursor += page
		if m.modelCursor >= len(m.modelList) {
			m.modelCursor = len(m.modelList) - 1
		}
		m.modelsModalSyncThinking()
	case "left", "h":
		m.modelsModalStepThinking(-1)
	case "right", "l":
		m.modelsModalStepThinking(1)
	case "enter":
		return m.modelsModalEnter()
	}
	return m, nil
}

// loadSelectedModel switches to the model at modelCursor from the models
// modal, carrying the staged thinking level. SelectModel hits the network
// (model list + context-limit probe), so the modal closes immediately and
// the switch runs in the background; the result arrives as modelSwitchMsg
// (handleModelSwitchMsg applies the transcript rebuild + context refresh,
// then the staged level).
func (m *Model) loadSelectedModel(thinking agent.ThinkingLevel) (tea.Model, tea.Cmd) {
	mdl := m.modelList[m.modelCursor]
	// Skip if already on this model.
	if mdl.Current {
		m.modal = ModalNone
		return m, nil
	}
	m.modal = ModalNone
	a := m.agent
	// Qualified selector for a non-default owner: the same model ID may be
	// listed by several providers, and the row's group names the one the
	// user picked (bare ids resolve to the first-listing profile).
	id := mdl.ID
	if prov := modelProviderName(mdl); prov != "default" {
		id = prov + "/" + id
	}
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	return m, func() tea.Msg {
		// HandleModelsCommand shares the inline /models <sel> path: same
		// switch, same "Switched to model: …" line.
		out, _, err := a.HandleModelsCommand(base, "/models "+id)
		return modelSwitchMsg{agent: a, out: out, err: err, thinking: thinking}
	}
}

// --- Help Modal ---

func (m *Model) renderHelpModal() string {
	sections := []struct {
		title string
		binds [][]string
	}{
		{"Commands", [][]string{
			{"enter", "Submit input (queues while busy)"},
			{"ctrl+c", "Cancel turn / Quit"},
			{"ctrl+q", "Queued messages (cancel/remove)"},
			{"ctrl+\\", "Force quit"},
			{"F1 / /help", "Show this help"},
			{"ctrl+v", "Toggle verbose"},
			{"ctrl+b", "Toggle sessions panel"},
			{"tab (scroll mode)", "Focus sessions panel"},
		}},
		{"Input Editing", [][]string{
			{"ctrl+a / home", "Line start"},
			{"ctrl+e / end", "Line end"},
			{"ctrl+k", "Kill to end"},
			{"ctrl+u", "Kill to start"},
			{"ctrl+w", "Kill word backward"},
			{"ctrl+left/right", "Word left/right"},
			{"ctrl+d", "Delete forward / EOF quit"},
			{"backspace", "Delete backward"},
			{"tab", "Complete slash cmds"},
		}},
		{"Scrolling", [][]string{
			{"↑ ↓ j k", "Scroll line (esc to focus viewport)"},
			{"pgup / pgdn", "Scroll page (input or viewport focus)"},
			{"home / end", "Top / bottom (empty input or viewport focus)"},
			{"click divider label", "Jump to latest / New messages"},
			{"[ / ]", "Previous / next prompt (viewport focus)"},
			{"mouse wheel", "Scroll viewport"},
			{"mouse (right edge)", "Prompt rail: hover = preview, click = jump"},
			{"i / enter", "Return to input"},
		}},
		{"Slash Commands", [][]string{
			{"/help", "Show this help"},
			{"/plan / /act", "Toggle plan/act mode"},
			{"/mode", "Show current mode"},
			{"/think", "Set thinking level"},
			{"/models", "List/switch models + thinking level"},
			{"/context", "Context usage details"},
			{"/new", "Start new session"},
			{"/resume", "List/restore/delete sessions"},
			{"/open", "Open a new live session"},
			{"/switch", "Switch live sessions"},
			{"/compact", "Compact history"},
			{"/smartcompact", "Smart compact on, off, or toggle"},
			{"/contextmode", "Context mode: default or deep_dive"},
			{"/queue", "Queued messages (send now, per-item cancel)"},
			{"/verbose", "Toggle verbose output"},
			{"/save-config", "Write config to .gogen/"},
			{"dir <path>", "Change working dir"},
			{"/exit", "Quit GoGen"},
		}},
		{"Sessions Panel", [][]string{
			{"click row", "Focus / resume session"},
			{"click ✕", "Close (live, stays saved) / delete (saved)"},
			{"wheel", "Scroll the list"},
			{"↑ ↓ j k", "Move (tab from scroll mode)"},
			{"enter", "Focus / resume"},
			{"n", "New live session"},
			{"x", "Close (stays saved)"},
			{"d", "Delete (confirm)"},
			{"[ / ]", "Resize panel"},
			{"i / esc", "Back to input"},
		}},
		{"Text Selection", [][]string{
			{"click+drag", "Select text in viewport"},
			{"ctrl+shift+c", "Copy selection"},
			{"right click", "Cancel selection"},
			{"esc", "Dismiss modal / focus viewport"},
		}},
	}

	var rows []styleLine

	// Title
	rows = append(rows, styleLine{text: "Keybindings", highlight: true})
	rows = append(rows, styleLine{text: "", highlight: false})

	for _, sec := range sections {
		// Section header
		rows = append(rows, styleLine{text: sec.title, highlight: false})
		for _, bind := range sec.binds {
			key := bind[0]
			desc := bind[1]
			// Pre-style: cyan key, plain desc, 24-CELL key column — padded
			// and truncated by visible width (%-24s measures the escape runes,
			// so with colors on the pad never applied and the description
			// column started right after the key; it also counts a wide rune
			// as one cell, so a key holding one would push the description).
			keyCol := ansiCyanOn + key + ansiReset
			line := "  " + padCells(keyCol, 24) + " " + desc
			rows = append(rows, styleLine{text: line, preStyled: true})
		}
		rows = append(rows, styleLine{text: "", highlight: false})
	}

	// Footer
	rows = append(rows, styleLine{text: "any key to close", highlight: false})

	return m.renderBorderedModal(rows)
}

func (m *Model) handleHelpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Any key dismisses the help overlay
	m.modal = ModalNone
	return m, nil
}

// --- Completion Modal ---

func (m *Model) renderCompletionModal() string {
	if len(m.completions) == 0 {
		return ""
	}
	// Build a single line with inline highlights separated by "  ".
	var b strings.Builder
	for i, c := range m.completions {
		if i == m.completionIdx {
			b.WriteString(ansiHighlightOn)
		} else {
			b.WriteString(ansiDimOn)
		}
		b.WriteString(c)
		b.WriteString(ansiReset)
		if i < len(m.completions)-1 {
			b.WriteString("  ")
		}
	}
	return m.renderBorderedModal([]styleLine{
		{text: b.String(), preStyled: true},
	})
}

func (m *Model) handleCompletionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.modal = ModalNone
		m.completions = nil
		return m, nil
	case "tab":
		m.completionIdx = (m.completionIdx + 1) % len(m.completions)
		return m, nil
	case "shift+tab":
		m.completionIdx--
		if m.completionIdx < 0 {
			m.completionIdx = len(m.completions) - 1
		}
		return m, nil
	case "enter":
		if m.completionIdx >= 0 && m.completionIdx < len(m.completions) {
			prefix, _, ok := agent.ResumeLinePrefix(m.completionLine)
			if ok {
				newArg := m.completions[m.completionIdx]
				if newArg == "del" {
					newArg = "del "
				}
				m.textarea.Reset()
				m.textarea.SetValue(prefix + newArg)
				m.textarea.CursorEnd()
			} else if strings.HasPrefix(strings.TrimRight(m.completionLine, " \t"), "/") {
				m.textarea.Reset()
				m.textarea.SetValue(m.completions[m.completionIdx] + " ")
				m.textarea.CursorEnd()
			}
		}
		m.modal = ModalNone
		m.completions = nil
		return m, nil
	}
	return m, nil
}

// resumeSelectedSession resumes the session at sessionCursor from the sessions modal.
func (m *Model) resumeSelectedSession() (tea.Model, tea.Cmd) {
	id := m.sessionList[m.sessionCursor].ID
	m.modal = ModalNone
	return m, m.resumeSavedRow(id)
}

// deleteSelectedSession asks for confirmation and deletes the session at
// sessionCursor from the sessions modal — the same ModalConfirm contract
// as the sidebar's delete (sidebarDeleteRow): a permanent deletion must
// never be a single keystroke. The sessions list stays open behind the
// dialog (confirmRestore) and is updated in place on confirm.
func (m *Model) deleteSelectedSession() (tea.Model, tea.Cmd) {
	s := m.sessionList[m.sessionCursor]
	label := s.Label
	if label == "" {
		label = s.ID
	}
	m.confirmText = fmt.Sprintf("Delete session %q?\nIts message history will be permanently deleted.", label)
	m.confirmRestore = ModalSessions
	m.confirmAction = func() (tea.Model, tea.Cmd) {
		cmd := m.deleteSavedRow(s.ID)
		if cmd == nil {
			// Refused (e.g. the session is still hosted live in a
			// background slot): nothing was deleted, so keep the row and
			// close the list so the refusal status line is visible.
			m.modal = ModalNone
			return m, nil
		}
		// Remove from the local list.
		m.sessionList = append(m.sessionList[:m.sessionCursor], m.sessionList[m.sessionCursor+1:]...)
		if m.sessionCursor >= len(m.sessionList) {
			m.sessionCursor = len(m.sessionList) - 1
		}
		if m.sessionCursor < 0 {
			m.sessionCursor = 0
		}
		if len(m.sessionList) == 0 {
			m.modal = ModalNone
		}
		return m, cmd
	}
	m.modal = ModalConfirm
	return m, nil
}
