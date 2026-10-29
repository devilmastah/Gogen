package agent

import (
	"reflect"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/skills"
)

// persistentStore normalizes a typed-nil SessionPersister to a real nil.
//
// A nil concrete pointer stored in the interface (the web workspace passes its
// *session.Store field, which is nil on hosts built around an agent without
// persistence) is NOT == nil, so every persistence guard would let it through
// and (*session.Store).Save would dereference its nil receiver — the panic the
// web bootstrap path hit when it created a session agent for a store-less
// workspace (session_factory's thinking-level seed flushes immediately).
// SessionAgentOptions.Store's "nil disables persistence" contract has to hold
// for that shape too.
func persistentStore(p SessionPersister) SessionPersister {
	if p == nil {
		return nil
	}
	if v := reflect.ValueOf(p); v.Kind() == reflect.Ptr && v.IsNil() {
		return nil
	}
	return p
}

// SessionAgentOptions carries the shared per-session agent wiring used by
// NewSessionAgent. Both the web server (Workspace.NewSessionAgent) and the
// TUI subagent spawner build one options struct and call the same factory, so
// every session — interactive or nested — is wired identically.
type SessionAgentOptions struct {
	// Provider is the session's own provider (fresh per session in web mode;
	// shared or fresh in the TUI).
	Provider llm.LLMProvider
	// Executor is the shared workspace executor (working dir + guards).
	Executor *Executor
	// Store is the shared session store; nil disables persistence.
	Store SessionPersister
	// Config supplies context-manager settings only (nil → defaults). Live
	// feature flags are passed explicitly below, NOT read from Config: the
	// web toggle mutates workspace/agent state, never the immutable Config.
	Config *config.Config

	GlobalMode        bool
	ProjectFilePath   string
	ProjectGuidelines string
	TestCommand       string
	LintCommand       string
	WorkingDir        string
	MCPRegistry       MCPToolRegistry
	// ToolHandlers overrides the builtin handler map (e.g. the web server's
	// fsMu-wrapped copy). nil → BuiltinToolHandlers.
	ToolHandlers         map[string]ToolHandler
	DebugCompareMessages bool
	// ThinkingLevel is seeded on fresh sessions and on snapshots that
	// predate the persisted thinking-level field.
	ThinkingLevel ThinkingLevel

	// FeatureFlags is the shared flag store the agent should read (web
	// mode: the workspace's single instance). When non-nil the agent reads
	// the shared state directly — no per-agent mirror, no sweep — and the
	// four value fields below are ignored.
	FeatureFlags *FeatureFlags
	// Live feature flags (board / subagent / nesting depth / concurrent
	// limit), used when FeatureFlags is nil. The TUI subagent spawner
	// passes the parent's values so nested sessions agree with it.
	BoardEnabled          bool
	SubagentsEnabled      bool
	SubagentMaxDepth      int
	SubagentMaxConcurrent int
	// BoardManager is the shared project board (nil when disabled). The web
	// server passes its single workspace manager so all sessions share one;
	// the TUI creates one per process.
	BoardManager *BoardManager
	// InstructionsEnabled mirrors the config agent_instructions flag; the
	// agent renders the AGENTS.md/CLAUDE.md section from its working dir
	// at construction and on every working-dir change.
	InstructionsEnabled bool
	// SkillsManager is the shared skill discovery manager (nil when the
	// host has none). The skill tool is exposed only when this is non-nil
	// AND the live skills flag is on.
	SkillsManager *skills.Manager
	// SkillsEnabled is the live skills flag, used only when FeatureFlags is
	// nil (the web path reads the shared store instead). The tool also
	// requires SkillsManager.
	SkillsEnabled bool
	// TerminalEnabled enables the persistent-terminal feature tools (the
	// terminal tool's open/send/read/signal/list/close actions and
	// bash_persistent). Used only when FeatureFlags is nil — the web path
	// reads the shared store's live flag instead.
	TerminalEnabled bool
	// MCPConfigured reports that MCP is enabled with at least one server in
	// the session's config (config.MCPActive), and MCPNested selects the
	// gateway exposure mode (config.MCPNested). Both are CONFIG answers taken
	// once at construction — never derived from the live registry — because
	// the tool set and the system prompt are part of the request prefix and
	// must not change when a slow server attaches mid-session.
	MCPConfigured bool
	MCPNested     bool
	// SubagentSpawner runs nested sessions (nil when unavailable; the
	// subagent tool additionally requires the feature flag).
	SubagentSpawner SubagentSpawner
}

// NewSessionAgent creates a fresh session agent: a new context manager over
// the given provider, sharing the executor, store, MCP registry, and tool
// handlers. When snap is non-nil the agent is restored from it under the
// given id (multi-session plan §2, session agent factory).
func NewSessionAgent(opts SessionAgentOptions, snap *SessionSnapshot, id string) *Agent {
	ctxMgr := contextmgr.NewManager(opts.Provider, contextmgr.SettingsFromConfig(opts.Config))
	a := NewAgent(opts.Provider, opts.Executor, ctxMgr)
	a.GlobalMode = opts.GlobalMode
	a.SetProjectContext(opts.ProjectFilePath, opts.ProjectGuidelines, opts.TestCommand, opts.LintCommand)
	// Enable instructions BEFORE SetWorkingDir below: the working-dir
	// seeding refreshes the instruction section, and must happen with the
	// flag already on.
	a.SetInstructionsEnabled(opts.InstructionsEnabled)
	// The workspace dir is authoritative for the agent's own WorkingDir and
	// the shared executor: NewAgent seeds both from the executor, which may
	// lag the workspace working dir in the window between a client's
	// working-dir change (handleWSConfig updates ws.WorkingDir first) and
	// the per-session sweep (applyWorkingDirToAll). Seeding here closes the
	// gap so a session created in that window is consistent from birth —
	// the sweep's SetWorkingDir for it later is then a no-op.
	a.SetWorkingDir(opts.WorkingDir)
	a.TodoManager = NewTodoManager(opts.WorkingDir)
	a.PinManager = NewPinManager()
	a.DebugCompareMessages = opts.DebugCompareMessages
	a.SessionStore = persistentStore(opts.Store)
	a.SetSessionID(id)
	a.SetMCPRegistry(opts.MCPRegistry)
	a.SetMCPTooling(opts.MCPConfigured, opts.MCPNested)
	if opts.ToolHandlers != nil {
		a.SetToolHandlers(opts.ToolHandlers)
	}
	if opts.FeatureFlags != nil {
		// Shared store (web mode): the agent reads the workspace's live
		// flags directly, so a settings toggle reaches it with no sweep.
		// The shared store owns skills/terminal too — the per-session value
		// options are deliberately NOT written here, or creating one session
		// would clobber the workspace-wide state.
		a.SetFeatureFlags(opts.FeatureFlags)
	} else {
		a.SetBoardEnabled(opts.BoardEnabled)
		a.SetSubagentsEnabled(opts.SubagentsEnabled)
		a.SetSubagentMaxDepth(opts.SubagentMaxDepth)
		a.SetSubagentMaxConcurrent(opts.SubagentMaxConcurrent)
		a.SetSkillsEnabled(opts.SkillsEnabled && opts.SkillsManager != nil)
		a.SetTerminalEnabled(opts.TerminalEnabled)
	}
	a.SetBoardManager(opts.BoardManager)
	a.SetSkillsManager(opts.SkillsManager)
	a.SetSubagentSpawner(opts.SubagentSpawner)
	if snap != nil {
		a.RestoreSession(*snap, id)
		// D1: model is per-session — a resumed session keeps its saved model
		// (RestoreSessionLocal already calls Provider.SetModel(snap.Model));
		// never overwrite it with the workspace default. Validate the saved
		// model asynchronously via the runtime owner after registration (see
		// loadOrCreateRuntime / sessionFork): the runtime broadcasts the
		// refresh to attached clients, and requireModelSelected surfaces the
		// gap on the first turn when the provider no longer lists the model.
		// A restored session keeps its saved thinking level
		// (RestoreSessionLocal restores it and syncs the provider). Seed the
		// workspace default only when the snapshot predates the level field
		// — and NEVER before the restore: SetThinkingLevel flushes, and a
		// flush before the restore writes an EMPTY snapshot over the
		// session's real file, wiping its persisted messages and index label
		// (the "closing an open session turns its title into a hash" bug —
		// merely opening a saved session corrupted its on-disk state until
		// the next flush with restored content).
		if snap.ThinkingLevel == "" {
			if level := NormalizeThinkingLevel(string(opts.ThinkingLevel)); level != "" {
				a.SetThinkingLevel(level)
			}
		}
	} else {
		// Fresh session: seed the workspace default thinking level so a
		// new pane's first turn does not start at the "off" default.
		if level := NormalizeThinkingLevel(string(opts.ThinkingLevel)); level != "" {
			a.SetThinkingLevel(level)
		}
	}
	return a
}
