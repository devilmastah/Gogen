package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gogen/internal/llm"
)

// SessionCommandAction tells clients about side effects (e.g. clear chat UI).
type SessionCommandAction string

const (
	SessionActionNone      SessionCommandAction = ""
	SessionActionClearChat SessionCommandAction = "clear_chat"
)

// SessionCommandResult is the outcome of a session slash command.
type SessionCommandResult struct {
	Output   string
	Action   SessionCommandAction
	Sessions []SessionInfo
	History  []llm.Message
}

// HandleSessionCommand processes /new, /resume, and sessions commands.
// newSessionID is required for /new (call session.NewID() from the caller).
func (a *Agent) HandleSessionCommand(ctx context.Context, input, newSessionID string) (SessionCommandResult, bool, error) {
	cmd, args := ParseSessionCommand(input)
	switch cmd {
	case "new":
		out, err := a.startNewSession(newSessionID)
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: AppendContextBrief(ctx, a, out), Action: SessionActionClearChat}, true, nil
	case "sessions", "resume":
		if args != "" {
			return a.handleResumeArg(ctx, args, newSessionID)
		}
		out, sessions, err := a.formatSessionList()
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: out, Sessions: sessions}, true, nil
	case "fork":
		if err := a.ForkSession(ctx, args, newSessionID); err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: AppendContextBrief(ctx, a, fmt.Sprintf("Forked new session %s.", newSessionID)), Action: SessionActionClearChat, History: a.SnapshotMessages()}, true, nil
	}
	return SessionCommandResult{}, false, nil
}

// ParseSessionCommand splits input into (command, args), stripping a leading
// "/". Shared by the agent (TUI) and the multi-session web server so both
// parse slash commands identically.
func ParseSessionCommand(input string) (cmd, args string) {
	trimmed := strings.TrimSpace(input)
	trimmed = strings.TrimPrefix(trimmed, "/")
	parts := strings.SplitN(trimmed, " ", 2)
	cmd = strings.ToLower(parts[0])
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return cmd, args
}

// ResetSessionState clears all session-related state for starting fresh.
// This is used when creating a new session or replacing a deleted current session.
func (a *Agent) ResetSessionState() {
	a.resetSessionState(nil)
}

// resetSessionState is the shared core of ResetSessionState and ForkSession:
// it clears all session-related state and publishes msgs as the new
// conversation in the same sequence (nil for a fresh session, the forked
// prefix for a fork). Keeping the reset list in one place means new state
// added here is covered by both paths automatically.
func (a *Agent) resetSessionState(msgs []llm.Message) {
	a.replaceMessages(msgs)
	a.clearTurnUsage()
	a.resetCompactionState()
	a.statsMu.Lock()
	a.UsageAccum = UsageAccumulator{}
	a.statsMu.Unlock()
	a.resetSaveTracking()
	a.clearViewDriftSnapshot()
	a.setSessionLabel("")
	a.SetSessionOneshot(false)
	if a.PinManager != nil {
		a.PinManager.ClearPins()
	}
	if a.TodoManager != nil {
		a.TodoManager.Clear()
	}
}

// ParseResumeDelArg splits a "/resume del <id>" argument into the id to
// delete. Returns (id, true, nil) for a valid "del <id>" form, (id, false,
// nil) for any other argument (the caller treats it as a plain resume
// target), and ("", false, err) for the malformed "del" / "del " forms. The
// error is the canonical "usage: resume del <id>" so the agent (TUI) and the
// web server surface identical messages.
func ParseResumeDelArg(args string) (string, bool, error) {
	if args == "del" {
		return "", false, fmt.Errorf("usage: resume del <id>")
	}
	if !strings.HasPrefix(args, "del ") {
		return "", false, nil
	}
	delID := strings.TrimSpace(strings.TrimPrefix(args, "del"))
	if delID == "" {
		return "", false, fmt.Errorf("usage: resume del <id>")
	}
	return delID, true, nil
}

// handleResumeArg routes "del", "latest", or session-ID sub-commands.
func (a *Agent) handleResumeArg(ctx context.Context, args, newSessionID string) (SessionCommandResult, bool, error) {
	if delID, ok, err := ParseResumeDelArg(args); ok || err != nil {
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		out, action, err := a.deleteSessionByID(ctx, delID, newSessionID)
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: out, Action: action}, true, nil
	}
	if args == "latest" {
		out, err := a.resumeLatestSession(ctx)
		if err != nil {
			return SessionCommandResult{}, true, err
		}
		return SessionCommandResult{Output: out, Action: SessionActionClearChat, History: a.SnapshotMessages()}, true, nil
	}
	out, err := a.resumeSessionByID(ctx, args)
	if err != nil {
		return SessionCommandResult{}, true, err
	}
	return SessionCommandResult{Output: out, Action: SessionActionClearChat, History: a.SnapshotMessages()}, true, nil
}

func (a *Agent) startNewSession(newID string) (string, error) {
	if strings.TrimSpace(newID) == "" {
		return "", fmt.Errorf("session id is required")
	}
	oldID := a.SessionID
	if a.SessionStore != nil {
		a.FlushSession()
	}
	a.SetSessionID(newID)
	a.ResetSessionState()
	if a.SessionStore != nil {
		a.FlushSession()
		if oldID != "" {
			return fmt.Sprintf("New session %s. Previous session %s saved — use `resume %s` to restore.",
				newID, oldID, oldID), nil
		}
		return fmt.Sprintf("New session %s.", newID), nil
	}
	return "New in-memory session (persistence disabled — history not saved).", nil
}

func (a *Agent) resumeSessionByID(ctx context.Context, id string) (string, error) {
	if a.SessionStore == nil {
		return "", fmt.Errorf("session persistence disabled")
	}
	// Flush the current session only if it has unsaved changes, so we don't
	// write a fresh timestamp on a session that was merely loaded.
	if a.sessionDirty.Load() {
		a.FlushSession()
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("session id is required")
	}
	snap, err := a.SessionStore.LoadInWorkingDir(a.WorkingDir, id)
	if err != nil {
		return "", err
	}
	// Restore locally so the client gets history immediately. Provider
	// validation / context-limit refresh runs in the background (same as
	// process startup) and must not block the resume WS response.
	model := snap.Model
	a.RestoreSession(snap, id)
	// Build the context brief before background model validation so we don't
	// race Snapshot against RefreshAfterModelChange.
	out := AppendContextBrief(ctx, a, ResumeOutputMessage(id, snap.Messages))
	// No OnModelChanged callback: headless hosts have no UI to refresh, and
	// a host that installed its own hook (the TUI root agent) keeps it.
	a.ValidateRestoredModelAsync(model, nil)
	return out, nil
}

// ResumeOutputMessage formats the "Resumed session …" confirmation line for
// the message list in view. Shared by every resume entry point so they
// announce the resume identically: callers pass either a just-loaded
// snapshot's Messages (rebind / TUI) or a live agent's SnapshotMessages (web),
// so the count always matches the label's source.
func ResumeOutputMessage(id string, msgs []llm.Message) string {
	label := llm.SessionLabel(msgs)
	if label != "" {
		return fmt.Sprintf("Resumed session %s (%d messages): \"%s\"", id, len(msgs), label)
	}
	return fmt.Sprintf("Resumed session %s (%d messages).", id, len(msgs))
}

// ResolveResumeTarget resolves a /resume argument to a concrete session id
// WITHOUT performing the resume: a plain id passes through (trimmed), and
// "latest" resolves to the newest saved top-level session other than the
// current one — the same rule resumeLatestSession applies, factored out so
// hosts can route an already-live target to a focus switch instead of
// rebinding a second agent onto the same SessionID (two agents with one
// SessionID both persist to the same file; the web dedupes against its
// registry in loadOrCreateRuntime for exactly this). "del" arguments are
// not resume targets: callers filter them via ParseResumeDelArg first.
func (a *Agent) ResolveResumeTarget(args string) (string, error) {
	if a.SessionStore == nil {
		return "", fmt.Errorf("session persistence disabled")
	}
	args = strings.TrimSpace(args)
	if args == "latest" {
		list, err := a.SessionStore.List(a.WorkingDir)
		if err != nil {
			return "", err
		}
		if len(list) == 0 {
			return "", fmt.Errorf("no saved sessions")
		}
		// Nested (subagent) sessions are not part of the flat list:
		// "resume latest" must never target a child (they are reachable
		// only through their parent's sidebar row, D2).
		for _, s := range list {
			if s.ID != a.SessionID && s.ParentID == "" {
				return s.ID, nil
			}
		}
		return "", fmt.Errorf("no other saved sessions to resume")
	}
	if args == "" {
		return "", fmt.Errorf("session id is required")
	}
	return args, nil
}

func (a *Agent) resumeLatestSession(ctx context.Context) (string, error) {
	target, err := a.ResolveResumeTarget("latest")
	if err != nil {
		return "", err
	}
	return a.resumeSessionByID(ctx, target)
}

func (a *Agent) deleteSessionByID(ctx context.Context, id, newSessionID string) (string, SessionCommandAction, error) {
	if a.SessionStore == nil {
		return "", SessionActionNone, fmt.Errorf("session persistence disabled")
	}
	if strings.TrimSpace(id) == "" {
		return "", SessionActionNone, fmt.Errorf("session id is required")
	}
	wasCurrent := id == a.SessionID
	// Validate the replacement id BEFORE the destructive delete: returning
	// the error afterwards would leave the agent on a session whose file is
	// already gone, and the next flush would resurrect it.
	if wasCurrent && strings.TrimSpace(newSessionID) == "" {
		return "", SessionActionNone, fmt.Errorf("session id is required")
	}
	if err := a.SessionStore.Delete(a.WorkingDir, id); err != nil {
		return "", SessionActionNone, err
	}
	if wasCurrent {
		a.SetSessionID(newSessionID)
		a.ResetSessionState()
		a.FlushSession()
		out := fmt.Sprintf("Deleted session %s (was current — started new session %s).", id, newSessionID)
		return AppendContextBrief(ctx, a, out), SessionActionClearChat, nil
	}
	return fmt.Sprintf("Deleted session %s.", id), SessionActionNone, nil
}

func (a *Agent) formatSessionList() (string, []SessionInfo, error) {
	if a.SessionStore == nil {
		return "Session persistence is disabled.", nil, nil
	}
	list, err := a.SessionStore.List(a.WorkingDir)
	if err != nil {
		return "", nil, err
	}
	// Nested (subagent) sessions are excluded from the flat /resume list:
	// they are only reachable through their parent's sidebar row (D2).
	// A fresh slice (not list[:0]): the caller may keep using `list`
	// afterwards, and reusing the backing array would silently corrupt it.
	flat := make([]SessionInfo, 0, len(list))
	for _, s := range list {
		if s.ParentID == "" {
			flat = append(flat, s)
		}
	}
	if len(flat) == 0 {
		return "No saved sessions.", flat, nil
	}
	var b strings.Builder
	b.WriteString("Saved sessions:\n")
	for _, s := range flat {
		fmt.Fprintf(&b, "  %s  (%d msgs)", s.ID, s.MessageCount)
		if s.Label != "" {
			fmt.Fprintf(&b, "  \"%s\"", s.Label)
		}
		if s.ID == a.SessionID {
			b.WriteString("  ← current")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nUse: resume <id>  |  resume latest  |  resume del <id>")
	return b.String(), flat, nil
}

// FormatSessionListForUI returns saved sessions without the slash-command help text.
func (a *Agent) FormatSessionListForUI() (string, []SessionInfo, error) {
	return a.formatSessionList()
}

// SessionListAll returns every saved session for the agent's working dir —
// INCLUDING nested (subagent) sessions. The web sidebar uses it so persisted
// children render under their parent row after a page reload / late attach
// (subagent_started/finished events are not replayed to connecting clients).
// The flat /resume list (formatSessionList) keeps excluding them.
func (a *Agent) SessionListAll() ([]SessionInfo, error) {
	if a.SessionStore == nil {
		return nil, nil
	}
	return a.SessionStore.List(a.WorkingDir)
}

// ForkMessages returns a copy of messages up to (and including) the fork
// point selected by args, without mutating any agent. args can be:
//   - "" or "last": fork from the last assistant message that produced output
//     (ghosts — reasoning-only or fully-empty assistant turns — are skipped)
//   - "<N>": fork from the Nth message (0-indexed raw index)
//   - "assistant <N>": fork from the Nth assistant message (0-indexed)
//   - "created <RFC3339Nano>": fork from message with the given CreatedAt
//
// Invisible assistant messages (no content, no refusal, no tool calls) are
// never used as a fork point: explicit index forks walk back to the nearest
// visible message, and stripping tool calls from a tool-call-only fork point
// drops the resulting empty message instead of leaving a ghost in the forked
// history. The multi-session web server uses this to fork into a NEW agent
// while leaving the source session untouched.
func ForkMessages(messages []llm.Message, args string) ([]llm.Message, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to fork from")
	}

	// isInvisibleAssistant reports whether an assistant message carries no
	// user-visible output (no content, no refusal, no tool calls). Such
	// messages — truncated reasoning-only turns, or ghosts left behind by
	// stripping tool calls — render as nothing in the UI, cannot be forked
	// from, and are invalid as the last message of a forked session.
	isInvisibleAssistant := func(m llm.Message) bool {
		return m.Role == "assistant" && m.Content == "" && m.Refusal == "" && len(m.ToolCalls) == 0
	}

	spec, err := parseForkArg(args, len(messages))
	if err != nil {
		return nil, err
	}
	idx, err := findForkIndex(messages, spec, isInvisibleAssistant)
	if err != nil {
		return nil, err
	}

	// Never fork from an invisible assistant message (a truncated
	// reasoning-only turn or a fully-empty ghost): walk back to the nearest
	// visible message so the forked session ends on a meaningful turn.
	for idx > 0 && isInvisibleAssistant(messages[idx]) {
		idx--
	}
	if isInvisibleAssistant(messages[idx]) {
		return nil, fmt.Errorf("no visible message found to fork from")
	}

	// Copy messages up to and including the fork point.
	forkedMsgs := append([]llm.Message(nil), messages[:idx+1]...)

	// If the fork point is an assistant message with tool calls, strip the
	// tool calls from it so the forked session doesn't have orphaned
	// tool calls with no corresponding results.
	if forkedMsgs[idx].Role == "assistant" && len(forkedMsgs[idx].ToolCalls) > 0 {
		forkedMsgs[idx].ToolCalls = nil
		// Stripping tool calls from a tool-call-only message (empty content,
		// empty refusal) leaves a fully-empty assistant message behind. Drop
		// any trailing empty assistant messages so the forked session doesn't
		// start with a ghost turn.
		for len(forkedMsgs) > 0 && isInvisibleAssistant(forkedMsgs[len(forkedMsgs)-1]) {
			forkedMsgs = forkedMsgs[:len(forkedMsgs)-1]
		}
		if len(forkedMsgs) == 0 {
			return nil, fmt.Errorf("cannot fork from an empty assistant message")
		}
	}

	return forkedMsgs, nil
}

// forkKind identifies which fork selector parseForkArg resolved.
type forkKind int

const (
	forkLast forkKind = iota
	forkCreated
	forkAssistant
	forkIndex
)

// forkSpec is a parsed /fork argument.
type forkSpec struct {
	kind forkKind
	// created is the parsed timestamp for forkCreated; createdRaw keeps the
	// caller's original (trimmed) text for error messages.
	created    time.Time
	createdRaw string
	assistant  int // forkAssistant: the Nth assistant message (0-indexed)
	index      int // forkIndex: raw message index
}

// parseForkArg parses a /fork argument: "last" (or empty), "created
// <RFC3339Nano>", "assistant <N>", or a raw message index. The raw index is
// validated against messageCount so out-of-range values fail here with the
// usage error; the other selectors validate during the search.
func parseForkArg(args string, messageCount int) (forkSpec, error) {
	args = strings.TrimSpace(args)
	switch {
	case args == "" || args == "last":
		return forkSpec{kind: forkLast}, nil
	case strings.HasPrefix(args, "created "):
		ts := strings.TrimSpace(strings.TrimPrefix(args, "created "))
		createdAt, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return forkSpec{}, fmt.Errorf("invalid timestamp %q: %v", ts, err)
		}
		return forkSpec{kind: forkCreated, created: createdAt, createdRaw: ts}, nil
	case strings.HasPrefix(args, "assistant "):
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(args, "assistant ")))
		if err != nil {
			return forkSpec{}, fmt.Errorf("usage: fork [assistant <N>] — invalid number: %v", err)
		}
		return forkSpec{kind: forkAssistant, assistant: n}, nil
	default:
		// Raw index into the Messages array (matches histIdx from the client).
		n, err := strconv.Atoi(args)
		if err != nil || n < 0 || n >= messageCount {
			return forkSpec{}, fmt.Errorf("usage: fork [<N> | last | assistant <N> | created <timestamp>] — invalid index %q", args)
		}
		return forkSpec{kind: forkIndex, index: n}, nil
	}
}

// findForkIndex resolves a parsed fork spec to a message index.
// isInvisibleAssistant excludes ghost assistant messages from "last" forks
// (the explicit-index walk-back stays with the caller).
func findForkIndex(messages []llm.Message, spec forkSpec, isInvisibleAssistant func(llm.Message) bool) (int, error) {
	switch spec.kind {
	case forkLast:
		// Fork from the last assistant message
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "assistant" && !isInvisibleAssistant(messages[i]) {
				return i, nil
			}
		}
		return -1, fmt.Errorf("no assistant message found to fork from")
	case forkCreated:
		for i, m := range messages {
			diff := m.CreatedAt.Sub(spec.created)
			if diff < 0 {
				diff = -diff
			}
			if diff <= time.Millisecond {
				return i, nil
			}
		}
		return -1, fmt.Errorf("message with timestamp %q not found", spec.createdRaw)
	case forkAssistant:
		count := 0
		for i, m := range messages {
			if m.Role == "assistant" {
				if count == spec.assistant {
					return i, nil
				}
				count++
			}
		}
		return -1, fmt.Errorf("assistant message %d not found (only %d assistant messages)", spec.assistant, count)
	default: // forkIndex
		return spec.index, nil
	}
}

// ForkSession forks the current conversation into a NEW session: the current
// session is saved first, then the agent's messages are replaced with the
// prefix up to the fork point selected by args (see ForkMessages for the
// accepted forms), and the new session is persisted under newSessionID
// (required; call session.NewID() from the caller).
func (a *Agent) ForkSession(ctx context.Context, args, newSessionID string) error {
	if strings.TrimSpace(newSessionID) == "" {
		return fmt.Errorf("session id is required")
	}
	if len(a.Messages) == 0 {
		return fmt.Errorf("no messages to fork from")
	}

	forkedMsgs, err := ForkMessages(a.Messages, args)
	if err != nil {
		return err
	}
	// The forked transcript copies the original's spill locator lines;
	// re-point them at the child's spill dir (hardlinks) so retrieval keeps
	// working after the original session is deleted (delete removes the
	// original's spill files).
	a.RepointSpillLocators(forkedMsgs, newSessionID)

	// Save current session (the original branch)
	if a.SessionStore != nil {
		a.FlushSession()
	}

	oldSessionID := a.SessionID

	// Start new session with the truncated history
	a.SetSessionID(newSessionID)
	a.resetSessionState(forkedMsgs)

	// Persist new session
	if a.SessionStore != nil {
		a.FlushSession()
		_ = a.SessionStore.TouchSession(a.WorkingDir, oldSessionID)
	}

	return nil
}
