package agent

import (
	"sync"
	"sync/atomic"

	"gogen/internal/codeindex"
	"gogen/internal/config"
	"gogen/internal/skills"
)

// FeatureFlags is the single live store for the board / subagent feature
// flags. A workspace owns one instance and every session agent it spawns
// reads the SAME instance (Agent.SetFeatureFlags), so a settings toggle is
// visible to all sessions immediately — no per-agent mirror and no sweep.
//
// All values are atomic: the web settings toggle writes them while
// per-turn tool derivation (llmTools/AllowedToolNames/executeTool) and the
// subagent spawner read them concurrently.
type FeatureFlags struct {
	boardEnabled          atomic.Bool
	subagentsEnabled      atomic.Bool
	automationsEnabled    atomic.Bool
	subagentMaxDepth      atomic.Int32
	subagentMaxConcurrent atomic.Int32
	reviewAgentEnabled    atomic.Bool
	// skillsEnabled / terminalEnabled gate the optional feature tools (the
	// skill tool; the terminal + bash_persistent tools). They live in the
	// shared store like the board/subagent flags, so the settings-modal
	// toggle reaches every session with no sweep.
	skillsEnabled    atomic.Bool
	terminalEnabled  atomic.Bool
	codeIndexEnabled atomic.Bool

	// gen is the store's monotonic change counter, bumped by every setter.
	// It is a component of the wire-overhead cache key (see overheadKey): a
	// live toggle through the SHARED store must invalidate every session
	// agent's cached tool set even though the agent's own setters were not
	// called, so the counter lives with the shared state. See
	// FeatureFlags.generation.
	gen atomic.Uint64
}

// generation returns the store's monotonic change counter. The wire-overhead
// cache keys on it (via overheadKey) so a live board/subagent toggle
// recomputes the tool set only when a flag actually changed.
func (f *FeatureFlags) generation() uint64 {
	return f.gen.Load()
}

// NewFeatureFlags creates a FeatureFlags store seeded with the given
// values. maxDepth/maxConcurrent <= 0 mean "unset" — readers apply the
// config defaults (see Agent.SubagentMaxDepth / SubagentMaxConcurrent).
func NewFeatureFlags(boardEnabled, subagentsEnabled bool, maxDepth, maxConcurrent int) *FeatureFlags {
	f := &FeatureFlags{}
	f.boardEnabled.Store(boardEnabled)
	f.subagentsEnabled.Store(subagentsEnabled)
	f.subagentMaxDepth.Store(int32(maxDepth))
	f.subagentMaxConcurrent.Store(int32(maxConcurrent))
	return f
}

// BoardEnabled reports whether the project kanban board feature is active.
func (f *FeatureFlags) BoardEnabled() bool {
	return f.boardEnabled.Load()
}

// SetBoardEnabled toggles the board feature.
func (f *FeatureFlags) SetBoardEnabled(on bool) {
	f.boardEnabled.Store(on)
	f.gen.Add(1)
}

// SubagentsEnabled reports whether the subagent feature is active.
func (f *FeatureFlags) SubagentsEnabled() bool {
	return f.subagentsEnabled.Load()
}

// AutomationsEnabled reports whether the automation scheduler may fire
// (host-level flag: the scheduler sweeps and fires due automations while a
// TUI/web host with this flag on is running, see internal/automation).
func (f *FeatureFlags) AutomationsEnabled() bool {
	return f.automationsEnabled.Load()
}

// SetAutomationsEnabled toggles the automation scheduler flag (live).
func (f *FeatureFlags) SetAutomationsEnabled(on bool) {
	f.automationsEnabled.Store(on)
	f.gen.Add(1)
}

// SetSubagentsEnabled toggles the subagent feature.
func (f *FeatureFlags) SetSubagentsEnabled(on bool) {
	f.subagentsEnabled.Store(on)
	f.gen.Add(1)
}

// SubagentMaxDepth returns the stored nesting-depth limit (0 = unset).
func (f *FeatureFlags) SubagentMaxDepth() int {
	return int(f.subagentMaxDepth.Load())
}

// SetSubagentMaxDepth stores the nesting-depth limit (0 = unset).
func (f *FeatureFlags) SetSubagentMaxDepth(depth int) {
	f.subagentMaxDepth.Store(int32(depth))
	f.gen.Add(1)
}

// SubagentMaxConcurrent returns the stored per-parent concurrent-subagent
// limit (0 = unset).
func (f *FeatureFlags) SubagentMaxConcurrent() int {
	return int(f.subagentMaxConcurrent.Load())
}

// SetSubagentMaxConcurrent stores the per-parent concurrent-subagent limit
// (0 = unset).
func (f *FeatureFlags) SetSubagentMaxConcurrent(n int) {
	f.subagentMaxConcurrent.Store(int32(n))
	f.gen.Add(1)
}

// ReviewAgentEnabled reports whether the board auto-review agent is
// active. Opt-in: the shared store is seeded off; the config seed and the
// settings toggle write it.
func (f *FeatureFlags) ReviewAgentEnabled() bool {
	return f.reviewAgentEnabled.Load()
}

// SetReviewAgentEnabled toggles the board auto-review agent.
func (f *FeatureFlags) SetReviewAgentEnabled(on bool) {
	f.reviewAgentEnabled.Store(on)
	f.gen.Add(1)
}

// SkillsEnabled reports whether the skill tool is active. Opt-in: the
// config seed and the settings toggle write it.
func (f *FeatureFlags) SkillsEnabled() bool {
	return f.skillsEnabled.Load()
}

// SetSkillsEnabled toggles the skill tool for every agent reading this
// store.
func (f *FeatureFlags) SetSkillsEnabled(on bool) {
	f.skillsEnabled.Store(on)
	f.gen.Add(1)
}

// CodeIndexEnabled reports whether search_meaning is active. Opt-in: the
// config seed and the settings toggle write it.
func (f *FeatureFlags) CodeIndexEnabled() bool {
	return f.codeIndexEnabled.Load()
}

// SetCodeIndexEnabled toggles search_meaning for every agent reading this store.
func (f *FeatureFlags) SetCodeIndexEnabled(on bool) {
	f.codeIndexEnabled.Store(on)
	f.gen.Add(1)
}

// TerminalEnabled reports whether the persistent-terminal feature tools
// (the terminal tool and bash_persistent) are active. Opt-in: the config
// seed and the settings toggle write it.
func (f *FeatureFlags) TerminalEnabled() bool {
	return f.terminalEnabled.Load()
}

// SetTerminalEnabled toggles the persistent-terminal feature tools for
// every agent reading this store.
func (f *FeatureFlags) SetTerminalEnabled(on bool) {
	f.terminalEnabled.Store(on)
	f.gen.Add(1)
}

// flags returns this agent's feature-flag store, lazily creating a private
// instance for agents that were never pointed at a shared one (standalone
// TUI/CLI agents, bare &Agent{} test literals).
func (a *Agent) flags() *FeatureFlags {
	if f := a.featureFlags.Load(); f != nil {
		return f
	}
	f := NewFeatureFlags(false, false, 0, 0)
	if !a.featureFlags.CompareAndSwap(nil, f) {
		f = a.featureFlags.Load()
	}
	return f
}

// SetFeatureFlags points this agent at a shared FeatureFlags store (the
// workspace's instance in web mode). From then on the agent's flag
// getters/setters read and write the shared state, so a workspace toggle is
// visible to this agent immediately — no per-agent mirror, no sweep.
// Passing nil detaches (the agent falls back to a lazily-created private
// store).
func (a *Agent) SetFeatureFlags(f *FeatureFlags) {
	a.featureFlags.Store(f)
}

// FeatureFlags returns the effective flag store this agent reads: the
// shared instance when one was attached, otherwise the lazily-created
// private one.
func (a *Agent) FeatureFlags() *FeatureFlags {
	return a.flags()
}

// SetBoardEnabled toggles the project kanban board feature for this agent.
// The web server publishes live toggles to the shared store; per-turn tool
// derivation (llmTools/AllowedToolNames) reads the value atomically.
func (a *Agent) SetBoardEnabled(on bool) {
	a.flags().SetBoardEnabled(on)
}

// BoardEnabled reports whether the board tool is registered for this agent.
func (a *Agent) BoardEnabled() bool {
	return a.flags().BoardEnabled()
}

// SetSubagentsEnabled toggles the subagent tool for this agent (see
// SetBoardEnabled for the live-toggle contract).
func (a *Agent) SetSubagentsEnabled(on bool) {
	a.flags().SetSubagentsEnabled(on)
}

// SubagentsEnabled reports whether the subagent tool is registered for this
// agent.
func (a *Agent) SubagentsEnabled() bool {
	return a.flags().SubagentsEnabled()
}

// SetSkillsEnabled toggles the skill tool for this agent (see
// SetBoardEnabled for the live-toggle contract: in web mode this writes the
// workspace's shared store, so the settings toggle reaches every session).
// The tool additionally requires an installed SkillsManager.
func (a *Agent) SetSkillsEnabled(on bool) {
	// No noteToolsChanged: the store's generation covers feature-flag
	// changes (see overheadKey), exactly like SetBoardEnabled.
	a.flags().SetSkillsEnabled(on)
}

// SetTerminalEnabled toggles the persistent-terminal feature tools for this
// agent (see SetBoardEnabled for the live-toggle contract: web mode writes
// the workspace's shared store).
func (a *Agent) SetTerminalEnabled(on bool) {
	a.flags().SetTerminalEnabled(on)
}

// TerminalEnabled reports whether the persistent-terminal feature tools are
// registered for this agent.
func (a *Agent) TerminalEnabled() bool {
	return a.flags().TerminalEnabled()
}

// SkillsEnabled reports whether the skill tool is registered for this agent.
func (a *Agent) SkillsEnabled() bool {
	return a.flags().SkillsEnabled()
}

// SetSubagentMaxDepth sets the maximum subagent nesting depth (main agent =
// depth 0). Values <= 0 fall back to the config default.
func (a *Agent) SetSubagentMaxDepth(depth int) {
	a.flags().SetSubagentMaxDepth(depth)
}

// SubagentMaxDepth returns the effective maximum subagent nesting depth.
func (a *Agent) SubagentMaxDepth() int {
	if d := a.flags().SubagentMaxDepth(); d > 0 {
		return d
	}
	return config.DefaultSubagentMaxDepth
}

// SetSubagentMaxConcurrent sets the maximum number of subagents that may
// run concurrently per parent session. Values <= 0 fall back to the config
// default.
func (a *Agent) SetSubagentMaxConcurrent(n int) {
	a.flags().SetSubagentMaxConcurrent(n)
}

// SetReviewAgentEnabled toggles the board auto-review agent for this agent
// (see SetBoardEnabled for the live-toggle contract). The flag gates the
// host-side trigger: with it on, a ticket moved into in_review spawns a
// headless review session (web host only — the TUI has no session spawner
// and never installs the trigger).
func (a *Agent) SetReviewAgentEnabled(on bool) {
	a.flags().SetReviewAgentEnabled(on)
}

// ReviewAgentEnabled reports whether the board auto-review agent is active.
func (a *Agent) ReviewAgentEnabled() bool {
	return a.flags().ReviewAgentEnabled()
}

// SetAutomationsEnabled toggles the automation scheduler flag for this
// agent (see SetReviewAgentEnabled for the live-toggle contract). The flag
// gates the HOST-side scheduler: with it on, a running TUI/web host fires
// due automations as headless runs.
func (a *Agent) SetAutomationsEnabled(on bool) {
	a.flags().SetAutomationsEnabled(on)
}

// AutomationsEnabled reports whether the automation scheduler may fire.
func (a *Agent) AutomationsEnabled() bool {
	return a.flags().AutomationsEnabled()
}

// SubagentMaxConcurrent returns the effective per-parent concurrent-subagent
// limit.
func (a *Agent) SubagentMaxConcurrent() int {
	if n := a.flags().SubagentMaxConcurrent(); n > 0 {
		return n
	}
	return config.DefaultSubagentMaxConcurrent
}

// featureWiring groups the shared feature managers (feature flags, board,
// skills, workspace instructions) and the host callbacks the web server
// installs on every session agent. See the featureFlags comment for the
// shared-store contract.
type featureWiring struct {
	// Feature flags, live-toggleable from the web settings modal (config WS
	// message). featureFlags points at the SHARED FeatureFlags store: in web
	// mode every session agent reads the workspace's single instance (set
	// via SetFeatureFlags at spawn time), so a settings toggle is visible to
	// all sessions with no per-agent mirror and no sweep. Standalone agents
	// (TUI/CLI, bare &Agent{} test literals) lazily get a private instance
	// on first access. The atomic stores inside publish toggles to every
	// concurrent reader: llmTools/AllowedToolNames consult
	// BoardEnabled/SubagentsEnabled per turn, and the toggle handler rebuilds
	// toolHandlers under toolMu so executeTool's map lookup is the gate.
	// SubagentMaxDepth bounds nesting (main agent = depth 0; default 1 =
	// subagents cannot spawn subagents) and SubagentMaxConcurrent bounds
	// per-parent concurrent subagents (default
	// config.DefaultSubagentMaxConcurrent).
	featureFlags atomic.Pointer[FeatureFlags]

	// boardManager is the shared project board (one instance per workspace
	// in web mode, per agent in TUI/CLI). Set when the board feature is
	// enabled; the nil-guard mirrors the MCP registry contract.
	boardManager atomic.Pointer[BoardManager]

	// skillsManager is the shared skill discovery manager (one per
	// workspace in web mode, per agent in TUI/CLI). The skill tool is
	// exposed only when the shared skillsEnabled flag is on AND the manager
	// is installed.
	skillsManager atomic.Pointer[skills.Manager]

	// The persistent-terminal feature tools (the terminal tool and
	// bash_persistent) are gated by the shared FeatureFlags store's
	// terminalEnabled flag: when off they appear nowhere — llmTools,
	// AllowedToolNames, and executeTool all derive from featureTools.

	// mcpConfigured / mcpNested are the MCP exposure policy, resolved ONCE at
	// construction from the session config (never from the live registry —
	// see SetMCPTooling). They drive three model-facing surfaces that must
	// agree: whether the nested MCP tools are registered, whether server
	// tool definitions are inlined into llmTools, and which MCP line the
	// system prompt carries. Being config-derived is what keeps the request
	// prefix byte-stable while a slow server is still connecting.
	mcpConfigured atomic.Bool
	mcpNested     atomic.Bool

	// instructionsEnabled mirrors the config agent_instructions flag (set at
	// construction and toggled live by the web settings sweep).
	// workspaceInstructions is the rendered AGENTS.md/CLAUDE.md section for
	// the CURRENT working dir, refreshed on every working-dir change (and on
	// a live enable) so the system prompt never carries a stale project's
	// instructions.
	instructionsEnabled   atomic.Bool
	instructionsMu        sync.RWMutex
	workspaceInstructions string

	// boardHookMu guards boardHook: the web server installs it so agent
	// board mutations broadcast a fresh board_state AND a success notice
	// (toast) to every client — the user sees agent-triggered board
	// changes even when they didn't initiate them. The callback receives
	// the mutation's output message ("Moved board item #1 to in_progress…").
	// Nil in TUI/CLI.
	boardHookMu sync.RWMutex
	boardHook   func(msg string)

	// jobNoticeHookMu guards jobNoticeHook: hosts install it so a
	// background job finishing naturally delivers a notice into the
	// session (job_notices feature). The callback receives a one-line
	// summary ("[job] job-xxx (command) exited with code N"). Nil when the
	// feature is off.
	jobNoticeHookMu sync.RWMutex
	jobNoticeHook   func(summary string)

	// codeIndex is the shared vector store for the working folder. It is not
	// a field grown onto Agent: the workspace owns one pointer and every
	// session reads it. Its mutex is its own lock. The progress callback
	// runs on the index's progress goroutine, after that lock is released,
	// and must not call Search, SearchRerank, Rebuild, or take the agent
	// turn lock. codeIndexConfig reads the live config at call time.
	// codeIndexEmbedder is a test double; production posts to the provider.
	// codeIndexReranker is a test double; production posts to the provider
	// when code_index_rerank is on, and is not called while it is off.
	codeIndex         atomic.Pointer[codeindex.Index]
	codeIndexConfig   atomic.Value // codeIndexConfig
	codeIndexEmbedder atomic.Value // codeIndexEmbedder
	codeIndexReranker atomic.Value // codeIndexReranker

	// toolGen counts model-facing tool-set reconfigurations that the SHARED
	// FeatureFlags generation does not capture: the MCP registry, the board/
	// skills managers, the subagent spawner, and the child report hook.
	// wireOverheadTokens keys its cache on toolGen + the flag store's
	// generation + the store identity (see overheadKey), so a stable tool
	// set costs one integer compare per turn instead of re-serializing every
	// tool schema; every setter that can change llmTools() must call
	// noteToolsChanged.
	toolGen atomic.Uint64
}

// noteToolsChanged marks the model-facing tool set as reconfigured so the
// wire-overhead cache (wireOverheadTokens) recomputes its estimate. Call it
// from any setter that can change the result of llmTools() — the MCP
// registry, the board/skills managers, the subagent spawner, the report
// hook — or the cached wire overhead could be reused against a changed
// tool set. Feature-flag changes are covered separately by the shared
// FeatureFlags generation.
func (a *Agent) noteToolsChanged() {
	a.toolGen.Add(1)
}
