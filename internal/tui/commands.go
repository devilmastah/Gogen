package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/onoff"
	"gogen/internal/session"

	tea "charm.land/bubbletea/v2"
)

// dispatchCommand handles slash commands and other special inputs.
// Returns (handled, quit, tea.Cmd).
func (m *Model) dispatchCommand(input string) (bool, bool, tea.Cmd) {
	trimmed := strings.TrimSpace(input)
	for _, cmd := range tuiCommands {
		if cmd.match(input, trimmed) {
			return cmd.run(m, input, trimmed)
		}
	}
	return false, false, nil
}

// tuiCommand is one slash-command family handled by dispatchCommand.
// Matchers are evaluated in table order (the original if/else order).
type tuiCommand struct {
	// match is a PURE string check (no side effects). The agent-delegated
	// families must accept exactly the inputs their agent handler accepts —
	// the handler runs only inside run — so commands cannot silently change
	// from handled to prompt-sent (or vice versa).
	match func(input, trimmed string) bool
	run   func(m *Model, input, trimmed string) (bool, bool, tea.Cmd)
}

// exactAny matches any of the given exact trimmed forms.
func exactAny(forms ...string) func(input, trimmed string) bool {
	return func(input, trimmed string) bool {
		for _, f := range forms {
			if trimmed == f {
				return true
			}
		}
		return false
	}
}

var tuiCommands = []tuiCommand{
	{match: exactAny("help", "/help"), run: cmdHelp},
	{match: exactAny("exit", "/exit", "quit", "/quit"), run: cmdExit},
	{match: exactAny("compact", "/compact"), run: cmdCompact},
	{match: func(input, trimmed string) bool {
		return trimmed == "smartcompact" || trimmed == "/smartcompact" ||
			strings.HasPrefix(trimmed, "smartcompact ") || strings.HasPrefix(trimmed, "/smartcompact ")
	}, run: cmdSmartCompact},
	{match: func(input, trimmed string) bool {
		return trimmed == "contextmode" || trimmed == "/contextmode" ||
			strings.HasPrefix(trimmed, "contextmode ") || strings.HasPrefix(trimmed, "/contextmode ")
	}, run: cmdContextMode},
	{match: exactAny("verbose", "/verbose"), run: cmdVerbose},
	{match: exactAny("subagents", "/subagents"), run: cmdSubagents},
	// Mode commands: mirror HandleModeCommand's accepted forms.
	{match: exactAny("plan", "/plan", "act", "/act", "mode", "/mode"), run: cmdMode},
	// Thinking command: mirror HandleThinkingCommand's accepted forms.
	{match: func(input, trimmed string) bool {
		return trimmed == "think" || trimmed == "/think" ||
			strings.HasPrefix(trimmed, "think ") || strings.HasPrefix(trimmed, "/think ")
	}, run: cmdThinking},
	{match: exactAny("context", "/context"), run: cmdContext},
	// Session commands: reuse the agent's pure parser, mirroring its cases.
	{match: func(input, trimmed string) bool {
		cmd, _ := agent.ParseSessionCommand(input)
		switch cmd {
		case "new", "sessions", "resume", "fork":
			return true
		}
		return false
	}, run: cmdSession},
	// save-config matches the RAW input (trailing whitespace matters),
	// preserving the original exact-string check.
	{match: func(input, trimmed string) bool {
		return input == "/save-config" || input == "save-config"
	}, run: cmdSaveConfig},
	// Models command: reuse the agent's pure parser.
	{match: func(input, trimmed string) bool {
		_, ok := agent.ParseModelsCommand(input)
		return ok
	}, run: cmdModels},
	{match: func(input, trimmed string) bool { return strings.HasPrefix(trimmed, "dir ") }, run: cmdDir},
	// Switch between actively hosted (live) sessions by number — the
	// engine behind the future sessions-sidebar ACTIVE section.
	{match: func(input, trimmed string) bool {
		return trimmed == "switch" || trimmed == "/switch" ||
			strings.HasPrefix(trimmed, "switch ") || strings.HasPrefix(trimmed, "/switch ")
	}, run: cmdSwitch},
	// Open a NEW live session through the shared web lifecycle core; it
	// becomes the focused session and appears in /switch's list.
	{match: func(input, trimmed string) bool {
		return trimmed == "open" || trimmed == "/open" ||
			strings.HasPrefix(trimmed, "open ") || strings.HasPrefix(trimmed, "/open ")
	}, run: cmdOpen},
	// /queue opens the queued-messages modal (per-item cancel). Reachable
	// mid-turn too — handleSubmitKey intercepts it before the busy
	// command refusal.
	{match: func(input, trimmed string) bool {
		return trimmed == "queue" || trimmed == "/queue"
	}, run: cmdQueue},
}

// cmdOpen handles /open [label]: spawn an additional live session via the
// shared workspace factory and focus it immediately (the web sidebar's
// "New" button, as a command).
func cmdOpen(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	label := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/open"), "open"))
	if m.lives == nil {
		m.appendChatLine(ErrorStyle.Render("Open: session hosting unavailable."))
		return true, false, nil
	}
	cmd := m.openNewLiveSession(label)
	m.appendChatLine(SystemStyle.Render("Switch back with /switch or the sessions panel."))
	return true, false, cmd
}

// cmdSwitch handles /switch <n>: focus another live session, leaving any
// running turn there in the background.
func cmdSwitch(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/switch"), "switch"))
	if m.lives == nil {
		m.appendChatLine(ErrorStyle.Render("Switch: only one session is hosted."))
		return true, false, nil
	}
	if arg == "" {
		// Interactive: the ACTIVE-sessions browser (same registry the
		// future sidebar renders).
		m.liveCursor = m.lives.active
		m.modal = ModalLiveSessions
		return true, false, nil
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(m.lives.sessions) {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Switch: invalid session %q (1-%d)", arg, len(m.lives.sessions))))
		return true, false, nil
	}
	i := n - 1
	if i == m.lives.active {
		m.appendChatLine(SystemStyle.Render("Already focused."))
		return true, false, nil
	}
	label := m.lives.sessions[i].label
	cmd := m.switchToLive(i)
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Switched to session: %s", label)))
	return true, false, cmd
}

// cmdSubagents opens the nested-session (subagent) list modal.
func cmdSubagents(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.modal = ModalSubagents
	return true, false, nil
}

func cmdHelp(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.modal = ModalHelp
	return true, false, nil
}

func cmdExit(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.flushAndQuit()
	return true, true, tea.Quit
}

func cmdCompact(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	if m.agent == nil {
		return true, false, nil
	}
	if m.streaming {
		m.appendChatLine(ErrorStyle.Render("Compact: wait for the current turn to finish (or cancel it first)."))
		return true, false, nil
	}
	if m.compacting {
		m.appendChatLine(ErrorStyle.Render("Compact: already running — cancel it with ctrl+c."))
		return true, false, nil
	}
	// CompactHistory makes an LLM summarization call; run it off the
	// Update thread so the UI stays responsive (the old inline call froze
	// the whole TUI for the request's duration). The result arrives as
	// compactResultMsg.
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	// The flags live on the FOCUSED live session (per-session compaction,
	// mirroring the streaming pattern); m.compacting is its mirror.
	sess := m.turnSession()
	sess.compacting = true
	sess.compactUserCancelled = false
	sess.compactCancel = cancel
	// Bump the generation BEFORE the goroutine starts: the result carries
	// the seq, so a later cancel + restart supersedes it cleanly (see
	// liveSession.compactSeq).
	sess.compactSeq++
	seq := sess.compactSeq
	m.compacting = true
	m.applyComposerPlaceholder()
	m.relayout()
	a := m.agent
	compactCmd := func() tea.Msg {
		defer cancel()
		err := a.CompactHistory(ctx)
		return compactResultMsg{agent: a, err: err, seq: seq}
	}
	return true, false, tea.Batch(m.spinner.Tick, compactCmd)
}

func cmdSmartCompact(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	fields := strings.Fields(trimmed)
	var want *bool
	if len(fields) >= 2 {
		on, ok := onoff.Parse(fields[1])
		if !ok {
			m.appendChatLine(SystemStyle.Render("Smart compact: use on or off"))
			return true, false, nil
		}
		want = &on
	}
	if m.agent == nil || m.agent.Context == nil {
		m.appendChatLine(SystemStyle.Render("Smart compact is unavailable in this session"))
		return true, false, nil
	}
	next := !m.agent.Context.SmartCompact()
	if want != nil {
		next = *want
	}
	state := "off"
	if next {
		state = "on"
	}
	apply := func(a *agent.Agent) {
		if a != nil {
			a.SetSmartCompact(next)
		}
	}
	if m.lives != nil {
		for _, sess := range m.lives.sessions {
			if sess != nil {
				apply(sess.agent)
			}
		}
	}
	apply(m.agent)
	if m.cfg != nil {
		m.cfg.SmartCompact = state
	}
	if m.workspace != nil {
		cfg := m.workspace.GetRuntimeConfig()
		cfg.SmartCompact = state
		m.workspace.SetRuntimeConfig(cfg)
	}
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Smart compact: %s (all live sessions; /save-config stores it)", state)))
	return true, false, nil
}

func cmdContextMode(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	if m.agent == nil || m.agent.Context == nil {
		m.appendChatLine(SystemStyle.Render("Context mode is unavailable in this session"))
		return true, false, nil
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		mode := config.DefaultContextMode
		if m.agent.Context.DeepDive() {
			mode = config.ContextModeDeepDive
		}
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Context mode: %s (use /contextmode %s or %s)", mode, config.DefaultContextMode, config.ContextModeDeepDive)))
		return true, false, nil
	}
	mode := strings.ToLower(fields[1])
	if mode != config.DefaultContextMode && mode != config.ContextModeDeepDive {
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Context mode: use %s or %s", config.DefaultContextMode, config.ContextModeDeepDive)))
		return true, false, nil
	}
	deepDive := mode == config.ContextModeDeepDive
	apply := func(a *agent.Agent) {
		if a != nil {
			a.SetDeepDive(deepDive)
		}
	}
	if m.lives != nil {
		for _, sess := range m.lives.sessions {
			if sess != nil {
				apply(sess.agent)
			}
		}
	}
	apply(m.agent)
	if m.cfg != nil {
		m.cfg.ContextMode = mode
	}
	if m.workspace != nil {
		cfg := m.workspace.GetRuntimeConfig()
		cfg.ContextMode = mode
		m.workspace.SetRuntimeConfig(cfg)
	}
	note := ""
	if deepDive && !m.agent.Context.AutoCompactEnabled() {
		note = "; auto-compaction is off, so the prompt keeps the default context advice"
	}
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Context mode: %s (all live sessions; /save-config stores it%s)", mode, note)))
	return true, false, nil
}

func cmdVerbose(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	m.verbose = !m.verbose
	state := "off"
	if m.verbose {
		state = "on"
	}
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Verbose tool output: %s", state)))
	return true, false, nil
}

func cmdMode(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	out, _ := m.agent.HandleModeCommand(input)
	if trimmed == "/plan" || trimmed == "plan" || trimmed == "/act" || trimmed == "act" {
		m.chatLines = renderMessages(m.agent.Messages, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String(), m.verbose)
		m.chatLines = append(m.chatLines, SystemStyle.Render(out))
		m.setViewportContent()
		m.pinFollow() // the transcript was replaced wholesale
		m.markTranscriptRendered()
	} else {
		m.appendChatLine(SystemStyle.Render(out))
	}
	// SetMode persists the session.
	m.checkPersistError()
	return true, false, nil
}

func cmdThinking(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	out, _ := m.agent.HandleThinkingCommand(input)
	m.appendChatLine(SystemStyle.Render(out))
	m.checkPersistError()
	return true, false, nil
}

func cmdContext(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	out, _ := m.agent.HandleContextCommand(m.ctx, input)
	m.appendChatLine(SystemStyle.Render(out))
	m.checkPersistError()
	return true, false, nil
}

func cmdSession(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	// /resume <target> (and the "sessions <target>" alias) routes through
	// resumeSavedRow so every TUI resume entry point shares the same
	// guards: a target still hosted in a background slot is FOCUSED
	// (switchToLive) instead of rebound onto a second agent — two agents
	// with one SessionID both persist to the same file and the rebound
	// agent's first full save wipes the other's pending delta (partial
	// restore on the next load; the web dedupes in loadOrCreateRuntime).
	// "del" and malformed arguments keep going through the agent for the
	// canonical usage errors.
	if cmd, args := agent.ParseSessionCommand(input); (cmd == "resume" || cmd == "sessions") && args != "" {
		if _, isDel, perr := agent.ParseResumeDelArg(args); !isDel && perr == nil {
			target, rerr := m.agent.ResolveResumeTarget(args)
			if rerr != nil {
				m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", rerr)))
				return true, false, nil
			}
			return true, false, m.resumeSavedRow(target)
		}
	}
	result, _, err := m.agent.HandleSessionCommand(m.ctx, input, session.NewID())
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
		// Errors from startNewSession/deleteSessionByID may surface a
		// half-completed persist; check anyway.
		m.checkPersistError()
		return true, false, nil
	}
	if result.Action == agent.SessionActionClearChat {
		// "resume del <current>" mutates the store on this early-return
		// path (unlike /new and plain resume, which do not); the refresh
		// returned by applySessionSwitch keeps the deleted row from
		// lingering in the sidebar until the next 30 s tick.
		return true, false, m.applySessionSwitch(result)
	} else if result.Sessions != nil {
		// Show session list modal
		m.sessionList = result.Sessions
		m.sessionCursor = 0
		m.modal = ModalSessions
	} else {
		m.appendChatLine(SystemStyle.Render(result.Output))
		if len(result.History) > 0 {
			m.chatLines = renderMessages(result.History, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String(), m.verbose)
			m.setViewportContent()
			m.pinFollow() // restored history starts at the live edge
			m.markTranscriptRendered()
		}
	}
	// Session commands (new, resume, delete) persist.
	m.checkPersistError()
	// The store index just changed — refresh the sidebar's unified list
	// off the Update thread.
	return true, false, m.requestSavedSessions()
}

// applySessionSwitch applies the TUI session-switch epilogue shared by the
// /session command path (cmdSession) and the sidebar resume path
// (resumeSavedRow): it clears the chat, appends the notice line plus the
// rendered history, scrolls the viewport to the bottom, re-stamps the
// session's recency (rebindActivity), moves the panel cursor to the focused
// session's row (the rebind changed which session is current — without this
// the selection stayed on the saved-and-left-behind session after /new),
// and returns the off-thread refresh cmds — a cold token-count cache probe
// (a synchronous probe would tokenize the whole history on the render loop)
// and a sidebar refresh. Callers own their per-entry-point extras
// (persist-error check) and return arity.
func (m *Model) applySessionSwitch(result agent.SessionCommandResult) tea.Cmd {
	m.chatLines = nil
	m.chatLines = append(m.chatLines, SystemStyle.Render(result.Output))
	if len(result.History) > 0 {
		m.chatLines = append(m.chatLines, renderMessages(result.History, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String(), m.verbose)...)
	}
	m.setViewportContent()
	m.pinFollow() // fresh transcript (the focused session changed)
	m.markTranscriptRendered()
	m.sessionID = m.agent.SessionID
	// /new and /resume <id> rebind the focused agent: re-stamp the
	// session's recency so the list does not reorder (see rebindActivity).
	m.rebindActivity()
	// The rebind changed the focused session: move the panel cursor to its
	// row (identity recorded), so /new selects the fresh session instead of
	// leaving the highlight on the saved-and-left-behind one — the same
	// sync switchToLive does for the focus-switch paths.
	m.sidebarCursor = m.sidebarFocusedRow()
	return tea.Batch(m.requestContextStats(), m.requestSavedSessions())
}

func cmdSaveConfig(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	if err := m.saveConfig(false); err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Save config failed: %v", err)))
	}
	return true, false, nil
}

func cmdModels(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	if m.agent == nil {
		return true, false, nil
	}
	// Both paths hit the network (the provider /models endpoint, and the
	// context-limit probe inside SelectModel); run them off the Update
	// thread so the UI stays responsive during the round trip. The old
	// no-arg path even made the list call twice (HandleModelsCommand +
	// ListModels); the split below makes exactly one.
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/models"), "models"))
	a := m.agent
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	if arg == "" {
		// If no selector, show interactive modal (on modelListMsg).
		m.statusMsg = "Loading models…"
		return true, false, func() tea.Msg {
			list, err := a.ListModels(base)
			return modelListMsg{agent: a, list: list, err: err}
		}
	}
	// Has selector — do the inline switch in the background.
	return true, false, func() tea.Msg {
		out, _, err := a.HandleModelsCommand(base, input)
		return modelSwitchMsg{agent: a, out: out, err: err}
	}
}

func cmdDir(m *Model, input, trimmed string) (bool, bool, tea.Cmd) {
	newDir := strings.TrimSpace(strings.TrimPrefix(trimmed, "dir "))
	absDir, err := filepath.Abs(newDir)
	if err != nil || !agent.DirExists(absDir) {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Error: directory does not exist: %s", newDir)))
	} else {
		m.agent.SetWorkingDir(absDir)
		m.agent.AfterWorkingDirChange()
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Changed working directory to: %s", absDir)))
	}
	m.checkPersistError()
	return true, false, nil
}

func (m *Model) saveConfig(includeSecrets bool) error {
	cfgPath, guidelinesPath, err := m.agent.SaveConfig(m.cfg, includeSecrets)
	if err != nil {
		return err
	}
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Wrote config to %s", cfgPath)))
	if guidelinesPath != "" {
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Wrote guidelines to %s", guidelinesPath)))
	}
	m.appendChatLine(SystemStyle.Render("Note: environment variables still override file values at runtime."))
	return nil
}

// deliveryRequestMsg requests a system message delivery (job completion
// notice, scheduled reminder, subagent report). Producers send it via
// m.program.Send from arbitrary goroutines; Update appends it to the
// target session's steering queue and drains when that session is idle, so
// Messages is only ever touched on the Update thread (via the streamCmd
// goroutine, the same ownership contract as user input).
type deliveryRequestMsg struct {
	// sid is the routing id of the session the notice belongs to (the
	// producer's own slot — job notices are per-session). Update resolves
	// it so a focus switch between the hook firing (background goroutine)
	// and this handler cannot land the notice on another session; empty
	// falls back to the focused session (direct constructions, tests).
	sid  string
	text string
}

// startTurn runs an agent turn on text for the FOCUSED session, rendering
// chatLine as the user-facing chat entry and wiring the per-session
// streaming state. chatLine is passed pre-rendered because the entry
// points style it differently (UserStyle label + plain text vs a fully
// SystemStyle line). Callers must ensure !m.streaming; the per-session
// wiring itself lives in startTurnOn (steering.go), which also serves
// background sessions' queue drains.
func (m *Model) startTurn(text, chatLine string) tea.Cmd {
	return m.startTurnWithResult(text, chatLine, nil)
}

// startTurnWithResult is startTurn plus the turn's exact-result callback (see
// startTurnOnWithResult): used by a focused child's targeted delivery, whose
// reply must reach the parent verbatim.
func (m *Model) startTurnWithResult(text, chatLine string, onResult func(string, error)) tea.Cmd {
	// Submitting is an explicit jump to the live edge: the user's own line
	// (and the streaming reply) must be visible even if they were reading
	// history when they hit enter.
	m.pinFollow()
	m.appendChatLine(chatLine)
	m.streaming = true
	m.resetStreamState(false)
	m.applyComposerPlaceholder()
	startProgress := m.setProgress(progressThinking, "thinking")
	m.relayout()
	return tea.Batch(startProgress, m.startTurnOnWithResult(m.turnSession(), text, onResult))
}

// drainDeliveries starts the next queued item (system delivery or user
// steering message — one FIFO queue per session, see steering.go) when the
// FOCUSED session is idle. Returns nil when nothing is queued or a turn
// (or an async /compact) is running; the next item drains at the next
// terminal boundary. Background sessions drain their own queues at their
// own turn boundaries (handleTurnFinishedMsg).
func (m *Model) drainDeliveries() tea.Cmd {
	if m.streaming || m.compacting {
		return nil
	}
	if m.deliveryDrops > 0 {
		// Report overflow drops only at a drain boundary (never mid-stream,
		// where an appended line would interleave with the stream's line
		// bookkeeping).
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("%d background message(s) dropped (delivery queue full).", m.deliveryDrops)))
		m.deliveryDrops = 0
	}
	return m.drainSessionQueue(m.focusedSession())
}

// submitUserInput sends user input to the agent for processing.
func (m *Model) submitUserInput(input string) tea.Cmd {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil
	}

	// Update history
	m.appendInputHistory(trimmed)

	// Show user message in chat and start the turn.
	return m.startTurn(trimmed, UserStyle.Render(userLabel)+" "+trimmed)
}

// makeDeleteApprover creates a delete approver that shows a modal.
// sessID attributes the request to its live session so the modal can name
// a background requester.
//
// The closure runs on the turn's stream goroutine (tool execution), so it
// must not touch Model state — it only sends the request to the Update
// thread (program.Send is thread-safe) and blocks on its OWN reply
// channel. The per-request channel replaces the old shared channel +
// in-flight flag, which raced the Update thread's dismissApproval and let
// concurrent approvals steal each other's answers.
func (m *Model) makeDeleteApprover(sessID string, send programSender) agent.DeleteApprover {
	return func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		ar := &approvalRequest{
			req:   req,
			sid:   sessID,
			reply: make(chan bool, 1),
		}
		// Show the approval modal via Bubble Tea. The Update thread queues
		// concurrent requests (handleApprovalRequestMsg) and replies on
		// ar.reply when the user answers or the turn unwinds.
		if send != nil {
			send.Send(approvalRequestMsg{ar: ar})
		}
		// Wait for the result on this request's own channel.
		select {
		case approved := <-ar.reply:
			return approved, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// makeCommandApprover creates a command approver that shows the approval
// modal for a blocked shell command the guard escalated. Like the delete
// approver it runs on the turn's stream goroutine, so it never touches
// Model state — it sends the request to the Update thread and blocks on its
// own reply channel.
func (m *Model) makeCommandApprover(sessID string, send programSender) agent.CommandApprover {
	return func(ctx context.Context, req agent.CommandRequest) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		ar := &approvalRequest{
			kind:    "command",
			command: req.Command,
			req:     agent.DeleteRequest{Reason: req.Reason},
			sid:     sessID,
			reply:   make(chan bool, 1),
		}
		if send != nil {
			send.Send(approvalRequestMsg{ar: ar})
		}
		select {
		case approved := <-ar.reply:
			return approved, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// approvalRequestMsg is an internal message to trigger the approval modal.
type approvalRequestMsg struct {
	ar *approvalRequest
}
