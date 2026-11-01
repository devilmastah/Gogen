package tui

import (
	"fmt"

	"gogen/internal/agent"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbletea/v2"
)

// codeIndexProgressMsg is the shared index snapshot, already rendered as one
// sentence. Walking updates the status bar only. Embedding and writing mark
// the rebuild busy. Idle after that busy stretch appends the finished line
// when there is no error. A search that only scans an up-to-date index does
// not append a line. Idle with an error leaves that sentence on the status
// bar. /reindex still reports the error through codeIndexDoneMsg. A watch
// refresh has no done message, so the status bar is the report.
type codeIndexProgressMsg struct {
	line  string
	phase string
	err   string
}

type codeIndexDoneMsg struct {
	err error
}

func (m *Model) Init() tea.Cmd {
	// The sidebar tick loop runs for the whole program lifetime (a no-op
	// while the panel is hidden) so toggling can never fork a second loop.
	return tea.Batch(m.textarea.Focus(), sidebarTickCmd())
}

// Update implements the Bubble Tea update contract; the per-message dispatch
// lives in handleMsg. On bubbletea v2 the terminal's key-disambiguation mode
// (kitty protocol / xterm modifyOtherKeys) is negotiated automatically, so
// ctrl+shift+c arrives as a normal KeyPressMsg and needs no special-casing.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Host messages posted from this goroutine must go through an
	// asynchronous handoff: tea.Program.Send writes to the very channel this
	// loop is reading, so an inline send from here deadlocks.
	m.inUpdate.Store(true)
	defer m.inUpdate.Store(false)
	return m.handleMsg(msg)
}

// handleMsg dispatches a tea message to its per-type handler. The textarea
// fallback at the end (cursor blink + normal input) runs for any message no
// case consumed — including the per-type handlers' own sub-messages.
func (m *Model) handleMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// One gate for everything that belongs to a streaming turn: the ownership
	// and staleness checks, and the "not streaming → drop" filter the fourteen
	// render handlers each opened with.
	//
	// The classification is the streamAttributed interface (embedded
	// streamMeta), so a new stream message is covered by construction. The two
	// deliberate exceptions are expressed HERE instead of by omitting a line
	// from a handler body:
	//   - terminal messages (streamEndMsg / streamErrorMsg) do not implement
	//     streamAttributed: handleTurnFinishedMsg resolves their session and
	//     generation itself, and a terminal for a session that lost focus must
	//     still finalize;
	//   - condensedNoteMsg renders regardless of the focused session's
	//     streaming state — it is a note from a background session, and
	//     dropping it while a *different* session streams lost the notice.
	if sa, ok := msg.(streamAttributed); ok {
		sid, seq := sa.attribution()
		// Ownership: drop render events whose owner lost focus between send
		// and delivery (switch-boundary stragglers).
		if !m.ownsStream(sid) {
			return m, nil
		}
		// Staleness: events from a SUPERSEDED turn (cancel + resubmit). The
		// old turn's goroutine may still be unwinding, and letting its
		// stragglers through would clobber the new turn's state (cancel func,
		// streaming flags, transcript) and surface a stale "context canceled".
		if m.staleTurn(sid, seq) {
			return m, nil
		}
		if _, isNote := msg.(condensedNoteMsg); !isNote && !m.streaming {
			return m, nil
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleWindowSizeMsg(msg)

	// Terminal window focus (View's ReportFocus): the completion bell
	// rings only while the window is blurred.
	case tea.FocusMsg:
		m.terminalBlurred = false
		return m, nil

	case tea.BlurMsg:
		m.terminalBlurred = true
		return m, nil

	case modelChangedMsg:
		return m.handleModelChangedMsg()

	case codeIndexProgressMsg:
		switch msg.phase {
		case "embedding", "writing":
			m.codeIndexBusy = true
			m.statusMsg = msg.line
			return m, nil
		case "walking":
			m.statusMsg = msg.line
			return m, nil
		}
		if m.codeIndexBusy && msg.err == "" {
			m.appendChatLine(SystemStyle.Render(msg.line))
		}
		m.codeIndexBusy = false
		if msg.err != "" {
			m.statusMsg = msg.line
			return m, nil
		}
		m.statusMsg = ""
		return m, nil

	case codeIndexDoneMsg:
		if msg.err != nil {
			m.codeIndexBusy = false
			m.statusMsg = ""
			m.appendChatLine(SystemStyle.Render(msg.err.Error()))
		}
		return m, nil

	case spinner.TickMsg:
		return m.handleSpinnerTick(msg)

	case tea.KeyPressMsg:
		// Only key presses are dispatched; key releases are ignored so a
		// terminal reporting event types cannot double-trigger actions.
		return m.handleKeyMsg(msg)

	// Streaming messages
	case streamStartMsg:
		return m.handleStreamStartMsg()

	case streamRoundStartMsg:
		return m.handleStreamRoundStartMsg()

	case streamTokenMsg:
		return m.handleStreamTokenMsg(msg)

	case streamThinkingMsg:
		return m.handleStreamThinkingMsg(msg)

	case streamStatsMsg:
		return m.handleStreamStatsMsg(msg)

	case streamCompactingMsg:
		return m.handleStreamCompactingMsg()

	case streamStallMsg:
		return m.handleStreamStallMsg()

	case streamRetryMsg:
		return m.handleStreamRetryMsg(msg)

	case streamToolCallMsg:
		return m.handleStreamToolCallMsg(msg)

	case streamToolCallArgsMsg:
		return m.handleStreamToolCallArgsMsg(msg)

	case streamToolCallFinalMsg:
		return m.handleStreamToolCallFinalMsg(msg)

	case streamToolExecuteMsg:
		return m.handleStreamToolExecuteMsg(msg)

	case streamToolResultMsg:
		return m.handleStreamToolResultMsg(msg)

	case streamRoundEndMsg:
		return m.handleStreamRoundEndMsg()

	case streamEndMsg:
		return m.handleTurnFinishedMsg(msg.sid, msg.seq, nil)

	case streamErrorMsg:
		return m.handleTurnFinishedMsg(msg.sid, msg.seq, msg.err)

	case condensedNoteMsg:
		return m.handleCondensedNoteMsg(msg)

	case contextStatsMsg:
		return m.handleContextStatsMsg(msg)

	case compactResultMsg:
		return m.handleCompactResultMsg(msg)

	case modelListMsg:
		return m.handleModelListMsg(msg)

	case modelSwitchMsg:
		return m.handleModelSwitchMsg(msg)

	case savedSessionsMsg:
		// A stale response (an older request resolving after a newer
		// one) must not clobber the fresh snapshot.
		if msg.seq == m.savedReqSeq && msg.ok {
			m.setSavedCache(msg.sessions)
		}
		return m, nil

	case sidebarTickMsg:
		// 30 s relative-time / store refresh (web's sidebar tick).
		var cmd tea.Cmd
		if m.sidebarVisible {
			cmd = m.requestSavedSessions()
		}
		return m, tea.Batch(cmd, sidebarTickCmd())

	// Approval request (show modal)
	case approvalRequestMsg:
		return m.handleApprovalRequestMsg(msg)

	// Continuable subagents: the parent's tool goroutine hands the Update
	// thread the work that needs slot ownership (admission + turn start,
	// targeted deliveries, releases) and parks on the returned channel.
	case childSpawnMsg:
		return m.handleChildSpawnMsg(msg)
	case childAdoptMsg:
		return m.handleChildAdoptMsg(msg)
	case childMessageMsg:
		return m.handleChildMessageMsg(msg)
	case childReleaseMsg:
		return m.handleChildReleaseMsg(msg)

	// System message delivery (queue + drain when idle)
	case deliveryRequestMsg:
		return m.handleDeliveryRequestMsg(msg)

	// Pass mouse events to the viewport for wheel scrolling
	case tea.MouseMsg:
		return m.handleMouseMsg(msg)
	}

	// Update textarea for cursor blink and normal input. The composer is
	// editable while a turn (or compaction) runs — steering: Enter queues
	// the message, ctrl+c interrupts (handleInputKey intercepts both
	// before this gate matters).
	if m.focus == FocusInput && m.modal == ModalNone && !(m.streaming || m.compacting) {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) handleWindowSizeMsg(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	// Inline-mode shrink guard: the alt-screen renderer only forces a full
	// repaint when the terminal SHRINKS in fullscreen mode; inline mode
	// keeps its incremental cell diff, which cannot predict how a real
	// terminal reflows wider rows after a shrink — the diff then desyncs
	// from the screen (stale truncated rows above, the frame repainted at
	// a shifted origin: stuck columns, ghost cursors). A shrunk layout must
	// therefore request a full repaint via ClearScreen, which re-baselines
	// renderer and terminal. Growth needs no clearing: new rows appear
	// below the existing frame and the incremental path stays exact.
	shrunk := m.ready && (msg.Width < m.width || msg.Height < m.height)
	m.SetSize(msg.Width, msg.Height)
	if shrunk {
		// v2: ClearScreen is a Msg; wrap it in the command that returns it.
		return m, func() tea.Msg { return tea.ClearScreen() }
	}
	return m, nil
}

func (m *Model) handleModelChangedMsg() (tea.Model, tea.Cmd) {
	// A background validation may have just resolved a restored session's
	// context window (or cleared/auto-selected its model). Re-rendering
	// alone reads the CACHED context line, which still holds the snapshot's
	// pre-warmed fallback; re-probe the indicator so it shows the resolved
	// window. The probe is async: the token-count cache is cold on the
	// restore path, and tokenizing must stay off the Update thread.
	if m.agent == nil {
		return m, nil
	}
	return m, m.requestContextStats()
}

func (m *Model) handleSpinnerTick(msg spinner.TickMsg) (tea.Model, tea.Cmd) {
	// The spinner also animates the /compact wait indicator (compacting
	// is not a turn, so progressAnimating does not cover it).
	if !m.progressAnimating() && !m.compacting {
		return m, nil
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m *Model) handleCompactResultMsg(msg compactResultMsg) (tea.Model, tea.Cmd) {
	// Clear the OWNING session's flags (resolved from the compacting
	// agent, not the focused mirror): the result may land while another
	// session is focused, and it must not touch that session's state.
	var wasUserCancelled bool
	if m.lives != nil && msg.agent != nil {
		if s := m.lives.ByAgent(msg.agent); s != nil {
			if msg.seq != s.compactSeq {
				// Superseded compaction (cancelled, then restarted before
				// the old goroutine finished unwinding): the late result is
				// dropped entirely. It must not clear the new run's flags
				// (input would unlock mid-compaction, ctrl+c would lose its
				// target), not report the old cancellation as a fresh
				// failure, and not flush — the new run may be rewriting
				// Messages right now.
				return m, m.drainDeliveries()
			}
			wasUserCancelled = s.compactUserCancelled
			s.compacting = false
			s.compactCancel = nil
			s.compactUserCancelled = false
			if m.lives.Active() == s {
				m.compacting = false
			}
		}
	}
	// Compaction boundary: the compacting strip row returns to the composer
	// (SetSize reserves it while compacting) and the placeholder flips back.
	m.applyComposerPlaceholder()
	m.relayout()
	if msg.agent == nil {
		return m, m.drainDeliveries()
	}
	if msg.err != nil {
		// A user-cancelled compact reports its context error; the cancel
		// key already announced it — don't double-report.
		if !wasUserCancelled {
			if m.streaming {
				// Never append mid-stream (the stream owns the line
				// bookkeeping); the status line carries the failure.
				m.statusMsg = "✗ Compact failed: " + msg.err.Error()
			} else {
				m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Compact failed: %v", msg.err)))
			}
		}
		return m, m.drainDeliveries()
	}
	// Persist the compacted history regardless of focus (the 5 s save
	// debounce could otherwise drop it).
	msg.agent.FlushSession()
	if m.agent != msg.agent || m.streaming {
		// The focused session changed (or a turn started) while
		// compaction ran: the compacted transcript is rebuilt from
		// Messages whenever that session is next focused or the turn
		// ends — rebuilding now would read Messages from under the
		// stream goroutine.
		m.statusMsg = "✓ History compacted"
		return m, m.drainDeliveries()
	}
	m.checkPersistError()
	// Compaction rewrote Messages: rebuild the transcript around the
	// summary (the old inline path left the pre-compact history on
	// screen, which no longer matched the agent's state).
	m.chatLines = renderMessages(m.agent.Messages, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String(), m.verbose)
	m.chatLines = append(m.chatLines, SystemStyle.Render(fmt.Sprintf("History compacted (%d messages remaining).", len(m.agent.Messages))))
	m.setViewportContent()
	m.pinFollow() // the transcript was replaced wholesale
	m.markTranscriptRendered()
	m.refreshContextStats()
	return m, m.drainDeliveries()
}

func (m *Model) handleModelListMsg(msg modelListMsg) (tea.Model, tea.Cmd) {
	if m.agent != msg.agent {
		// The focused session changed while the list was loading; a list
		// from another endpoint would mislead the selector. Surface the
		// outcome instead of silently swallowing the command — this also
		// replaces the "Loading models…" hint, which would otherwise
		// linger until the next keypress.
		m.statusMsg = "Models: focus moved on — reopen /models here"
		return m, nil
	}
	m.statusMsg = "" // the "Loading models…" hint is obsolete either way
	if msg.err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Models: %v", msg.err)))
		return m, nil
	}
	m.modelList = msg.list
	m.modelCursor = 0
	// Stage the current level (web subagent-picker seeding) and drop it
	// when the model under the cursor does not accept it.
	m.thinkingSel = m.agent.ThinkingLevel
	if m.thinkingSel == "" {
		m.thinkingSel = agent.ThinkingOff
	}
	m.modelsModalSyncThinking()
	m.modal = ModalModels
	return m, nil
}

func (m *Model) handleModelSwitchMsg(msg modelSwitchMsg) (tea.Model, tea.Cmd) {
	// A turn is still streaming: rebuilding the transcript here would read
	// Messages from under the stream goroutine AND replace chatLines, so the
	// live assistant/thinking line indices would point at the wrong lines.
	// Mark the transcript stale instead — the turn-end convergence rebuild
	// re-renders it from Messages (the same guard handleCompactResultMsg uses).
	if m.streaming {
		if msg.err != nil {
			m.statusMsg = "✗ Models: " + msg.err.Error()
		} else {
			m.statusMsg = msg.out
		}
		if s := m.turnSession(); s != nil {
			s.transcriptStale = true
		}
		return m, nil
	}
	if m.agent != msg.agent {
		// The switch applied to a session that is no longer focused; its
		// transcript (with the new model line) is rebuilt whenever it is
		// next focused. Report via the status line — appending to
		// chatLines would leak into ANOTHER session's transcript.
		if msg.err != nil {
			m.statusMsg = "✗ Models: " + msg.err.Error()
		} else {
			m.statusMsg = msg.out
		}
		return m, nil
	}
	if msg.err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Models: %v", msg.err)))
		return m, nil
	}
	m.refreshContextStats()
	// Re-render chat lines with the updated model line, THEN append the
	// switch notice — the old order appended first and the rebuild wiped
	// the line, so modal selections silently showed nothing.
	m.chatLines = renderMessages(m.agent.Messages, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String(), m.verbose)
	m.chatLines = append(m.chatLines, SystemStyle.Render(msg.out))
	// Staged thinking level (models modal): apply AFTER the switch so
	// HandleThinkingCommand's validation runs against the new model's
	// accepted set.
	if msg.thinking != "" && string(msg.thinking) != string(m.agent.ThinkingLevel) {
		if out, _ := m.agent.HandleThinkingCommand("/think " + string(msg.thinking)); out != "" {
			m.chatLines = append(m.chatLines, SystemStyle.Render(out))
		}
	}
	m.setViewportContent()
	m.pinFollow() // the transcript was replaced wholesale
	m.markTranscriptRendered()
	return m, nil
}

func (m *Model) handleKeyMsg(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Clear transient status message on any key press
	if m.statusMsg != "" {
		m.statusMsg = ""
	}
	if key.Matches(msg, m.keys.CopySelection) {
		_, cmd := m.copySelection()
		return m, cmd
	}
	// Global hotkeys that work regardless of focus/modal
	switch {
	case key.Matches(msg, m.keys.ForceQuit):
		m.cancelActiveStream()
		m.cancelCompactForQuit()
		m.dismissApproval(false)
		m.flushAndQuit()
		return m, tea.Quit
	case msg.String() == "ctrl+b" && m.modal == ModalNone:
		return m, m.toggleSidebar()
	}
	// NOTE: [ / ] are deliberately NOT global hotkeys — they mean
	// different things per focus (viewport: prompt-rail jump, sidebar:
	// panel resize) and are handled in handleViewportKey /
	// handleSidebarKey. A global intercept here shadowed the viewport's
	// tocJumpNext and made the documented "[ / ] previous/next prompt"
	// keybinding dead.

	// If a modal is active, handle modal keys
	if m.modal != ModalNone {
		return m.handleModalKey(msg)
	}

	// Dispatch based on focus
	if m.focus == FocusViewport {
		return m.handleViewportKey(msg)
	}
	if m.focus == FocusSidebar {
		// Auto-hide (narrow terminal) can leave focus on an invisible
		// panel — fall back to the input instead of swallowing keys.
		if m.sidebarOffsetX() == 0 {
			m.focus = FocusInput
			return m, m.textarea.Focus()
		}
		return m.handleSidebarKey(msg)
	}
	return m.handleInputKey(msg)
}

func (m *Model) handleStreamStartMsg() (tea.Model, tea.Cmd) {
	m.handleStreamStart()
	return m, m.setProgress(progressThinking, "thinking")
}

func (m *Model) handleStreamRoundStartMsg() (tea.Model, tea.Cmd) {
	m.handleStreamRoundStart()
	return m, m.setProgress(progressThinking, "thinking")
}

func (m *Model) handleStreamTokenMsg(msg streamTokenMsg) (tea.Model, tea.Cmd) {
	m.handleStreamToken(msg.token)
	return m, m.setProgress(progressActive, "")
}

func (m *Model) handleStreamThinkingMsg(msg streamThinkingMsg) (tea.Model, tea.Cmd) {
	m.handleStreamThinking(msg.token)
	return m, m.setProgress(progressActive, "")
}

// handleStreamStatsMsg stores the shared SpeedMeter's smoothed token rate
// for the progress line. The message is timer-driven (a few per second
// while the round streams) and always belongs to the focused session —
// background sessions' adapters buffer their events instead of sending
// them — so mirroring onto the focused liveSession keeps a focus switch
// from showing a stale rate.
func (m *Model) handleStreamStatsMsg(msg streamStatsMsg) (tea.Model, tea.Cmd) {
	m.streamSpeedLine = DimStyle.Render(fmt.Sprintf("%.0f tok/s", msg.toksPerSec))
	if s := m.focusedSession(); s != nil {
		s.streamSpeedLine = m.streamSpeedLine
	}
	return m, nil
}

// handleStreamCompactingMsg flips the strip to a compacting indicator while
// the mid-turn summarization runs (a full non-streaming LLM call with no
// stream callbacks). The next progress event — token, stats, round
// boundary, or turn end — replaces the label.
func (m *Model) handleStreamCompactingMsg() (tea.Model, tea.Cmd) {
	return m, m.setProgress(progressCompacting, "compacting history")
}

// handleStreamStallMsg swaps the bare "thinking" spinner for a "still
// waiting" label after a silent stretch (long prefill or a stalled
// upstream). Only meaningful while nothing is flowing: once tokens or tool
// args stream, the phase is active and the stall signal is moot — and while
// a recovery is running (progressRetry) the retry reason already explains the
// silence, so the generic wait must not replace it.
//
// The "is this session streaming at all" half of the old guard is gone:
// handleMsg drops stream-render events for a non-streaming Model before
// dispatch, so only the progress line's own state matters here.
func (m *Model) handleStreamStallMsg() (tea.Model, tea.Cmd) {
	if m.progressPhase != progressThinking || m.progressRetry {
		return m, nil
	}
	return m, m.setProgress(progressThinking, "still waiting on model")
}

// handleStreamRetryMsg relabels the spinner for a muted regeneration
// (streaming retry or the non-streaming fallback): the silent stretch that
// follows is the retry's whole generation, not a hang. Any later progress
// event restores the normal label (setProgress clears the retry mark).
func (m *Model) handleStreamRetryMsg(msg streamRetryMsg) (tea.Model, tea.Cmd) {
	cmd := m.setProgress(progressThinking, "retrying stream ("+msg.reason+")")
	// setProgress cleared the mark for the normal updates; this label IS the
	// retry state, so re-mark it (mirrored for a focus switch).
	m.progressRetry = true
	if s := m.focusedSession(); s != nil {
		s.progressRetry = true
	}
	return m, cmd
}

func (m *Model) handleStreamToolCallMsg(msg streamToolCallMsg) (tea.Model, tea.Cmd) {
	m.handleStreamToolCall(msg.index, msg.id, msg.name)
	return m, m.setProgress(progressActive, "")
}

func (m *Model) handleStreamToolCallArgsMsg(msg streamToolCallArgsMsg) (tea.Model, tea.Cmd) {
	m.handleStreamToolArgs(msg.index, msg.id, msg.delta)
	return m, m.setProgress(progressActive, "")
}

func (m *Model) handleStreamToolCallFinalMsg(msg streamToolCallFinalMsg) (tea.Model, tea.Cmd) {
	m.handleStreamToolCallFinal(msg.index, msg.tc)
	return m, nil
}

func (m *Model) handleStreamToolExecuteMsg(msg streamToolExecuteMsg) (tea.Model, tea.Cmd) {
	label := "running tool"
	if msg.name != "" {
		label = "running " + msg.name
	}
	return m, m.setProgress(progressTool, label)
}

func (m *Model) handleStreamToolResultMsg(msg streamToolResultMsg) (tea.Model, tea.Cmd) {
	m.handleStreamToolResult(msg.id, msg.name, msg.result, msg.success)
	return m, m.setProgress(progressThinking, "thinking")
}

func (m *Model) handleStreamRoundEndMsg() (tea.Model, tea.Cmd) {
	m.handleStreamRoundEnd()
	return m, m.setProgress(progressThinking, "thinking")
}

func (m *Model) handleStreamEndMsg() (tea.Model, tea.Cmd) {
	// Always process – resets streaming state
	m.handleStreamEnd()
	m.bellIfBlurred() // web parity: "Agent finished responding" notification
	return m, tea.Batch(m.refocusInput(), m.drainDeliveries())
}

func (m *Model) handleCondensedNoteMsg(msg condensedNoteMsg) (tea.Model, tea.Cmd) {
	// Last-resort condensation announcement (Phase 0e): a system line in
	// the chat, not part of the LLM history.
	m.appendChatLine(DimStyle.Render("System: " + msg.note))
	return m, nil
}

func (m *Model) handleContextStatsMsg(msg contextStatsMsg) (tea.Model, tea.Cmd) {
	// Optional async refresh (session commands); stream end updates sync.
	// Drop a probe that landed after focus moved on: the request runs
	// off-thread, so a slow probe for the previous session must not
	// overwrite the focused session's indicator.
	if m.agent != nil && msg.sid != "" && msg.sid != m.agent.SessionID {
		return m, nil
	}
	m.contextStats = msg.stats
	m.contextLine = agent.FormatContextBrief(msg.stats)
	return m, nil
}

func (m *Model) handleApprovalRequestMsg(msg approvalRequestMsg) (tea.Model, tea.Cmd) {
	// Stamp the requesting turn's generation NOW (Update thread): the
	// approver closure runs on the stream goroutine and must never read
	// liveSession fields, but turn-end pruning (pruneQueuedApprovals)
	// needs the generation to recognize a dead queued request.
	if msg.ar != nil && msg.ar.sid != "" && m.lives != nil {
		if s := m.lives.ByID(msg.ar.sid); s != nil {
			msg.ar.turnSeq = s.turnSeq
		}
	}
	if m.approvalUI == nil {
		// Take over whatever modal is open (the requesting turn is blocked
		// on the answer); the previous modal's data stays on the Model, so
		// it is restored when the approval queue drains (dismissApproval).
		if m.modal != ModalNone && m.modal != ModalApproval {
			m.modalBeforeApproval = m.modal
		}
		m.approvalUI = &approvalUIState{
			kind:    msg.ar.kindOrDelete(),
			command: msg.ar.command,
			paths:   msg.ar.req.Paths,
			reason:  msg.ar.req.Reason,
			cursor:  1, // default to Yes
			ar:      msg.ar,
		}
		m.modal = ModalApproval
		m.bellIfBlurred() // web parity: "Approval needed" notification
		return m, nil
	}
	// An approval is already on screen: queue this one behind it. Its
	// requester stays blocked on its own reply channel until promoted.
	m.pendingApprovals = append(m.pendingApprovals, msg.ar)
	m.statusMsg = "Another approval is queued…"
	return m, nil
}

func (m *Model) handleMouseMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Border drag-resize, panel clicks, and panel-area events take
	// precedence over text selection; wheel events over the chat fall
	// through to the viewport.
	if ev, ok := normalizeMouseEvent(msg); ok {
		// The models modal's chips/rows are clickable; consume before the
		// panel and selection handlers (a click over the overlay must not
		// start a text selection under it). No-op for every other modal.
		if m.handleModelsModalMouse(ev) {
			return m, nil
		}
		// Any other modal paints an opaque overlay over the whole frame:
		// pointer events must not act underneath it (presses used to start
		// text selections under the overlay, wheel events scrolled the
		// covered chat invisibly, and a border press started a panel
		// drag). Releases still reach the selection handler so an
		// in-flight drag finalizes and scrolling is not left locked after
		// the modal closes.
		if m.modal != ModalNone {
			m.sidebarHovering = false
			m.viewJumpHover = false // the button is not rendered under a modal
			if ev.kind == mouseRelease {
				m.handleMouseSelection(ev)
			}
			return m, nil
		}
		if m.handleSidebarResizeMouse(ev) {
			return m, nil
		}
		if consumed, cmd := m.handleSidebarMouse(ev); consumed {
			// The panel consumed the event: the mouse is over the panel
			// area, so its border renders highlighted.
			m.sidebarHovering = true
			m.viewJumpHover = false // the pointer left the chat column
			return m, cmd
		}
		m.sidebarHovering = false
		// Jump button (the divider label): hover highlight + click-to-jump.
		// Handled before the selection handler so a press on the label
		// never starts a text selection.
		if m.handleJumpButtonMouse(ev) {
			return m, nil
		}
		if m.handleTocMouse(ev) {
			return m, nil
		}
		if m.handleMouseSelection(ev) {
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	// A wheel scroll is a manual scroll: record the follow state so a
	// wheel-up detaches from the live edge (streaming stops re-pinning).
	if _, isWheel := msg.(tea.MouseWheelMsg); isWheel {
		m.viewportScrolledByUser()
	}
	return m, tea.Batch(cmd)
}
