package tui

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/server"
	"gogen/internal/session"
	"gogen/internal/subagents"
)

// subagentRecord is one finished nested session shown by /subagents.
type subagentRecord struct {
	label  string
	report string
	err    error
}

// tuiSubagentSpawner runs nested sessions for the TUI. There is exactly ONE
// per host: the runner builds it and installs it on the workspace (so every
// session created there — opened panes, resumed sessions, and the children
// themselves — inherits it) and on the root agent, mirroring the web server's
// single workspace spawner. The Model only attaches its /subagents list.
//
// A FOREGROUND child runs inline on the parent's turn and its report is the tool
// result; a BACKGROUND child is hosted as a live session by the continuable
// host in internal/tui/children.go (own pane, nested sidebar row, retention).
// Either way the final report also lands in the /subagents modal list. Nesting
// follows subagent_max_depth (default 1 = children cannot spawn).
type tuiSubagentSpawner struct {
	// Spawner is the shared continuable-subagent lifecycle: this host embeds
	// it so the subagent tool gains run_in_background and the control tools,
	// and so the child registry is the single source of truth for
	// list_agents / get_agent_result / wait_agent.
	*subagents.Spawner[*childHandle]
	cfg *config.Config
	// ws is the shared web lifecycle core (server.NewWorkspaceForHost). When
	// attached, children are built through it — the SAME factory the web
	// server uses (per-child provider, live feature flags, MCP/skills/board
	// managers, and fsMu-wrapped tool handlers, so a child's file mutations
	// serialize with every other session in this process). Nil in tests and
	// for single-session embeds, which fall back to the hand-built options.
	ws *server.Workspace

	// recordMu guards record, the /subagents list sink. The Model owns the
	// list and is built after this spawner, so the sink is attached once the
	// Model exists (Update thread, before any child can spawn); children
	// append from their turn goroutine, hence the guard.
	recordMu sync.Mutex
	record   func(subagentRecord)

	// senderMu guards sender, the program-sender accessor the lifecycle uses
	// to reach the Update thread (child admission, send_message, releases).
	// Installed by the runner once the program exists; nil before that.
	senderMu sync.Mutex
	sender   func() programSender
	// stopped mirrors the runner's "event loop exited" latch, so a host post
	// after the loop ends fails instead of parking the caller forever.
	stopped *atomic.Bool

	// providerFactory builds the child provider for the nil-workspace path;
	// nil = the OpenAI with-profiles default (tests inject a mock).
	providerFactory func(cfg *config.Config, parent *agent.Agent) llm.LLMProvider
}

// The TUI host must satisfy the CONTINUABLE contract: that is what makes the
// agent package expose run_in_background and the control tools to the TUI's
// model, so a dropped or renamed method would silently downgrade the feature to
// the foreground-only v1 behaviour.
var _ agent.ContinuableSubagentSpawner = (*tuiSubagentSpawner)(nil)

// newTUISubagentSpawner builds the host spawner around the shared continuable
// lifecycle (internal/subagents). One per host: the lifecycle's child registry
// must not be duplicated per Model.
func newTUISubagentSpawner(cfg *config.Config, ws *server.Workspace) *tuiSubagentSpawner {
	sp := &tuiSubagentSpawner{cfg: cfg, ws: ws}
	sp.Spawner = subagents.New[*childHandle](sp)
	return sp
}

// setRecorder attaches the /subagents list sink.
func (sp *tuiSubagentSpawner) setRecorder(record func(subagentRecord)) {
	if sp == nil {
		return
	}
	sp.recordMu.Lock()
	sp.record = record
	sp.recordMu.Unlock()
}

// addRecord appends one finished child to the /subagents list, when a sink is
// attached (headless/tests without a Model drop the record harmlessly).
func (sp *tuiSubagentSpawner) addRecord(r subagentRecord) {
	if sp == nil {
		return
	}
	sp.recordMu.Lock()
	record := sp.record
	sp.recordMu.Unlock()
	if record != nil {
		record(r)
	}
}

// newChildAgent builds the child session agent: through the shared workspace
// factory when one is attached (web parity), otherwise from the parent's live
// wiring (the single-session/test path).
func (sp *tuiSubagentSpawner) newChildAgent(parent *agent.Agent) *agent.Agent {
	if sp.ws != nil {
		return sp.ws.NewSessionAgent(nil, session.NewID())
	}
	return agent.NewSessionAgent(agent.SessionAgentOptions{
		Provider:              sp.childProvider(parent),
		Executor:              parent.Executor,
		Store:                 parent.SessionStore,
		Config:                sp.cfg,
		GlobalMode:            parent.GlobalMode,
		ProjectFilePath:       parent.ProjectFilePath,
		ProjectGuidelines:     parent.ProjectGuidelines,
		TestCommand:           parent.TestCommand,
		LintCommand:           parent.LintCommand,
		WorkingDir:            parent.WorkingDir,
		MCPRegistry:           parent.MCPRegistry,
		MCPConfigured:         parent.MCPConfigured(),
		MCPNested:             parent.MCPNested(),
		DebugCompareMessages:  parent.DebugCompareMessages,
		BoardEnabled:          parent.BoardEnabled(),
		SubagentsEnabled:      parent.SubagentsEnabled(),
		SubagentMaxDepth:      parent.SubagentMaxDepth(),
		SubagentMaxConcurrent: parent.SubagentMaxConcurrent(),
		BoardManager:          parent.BoardManager(),
		SkillsManager:         parent.SkillsManager(),
		TerminalEnabled:       parent.TerminalEnabled(),
		InstructionsEnabled:   parent.InstructionsEnabled(),
		SubagentSpawner:       sp, // nesting allowed up to the configured depth
	}, nil, session.NewID())
}

// childProvider builds the child provider for the nil-workspace path.
//
// The child must inherit the PARENT's current model, not the startup config
// value: the user may have switched models at runtime (/model), or the model
// may only have been selected interactively (cfg OpenAIModel empty) — a child
// seeded from cfg would fail requireModelSelected or run a different model
// than its parent. The provider working dir follows the parent's live working
// dir too. An explicit model argument on the subagent tool wins over
// inheritance (see the model cascade in Spawn). The child carries ALL
// registered OpenAI-compatible profiles (the legacy fields form the implicit
// default), like the parent: a model inherited from a secondary provider must
// route to its owning endpoint, not fall back to the default client (D9).
func (sp *tuiSubagentSpawner) childProvider(parent *agent.Agent) llm.LLMProvider {
	if sp.providerFactory != nil {
		return sp.providerFactory(sp.cfg, parent)
	}
	openaiProv := llm.NewOpenAIProviderWithProfiles(
		llm.ProviderProfiles(sp.cfg.OpenAIKey, sp.cfg.OpenAIModel, sp.cfg.OpenAIURL, sp.cfg.OpenAIProviders),
		sp.cfg.OpenAIModel, parent.WorkingDir, nil)
	if pp, ok := parent.Provider.(*llm.OpenAIProvider); ok {
		// Share the parent's per-endpoint catalog cache too: a child must not
		// re-download the catalog the parent already holds.
		openaiProv.SetEndpointCatalog(pp.EndpointCatalog())
		openaiProv.SetPropsCapsCache(pp.PropsCapsCache())
	}
	return openaiProv
}

// Fork implements agent.ContinuableSubagentSpawner: a foreground child seeded
// with a deep copy of the parent's messages, plus the parent's model, mode and
// thinking level. Foreground, like the web fork: the parent's turn waits for
// it, and the parent transcript is untouched.
func (sp *tuiSubagentSpawner) Fork(ctx context.Context, parent *agent.Agent, job string, depth int) (string, error) {
	if parent == nil {
		return "", fmt.Errorf("subagent_fork: parent agent is nil")
	}
	if sp.ws == nil {
		return "", fmt.Errorf("subagent_fork: not available without a workspace")
	}
	ok, guardHit := sp.AcquireForeground(parent.SessionID)
	if !ok {
		return "", fmt.Errorf("subagent_fork: %w", sp.SpawnCapError(parent.SessionID, guardHit))
	}
	defer sp.ReleaseForeground(parent.SessionID)
	forkedMsgs, err := agent.ForkMessages(parent.SnapshotMessages(), "last")
	if err != nil {
		return "", fmt.Errorf("subagent_fork: %w", err)
	}
	mode, thinking := parent.ModeAndThinkingLevel()
	if job == "" {
		job = "Continue this session from the fork point."
	}
	newID := session.NewID()
	// Re-point the forked transcript's spill locators at the child's spill dir
	// (hardlinks) so retrieval keeps working after the parent session is
	// deleted (the delete cascade removes the parent's spill files).
	parent.RepointSpillLocators(forkedMsgs, newID)
	forkRef := parent.CurrentModelRef()
	snap := &agent.SessionSnapshot{
		WorkingDir:    parent.WorkingDir,
		Model:         forkRef.WireID(),
		ModelProvider: forkRef.ProfileName(),
		Mode:          mode.String(),
		ThinkingLevel: string(thinking),
		Label:         "fork: " + parent.SessionLabelSnapshot(),
		LabelRenamed:  true,
		Messages:      forkedMsgs,
		ParentID:      parent.SessionID,
	}
	child := sp.ws.NewSessionAgent(snap, newID)
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	report, err := child.StreamProcessInputWithImages(ctx, job, nil, nil)
	child.FinishSubagentOutcome(report, err)
	report = agent.TruncateSubagentReport(report)
	sp.addRecord(subagentRecord{label: snap.Label, report: report, err: err})
	if err != nil {
		return report, fmt.Errorf("subagent_fork %s: %w", newID, err)
	}
	return report, nil
}

func (sp *tuiSubagentSpawner) Spawn(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	// The foreground child holds a concurrency slot for its whole turn, like
	// the web host, so a foreground spawn cannot bypass the configured limit.
	ok, guardHit := sp.AcquireForeground(parent.SessionID)
	if !ok {
		return "", sp.SpawnCapError(parent.SessionID, guardHit)
	}
	defer sp.ReleaseForeground(parent.SessionID)
	child := sp.newChildAgent(parent)
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	// Model cascade (fully shared with the web spawner — see
	// ResolveSubagentModel/ApplySubagentModel): explicit tool argument >
	// configured subagent model > the parent's live model (unless it is
	// the default the child is already seeded with). The explicit
	// argument is a hard requirement and fails the spawn when
	// unselectable; the other tiers fall back to the seeded default
	// (fail open). The parent tier passes the qualified selector form
	// ("profile/id" for a non-default owner) so the child selects the
	// SAME (profile, ID) the parent runs on.
	m, src := agent.ResolveSubagentModel(model, sp.cfg.SubagentModel, parent.CurrentModelRef().String(), sp.cfg.OpenAIModel)
	if err := agent.ApplySubagentModel(ctx, child, m, src); err != nil {
		return "", err
	}
	// Reasoning effort: the same cascade as the web spawner — the
	// configured subagent level (subagent_thinking_level) wins; empty =
	// inherit the parent's live level. A level the child's final model
	// does not accept is omitted (shared helper, so the hosts cannot
	// drift).
	agent.ApplySubagentThinkingLevel(child, parent, sp.cfg.SubagentThinkingLevel)
	label := agent.SubagentLabel(job)
	_, _ = child.RenameSession(label)
	// The job wrapper applies AFTER label derivation, so the /subagents
	// list keeps the original job the parent wrote; only the child's first
	// message carries the wrapped job.
	report, err := child.StreamProcessInputWithImages(ctx, agent.FormatSubagentJob(job), nil, nil)
	// Persist the final outcome on the child's snapshot (shared with the
	// web spawner — FinishSubagentOutcome): the sidebar reads it from the
	// sessions payload when the live subagent events are gone.
	child.FinishSubagentOutcome(report, err)
	report = agent.TruncateSubagentReport(report)
	sp.addRecord(subagentRecord{label: label, report: report, err: err})
	return report, err
}
