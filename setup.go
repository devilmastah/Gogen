package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/codeindex"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/projectfile"
	"gogen/internal/session"
	"gogen/internal/skills"
)

// newAgent builds the provider, context manager, executor, agent, and
// session store from the merged config, then restores the most recent saved
// session and runs the legacy-todos migration. It returns the agent, the
// model name restored from disk (empty when session persistence is disabled
// or nothing was saved), and the startup notices every mode must surface:
// non-TUI modes print them to stderr, while the TUI routes them through its
// managed render path — a raw stderr line ahead of the inline-rendered,
// terminal-height frame desyncs the renderer's cell diff from the real
// screen (ghost cursors, stuck columns after scroll). The caller owns the
// agent's lifecycle defers (a.Close, a.FlushPending, MCP shutdown) so the
// existing shutdown order is preserved. The session store stays internal to
// setup: main never reads it after construction.
//
// restoreSession=false skips the auto-restore entirely. One-shot (-p) runs
// need it: runSinglePrompt resets straight to a fresh session, so decoding
// the previous conversation only to throw it away cost a full session-file
// read (and started the model-validation path against a session nobody
// continues) before the first prompt.
func newAgent(cfg *config.Config, isGlobalMode bool, restoreSession bool) (*agent.Agent, string, []string) {
	var notices []string
	// The provider carries ALL registered OpenAI-compatible profiles (the
	// legacy fields form the implicit default profile), so the TUI's /models
	// list and the web model picker both aggregate every endpoint, and each
	// model routes to its owning endpoint.
	provider := llm.NewOpenAIProviderWithProfiles(
		llm.ProviderProfiles(cfg.OpenAIKey, cfg.OpenAIModel, cfg.OpenAIURL, cfg.OpenAIProviders),
		cfg.OpenAIModel, cfg.WorkingDir, nil)

	// Derive a stable prompt-cache key from the working directory so
	// provider-side prefix caches survive sequential requests.
	provider.SetPromptCacheKey(llm.ProjectPromptCacheKey(cfg.WorkingDir))
	provider.SetPreserveReasoningMode(cfg.PreserveReasoning)

	ctxMgr := contextmgr.NewManager(provider, contextmgr.SettingsFromConfig(cfg))

	exec := agent.NewExecutorWithGuard(cfg.WorkingDir, agent.NewCommandGuard(cfg.CommandSafetyMode, agent.ParseAllowlist(cfg.CommandAllowlist)))
	exec.SetDeleteApproval(!strings.EqualFold(cfg.DeleteApproval, "off"))
	// Blocklist matches escalate to a user approval; command_approval: off
	// keeps the hard block instead of prompting.
	exec.SetCommandApproval(!strings.EqualFold(cfg.CommandApproval, "off"))
	exec.SetSandbox(cfg.CommandSandbox)
	// The executor bounds in-memory command output at the same cap the
	// context manager later applies to tool results; 0 (explicit "no cap")
	// passes through unbounded.
	exec.SetMaxToolOutputBytes(cfg.MaxToolResultBytes)
	if cfg.CommandIdleTimeoutSecs > 0 {
		exec.SetIdleTimeout(time.Duration(cfg.CommandIdleTimeoutSecs) * time.Second)
	}
	if isGlobalMode {
		// In global mode, relax the path boundary to the user's home directory.
		exec.PathBoundary = projectfile.HomeDir()
	}
	a := agent.NewAgent(provider, exec, ctxMgr)
	a.GlobalMode = isGlobalMode
	a.SetProjectContext(cfg.ProjectFilePath, cfg.ProjectGuidelines, cfg.TestCommand, cfg.LintCommand)
	a.TodoManager = agent.NewTodoManager(cfg.WorkingDir)
	a.PinManager = agent.NewPinManager()
	a.DebugCompareMessages = cfg.DebugCompareMessages
	// Feature flags: seeded from startup config (TUI/CLI path; the web
	// server re-seeds from its workspace on every NewSessionAgent and
	// supports live toggles via the config WS message).
	a.SetBoardEnabled(cfg.BoardEnabled())
	a.SetSubagentsEnabled(cfg.SubagentEnabled())
	a.SetAutomationsEnabled(cfg.AutomationsEnabled())
	a.SetSubagentMaxDepth(cfg.SubagentDepth())
	a.SetSubagentMaxConcurrent(cfg.SubagentLimit())
	if cfg.BoardEnabled() {
		// Project board: per-process manager rooted at the working dir
		// (global mode → the global board dir). Re-pointed on /dir changes
		// via AfterWorkingDirChange.
		a.SetBoardManager(agent.NewBoardManager(cfg.WorkingDir, isGlobalMode))
	}
	// Skills: per-process discovery manager (project .gogen/skills plus the
	// global config dir; global mode → user root only). Installed
	// UNCONDITIONALLY (the manager is a stateless directory pointer and only
	// scans on demand), so enabling the skills flag from the web settings
	// modal can expose the tool live — the flag, not the manager, is the
	// gate (see featureTools).
	a.SetSkillsManager(skills.NewManager(cfg.WorkingDir, isGlobalMode))
	a.SetSkillsEnabled(cfg.SkillsEnabled())
	// Persistent terminal tools (terminal_* and bash_persistent): act-mode
	// feature tools gated through featureTools(), toggled live from the web
	// settings modal.
	a.SetTerminalEnabled(cfg.TerminalEnabled())
	// Code index: off until the user turns it on. Open does not create
	// .gogen/index. The first search_meaning or /reindex is what writes it.
	a.SetCodeIndexEnabled(cfg.CodeIndexEnabled())
	a.SetCodeIndex(codeindex.Open(cfg.WorkingDir))
	a.SetCodeIndexConfig(func() config.Config {
		if cfg == nil {
			return config.Config{}
		}
		return *cfg
	})
	a.SyncCodeIndexWatch()
	// Workspace instructions (AGENTS.md/CLAUDE.md): merged below the
	// project guidelines at view-build time from the CURRENT working dir
	// (agent.EffectiveGuidelines), so /dir and web workspace changes
	// re-render them and the content is never baked into a saved
	// .gogen/gogen.md.
	a.SetInstructionsEnabled(cfg.AgentInstructionsEnabled())
	a.RefreshWorkspaceInstructions(cfg.WorkingDir)
	if cfg.DebugCompareMessages && !agent.ViewDriftCompiledIn() {
		notices = append(notices, "GOGEN_DEBUG_COMPARE_MESSAGES requires a debug build (-tags debug); ignoring")
		a.DebugCompareMessages = false
	}

	sessionEnabled := !strings.EqualFold(os.Getenv("GOGEN_SESSION_PERSIST"), "off")
	sessionOpts := session.StoreOptions{
		MaxCount:   cfg.SessionMaxCount,
		MaxAgeDays: cfg.SessionMaxAgeDays,
	}
	store := session.NewStoreWithOptions(sessionEnabled, sessionOpts)
	if isGlobalMode {
		// Use global session dir ~/.local/share/gogen/sessions/
		store.SetGlobalDir(projectfile.GlobalSessionDir())
	}
	a.SessionStore = store
	a.SetSessionID(session.NewID())
	// Local-only restore: avoid blocking startup on provider ListModels.
	var restoredModel string
	if sessionEnabled && restoreSession {
		if latest, err := store.LatestID(cfg.WorkingDir); err == nil && latest != "" {
			if snap, err := store.LoadInWorkingDir(cfg.WorkingDir, latest); err == nil {
				a.RestoreSession(snap, latest)
				restoredModel = snap.Model
				notice := fmt.Sprintf("Session %s (%d msgs)", latest, len(a.Messages))
				// The resumed mode is session state the model acts on: a
				// session left in plan mode comes back read-only (no
				// write_file/patch_file/execute_command). Say so instead of
				// resuming silently — the status bar is the only other cue,
				// and a headless -p run has none at all.
				if mode, _ := a.ModeAndThinkingLevel(); mode == agent.ModePlan {
					notice += " — resumed in PLAN mode (read-only; /act to edit)"
				}
				notices = append(notices, notice)
			}
		}
	}
	// One-time migration: adopt project-global todos.json into the current
	// session when it has no todos yet, then rename the legacy file.
	if a.ImportLegacyTodos() {
		notices = append(notices, fmt.Sprintf("Migrated legacy todos into session %s", a.SessionID))
	}
	if name := provider.ModelName(); name != "" {
		notices = append(notices, "Model: "+name)
	} else {
		notices = append(notices, "No model selected; use /models to choose")
	}
	return a, restoredModel, notices
}
