package agent

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// SubagentLabel derives the child session's sidebar title from the job.
// Shared by the web and TUI spawners so labels cannot drift between modes.
func SubagentLabel(job string) string {
	first := job
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	// Rune-safe cut: slicing on bytes could split a multi-byte character
	// and inject invalid UTF-8 into the sidebar label.
	if r := []rune(first); len(r) > 60 {
		first = string(r[:60]) + "…"
	}
	if first == "" {
		return "subagent"
	}
	return "subagent: " + first
}

// MaxSubagentReportBytes caps the report returned to the parent tool call
// (mirrors the general tool-result truncation spirit; the child's full
// transcript stays inspectable via its pane).
const MaxSubagentReportBytes = 64 * 1024

// TruncateSubagentReport bounds a subagent's final report to
// MaxSubagentReportBytes. Shared by the web and TUI spawners.
func TruncateSubagentReport(report string) string {
	return contextmgr.Truncate(report, MaxSubagentReportBytes,
		contextmgr.TruncateOptions{Marker: "… (truncated)"})
}

// FinishSubagentOutcome writes the child's final outcome onto its snapshot
// and persists it, so the sidebar can render the true result after a
// reload/restart (the subagent_started/finished events are not replayed to
// connecting clients). The turn's own flush already wrote the transcript;
// this full flush adds the outcome fields (persistMu serializes against any
// tail of the turn's write). Status is "success" or "failed" from err; the
// summary is the error message on failure (the report is whatever the child
// produced before dying) or the truncated report on success. Shared by the
// web and TUI spawners so the two hosts cannot drift.
func (a *Agent) FinishSubagentOutcome(report string, err error) {
	status := "success"
	summary := TruncateSubagentReport(report)
	if err != nil {
		status = "failed"
		summary = err.Error()
	}
	a.SetSubagentOutcome(status, summary)
	a.FlushSession()
}

// SubagentModelSource identifies which tier of the subagent model cascade
// won (see ResolveSubagentModel). The source drives the apply policy at the
// spawner: the explicit tool argument is a hard requirement (a model that
// cannot be selected fails the spawn), while the configured and parent
// tiers fall back to the model the child's provider was seeded with
// (fail open).
type SubagentModelSource int

const (
	// SubagentModelInherited: no tier applies; the child keeps the model
	// its provider was seeded with (the workspace default).
	SubagentModelInherited SubagentModelSource = iota
	// SubagentModelExplicit: the subagent tool's explicit model argument.
	SubagentModelExplicit
	// SubagentModelConfigured: the configured subagent_model setting.
	SubagentModelConfigured
	// SubagentModelParent: the parent's live model (only when it differs
	// from the workspace default — the child is already seeded with that).
	SubagentModelParent
)

// String names the tier for log messages.
func (s SubagentModelSource) String() string {
	switch s {
	case SubagentModelExplicit:
		return "explicit model"
	case SubagentModelConfigured:
		return "configured subagent model"
	case SubagentModelParent:
		return "parent model"
	default:
		return "inherited model"
	}
}

// ResolveSubagentModel decides which model a subagent child runs on.
// Cascade: the subagent tool's explicit model argument > the configured
// subagent model (subagent_model) > the parent's live model (skipped when
// it equals workspaceDefault, since the child's provider is already seeded
// with that). Returns "" with SubagentModelInherited when no tier applies.
// The spawner applies the winner with ApplySubagentModel. Shared by the web
// and TUI spawners so the two cannot drift (like ApplySubagentThinkingLevel).
func ResolveSubagentModel(explicit, configured, parentModel, workspaceDefault string) (string, SubagentModelSource) {
	switch {
	case explicit != "":
		return explicit, SubagentModelExplicit
	case configured != "":
		return configured, SubagentModelConfigured
	case parentModel != "" && parentModel != workspaceDefault:
		return parentModel, SubagentModelParent
	default:
		return "", SubagentModelInherited
	}
}

// ApplySubagentModel selects the model resolved by ResolveSubagentModel on
// the child agent, applying the source's policy: SubagentModelExplicit is a
// hard requirement — a model that cannot be selected returns an error (the
// spawner fails the spawn); the other tiers log and fall back to the model
// the child's provider was seeded with (fail open). An empty model
// (SubagentModelInherited) selects nothing. Shared by the web and TUI
// spawners so the two cannot drift (like ApplySubagentThinkingLevel).
func ApplySubagentModel(ctx context.Context, child *Agent, model string, src SubagentModelSource) error {
	if model == "" {
		return nil
	}
	if err := child.SelectModel(ctx, model); err != nil {
		if src == SubagentModelExplicit {
			return fmt.Errorf("subagent model: %w", err)
		}
		log.Printf("subagent: %s %q not selectable on child (%v); using seeded default model", src, model, err)
	}
	return nil
}

// ApplySubagentThinkingLevel resolves and applies the child's reasoning
// effort: the configured subagent level (the subagent_thinking_level
// setting) when non-empty, otherwise the parent's live level (empty =
// inherit). "off" and empty are the child's default and set nothing. A
// level the child's FINAL model does not accept is omitted (policy B) and
// logged — the tool's explicit model argument can override the configured
// subagent model, so validity is resolved against the child's model at
// spawn time, never at save time. Shared by the web and TUI spawners so
// the two cannot drift (like SubagentLabel).
func ApplySubagentThinkingLevel(child, parent *Agent, configured string) {
	level := NormalizeThinkingLevel(configured)
	if level == "" {
		_, level = parent.ModeAndThinkingLevel()
	}
	if level == "" || level == ThinkingOff {
		return
	}
	if !slices.Contains(child.CurrentModelEfforts(), string(level)) {
		log.Printf("subagent: thinking level %q not accepted by child model %q; omitted", level, child.CurrentModel())
		return
	}
	child.SetThinkingLevel(level)
}

// SubagentSpawner runs a nested session with a job prompt and returns its
// final report. Installed by the web server (workspace + registry) and the
// TUI runner; nil means the subagent tool is unavailable ("spawner not
// installed") — the same nil-guard contract as the MCP registry.
type SubagentSpawner interface {
	// Spawn runs a nested agent turn for job (a full prompt). parent is the
	// agent that issued the tool call (the spawner uses it to resolve the
	// parent runtime for client broadcasts and approval routing). depth is
	// the child's nesting depth (parent = 0, child = 1, ...). model
	// overrides the child's model when non-empty. Returns the child's final
	// response.
	Spawn(ctx context.Context, parent *Agent, job, model string, depth int) (string, error)
}

// ContinuableSubagentSpawner is the optional extension of SubagentSpawner
// for background/continuable subagents (web host; the TUI stays foreground
// in v1). When the installed spawner implements it, the subagent tool gains
// run_in_background and the control tools (subagent_fork, list_agents,
// get_agent_result, wait_agent, send_message, interrupt_agent).
//
// Return contract (one logical path per run, as in the other harnesses):
// a background child's outcome reaches the parent through a compact terminal
// event plus the authoritative GetAgentResult/WaitAgent result, and one exact
// reply is delivered per send_message. Progress is pull-side via ListAgents;
// children never push intermediate messages into the parent conversation.
type ContinuableSubagentSpawner interface {
	// SpawnBackground starts a nested session that keeps running after this
	// call returns. The parent receives a compact terminal event when the
	// child finishes; while it lives it can be messaged, interrupted,
	// listed, waited on, and queried for its authoritative result.
	SpawnBackground(ctx context.Context, parent *Agent, job, model string, depth int) (id string, err error)
	// Fork seeds a child with a deep copy of the parent's messages and runs
	// one foreground turn. job may be empty for the default continuation
	// prompt. Returns the fork's final output.
	Fork(ctx context.Context, parent *Agent, job string, depth int) (string, error)
	// ListAgents lists the caller's registered children (id, label, status,
	// depth, latest progress line). Finished children remain available only for
	// their retention window; GetAgentResult resolves older persisted results.
	ListAgents(caller *Agent) (string, error)
	// GetAgentResult returns the authoritative current/final result for one
	// of the caller's background children. It remains readable after the
	// in-memory child is released because the host resolves the persisted
	// child outcome. Running children return their current status rather than
	// an error, so callers can use WaitAgent when they need to block.
	GetAgentResult(caller *Agent, agentID string) (string, error)
	// WaitAgent blocks until the child reaches a terminal main-run state or
	// the caller's context/timeout expires. A timeout is reported as current
	// status, not an error; cancellation returns an error while the child
	// keeps running unless the caller separately interrupts it.
	WaitAgent(ctx context.Context, caller *Agent, agentID string, timeout time.Duration) (string, error)
	// SendMessage delivers text to a live child of caller as a new user
	// message; queued when the child is mid-turn.
	SendMessage(caller *Agent, agentID, text string) error
	// InterruptAgent cancels a live child's in-flight turn (no-op when
	// idle). The child session stays alive.
	InterruptAgent(caller *Agent, agentID string) error
}

// subagentToolDef is the LLM-facing schema for the subagent tool. Like the
// board tool it lives OUTSIDE builtinToolDefs (MCP-style gating): exposed
// only when the subagent feature is enabled AND a spawner is installed.
// bg=true (the installed spawner implements ContinuableSubagentSpawner)
// adds the run_in_background parameter; TUI v1 keeps the foreground-only
// schema exactly. maxConcurrent is the live per-parent concurrent-subagent
// limit (subagent_max_concurrent) surfaced in the description so the model
// knows how many background children it can run at once.
func subagentToolDef(bg bool, maxConcurrent int) llm.Tool {
	desc := "Spawn a child agent on an isolated task; its final output returns as the tool result. " +
		"Use for subtasks that would otherwise bloat this context. Subagents cannot " +
		"spawn subagents by default. " +
		fmt.Sprintf("At most %d may run concurrently.", maxConcurrent)
	props := map[string]any{
		"job":   toolProp("string", "Complete, self-contained task for the child agent"),
		"model": toolProp("string", "Optional model override"),
	}
	if bg {
		desc += " run_in_background=true returns the child id immediately; a compact " +
			"terminal event is queued for this parent. Use get_agent_result or wait_agent for the " +
			"authoritative report, list_agents to check status/progress, send_message to queue a " +
			"follow-up turn, and interrupt_agent to cancel the current turn."
		props["run_in_background"] = toolProp("boolean", "Run in background; returns the child id immediately")
	}
	return toolDef("subagent", desc,
		toolSchema(props, []string{"job"}...),
	)
}

// subagentForkToolDef is the schema for the fork tool: a child seeded with
// a deep copy of this session's history, then one foreground turn.
func subagentForkToolDef() llm.Tool {
	return toolDef("subagent_fork",
		"Fork this session: the child starts with this transcript and runs a job; its edits and "+
			"turns stay in the child, this conversation is untouched. Use to explore an alternative "+
			"approach or continue this task without blocking. Foreground: returns the fork's final output.",
		toolSchema(map[string]any{
			"job": toolProp("string", "Optional job (default: continue this session)"),
		}),
	)
}

// listAgentsToolDef is the schema for the read-only live-child listing. Each
// child's latest progress line is included (bounded): this is the PULL side
// of a background child's progress — reports are never pushed into the
// parent's conversation as turns, so the model checks deliberately.
func listAgentsToolDef() llm.Tool {
	return toolDef("list_agents",
		"List this session's child subagents: id, label, status (running/idle/finished), depth, and each "+
			"child's latest progress line. Read-only. Use to resolve agent ids for get_agent_result/"+
			"wait_agent/send_message/interrupt_agent, to check background status, or to see what a child "+
			"is doing. Finished children remain listed briefly.",
		toolSchema(map[string]any{}),
	)
}

// getAgentResultToolDef retrieves the authoritative current or persisted
// outcome for one child. Keeping the potentially large report behind a pull
// tool lets completion delivery stay compact and bounded.
func getAgentResultToolDef() llm.Tool {
	return toolDef("get_agent_result",
		"Get a background subagent's current status or final result by agent_id. The result is "+
			"authoritative and remains available after the child leaves the live list. Read-only.",
		toolSchema(map[string]any{
			"agent_id": toolProp("string", "Subagent id (from list_agents or subagent)"),
		}, "agent_id"),
	)
}

func waitAgentToolDef() llm.Tool {
	return toolDef("wait_agent",
		"Wait for a background subagent's main run to finish, up to timeout_ms (default 60000, "+
			"maximum 600000). A timeout returns the current status without stopping the child; "+
			"cancelling the parent turn also leaves the child running. Use get_agent_result after "+
			"the wait when the authoritative result is needed.",
		toolSchema(map[string]any{
			"agent_id":   toolProp("string", "Subagent id (from list_agents or subagent)"),
			"timeout_ms": toolProp("integer", "Maximum wait in milliseconds (default 60000, max 600000)"),
		}, "agent_id"),
	)
}

// sendMessageToolDef is the schema for steering/resuming a background
// subagent: the message is delivered as its next user turn (queued when it
// is mid-turn) and the child's reply is injected into this session as one
// message when that turn finishes — the single return channel per run.
func sendMessageToolDef() llm.Tool {
	return toolDef("send_message",
		"Send a message to a background subagent of this session (agent_id) — running, interrupted, or "+
			"finished-but-retained; queued if it is mid-turn. Its reply is injected into this session "+
			"when the turn finishes. Use to queue a follow-up, ask a finished one for more detail, or "+
			"request a stop-and-report. This does not alter the already-running LLM request; use "+
			"interrupt_agent first when the current turn must be stopped immediately.",
		toolSchema(map[string]any{
			"agent_id": toolProp("string", "Subagent id (from list_agents)"),
			"message":  toolProp("string", "The message to deliver to the subagent"),
		}, "agent_id", "message"),
	)
}

// interruptAgentToolDef is the schema for cancelling a child's in-flight
// turn without killing the child session.
func interruptAgentToolDef() llm.Tool {
	return toolDef("interrupt_agent",
		"Cancel a subagent's in-flight turn (agent_id); the session stays alive and can be messaged again. "+
			"No-op when idle. Use when a background subagent is stuck or off-track.",
		toolSchema(map[string]any{
			"agent_id": toolProp("string", "Subagent id (from list_agents)"),
		}, "agent_id"),
	)
}

// continuable returns the installed spawner as a ContinuableSubagentSpawner
// when it supports background/continuable subagents.
func (a *Agent) continuableSpawner() ContinuableSubagentSpawner {
	cs, _ := a.SubagentSpawner().(ContinuableSubagentSpawner)
	return cs
}

// handleSubagent executes a subagent tool call. Reachable only when the
// subagent feature is enabled (all tool surfaces gate on SubagentsEnabled),
// so the spawner nil-guard is the only check needed here.
func handleSubagent(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	spawner := a.SubagentSpawner()
	if spawner == nil {
		return "", fmt.Errorf("subagent spawner is not installed (subagents are unavailable in this mode)")
	}
	job, err := stringArg(args, "job")
	if err != nil {
		return "", err
	}
	model, _ := stringArgOptional(args, "model")
	if a.SubagentDepth() >= a.SubagentMaxDepth() {
		return "", fmt.Errorf("subagent nesting depth limit reached (%d of %d): this subagent cannot spawn further subagents",
			a.SubagentDepth(), a.SubagentMaxDepth())
	}
	bg, err := boolArg(args, "run_in_background", false)
	if err != nil {
		return "", err
	}
	if bg {
		cs := a.continuableSpawner()
		if cs == nil {
			return "", fmt.Errorf("background subagents are not supported in this mode")
		}
		id, err := cs.SpawnBackground(ctx, a, job, model, a.SubagentDepth())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Subagent %s started in background (label: %s). "+
			"A compact terminal event will be queued for this parent. Use wait_agent to block for completion, "+
			"get_agent_result for the authoritative report, list_agents for status/progress, send_message "+
			"to queue a follow-up turn, and interrupt_agent to cancel the current turn.", id, SubagentLabel(job)), nil
	}
	return spawner.Spawn(ctx, a, job, model, a.SubagentDepth())
}

// handleSubagentFork executes the fork tool (foreground). Reachable only
// when the subagent feature is enabled and the spawner is continuable.
func handleSubagentFork(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("subagent_fork is not supported in this mode")
	}
	job, _ := stringArgOptional(args, "job")
	if a.SubagentDepth() >= a.SubagentMaxDepth() {
		return "", fmt.Errorf("subagent nesting depth limit reached (%d of %d): this subagent cannot spawn further subagents",
			a.SubagentDepth(), a.SubagentMaxDepth())
	}
	return cs.Fork(ctx, a, job, a.SubagentDepth())
}

// handleListAgents lists the caller's live children (read-only).
func handleListAgents(_ context.Context, a *Agent, _ map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("list_agents is not supported in this mode")
	}
	return cs.ListAgents(a)
}

func handleGetAgentResult(_ context.Context, a *Agent, args map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("get_agent_result is not supported in this mode")
	}
	agentID, err := stringArg(args, "agent_id")
	if err != nil {
		return "", err
	}
	return cs.GetAgentResult(a, agentID)
}

const (
	defaultWaitAgentTimeout = 60 * time.Second
	maxWaitAgentTimeout     = 10 * time.Minute
)

func handleWaitAgent(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("wait_agent is not supported in this mode")
	}
	agentID, err := stringArg(args, "agent_id")
	if err != nil {
		return "", err
	}
	ms, err := intArgOptional(args, "timeout_ms")
	if err != nil {
		return "", err
	}
	if ms < 0 {
		return "", fmt.Errorf("argument %q must be zero or a positive integer", "timeout_ms")
	}
	if ms > int(maxWaitAgentTimeout/time.Millisecond) {
		return "", fmt.Errorf("argument %q exceeds the maximum of %d milliseconds", "timeout_ms", maxWaitAgentTimeout/time.Millisecond)
	}
	timeout := defaultWaitAgentTimeout
	if ms > 0 {
		if time.Duration(ms) > maxWaitAgentTimeout/time.Millisecond {
			timeout = maxWaitAgentTimeout
		} else {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}
	return cs.WaitAgent(ctx, a, agentID, timeout)
}

// handleSendMessage delivers a message to a live background subagent.
func handleSendMessage(_ context.Context, a *Agent, args map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("send_message is not supported in this mode")
	}
	agentID, err := stringArg(args, "agent_id")
	if err != nil {
		return "", err
	}
	message, err := stringArg(args, "message")
	if err != nil {
		return "", err
	}
	if err := cs.SendMessage(a, agentID, message); err != nil {
		return "", err
	}
	return fmt.Sprintf("Message delivered to subagent %s.", agentID), nil
}

// handleInterruptAgent cancels a subagent's in-flight turn.
func handleInterruptAgent(_ context.Context, a *Agent, args map[string]any) (string, error) {
	cs := a.continuableSpawner()
	if cs == nil {
		return "", fmt.Errorf("interrupt_agent is not supported in this mode")
	}
	agentID, err := stringArg(args, "agent_id")
	if err != nil {
		return "", err
	}
	if err := cs.InterruptAgent(a, agentID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Interrupted subagent %s.", agentID), nil
}

// SetSubagentSpawner installs the nested-session runner (nil detaches).
// Called once at startup by the web server and TUI runner; the subagent tool
// stays gated on SubagentsEnabled regardless.
func (a *Agent) SetSubagentSpawner(s SubagentSpawner) {
	a.spawnerMu.Lock()
	a.spawner = s
	a.spawnerMu.Unlock()
	a.noteToolsChanged()
}

// SubagentSpawner returns the installed nested-session runner (nil when
// subagents are unavailable in this mode).
func (a *Agent) SubagentSpawner() SubagentSpawner {
	a.spawnerMu.RLock()
	defer a.spawnerMu.RUnlock()
	return a.spawner
}

// SetSubagentDepth sets this agent's nesting depth (0 = main agent). The
// spawner sets depth+1 on children it creates.
func (a *Agent) SetSubagentDepth(depth int) {
	a.subagentDepth.Store(int32(depth))
}

// SubagentDepth returns this agent's nesting depth (0 = main agent).
func (a *Agent) SubagentDepth() int {
	return int(a.subagentDepth.Load())
}

// SetParentID marks this session as a nested (subagent) child of parentID
// (empty clears the mark).
func (a *Agent) SetParentID(parentID string) {
	a.parentID.Store(parentID)
}

// ParentID returns the parent session id for nested (subagent) sessions
// (empty for top-level sessions).
func (a *Agent) ParentID() string {
	v, _ := a.parentID.Load().(string)
	return v
}

// subagentOutcome is the persisted final outcome of a nested (subagent)
// session. Status is "" (unknown / not finished), "success", or "failed".
type subagentOutcome struct {
	Status  string
	Summary string
}

// SetSubagentOutcome records the final outcome of a nested (subagent)
// session. The spawner writes it at every terminal transition (natural
// completion, cancellation, eviction) so the persisted snapshot carries the
// truth across restarts; the sidebar reads it from the sessions payload
// when the live subagent_started/finished events are gone. Empty status
// means the child has not finished (or is not a subagent).
func (a *Agent) SetSubagentOutcome(status, summary string) {
	a.subagentOutcome.Store(&subagentOutcome{Status: status, Summary: summary})
}

// SubagentOutcome returns the recorded final outcome: the status ("" /
// "success" / "failed") and the summary text.
func (a *Agent) SubagentOutcome() (string, string) {
	v := a.subagentOutcome.Load()
	if v == nil {
		return "", ""
	}
	o := v.(*subagentOutcome)
	return o.Status, o.Summary
}

// subagentWiring groups the nested-session (subagent) state: the installed
// spawner, this agent's nesting depth and parent link, and the persisted
// subagent outcome. A child has one logical return path per run — its final
// message (the foreground spawn result or a compact background terminal event
// plus the authoritative result pull) plus, per send_message, the exact reply.
// Children never push progress into the parent conversation as turns; the
// parent PULLS progress with list_agents.
type subagentWiring struct {
	// spawnerMu guards spawner (the nested-session runner installed once at
	// startup by the server/TUI). Reads happen per turn (llmTools,
	// executeTool), so the guard keeps the write/read race-free.
	spawnerMu sync.RWMutex
	spawner   SubagentSpawner
	// subagentDepth is this agent's nesting depth (main agent = 0; the
	// spawner sets depth+1 on children). Bounds spawning against
	// SubagentMaxDepth (default 1 = subagents cannot spawn subagents).
	subagentDepth atomic.Int32
	// parentID is non-empty for nested (subagent) sessions: persisted into
	// the session snapshot so the store can exclude them from the flat
	// list, cascade-delete them with the parent, and cap them per parent.
	parentID atomic.Value // string
	// subagentOutcome records the final outcome of a nested (subagent)
	// session (nil for top-level sessions and children that have not
	// finished). Persisted into the snapshot so the sidebar can render the
	// true result after a reload/restart, when the subagent_started/
	// finished events are not replayed.
	subagentOutcome atomic.Value // *subagentOutcome
}
