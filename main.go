package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"gogen/internal/agent"
	"gogen/internal/automation"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
	"gogen/internal/profiling"
	"gogen/internal/projectfile"
	"gogen/internal/session"
	"gogen/internal/spill"
	"gogen/internal/statusdisplay"
	"gogen/internal/treesitter"
)

type cliFlags struct {
	web               bool
	host              string
	verbose           bool
	dir               string
	url               string
	global            bool
	saveConfig        bool
	saveConfigSecrets bool
	prompt            string
	saveConfigPath    string
}

func parseCLIOptions() (cliFlags, string) {
	opts := cliFlags{}
	flag.BoolVar(&opts.web, "web", false, "Run in Web mode")
	flag.StringVar(&opts.host, "host", "", "Listen host for --web (e.g. 0.0.0.0, default 127.0.0.1)")
	flag.BoolVar(&opts.verbose, "verbose", false, "Show full tool output in CLI mode")
	flag.StringVar(&opts.dir, "dir", "", "Specify the working directory")
	flag.StringVar(&opts.url, "url", "", "OpenAI API base URL (e.g. https://api.openai.com/v1)")
	flag.BoolVar(&opts.global, "global", false, "Run in global mode (ignore project config, use ~/.config/gogen/)")
	flag.BoolVar(&opts.saveConfig, "save-config", false, "Write effective config to .gogen/gogen.conf and guidelines to .gogen/gogen.md, then exit")
	flag.BoolVar(&opts.saveConfigSecrets, "save-config-secrets", false, "Include openai_api_key when using --save-config")
	flag.StringVar(&opts.prompt, "p", "", "Run a single prompt and exit (non-interactive)")
	flag.StringVar(&opts.saveConfigPath, "save-config-path", "", "Output path for --save-config config file (default .gogen/gogen.conf)")

	flag.Parse()

	workingDir, resolvedPrompt, err := resolvePositionalArgs(opts.dir, opts.prompt, flag.Args())
	if err != nil {
		log.Fatal(err)
	}
	opts.prompt = resolvedPrompt

	absWD, err := filepath.Abs(workingDir)
	if err != nil {
		log.Fatal(err)
	}
	return opts, absWD
}

// resolvePositionalArgs maps the positional arguments (`[dir] [prompt]`) plus
// the --dir / -p flag values onto a working directory and prompt. The first
// positional argument is the working directory, but --dir overrides it; when
// --dir is set the first positional therefore becomes the prompt instead of
// being silently discarded. A leftover prompt flag wins over a positional
// prompt, and any extra positional argument is a usage error.
func resolvePositionalArgs(dir, prompt string, args []string) (workingDir, resolvedPrompt string, err error) {
	workingDir = "."
	if dir != "" {
		workingDir = dir
	} else if len(args) > 0 {
		workingDir = args[0]
		args = args[1:]
	}
	if len(args) > 0 {
		if prompt == "" {
			prompt = args[0]
		}
		if len(args) > 1 {
			return workingDir, prompt, fmt.Errorf("usage: gogen [flags] [dir] [prompt]")
		}
	}
	return workingDir, prompt, nil
}

func handleSaveConfigFlag(opts cliFlags, isGlobalMode bool, workingDir string, cfg *config.Config, pf *projectfile.ProjectFile) bool {
	if !opts.saveConfig {
		return false
	}
	if isGlobalMode {
		if err := projectfile.SaveGlobalConfig(cfg, projectfile.WriteOptions{IncludeSecrets: opts.saveConfigSecrets}); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Wrote global config to %s\n", projectfile.GlobalConfigPath())
		fmt.Println("Note: environment variables still override file values at runtime.")
		return true
	}
	outPath := opts.saveConfigPath
	if outPath == "" {
		outPath = projectfile.DefaultSavePath(workingDir)
	} else if !filepath.IsAbs(outPath) {
		outPath = filepath.Join(workingDir, outPath)
	}
	guidelinesPath := projectfile.DefaultGuidelinesSavePath(workingDir)
	guidelines := cfg.ProjectGuidelines
	if pf != nil && guidelines == "" {
		guidelines = pf.Guidelines
	}
	if err := projectfile.SaveConfig(outPath, guidelinesPath, cfg, guidelines, projectfile.WriteOptions{IncludeSecrets: opts.saveConfigSecrets}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Wrote config to %s\n", outPath)
	fmt.Printf("Wrote guidelines to %s\n", guidelinesPath)
	fmt.Println("Note: environment variables still override file values at runtime.")
	return true
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		os.Exit(1)
	}
}

// run executes the selected mode (single prompt, web, or TUI) and returns an
// error for the single-prompt path. All deferred cleanup (agent close,
// session flush, MCP shutdown, profiling stop) is registered here so it runs
// on both the success and error paths; main only translates a returned error
// into a non-zero exit code.
func run() error {
	// The `gogen automation` subcommand tree (internal/automation) talks
	// straight to the file store: no agent, no API key, no mode setup.
	// Dispatched before everything else so its own flags (-json, --daily,
	// ...) never reach the main flag parser.
	if len(os.Args) > 1 && os.Args[1] == "automation" {
		return automation.RunCLI(os.Args[2:])
	}

	opts, workingDir := parseCLIOptions()

	profiling.Start()
	defer profiling.Stop()

	isGlobalMode := opts.global || projectfile.IsGlobalModeEnv()
	// Global mode keeps its state out of the project dir (sessions, board,
	// config, guidelines): spill trees follow the sessions they belong to.
	if isGlobalMode {
		spill.SetGlobalRoot(projectfile.GlobalSpillDir())
	}

	var verboseOverride *bool
	if opts.verbose {
		v := true
		verboseOverride = &v
	}

	var pf *projectfile.ProjectFile
	// startupNotices collects pre-TUI messages (config adoption, API-key
	// warning, agent setup) so they can be surfaced through each mode's
	// proper channel: stderr for non-interactive modes, the TUI's managed
	// render path for interactive mode (a raw line ahead of the inline
	// frame desyncs the renderer).
	var startupNotices []string
	if isGlobalMode {
		pf = projectfile.LoadGlobalConfig()
		if pf != nil {
			startupNotices = append(startupNotices, "Using global config from "+pf.Path)
		} else {
			startupNotices = append(startupNotices, "Global mode: no global config found at "+projectfile.GlobalConfigPath()+", using defaults")
		}
	} else {
		var loadErr error
		pf, loadErr = projectfile.LoadFromWorkingDir(workingDir)
		if loadErr != nil {
			log.Fatalf("project file: %v", loadErr)
		}
	}

	fileFlags := projectfile.FlagOverrides{
		WorkingDir: workingDir,
		OpenAIURL:  opts.url,
		CLIVerbose: verboseOverride,
		WebBind:    opts.host,
	}
	cfg := projectfile.Merge(pf, fileFlags)
	if pf != nil {
		cfg.ProjectGuidelines = pf.Guidelines
		cfg.ProjectFilePath = pf.Path
	}
	// A host may append servers for this process only (the VS Code editor
	// bridge). They are not written to the config file.
	cfg.HostMCPServers = config.MergeHostMCPServers(cfg.MCPServers, config.LoadHostMCPServers())

	if handleSaveConfigFlag(opts, isGlobalMode, workingDir, cfg, pf) {
		return nil
	}

	// Workspace instruction files (AGENTS.md / CLAUDE.md) are merged below
	// the project guidelines at VIEW-BUILD time from the agent's current
	// working dir (agent.EffectiveGuidelines), so a /dir or web workspace
	// change re-renders them and the content is never baked into a saved
	// .gogen/gogen.md.

	if cfg.OpenAIKey == "" {
		startupNotices = append(startupNotices, "Warning: OPENAI_API_KEY is not set. Some endpoints may require an API key.")
	}

	applyRuntimeConfig(cfg)

	// Pre-load the cl100k_base tokenizer so the first token-counting call
	// does not block on the ~2.6 MB init overhead.
	contextmgr.WarmTokenizer()

	a, restoredModel, agentNotices := newAgent(cfg, isGlobalMode)
	startupNotices = append(startupNotices, agentNotices...)
	// Background jobs (execute_command background=true) are owned by the
	// session and killed when it closes; this defer covers the TUI, CLI, and
	// web default-session agents at process exit (web session agents are
	// closed by ShutdownSessions). Idempotent.
	defer a.Close()
	// Clear the LAN status-screen row on exit (best effort, bounded) so it
	// cannot linger for its TTL after Gogen is gone.
	defer statusdisplay.Shutdown()

	mcpH := startMCP(a, cfg)
	defer closeMCP(mcpH)

	// Inherited SIG_IGN sticks across Notify unless cleared first. SIGHUP is
	// included because closing the terminal (or a dropped SSH session) sends
	// it: with no handler the runtime terminates the process immediately, so
	// no defer runs — no session flush, no ShutdownSessions — and the last
	// unsaved state is lost. Handling it routes a terminal close through the
	// same graceful shutdown as SIGINT/SIGTERM (context cancellation → the
	// mode's exit sweep). On Windows SIGHUP is defined but never delivered,
	// so this is a no-op there.
	signal.Reset(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	defer a.FlushPending()

	if opts.prompt != "" {
		printStartupNotices(startupNotices)
		// Headless: no OnModelChanged hook — the background validation only
		// refreshes the context limit / auto-selects a sole model.
		a.ValidateRestoredModelAsync(restoredModel, nil)
		return runSinglePrompt(ctx, a, opts.prompt, cfg)
	}

	if opts.web {
		printStartupNotices(startupNotices)
		return runWeb(ctx, a, cfg, restoredModel, webTokenStatePath(isGlobalMode, workingDir))
	}

	// Default: TUI mode. Notices go through the TUI's managed render path
	// (tea.Println), never raw stderr — see tui.SetStartupNotices.
	runTUI(ctx, a, cfg, restoredModel, startupNotices)
	return nil
}

// printStartupNotices writes startup notices to stderr for the non-TUI
// modes: they have no managed render surface, so direct stderr output is
// correct there and preserves the historical behavior.
func printStartupNotices(notices []string) {
	for _, n := range notices {
		fmt.Fprintln(os.Stderr, n)
	}
}

// runSinglePrompt processes one user prompt. It returns the error so the
// caller can exit non-zero after the deferred cleanup in run has executed.
// It uses the already-initialized agent and config but skips the TUI/web.
func runSinglePrompt(ctx context.Context, a *agent.Agent, prompt string, cfg *config.Config) error {
	// In single-prompt mode there is no interactive approval modal, so a
	// delete that requires approval would be blocked (safely) by the
	// ErrDeleteApprovalRequired error. Only skip the approval check when the
	// user explicitly opted out via delete_approval: off; otherwise warn so
	// the block is not surprising.
	if strings.EqualFold(cfg.DeleteApproval, "off") {
		a.Executor.SetDeleteApproval(false)
	} else if a.Executor.DeleteApprovalRequired() {
		fmt.Fprintf(os.Stderr, "Note: delete requires approval (delete_approval: %s) and is blocked in single-prompt mode; set GOGEN_DELETE_APPROVAL=off to allow deletes.\n", cfg.DeleteApproval)
	}

	// Start a fresh session: clear any restored conversation state.
	a.ResetSessionState()
	a.SetSessionID(session.NewID())
	a.SetSessionOneshot(true)
	// A one-shot run is a NEW session and must not inherit a plan mode left
	// in the auto-restored session (restore happens in newAgent, before this
	// function). Without this, `gogen -p "..."` in a directory whose last
	// session was /plan'd silently runs with the read-only tool set: the
	// model loses write_file/patch_file/execute_command and nobody sees a
	// mode indicator in a headless run. SetMode flushes, so the fresh id is
	// assigned first.
	a.SetMode(agent.ModeAct)
	a.FlushSession()

	// Automation runs report their session id through this file so the
	// scheduler can link the fired session into the run history (the
	// SessionIDFileEnv contract, see internal/automation/fire.go).
	// Best-effort: a failed write never breaks the headless run.
	if p := os.Getenv(automation.SessionIDFileEnv); p != "" {
		_ = os.WriteFile(p, []byte(a.SessionID), 0o644)
	}

	var final strings.Builder
	handlers := &llm.StreamHandlers{
		OnStart: func() {
			if cfg.CLIVerbose {
				fmt.Fprintln(os.Stderr, "─── prompt ───")
			}
		},
		OnToken: func(token string) {
			final.WriteString(token)
			if cfg.CLIVerbose {
				fmt.Print(token)
			}
		},
		OnStreamEnd: func() {
			if cfg.CLIVerbose {
				fmt.Fprintln(os.Stderr, "")
			}
		},
	}

	_, err := a.StreamProcessInput(ctx, prompt, handlers)
	if err != nil {
		return err
	}
	if !cfg.CLIVerbose {
		fmt.Println(final.String())
	}
	return nil
}

// generateToken returns a cryptographically random 32-byte hex string.
func generateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func applyRuntimeConfig(cfg *config.Config) {
	// The status-screen reporter is a passive turn observer: install it
	// unconditionally (the on/off gate lives in the reporter) so a runtime
	// toggle in the web settings modal takes effect without re-wiring
	// anything.
	agent.ConfigureTurnObserver(statusdisplay.Notify)
	switch cfg.StatusDisplayMode() {
	case config.StatusDisplayOn:
		statusdisplay.Configure(true, cfg.StatusDisplayHost, cfg.StatusDisplayToken, cfg.StatusDisplaySource, cfg.WorkingDir)
	case config.StatusDisplayBrowser:
		// Browser mode is a web-mode feature: there the server probes first
		// and the web UI is the fallback sender. The TUI/CLI has no browser
		// to fall back to, so it just probes and reports directly when the
		// display answers (the web server's own startup re-applies the full
		// browser-mode behavior when it runs).
		statusdisplay.Probe(cfg.StatusDisplayHost, cfg.StatusDisplayToken, cfg.StatusDisplaySource, cfg.WorkingDir)
	case config.StatusDisplayOff:
		statusdisplay.Configure(false, cfg.StatusDisplayHost, cfg.StatusDisplayToken, cfg.StatusDisplaySource, cfg.WorkingDir)
	default: // auto: probe for the screen, enable reporting when it answers
		statusdisplay.Probe(cfg.StatusDisplayHost, cfg.StatusDisplayToken, cfg.StatusDisplaySource, cfg.WorkingDir)
	}
	treesitter.Configure(cfg.TreeSitterEnabled(), cfg.TreeSitterLangs)
	agent.ConfigureWebFetch(cfg.WebFetchEnabled(), cfg.WebFetchMode, cfg.WebAllowedDomains)
	agent.ConfigureWebSearchEnabled(cfg.WebSearchEnabled())
	agent.ConfigureOutputSpill(cfg.OutputSpillEnabled())
	agent.ConfigureWebSearch(cfg.WebSearchBackend, cfg.WebSearchAPIKey)
	agent.ConfigureSystemPrompt(cfg.SystemPrompt)
	agent.ConfigureSubagentPrompt(cfg.SubagentPrompt)
	if cfg.DebugLog != "" || cfg.DebugSession != "" {
		debuglog.Configure(cfg.DebugLog, cfg.DebugSession)
	}
}
