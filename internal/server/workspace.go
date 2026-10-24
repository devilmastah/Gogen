package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/modelinfo"
	"gogen/internal/session"
	"gogen/internal/skills"
)

// Workspace is the shared, session-independent state of the web server
// (multi-session plan §2). Every session in one web process shares a single
// Workspace: the executor (working dir + secure path + command guard), the
// session store, the config, the MCP registry, and a provider factory. The
// factory creates a fresh provider per session (each owns its own
// model/thinking-level state, E6) seeded from the workspace default model
// and thinking level (D1 — the model is per-session).
type Workspace struct {
	Exec   *agent.Executor
	Store  *session.Store
	Config *config.Config

	GlobalMode bool
	// Project context shared by all session agents.
	ProjectFilePath   string
	ProjectGuidelines string
	TestCommand       string
	LintCommand       string

	DebugCompareMessages bool
	// MCPRegistry is shared by all session agents (thread-safe, E20).
	MCPRegistry agent.MCPToolRegistry

	// Model is the workspace default model: new providers are seeded
	// with it. set_model never mutates it — each pane's model lives on its
	// own provider instance. The field is written once at construction and
	// again by the web startup validation goroutine (runWeb after
	// ValidateRestoredModel resolves the effective model, possibly
	// auto-selecting a sole model or clearing a stale one), while
	// ProviderFactory reads it concurrently when new sessions are created —
	// a plain field access raced those readers (data race under -race).
	// Production access must go through DefaultModel/SetDefaultModel; the
	// field stays exported only so existing tests can read it directly.
	Model string
	// modelMu guards Model. Leaf lock: never held while acquiring another
	// workspace lock; provider construction reads it under RLock.
	modelMu sync.RWMutex
	// ThinkingLevel is the workspace-level default thinking level new
	// sessions start with (per-session afterwards).
	ThinkingLevel string

	// featureFlags is the SINGLE live store for the board / subagent
	// feature flags. Every session agent spawned by NewSessionAgent reads
	// this same instance (agent.SetFeatureFlags), so a web settings toggle
	// (config WS message) is visible to all sessions immediately — no
	// per-agent mirror and no sweep. All values are atomic: the toggle
	// writes them while per-turn tool derivation reads them concurrently.
	// Lazily created for workspaces built as bare &Workspace{} literals
	// (tests skip newWorkspaceFromAgent).
	featureFlags atomic.Pointer[agent.FeatureFlags]

	// boardManager is the single shared project board for this workspace
	// (all session agents share one instance, so claims and moves serialize
	// in-process). Created when the board feature is enabled; kept (with its
	// data) when disabled so re-enabling restores the board.
	boardManagerMu sync.RWMutex
	boardManager   *agent.BoardManager

	// skillsManager is the shared skill discovery manager for this
	// workspace. Skills is config-only in v1 (no live toggle): the manager
	// is set once at construction and never reassigned, so plain-field
	// reads at session creation are race-free.
	skillsManager *skills.Manager

	// jobNotices mirrors the config job_notices flag (config-only, set at
	// construction). jobNoticeDeliverer is installed by NewServer (it needs
	// the registry) and resolves a session id to its live runtime at fire
	// time; NewSessionAgent installs the per-agent hook on top of it.
	jobNotices         bool
	jobNoticeDeliverer func(agentID, summary string)

	// terminalEnabled mirrors the config terminal flag (config-only, set at
	// construction). Every session agent spawned by NewSessionAgent is seeded
	// with it, so the persistent-terminal feature tools are on/off uniformly.
	terminalEnabled bool

	// BoardChangedHook is invoked after any board mutation made through a
	// session agent's board tool, with the mutation's output message; the
	// web server sets it to broadcast a fresh board_state and a success
	// notice (toast) to every client. Set once at construction (before
	// any session is created), read at session creation — plain field, like
	// MCPRegistry.
	BoardChangedHook func(msg string)

	// SubagentSpawner runs nested sessions for session agents. The web
	// server installs it at construction (it needs the registry); set once
	// before any session is created, read at session creation.
	SubagentSpawner agent.SubagentSpawner

	// ProviderFactory creates a per-session provider. It must seed the
	// provider with the workspace Model + ThinkingLevel so a new pane's
	// first turn never fails requireModelSelected.
	ProviderFactory func() llm.LLMProvider

	// Resolver is the shared models.dev resolver. One per workspace so N
	// per-session providers do not issue N parallel catalog fetches or race
	// N atomic writes to the same .gogen/models.json cache file.
	Resolver *modelinfo.Resolver

	// EndpointCatalog is the shared per-endpoint /v1/models cache (see
	// llm.EndpointCatalog). One per workspace so N per-session providers
	// download an endpoint's catalog once per TTL window instead of once per
	// session — the same request repeated N times against an endpoint that
	// may be serializing generations. Set once at construction; read at
	// provider construction — plain field, like Resolver.
	EndpointCatalog *llm.EndpointCatalog

	// PropsCapsCache is the shared llama.cpp /props capability cache (see
	// llm.PropsCapsCache). One per workspace: the capability belongs to the
	// endpoint (+model), so N per-session providers dial it once instead of
	// once each — every probe a fresh TCP connection. Set once at
	// construction; read at provider construction — plain field, like
	// Resolver.
	PropsCapsCache *llm.PropsCapsCache

	// wdMu guards WorkingDir. The field is written by handleWSConfig on one
	// connection's WS read loop (working-dir change) and read concurrently
	// from other connections' read loops (session ops, NewSessionAgent) and
	// from per-session provider construction — a plain field access raced
	// those readers (data race under -race). Production access must go
	// through GetWorkingDir/SetWorkingDir; the field stays exported only so
	// existing tests can read it directly.
	wdMu       sync.RWMutex
	WorkingDir string

	// OpenAIProviders is the live registered OpenAI-compatible provider list
	// (the additional profiles beyond the implicit default built from the
	// legacy config fields). The web provider_save/provider_delete handlers
	// write it while the provider factory reads it concurrently at session
	// creation — a plain field access raced those readers (data race under
	// -race). Production access must go through Get/SetOpenAIProviders; the
	// field stays exported only so existing tests can read it directly.
	providerMu      sync.RWMutex
	OpenAIProviders []config.OpenAIProviderConfig

	// runtime is the live-adjustable config overlay (the web settings
	// modal): seeded from the startup config at construction and updated by
	// the runtime-config WS handler. It is the source of truth for the
	// settings push, for new-session seeding (NewSessionAgent), and for
	// persistence (effectiveConfig); the runtime TARGETS (executor,
	// process globals, per-session context managers, session store) are
	// applied separately by the handler. OpenAIProviders is NOT read from
	// here — ws.OpenAIProviders is authoritative (see above).
	runtimeMu sync.RWMutex
	runtime   config.Config

	// fsMu serializes filesystem-mutating operations across sessions:
	// editor writes (fs_write/fs_replace/fs_apply_patch) and agent tools
	// that mutate the tree. Lock order: session turnMu → fsMu; never the
	// reverse (editor writes take fsMu alone).
	fsMu sync.RWMutex
}

// GetWorkingDir returns the workspace-wide working directory.
// Thread-safe: read by session lifecycle ops and provider construction on
// any connection's read loop while handleWSConfig may update it.
func (ws *Workspace) GetWorkingDir() string {
	if ws == nil {
		return ""
	}
	ws.wdMu.RLock()
	defer ws.wdMu.RUnlock()
	return ws.WorkingDir
}

// SetWorkingDir updates the workspace-wide working directory.
// Thread-safe. Does NOT touch the shared executor or any session agent —
// handleWSConfig applies the change to every session agent (under each
// session's turn lock) via applyWorkingDirToAll.
func (ws *Workspace) SetWorkingDir(dir string) {
	if ws == nil {
		return
	}
	ws.wdMu.Lock()
	ws.WorkingDir = dir
	ws.wdMu.Unlock()
}

// DefaultModel returns the workspace default model new session providers
// are seeded from. Thread-safe: read by provider construction on any
// connection's read loop while the web startup validation goroutine may
// update it after resolving the effective model.
func (ws *Workspace) DefaultModel() string {
	if ws == nil {
		return ""
	}
	ws.modelMu.RLock()
	defer ws.modelMu.RUnlock()
	return ws.Model
}

// SetDefaultModel updates the workspace default model. Called after startup
// model validation (runWeb) so new sessions seed from the resolved model —
// including "" when validation cleared a stale restored model, so a new
// pane's first turn surfaces the "no model selected" gap instead of
// inheriting an invalid model. Also called by the settings modal's
// default-profile save (provider_save) so a live default-model edit seeds
// new sessions immediately, like the base-URL/key edits.
func (ws *Workspace) SetDefaultModel(name string) {
	if ws == nil {
		return
	}
	ws.modelMu.Lock()
	ws.Model = name
	ws.modelMu.Unlock()
}

// fsMutatingTools are agent tools that mutate the working tree. They take the
// workspace fsMu during execution (lock order: turnMu → fsMu). Read-only
// tools and non-FS tools (execute_command, git mutations, web fetch/search)
// are deliberately NOT classified here: git mutations are serialized by the
// executor/git itself, and execute_command must not block editor saves for
// its whole runtime (multi-session plan §2.4).
//
// The set is derived from the MutatesFS flag in the agent tool registry
// (agent.FSMutatingToolNames), so a mutating tool is locked by declaration in
// its ToolDef — no second hand-maintained map to drift.
var fsMutatingTools = func() map[string]bool {
	names := agent.FSMutatingToolNames()
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}()

// wrapToolHandlers returns a copy of handlers in which every FS-mutating tool
// acquires fsMu for the duration of its execution. Non-mutating handlers are
// passed through untouched.
func wrapToolHandlers(handlers map[string]agent.ToolHandler, fsMu *sync.RWMutex) map[string]agent.ToolHandler {
	out := make(map[string]agent.ToolHandler, len(handlers))
	for name, h := range handlers {
		if !fsMutatingTools[name] {
			out[name] = h
			continue
		}
		name, h := name, h
		out[name] = func(ctx context.Context, a *agent.Agent, args map[string]any) (string, error) {
			fsMu.Lock()
			defer fsMu.Unlock()
			return h(ctx, a, args)
		}
	}
	return out
}

// flags returns the shared feature-flag store, lazily creating it for
// workspaces built outside newWorkspaceFromAgent (bare &Workspace{} test
// literals).
func (ws *Workspace) flags() *agent.FeatureFlags {
	if f := ws.featureFlags.Load(); f != nil {
		return f
	}
	f := agent.NewFeatureFlags(false, false, 0, 0)
	if !ws.featureFlags.CompareAndSwap(nil, f) {
		f = ws.featureFlags.Load()
	}
	return f
}

// FeatureFlags returns the shared flag store every session agent reads —
// the single source of truth for the live board / subagent feature flags.
func (ws *Workspace) FeatureFlags() *agent.FeatureFlags {
	return ws.flags()
}

// GetBoardEnabled returns the live board feature flag.
func (ws *Workspace) GetBoardEnabled() bool {
	return ws.flags().BoardEnabled()
}

// SetBoardEnabled updates the live board feature flag. The web settings
// toggle writes the shared store every session agent reads — the toggle is
// visible to all sessions immediately, no sweep.
func (ws *Workspace) SetBoardEnabled(on bool) {
	ws.flags().SetBoardEnabled(on)
}

// GetAutomationsEnabled returns the live automations feature flag (host
// scheduler gate; the scheduler start/stop rides on the server).
func (ws *Workspace) GetAutomationsEnabled() bool {
	return ws.flags().AutomationsEnabled()
}

// SetAutomationsEnabled updates the live automations feature flag (shared
// store, see SetBoardEnabled).
func (ws *Workspace) SetAutomationsEnabled(on bool) {
	ws.flags().SetAutomationsEnabled(on)
}

// GetSubagentEnabled returns the live subagent feature flag.
func (ws *Workspace) GetSubagentEnabled() bool {
	return ws.flags().SubagentsEnabled()
}

// SetSubagentEnabled updates the live subagent feature flag (shared store,
// see SetBoardEnabled).
func (ws *Workspace) SetSubagentEnabled(on bool) {
	ws.flags().SetSubagentsEnabled(on)
}

// GetReviewAgentEnabled returns the live board auto-review flag.
func (ws *Workspace) GetReviewAgentEnabled() bool {
	return ws.flags().ReviewAgentEnabled()
}

// SetReviewAgentEnabled updates the live board auto-review flag (shared
// store, see SetBoardEnabled). The trigger re-checks it on every fire, so
// the settings toggle is live.
func (ws *Workspace) SetReviewAgentEnabled(on bool) {
	ws.flags().SetReviewAgentEnabled(on)
}

// GetSubagentMaxDepth returns the live subagent nesting-depth limit.
func (ws *Workspace) GetSubagentMaxDepth() int {
	return ws.flags().SubagentMaxDepth()
}

// SetSubagentMaxDepth updates the live subagent nesting-depth limit.
func (ws *Workspace) SetSubagentMaxDepth(depth int) {
	ws.flags().SetSubagentMaxDepth(depth)
}

// GetSubagentMaxConcurrent returns the live per-parent concurrent-subagent
// limit (0 = unset; readers resolve the effective value via
// config.DefaultSubagentMaxConcurrent).
func (ws *Workspace) GetSubagentMaxConcurrent() int {
	return ws.flags().SubagentMaxConcurrent()
}

// SetSubagentMaxConcurrent updates the live per-parent concurrent-subagent
// limit.
func (ws *Workspace) SetSubagentMaxConcurrent(n int) {
	ws.flags().SetSubagentMaxConcurrent(n)
}

// GetBoardManager returns the shared project board manager (nil when the
// board feature has never been enabled).
func (ws *Workspace) GetBoardManager() *agent.BoardManager {
	ws.boardManagerMu.RLock()
	defer ws.boardManagerMu.RUnlock()
	return ws.boardManager
}

// ensureBoardManager returns the shared board manager, creating it (rooted
// at the current working dir / global board dir) on first use. Callers that
// enable the board feature must call this before seeding session agents.
func (ws *Workspace) ensureBoardManager() *agent.BoardManager {
	ws.boardManagerMu.Lock()
	defer ws.boardManagerMu.Unlock()
	if ws.boardManager == nil {
		ws.boardManager = agent.NewBoardManager(ws.GetWorkingDir(), ws.GlobalMode)
	}
	return ws.boardManager
}

// providerListFromConfig returns a copy of the config's registered provider
// list (nil-safe; the workspace seeds its live list from it at
// construction).
func providerListFromConfig(cfg *config.Config) []config.OpenAIProviderConfig {
	if cfg == nil {
		return nil
	}
	return append([]config.OpenAIProviderConfig(nil), cfg.OpenAIProviders...)
}

// runtimeSeed returns the initial live-adjustable config overlay: a copy of
// the startup config (nil-safe — zero config for tests).
func runtimeSeed(cfg *config.Config) config.Config {
	if cfg == nil {
		return config.Config{}
	}
	return *cfg
}

// GetRuntimeConfig returns a copy of the live-adjustable config overlay.
// Thread-safe: read by the provider factory, session creation, the config
// push, and persistence while the settings modal may update it.
func (ws *Workspace) GetRuntimeConfig() config.Config {
	if ws == nil {
		return config.Config{}
	}
	ws.runtimeMu.RLock()
	defer ws.runtimeMu.RUnlock()
	return ws.runtime
}

// SetRuntimeConfig replaces the live-adjustable config overlay (the
// runtime-config WS handler builds a modified copy and swaps it in).
// Thread-safe.
func (ws *Workspace) SetRuntimeConfig(c config.Config) {
	if ws == nil {
		return
	}
	ws.runtimeMu.Lock()
	ws.runtime = c
	ws.runtimeMu.Unlock()
}

// ApprovalHold returns the configured approval-hold window (0 = deny
// pending approvals immediately when the last client detaches). Reads the
// live runtime overlay (web_approval_hold_secs).
func (ws *Workspace) ApprovalHold() time.Duration {
	if ws == nil {
		return 0
	}
	secs := ws.GetRuntimeConfig().WebApprovalHoldSecs
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// GetOpenAIProviders returns a copy of the live registered provider list
// (the additional profiles beyond the implicit default). Thread-safe: read
// by the provider factory at session creation while the web settings modal
// may update it.
func (ws *Workspace) GetOpenAIProviders() []config.OpenAIProviderConfig {
	if ws == nil {
		return nil
	}
	ws.providerMu.RLock()
	defer ws.providerMu.RUnlock()
	return append([]config.OpenAIProviderConfig(nil), ws.OpenAIProviders...)
}

// SetOpenAIProviders replaces the live registered provider list
// (provider_save/provider_delete). Thread-safe.
func (ws *Workspace) SetOpenAIProviders(providers []config.OpenAIProviderConfig) {
	if ws == nil {
		return
	}
	ws.providerMu.Lock()
	ws.OpenAIProviders = append([]config.OpenAIProviderConfig(nil), providers...)
	ws.providerMu.Unlock()
}

// providerProfiles returns the registered OpenAI-compatible provider list
// for the provider factory: the implicit default profile from the workspace
// runtime overlay's legacy fields (live-editable via provider_save
// "default"), followed by the live additional providers (in order — profile
// order is the duplicate-model-ID precedence).
func (ws *Workspace) providerProfiles() []llm.ProviderProfile {
	r := ws.GetRuntimeConfig()
	// The live additional-provider list (ws.OpenAIProviders) is
	// authoritative — the runtime overlay's OpenAIProviders copy goes stale
	// (provider_save writes the workspace store, never the overlay).
	return llm.ProviderProfiles(r.OpenAIKey, r.OpenAIModel, r.OpenAIURL, ws.GetOpenAIProviders())
}

// subagentDepthFrom resolves the effective nesting-depth default for a
// possibly-nil config (NewServer tolerates nil configs in tests).
func subagentDepthFrom(cfg *config.Config) int {
	if cfg == nil {
		return config.DefaultSubagentMaxDepth
	}
	return cfg.SubagentDepth()
}

// subagentLimitFrom resolves the effective concurrent-subagent limit for a
// possibly-nil config (NewServer tolerates nil configs in tests).
func subagentLimitFrom(cfg *config.Config) int {
	if cfg == nil {
		return config.DefaultSubagentMaxConcurrent
	}
	return cfg.SubagentLimit()
}

// buildToolHandlers returns the agent tool handler map for the workspace:
// the builtin handlers wrapped with the workspace fsMu (so concurrent
// sessions serialize file mutations). The feature-gated tools (board,
// subagent) are deliberately NOT in the map — the agent package routes them
// explicitly in executeTool under the atomic feature flags, so a live
// toggle needs no handler-map rebuild.
func (ws *Workspace) buildToolHandlers() map[string]agent.ToolHandler {
	handlers := wrapToolHandlers(agent.BuiltinToolHandlers(), &ws.fsMu)
	return handlers
}

// NewSessionAgent creates a fresh session agent from the workspace: a new
// per-session provider + context manager, sharing the workspace executor,
// store, MCP registry, and fsMu-wrapped tool handlers. When snap is non-nil
// the agent is restored from it under the given id (multi-session plan §2,
// session agent factory). The wiring lives in agent.NewSessionAgent (shared
// with the TUI subagent spawner, D9); this wrapper supplies the
// workspace-specific pieces: the provider factory, the fsMu wrap, and the
// live feature flags.
func (ws *Workspace) NewSessionAgent(snap *agent.SessionSnapshot, id string) *agent.Agent {
	// The session seeds its context-manager settings from the LIVE runtime
	// overlay (not the immutable startup config), so a settings-modal
	// context change applies to sessions created afterwards too.
	runtimeCfg := ws.GetRuntimeConfig()
	opts := agent.SessionAgentOptions{
		Provider:          ws.ProviderFactory(),
		Executor:          ws.Exec,
		Config:            &runtimeCfg,
		GlobalMode:        ws.GlobalMode,
		ProjectFilePath:   ws.ProjectFilePath,
		ProjectGuidelines: ws.ProjectGuidelines,
		TestCommand:       ws.TestCommand,
		LintCommand:       ws.LintCommand,
		WorkingDir:        ws.GetWorkingDir(),
		MCPRegistry:       ws.MCPRegistry,
		// Exposure policy comes from the STARTUP config, not the runtime
		// overlay and not the live registry. The MCP manager's lifecycle is
		// bound to startup config (the settings modal marks `mcp` as
		// restart-scoped), so a live toggle must not add or remove the gateway
		// while the servers keep running (or stay stopped) underneath; and a
		// registry-derived answer would move the request prefix mid-session,
		// because the tool set and system prompt lead it.
		MCPConfigured:        ws.Config.MCPActive(),
		MCPNested:            ws.Config.MCPNested(),
		ToolHandlers:         ws.buildToolHandlers(),
		DebugCompareMessages: ws.DebugCompareMessages,
		ThinkingLevel:        agent.ThinkingLevel(ws.ThinkingLevel),
		FeatureFlags:         ws.flags(),
		BoardManager:         ws.GetBoardManager(),
		SkillsManager:        ws.skillsManager,
		InstructionsEnabled:  runtimeCfg.AgentInstructionsEnabled(),
		SubagentSpawner:      ws.SubagentSpawner,
		TerminalEnabled:      ws.terminalEnabled,
	}
	if ws.Store != nil {
		// Assigned conditionally on purpose: ws.Store is a concrete
		// *session.Store, and a nil one stored in the SessionPersister
		// interface is NOT nil — it would slip past every persistence guard
		// and dereference a nil receiver on the session's first flush (the
		// bootstrap path from a host built without a store).
		opts.Store = ws.Store
	}
	a := agent.NewSessionAgent(opts, snap, id)
	a.SetOnBoardChanged(ws.BoardChangedHook)
	if ws.jobNotices && ws.jobNoticeDeliverer != nil {
		// Job-completion notices: the hook resolves THIS session's live
		// runtime at fire time (the deliverer is registry-backed), so a
		// notice never targets a stale runtime.
		sid := a.SessionID
		a.SetJobNoticeHook(func(summary string) {
			ws.jobNoticeDeliverer(sid, summary)
		})
	}
	return a
}

// NewWorkspaceForHost builds a Workspace around an existing default-session
// agent. It is the embedding seam for non-HTTP hosts (the TUI): the returned
// workspace shares the web lifecycle core — NewSessionAgent seeding from the
// shared store/board/skills/MCP managers, the live registered-provider list,
// and runtime config — so both frontends drive one implementation.
// NewServer builds its workspace via newWorkspaceFromAgent directly; this
// wrapper exists so non-HTTP hosts get the same core without the HTTP
// surface.
func NewWorkspaceForHost(a *agent.Agent, cfg *config.Config) *Workspace {
	return newWorkspaceFromAgent(a, cfg)
}

// newWorkspaceFromAgent builds a Workspace that shares the given agent's
// executor and store. Used by NewServer while the server hosts the default
// session (the agent itself is registered directly; NewSessionAgent is only
// exercised once later phases create additional sessions). The provider
// factory seeds each new provider from the agent's current model/thinking.
func newWorkspaceFromAgent(a *agent.Agent, cfg *config.Config) *Workspace {
	ws := &Workspace{
		Exec:                 a.Executor,
		Config:               cfg,
		GlobalMode:           a.GlobalMode,
		ProjectFilePath:      a.ProjectFilePath,
		ProjectGuidelines:    a.ProjectGuidelines,
		TestCommand:          a.TestCommand,
		LintCommand:          a.LintCommand,
		DebugCompareMessages: a.DebugCompareMessages,
		MCPRegistry:          a.MCPRegistry,
		Model:                a.CurrentModel(),
		ThinkingLevel:        string(a.ThinkingLevel),
		WorkingDir:           a.Executor.GetWorkingDir(),
		jobNotices:           cfg != nil && cfg.JobNoticesEnabled(),
		terminalEnabled:      cfg != nil && cfg.TerminalEnabled(),
		OpenAIProviders:      providerListFromConfig(cfg),
		runtime:              runtimeSeed(cfg),
	}
	// The single shared flag store: every session agent spawned by
	// NewSessionAgent reads this instance (no per-agent mirror, no sweep).
	ws.featureFlags.Store(agent.NewFeatureFlags(
		cfg != nil && cfg.BoardEnabled(),
		cfg != nil && cfg.SubagentEnabled(),
		subagentDepthFrom(cfg),
		subagentLimitFrom(cfg),
	))
	if cfg != nil {
		ws.flags().SetReviewAgentEnabled(cfg.ReviewAgentEnabled())
		ws.flags().SetAutomationsEnabled(cfg.AutomationsEnabled())
	}
	if cfg != nil && cfg.BoardEnabled() {
		// The workspace owns the single board manager; session agents are
		// seeded from it in NewSessionAgent.
		ws.boardManager = agent.NewBoardManager(ws.WorkingDir, ws.GlobalMode)
	}
	if a.SkillsManager() != nil {
		// The workspace shares the agent's skill manager (setup.go created
		// it when skills is enabled); session agents are seeded from it in
		// NewSessionAgent.
		ws.skillsManager = a.SkillsManager()
	}
	if st, ok := a.SessionStore.(*session.Store); ok {
		ws.Store = st
	}
	ws.Resolver = modelinfo.NewResolver(modelinfo.CachePath(ws.WorkingDir))
	if _, ok := a.Provider.(*llm.OpenAIProvider); ok {
		// Production path (main.go): every session gets a fresh provider
		// seeded with the workspace default model + thinking level, sharing
		// the workspace models.dev resolver (E6, D1). The provider carries
		// ALL registered OpenAI-compatible profiles (the legacy fields form
		// the implicit default), so every model in the picker routes to its
		// owning endpoint.
		// One shared catalog cache per workspace: the endpoint's /v1/models
		// is a property of the endpoint, so N sessions must not download it
		// N times (E6 sibling of Resolver).
		ws.EndpointCatalog = llm.NewEndpointCatalog()
		// ...and for the /props capability probe: one dial per endpoint+model
		// per process, not per session.
		ws.PropsCapsCache = llm.NewPropsCapsCache()
		ws.ProviderFactory = func() llm.LLMProvider {
			wd := ws.GetWorkingDir()
			r := ws.GetRuntimeConfig()
			p := llm.NewOpenAIProviderWithProfiles(ws.providerProfiles(), r.OpenAIModel, wd, ws.Resolver)
			p.SetEndpointCatalog(ws.EndpointCatalog)
			p.SetPropsCapsCache(ws.PropsCapsCache)
			p.SetPromptCacheKey(llm.ProjectPromptCacheKey(wd))
			p.SetPreserveReasoningMode(r.PreserveReasoning)
			if m := ws.DefaultModel(); m != "" {
				_ = p.SetModel(m)
			}
			p.SetThinkingLevel(ws.ThinkingLevel)
			return p
		}
		// The default session's provider joins the shared catalog cache too:
		// the session hosting the workspace must not download the catalog
		// its siblings will reuse.
		if p, ok := a.Provider.(*llm.OpenAIProvider); ok {
			p.SetEndpointCatalog(ws.EndpointCatalog)
			p.SetPropsCapsCache(ws.PropsCapsCache)
		}
	} else {
		// Test/embed path: the agent was built with a custom provider (mock
		// or stub) that has no mutable model/thinking state to isolate, so
		// new sessions share it. E6's per-session isolation only matters for
		// the real OpenAIProvider.
		ws.ProviderFactory = func() llm.LLMProvider {
			return a.Provider
		}
	}
	return ws
}
