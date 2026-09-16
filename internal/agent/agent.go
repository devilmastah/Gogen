package agent

import (
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// Agent is the per-session coding agent: it owns the conversation,
// the LLM provider, and the tool executor, plus the per-session state
// the agent loop, persistence, and feature tools need.
//
// The unexported state is grouped into embedded domain structs
// (sessionPersist, sessionStats, compactionState, backgroundState,
// featureWiring, subagentWiring, toolRegistry, turnCounters,
// sessionMeta), defined in the matching domain files; their fields
// are promoted, so a.field access is unchanged. New per-session
// state belongs in a domain group, not a flat field here.
type Agent struct {
	Provider llm.LLMProvider
	Executor *Executor

	// Conversation state
	Context *contextmgr.Manager
	// Messages is owned by the turn goroutine for writes (append/compact/
	// restore); lock-free readers (ContextStats, SnapshotMessages) read it
	// under statsMu. See the statsMu comment in sessionStats (context_stats.go).
	Messages []llm.Message
	// PinManager is internally synchronized (methods take their own mutex);
	// the reference itself is set at construction and never reassigned.
	PinManager *PinManager
	// TodoManager is internally synchronized (Snapshot/AddTodo/... take the
	// manager's own mutex); the reference is set at construction and never
	// reassigned. The web fork path reads it without the session turnMu on
	// purpose (snapshot is lock-free w.r.t. turnMu — see sessionFork).
	TodoManager *TodoManager

	// Session persistence
	SessionStore SessionPersister
	// SessionID is guarded by the session turnMu: TUI runs on a single owner
	// goroutine, and web lifecycle ops (new/resume/fork/delete) write it
	// under the pane's turnMu or at construction (main.go startup restore,
	// NewSessionAgent). Do not read it for a live session without holding
	// turnMu.
	SessionID string
	// SessionLabel is guarded by statsMu (setSessionLabel /
	// SessionLabelSnapshot): web probes read it without turnMu while the
	// turn goroutine may derive it from the first user message.
	SessionLabel   string
	SessionOneshot bool // true if this session was created by a single-prompt (-p) invocation
	// UsageAccum is guarded by statsMu (clearTurnUsage / snapshot reads).
	UsageAccum UsageAccumulator

	// OnModelChanged, when non-nil, is invoked after ValidateRestoredModel
	// processes a restored model so hosts can refresh their UI (the model
	// may have been cleared or replaced by sole-model auto-select). Set at
	// construction by the TUI/web hosts before the validation goroutine is
	// spawned (that direct assignment is safe: the spawn happens after it
	// in the same goroutine); called from the validation goroutine. Must
	// not block. ValidateRestoredModelAsync is the shared install-and-spawn
	// wiring; a nil callback there preserves a hook installed here, and its
	// own install is serialized with the validation read by
	// onModelChangedMu so overlapping validations cannot race the field.
	OnModelChanged func()

	// lastTurnUsage is the provider-reported usage of the last API round
	// (guarded by statsMu; see sessionStats).
	lastTurnUsage *llm.Usage

	// DebugCompareMessages enables view-fingerprint comparison across turns
	// and session restores (GOGEN_DEBUG_COMPARE_MESSAGES). Only effective in
	// binaries built with `-tags debug`; production builds compile the
	// detector out (see view_drift_release.go).
	DebugCompareMessages bool
	lastViewMessages     []llm.Message // debug builds only; unused in release

	// ThinkingLevel controls how much reasoning/thinking the model should use.
	// When "off", no thinking parameter is sent to the API.
	ThinkingLevel ThinkingLevel

	// Runtime / project
	WorkingDir        string
	Mode              Mode
	GlobalMode        bool
	ProjectGuidelines string
	ProjectFilePath   string
	TestCommand       string
	LintCommand       string
	projectProfile    string
	MCPRegistry       MCPToolRegistry

	// Domain state groups (embedded; fields promoted onto Agent).
	sessionPersist
	sessionStats
	compactionState
	backgroundState
	featureWiring
	subagentWiring
	toolRegistry
	turnCounters
	sessionMeta
	terminalState
}

func NewAgent(provider llm.LLMProvider, executor *Executor, ctxMgr *contextmgr.Manager) *Agent {
	return &Agent{
		Provider:      provider,
		Executor:      executor,
		Context:       ctxMgr,
		Messages:      []llm.Message{},
		WorkingDir:    executor.GetWorkingDir(),
		Mode:          ModeAct,
		GlobalMode:    false,
		ThinkingLevel: ThinkingOff,
		toolRegistry:  toolRegistry{toolHandlers: BuiltinToolHandlers()},
	}
}

// SetMCPRegistry attaches the MCP tool registry (nil detaches). It bumps the
// tool generation so the wire-overhead cache recomputes against the changed
// model-facing tool set; assign this field directly only if the cached
// overhead does not matter (tests).
func (a *Agent) SetMCPRegistry(reg MCPToolRegistry) {
	a.MCPRegistry = reg
	a.noteToolsChanged()
}
