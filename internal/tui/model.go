package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/session"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// tabWidthCells mirrors the viewport render style's default TabWidth
// (lipgloss expands every tab to this many spaces before its own wrap
// pass — a plain replace, not tab stops).
const tabWidthCells = 4

// wrapWidth returns the available width for word-wrapping inside the viewport.
func (m *Model) wrapWidth() int {
	w := m.viewport.Width
	if w < 10 {
		w = 10
	}
	w -= m.viewport.Style.GetHorizontalFrameSize()
	if w < 10 {
		w = 10
	}
	return w
}

// normalizeCR removes carriage returns from a chat line before it is
// wrapped. A CR is the one raw control byte that survives the whole render
// pipeline as a real cursor move: ultraviolet's printString handles exactly
// three cursor-affecting inputs — "\n" (y++), "\r" (x = the FRAME's left
// margin) and SGR — and drops every other escape. So a CR left in a chat
// line makes the renderer send the pen back to column 0 of the frame and
// repaint over whatever sits there, wiping the sidebar, the divider and the
// left of the transcript row.
//
// Command output is the source: execute_command and bash_persistent return
// child bytes verbatim, and handleStreamToolResult splits the result on "\n"
// only, so every CRLF leaves a trailing CR and a progress bar
// ("10%\r50%\rdone") leaves mid-line ones. That made the frame's painted
// rows disagree with the model's cell model, which is what breaks the layout.
//
// CRLF collapses to LF (the newline it already is — SetContent/SetContentMax
// have always done this); a bare CR is dropped, because in a static
// transcript its only meaning ("overwrite this row") cannot be honoured and
// keeping it is what corrupts the frame. The content of the row is
// unchanged, so no output is lost. This runs on every wrap, so the
// streaming and renderMessages history paths are both covered.
func normalizeCR(line string) string {
	if !strings.ContainsRune(line, '\r') {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c != '\r' {
			b.WriteByte(c)
			continue
		}
		// CRLF: the CR is redundant, the following LF already breaks the
		// line. A bare CR falls through and is dropped.
		if i+1 < len(line) && line[i+1] == '\n' {
			continue
		}
	}
	return b.String()
}

// wrapLine wraps a single chat line ready for display. lipgloss.Wrap does
// word-wrapping plus hard-wrapping of overlong tokens (URLs, paths, etc.) in
// one pass, and re-applies any SGR style that is still open onto every
// continuation line while closing it before end-of-line — so styles survive
// wrapping without manual SGR bookkeeping here (the hand-rolled tracker this
// replaced was the source of the v2 label-bleed bug fixed in a3b358b).
func (m *Model) wrapLine(line string) []string {
	w := m.wrapWidth()
	// Drop carriage returns BEFORE wrapping (see normalizeCR): a CR is the
	// one byte that makes the renderer move the pen, and wrapping around it
	// would spread the repaint over several rows.
	line = normalizeCR(line)
	// Expand tabs BEFORE wrapping. Widths disagree on a raw tab: the wrap
	// pass counts it as one cell, cellWidth as zero, and the
	// viewport's View render replaces it with tabWidthCells spaces before its
	// own wrap pass. A tab near the wrap boundary therefore passed through
	// here as a single ≤limit part and then re-wrapped inside View onto two
	// terminal rows — every row below it shifted down one, scrolling landed
	// on the wrong row, and the top of the message appeared stuck. Only
	// tab-bearing lines were affected (diffs, tool output, pasted code).
	// Pre-expanding makes the wrap pass see exactly what the render pass sees.
	line = strings.ReplaceAll(line, "\t", strings.Repeat(" ", tabWidthCells))
	wrapped := wrapCells(line, w)
	parts := strings.Split(wrapped, "\n")
	// Strip trailing empty elements caused by a trailing newline.
	// Without this, a trailing \n creates a blank visual line that
	// flickers during streaming or persists in the final output.
	for len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// buildFromPrefix rebuilds the viewport from wrappedPrefix + the last chat
// line. All incremental updaters call this instead of the full re-wrap path.
//
// During streaming, this is the hot path called on every token batch (~32 ms).
// Only the last chat line changes, so we re-wrap just that line and splice it
// onto the cached prefixLines (viewport lines for chatLines[:len-1]) instead
// of re-splitting the entire wrapped content into lines on every flush — the
// old SetContentMax path was O(conversation) per token batch. The max line
// width is likewise maintained incrementally (only the new last line is
// measured), skipping the O(N) cellWidth scan of the full conversation
// that the stock bubbles viewport performs. The full wrappedContent string is
// materialized lazily via wrappedContentString() for consumers that need it
// (selection rendering); streaming itself only needs the split lines.
func (m *Model) buildFromPrefix() {
	if len(m.chatLines) == 0 {
		m.wrappedContent = ""
		m.wrappedLines = nil
		m.wrappedLinesDirty = false
		m.maxWrappedWidth = 0
		m.lastWrapped = ""
		m.wrappedContentDirty = false
		m.prefixLines = nil
		m.clearSelection()
		m.viewport.SetContentMax("", 0)
		// styledLines is computed lazily in ensureStyledLines().
		m.styledLinesDirty = true
		return
	}
	lastParts := m.wrapLine(m.chatLines[len(m.chatLines)-1])
	lastWrapped := strings.Join(lastParts, "\n")
	m.lastWrapped = lastWrapped
	m.wrappedContentDirty = true
	m.wrappedLinesDirty = true
	// Incremental max width: only scan the newly wrapped last line.
	for _, p := range lastParts {
		if w := cellWidth(p); w > m.maxWrappedWidth {
			m.maxWrappedWidth = w
		}
	}
	// Splice the freshly wrapped last line onto the cached prefix lines.
	lastLines := strings.Split(lastWrapped, "\n")
	vp := m.viewport.Lines()
	vp = vp[:0]
	vp = append(vp, m.prefixLines...)
	vp = append(vp, lastLines...)
	m.viewport.SetContentLines(vp, m.maxWrappedWidth)
	m.clearSelection()
	// styledLines is computed lazily in ensureStyledLines().
	m.styledLinesDirty = true
}

// wrappedContentString returns the full wrapped chat content, materializing
// the lazily-maintained wrappedPrefix+lastWrapped concatenation on first
// access. Selection rendering and tests are the only consumers; streaming
// updates avoid building (and re-splitting) the full string on every token
// batch. The cached value is byte-identical to what buildFromPrefix used to
// compute eagerly.
func (m *Model) wrappedContentString() string {
	if m.wrappedContentDirty {
		m.wrappedContent = m.wrappedPrefix + m.lastWrapped
		m.wrappedContentDirty = false
	}
	return m.wrappedContent
}

// markTranscriptRendered records the agent conversation epoch the transcript
// was just rendered from, so the next reshape check
// (rebuildTranscriptIfReshaped) does not rebuild the same state again.
// Call it from every path that rebuilds the transcript from the FOCUSED
// agent's own messages.
func (m *Model) markTranscriptRendered() {
	if m.agent == nil {
		return
	}
	m.historyEpoch = m.agent.HistoryEpoch()
}

// rebuildTranscriptFrom re-renders the focused transcript from a's committed
// messages (via a SnapshotMessages, so it is safe to call once the turn
// goroutine has returned) and records the epoch it was built from. The
// per-turn stream state is reset first: chatLines is replaced wholesale, so
// any stream line index would point at the wrong line.
func (m *Model) rebuildTranscriptFrom(a *agent.Agent) {
	// The stream state is dropped unconditionally: chatLines is about to be
	// replaced wholesale, so any live line index would point at the wrong
	// line (nil agent leaves the empty transcript, same as the join path).
	m.resetStreamState(false)
	if a == nil {
		return
	}
	m.chatLines = renderMessages(a.SnapshotMessages(), a.WorkingDir, a.CurrentModel(), a.Mode.String())
	m.historyEpoch = a.HistoryEpoch()
}

// rebuildTranscriptIfReshaped rebuilds the transcript when the agent replaced
// the conversation wholesale since it was last rendered (agent.HistoryEpoch
// moved: a mid-turn auto-compaction, a restore, a fork, or a rollback).
//
// The transcript is otherwise append-only, so without this a long-lived
// session keeps every line it has ever rendered — chatLines plus its wrapped
// (prefixLines, wrappedContent) and styled (styledLines, wrappedLines) copies
// and the prompt-rail anchors — even though the agent's own history is
// compacted in place. That is unbounded memory growth proportional to total
// tokens ever produced, not to the size of the current conversation.
//
// It must run at a turn boundary (the turn goroutine has returned); the
// explicit /compact and session-switch paths already rebuild on their own and
// mark the epoch. Returns true when it rebuilt.
func (m *Model) rebuildTranscriptIfReshaped() bool {
	if m.agent == nil || m.agent.HistoryEpoch() == m.historyEpoch {
		return false
	}
	m.rebuildTranscriptFrom(m.agent)
	m.setViewportContent()
	// Not pinFollow: a compaction is not a user navigation event, so a
	// reader who scrolled away keeps their place (turn-end parity, see
	// handleStreamEnd).
	m.followBottom()
	return true
}

// setViewportContent performs a full re-wrap of all chatLines and rebuilds
// the incremental prefix.  Use this after window‑resize, session restore,
// mode changes, or other events that touch the whole buffer. It is not
// resize-only: every mid-line stream write (the streaming funnels route any
// chatLines mutation other than the last line here) and every round/turn-end
// reset lands here too, so the work below stays single-pass: one wrap per
// chat line, one width scan, no re-split of the joined content.
func (m *Model) setViewportContent() {
	if m.width <= 2 {
		return
	}
	// The open thinking block's chat line holds PRE-WRAPPED output (the
	// incremental cache, see streaming.go): re-wrapping it at a changed
	// width would break at the old wrap seams. Rebuild the cache at the
	// new width and rewrite the line BEFORE the re-wrap, which then
	// re-wraps already-wrapped lines idempotently (same width).
	if m.streamThinkingOpen {
		m.rebuildStreamThinkingCache()
		m.chatLines[m.streamThinkingLine] = m.streamThinkingDisplay()
	}
	var wrappedParts []string
	var lastParts []string
	// Prompt-rail anchors (web TOC parity): record where each user prompt
	// starts in the wrapped content. wrapLine is deterministic, so the
	// per-line wrap counts give the physical offsets.
	anchors := m.tocAnchors[:0]
	lineNo := 0
	for _, line := range m.chatLines {
		lastParts = m.wrapLine(line)
		wrappedParts = append(wrappedParts, lastParts...)
		if isUserPromptLine(line) {
			anchors = append(anchors, tocAnchor{line: lineNo, text: promptTextOf(line)})
		}
		lineNo += len(lastParts)
	}
	m.tocAnchors = anchors
	m.wrappedContent = strings.Join(wrappedParts, "\n")
	m.wrappedLinesDirty = true // lazily compute on next selection access
	m.clearSelection()
	// Full re-scan of all wrapped lines. Not actually rare: besides resize,
	// session restore, and mode changes this runs at every round and turn
	// end (streaming.go resetStreamState/handleStreamEnd) and on every
	// mid-line stream write (streaming.go routes any chatLines mutation
	// that is not the last line here).
	m.maxWrappedWidth = 0
	for _, p := range wrappedParts {
		if w := cellWidth(p); w > m.maxWrappedWidth {
			m.maxWrappedWidth = w
		}
	}
	// Publish the already-split lines directly — SetContentMax would split
	// wrappedContent right back after the join above.
	m.viewport.SetContentLines(wrappedParts, m.maxWrappedWidth)
	// styledLines is computed lazily in ensureStyledLines().
	m.styledLinesDirty = true

	// Rebuild the prefix pointing at all lines except the last. wrapLine is
	// deterministic and the width is unchanged since the loop above, so the
	// head of wrappedParts IS chatLines[:len-1]'s wrap — slice it instead of
	// re-wrapping every line a second time. prefixLines aliases the
	// viewport's line backing array (SetContentLines stores the slice
	// as-is); the pair is rebuilt together here and buildFromPrefix copies
	// from prefixLines before republishing, so the shared backing is never
	// read stale.
	if len(m.chatLines) > 1 {
		prefixParts := wrappedParts[:len(wrappedParts)-len(lastParts)]
		m.wrappedPrefix = strings.Join(prefixParts, "\n") + "\n"
		m.prefixLines = prefixParts
	} else {
		m.wrappedPrefix = ""
		m.prefixLines = nil
	}
	// The full content was materialized above; keep the incremental state
	// consistent so the next buildFromPrefix splices onto the right prefix.
	m.lastWrapped = strings.Join(lastParts, "\n")
	m.wrappedContentDirty = false
}

func (m *Model) View() tea.View {
	if !m.ready {
		return tea.NewView("Initializing...")
	}

	if m.quitting {
		return tea.NewView("")
	}

	main := m.renderMainColumn()

	// Sidebar panel (live sessions) — only when visible AND the main
	// column keeps its minimum width (mainWidth auto-hides otherwise).
	if m.sidebarOffsetX() > 0 {
		mainLines := strings.Count(main, "\n") + 1
		main = lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(mainLines), main)
	}

	// Prompt rail (web TOC parity): dots + hover preview pasted into the
	// frame; a no-op unless the pointer is in the rail's trigger strip.
	main = m.applyTocOverlay(main)

	// Jump-to-latest label while the reader is parked above the live edge
	// (web parity: #scroll-bottom-btn); a no-op while following.
	main = m.applyJumpButtonOverlay(main)

	v := tea.NewView(main)
	// Mouse reporting: cell motion covers wheel scrolls and drag-selection.
	v.MouseMode = tea.MouseModeCellMotion
	// Focus reporting: tea.FocusMsg / tea.BlurMsg drive the completion
	// bell (bellIfBlurred) — it rings only while the window is blurred.
	v.ReportFocus = true

	// Modal overlay — renders on opaque background so nothing bleeds through
	if m.modal != ModalNone {
		return tea.NewView(renderModalOverlay(m.renderModal(), m.width, m.height))
	}

	return v
}

// viewportFocusIndicator is the divider hint painted while the viewport
// has focus. It owns the divider's right end, which the jump button yields
// to (jumpbutton.go).
const viewportFocusIndicator = " [SCROLL] Press i or Esc to return to input "

// renderMainColumn assembles the chat column (viewport, divider, input,
// status bar) WITHOUT the sidebar. Kept separate so the sidebar can match
// its exact row height in View.
func (m *Model) renderMainColumn() string {
	// Viewport content: use selection-aware render when selecting,
	// otherwise use the stock viewport render.
	var vpView string
	if m.selection != nil && m.selection.Active {
		vpView = m.renderViewportWithSelection()
	} else {
		vpView = m.viewport.View()
	}

	// Input band: the composer, plus — while a turn (or compaction) runs —
	// the one-row progress strip ABOVE it (steering: the composer stays
	// visible and editable while busy; Enter queues, ctrl+c interrupts).
	// SetSize reserves the strip row out of the textarea's rows, so the
	// band's total height never changes at turn boundaries. busyStripRows
	// is the strip predicate shared with SetSize's budget: on a terminal
	// too short to hold the strip next to the composer it reports 0 and
	// the band stays composer-only, so the rendered band can never
	// disagree with the budgeted one and overflow the terminal.
	var inputArea string
	switch {
	case m.streaming:
		inputArea = m.textarea.View()
		if m.busyStripRows() > 0 {
			inputArea = m.renderProgressInput() + "\n" + inputArea
		}
	case m.compacting:
		inputArea = m.textarea.View()
		if m.busyStripRows() > 0 {
			inputArea = m.renderCompactingInput() + "\n" + inputArea
		}
	default:
		inputArea = m.textarea.View()
	}

	// Divider with focus indicator.  Cache the rendered string and only
	// rebuild when width, focus, or streaming state change. Sized to the
	// MAIN COLUMN (not the terminal): with the sidebar visible the column
	// is narrower, and an over-wide divider would grow the combined frame
	// past the terminal.
	main := m.mainWidth()
	dividerDirty := m.dividerCacheWidth != main ||
		m.dividerCacheFocus != m.focus ||
		m.dividerCacheStream != m.streaming
	if dividerDirty {
		if m.focus == FocusViewport {
			line := strings.Repeat("─", main)
			keep := max(0, main-len(viewportFocusIndicator))
			m.dividerCache = DividerStyle.Render(sliceByRuneCount(line, keep) + viewportFocusIndicator)
		} else if m.streaming {
			m.dividerCache = DimStyle.Render(strings.Repeat("─", main))
		} else {
			m.dividerCache = DividerStyle.Render(strings.Repeat("─", main))
		}
		m.dividerCacheWidth = main
		m.dividerCacheFocus = m.focus
		m.dividerCacheStream = m.streaming
	}
	divider := m.dividerCache

	// Assemble
	return lipgloss.JoinVertical(
		lipgloss.Left,
		vpView,
		divider,
		inputArea,
		m.renderStatusBar(),
	)
}

// refreshSavedSessions synchronously snapshots the persisted-session
// index backing the sidebar's unified list (best-effort; errors leave the
// cache empty). Reserved for paths where no program exists yet (NewModel)
// or where a synchronous read is required (tests); runtime callers use
// requestSavedSessions, which keeps the store I/O off the Update thread.
func (m *Model) refreshSavedSessions() {
	if m.agent == nil {
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	result, _, err := m.agent.HandleSessionCommand(ctx, "sessions", session.NewID())
	if err != nil {
		return
	}
	m.savedCache = result.Sessions
}

// requestSavedSessions snapshots the persisted-session index off the
// Update thread (the store List is disk I/O; it used to run inline on
// every turn end, switch, and 30 s tick). The result arrives as
// savedSessionsMsg; a stale response (an older request resolving after a
// newer one) is dropped via savedReqSeq.
func (m *Model) requestSavedSessions() tea.Cmd {
	if m.agent == nil {
		return nil
	}
	a := m.agent
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	m.savedReqSeq++
	seq := m.savedReqSeq
	return func() tea.Msg {
		result, _, err := a.HandleSessionCommand(ctx, "sessions", session.NewID())
		if err != nil {
			return savedSessionsMsg{seq: seq, ok: false}
		}
		return savedSessionsMsg{seq: seq, sessions: result.Sessions, ok: true}
	}
}

// requestContextStats probes context usage off the Update thread and
// delivers it via the existing contextStatsMsg path. Used after
// resume/fork, where the token-count cache is cold and the synchronous
// probe would tokenize the whole restored conversation on the render
// loop. Stream-boundary callers keep the synchronous variants (the
// round-start rebase needs the value inline).
func (m *Model) requestContextStats() tea.Cmd {
	if m.agent == nil {
		return nil
	}
	a := m.agent
	// Tag the probe with the session it was requested for, on the Update
	// thread: the agent's SessionID can change in place (resume) while the
	// probe runs off-thread, and the handler drops the message once focus
	// has moved to another session.
	sid := a.SessionID
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		return contextStatsMsg{stats: a.ContextStats(ctx), sid: sid}
	}
}

// renderModalOverlay covers the main view with an opaque background block
// and centers the modal on top of it. lipgloss.Place is a noöp when the
// modal exceeds the terminal in either dimension, matching the old manual
// clamping; ModalOverlayBackground paints every cell, padding included.
func renderModalOverlay(modal string, width, height int) string {
	return ModalOverlayBackground.Render(
		lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, modal),
	)
}

// refocusInput restarts the textarea cursor blink after streaming (blink ticks
// are ignored while streaming==true, so the blink loop must be restarted).
func (m *Model) refocusInput() tea.Cmd {
	if m.focus != FocusInput || m.modal != ModalNone {
		return nil
	}
	return m.textarea.Focus()
}

// bumpContextEstimate updates the status-bar context indicator with an
// approximate running total while a turn is streaming. It never reads
// a.Messages (which would race with the streaming goroutine mutating it) —
// it only combines the baseline captured in handleStreamStart with a local
// character-based estimate of text that has already safely arrived on the
// Update thread via stream messages.
func (m *Model) bumpContextEstimate(delta string) {
	if delta == "" {
		return
	}
	if m.contextEst.Base() <= 0 && m.contextStats.Snapshot.Limit <= 0 {
		// No baseline yet (e.g. first turn before any refresh) — nothing
		// meaningful to show until refreshContextStats() runs.
		return
	}
	// LiveEstimate uses the shared bytes/4 approximation internally
	// (tokenizer-free, so it stays cheap mid-stream). The exact count is
	// restored by refreshContextStats() as soon as streaming ends.
	m.contextEst.Add(delta)
	snap := m.contextStats.Snapshot
	snap.Used = m.contextEst.Used()
	if snap.Limit > 0 {
		snap.Percent = float64(snap.Used) / float64(snap.Limit)
	}
	display := m.contextStats
	display.Snapshot = snap
	if line := agent.FormatContextBrief(display); line != "" {
		m.contextLine = line + " (est.)"
	}
}

// refreshContextStats updates the status-bar context indicator immediately.
// ContextStats is safe to call concurrently with a running turn (it
// snapshots shared state under statsMu), but this unguarded variant is
// reserved for paths where a minimal snapshot is acceptable (stream end,
// error, session changes); mid-turn callers should use
// refreshContextStatsMidTurn, which never blanks the indicator.
func (m *Model) refreshContextStats() {
	if m.agent == nil {
		m.contextStats = agent.TurnContext{}
		m.contextLine = ""
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	stats := m.agent.ContextStats(ctx)
	m.contextStats = stats
	m.contextLine = agent.FormatContextBrief(stats)
}

// refreshContextStatsMidTurn refreshes the context indicator from the agent
// at a round boundary while a turn is streaming. ContextStats is safe to
// call concurrently with a running turn (it snapshots shared state under
// statsMu and tokenizes a clone), so this does not race the streaming
// goroutine. A minimal snapshot (e.g. the caller context was cancelled and
// ContextStats short-circuited) is not adopted, so the indicator never
// blanks out mid-turn.
func (m *Model) refreshContextStatsMidTurn() {
	if m.agent == nil {
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	stats := m.agent.ContextStats(ctx)
	if stats.Snapshot.Used <= 0 && stats.Snapshot.Limit <= 0 {
		return
	}
	m.contextStats = stats
	m.contextLine = agent.FormatContextBrief(stats)
}

// flushAndQuit persists any unsaved state before the program exits —
// without this, the 5 s debounce could drop the last few seconds of state.
// Clean sessions are left untouched: a forced write would re-stamp each
// session's Updated timestamp with ~now in sweep order (the focused
// session first, so it received the OLDEST stamp), reshuffling the
// saved-session list on restart — recency must keep reflecting the last
// interaction, not the quit sweep.
func (m *Model) flushAndQuit() {
	m.quitting = true
	m.flushAllSessions()
}

// flushAllSessions persists any unsaved state on the focused agent and every
// background live session. Each agent owns its persisted state and the
// debounce window is per-agent, so a session whose last turn ended (or whose
// last write failed) just before exit would otherwise lose its tail.
//
// FlushPending, not FlushSession: it writes only sessions that are DIRTY (an
// unsaved turn tail, a debounced write, or a failed write awaiting retry),
// so a clean session is left untouched and keeps its earned Updated
// timestamp — a forced write on every session re-stamped them in sweep order
// and reshuffled the saved-session list on restart (recency must reflect the
// last interaction, not the exit sweep). This is the TUI twin of the web
// ShutdownSessions / evictRuntime fix.
//
// Called by flushAndQuit (in-app quit) and by TUI.Run after the program
// returns on a context cancellation (SIGINT/SIGTERM/SIGHUP) — the signal
// exit used to skip the sweep entirely, so a dirty background session was
// lost even though the default agent was still covered by main's deferred
// FlushPending.
func (m *Model) flushAllSessions() {
	if m.agent != nil {
		m.agent.FlushPending()
	}
	if m.lives != nil {
		for _, s := range m.lives.sessions {
			if s.agent == nil || s.agent == m.agent {
				continue
			}
			s.agent.FlushPending()
		}
	}
}

// checkPersistError surfaces any pending session-save error in the status
// bar. Call from any code path that may have triggered Agent.persistSession
// (slash commands, /rename tool, etc.) so silent save failures aren't lost.
func (m *Model) checkPersistError() {
	if m.agent == nil {
		return
	}
	if err := m.agent.ConsumePersistError(); err != nil {
		m.statusMsg = fmt.Sprintf("Warning: failed to save session: %v", err)
	}
}

// bellIfBlurred rings the terminal bell while the window is NOT focused —
// the TUI equivalent of the web's desktop notifications (turn end, turn
// error, approval request). No bell while the user is looking at the
// terminal; terminals that don't report focus never blur, so they never
// bell.
func (m *Model) bellIfBlurred() {
	if !m.terminalBlurred {
		return
	}
	m.bellsRung++
	// BEL to the tty (the program's default output). A lone control
	// character interleaved with renderer frames is harmless — the
	// terminal emulates it, it never shifts the cursor. The program-nil
	// guard keeps literal-built test models quiet.
	if m.program != nil {
		fmt.Fprint(os.Stdout, "\a")
	}
}

func (m *Model) handleStreamError(err error) {
	wasStreaming := m.streaming
	m.streaming = false
	m.dismissApproval(false)
	m.clearProgress()
	m.resetStreamState(false)
	m.refreshContextStats()
	if m.agent != nil {
		if persistErr := m.agent.ConsumePersistError(); persistErr != nil {
			m.statusMsg = fmt.Sprintf("Warning: failed to save session: %v", persistErr)
		}
	}
	if err == nil {
		return
	}
	// UI cancel already printed "Cancelled." — don't duplicate context.Canceled.
	if !wasStreaming && (err == context.Canceled || strings.Contains(err.Error(), "context canceled")) {
		return
	}
	m.bellIfBlurred() // web parity: turn-error notification (cancels stay quiet)
	m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Error: %v", err)))
}

// sliceByRuneCount returns the prefix of s containing at most n runes.
// Uses rune-counting so it does not split multi-byte UTF-8 characters.
func sliceByRuneCount(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if n >= len(runes) {
		return s
	}
	return string(runes[:n])
}

func summarizeResult(result string, success bool) string {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		if success {
			return "(empty)"
		}
		return "(no output)"
	}
	lines := strings.Count(trimmed, "\n") + 1
	chars := len(trimmed)
	if !success {
		first := trimmed
		if idx := strings.Index(first, "\n"); idx >= 0 {
			first = first[:idx]
		}
		if len(first) > 120 {
			first = truncateRunes(first, 117) + "..."
		}
		return fmt.Sprintf("%s (%d chars)", first, chars)
	}
	if lines == 1 && chars <= 120 {
		return trimmed
	}
	return fmt.Sprintf("(%d lines, %d chars)", lines, chars)
}
