package tui

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"

	"gogen/internal/agent"
	"gogen/internal/codeindex"
	"gogen/internal/config"
	"gogen/internal/server"

	tea "charm.land/bubbletea/v2"
)

const assistantLabel = "GoGen:"
const userLabel = "You:"

// noticeLabel prefixes system-delivered messages (job notices, subagent
// reports): they are injected as user messages in the transcript, but
// rendering them as "You:" would imply the user typed them.
const noticeLabel = "Notice:"

// TUI runs the terminal UI.
type TUI struct {
	agent *agent.Agent
	cfg   *config.Config

	// workspace is the shared web lifecycle core (server.NewWorkspaceForHost):
	// additional live sessions are spawned through its NewSessionAgent so
	// both frontends drive one implementation. May be nil (tests/embedders).
	workspace *server.Workspace

	// spawner is the ONE nested-session runner of this host. It is installed
	// on the workspace (so every session the workspace creates — opened panes,
	// resumed sessions, and the children themselves — inherits it) and on the
	// root agent, exactly like the web server's workspace spawner. The Model
	// attaches its /subagents list when it is built. Nil when subagents are
	// disabled.
	spawner *tuiSubagentSpawner

	// program is the running Bubble Tea program, stored atomically: it is
	// written in Run and read by ForceRender from the background
	// validation goroutine.
	program atomic.Pointer[tea.Program]
	// modelChanged records a background model change (restore validation)
	// that arrived before Run started, so the first render reflects it.
	modelChanged atomic.Bool
	// startupNotices holds setup-phase messages (model selection, session
	// restore) surfaced through the managed render path instead of raw
	// stderr: the inline renderer owns every row of the terminal-height
	// frame, and an untracked line above it desyncs the cell diff from the
	// real screen (ghost cursors, stale columns). Set before Run via
	// SetStartupNotices; delivered via Println in order once the program's
	// event loop starts consuming.
	startupNotices []string
}

// New creates a new TUI runner.
func New(a *agent.Agent, cfg *config.Config) *TUI {
	return NewWithWorkspace(a, cfg, nil)
}

// NewWithWorkspace is New with the shared web lifecycle core attached;
// /open spawns additional live sessions through it. A nil workspace keeps
// single-session behavior (New callers, tests).
func NewWithWorkspace(a *agent.Agent, cfg *config.Config, ws *server.Workspace) *TUI {
	t := &TUI{agent: a, cfg: cfg, workspace: ws}
	// Nested sessions run through one host-level spawner, wired here rather
	// than per Model: the workspace seeds it into every session it creates
	// (like the web server) and the root agent gets it explicitly (it was
	// built by setup, not by the workspace). A nil workspace keeps the
	// per-Model fallback (tests, single-session embeds).
	if a != nil && cfg != nil && cfg.SubagentEnabled() {
		t.spawner = newTUISubagentSpawner(cfg, ws)
		if ws != nil {
			ws.SubagentSpawner = t.spawner
		}
		a.SetSubagentSpawner(t.spawner)
	}
	// Background validation (ValidateRestoredModel) may clear or
	// auto-select a restored session's model after the UI opened; hook it
	// so the status bar re-renders even while the terminal is idle.
	if a != nil {
		a.OnModelChanged = t.ForceRender
	}
	// The job-notice hook is installed in Run: it needs the root
	// liveSession (focus gate + replay buffer), which exists only once the
	// Model is built. A job finishing before Run is dropped (at-most-once,
	// same as when the program had not started yet).
	return t
}

// installJobNoticeHook makes the root session's job-completion notices
// focus-aware (the same contract openNewLiveSession applies to opened
// sessions): while the root slot is focused, the summary runs a normal
// delivery turn via deliver; while it is backgrounded, the summary is
// buffered as an attributed condensed note on the root slot and surfaced
// when focus returns (switchToLive). No-op unless job_notices is
// explicitly enabled and the model hosts a live registry.
func (t *TUI) installJobNoticeHook(m *Model, deliver func(summary string)) {
	if t.agent == nil || t.cfg == nil || !t.cfg.JobNoticesEnabled() || m == nil || m.lives == nil {
		return
	}
	root := m.lives.Active()
	t.agent.SetJobNoticeHook(jobNoticeHookFor(root, deliver))
}

// SetStartupNotices stores pre-TUI messages (setup banners) that Run
// delivers through tea's managed above-frame channel rather than raw
// stderr, whose untracked lines ahead of the inline frame desync the
// renderer. Call before Run.
func (t *TUI) SetStartupNotices(notices []string) {
	t.startupNotices = notices
}

// ForceRender schedules a re-render after a background state change (e.g.
// model validation) altered what the status bar shows. Safe to call from any
// goroutine; when the program is not running yet the change is drained once
// Run starts.
func (t *TUI) ForceRender() {
	t.modelChanged.Store(true)
	if p := t.program.Load(); p != nil {
		p.Send(modelChangedMsg{})
	}
}

// Run starts the Bubble Tea program loop.
// Alt-screen is off so terminal scrollback and Shift+click text selection work.
// Mouse reporting is on so the viewport handles wheel scrolls.
func (t *TUI) Run(ctx context.Context) {
	m := NewModel(t.agent, t.cfg)
	m.ctx = ctx
	m.workspace = t.workspace
	m.spawner = t.spawner

	// From here until p.Run returns, the inline renderer owns the terminal:
	// divert every diagnostic write away from it (see divertTerminalWrites
	// for the corruption this prevents and which call sites are affected).
	// Installed before the program exists so setup-time logging is covered
	// too. GOGEN_TUI_LOG diverts to a file instead of discarding.
	restoreTerminalWrites := divertTerminalWrites()
	defer func() {
		// Idempotent; the explicit call below already restores so that
		// teardown diagnostics reach the real stderr.
		restoreTerminalWrites()
	}()

	// v2: mouse reporting is requested declaratively from View()
	// (MouseModeCellMotion), not via a program option.
	p := tea.NewProgram(
		m,
		tea.WithContext(ctx),
	)

	m.program = p
	t.program.Store(p)
	if a := t.agent; a != nil {
		if ix := a.CodeIndex(); ix != nil {
			ix.SetProgress(func(prog codeindex.Progress) {
				if pr := t.program.Load(); pr != nil {
					pr.Send(codeIndexProgressMsg{line: prog.Line(), phase: string(prog.Phase), err: prog.Error})
				}
			})
		}
	}
	// The lifecycle reaches the Update thread through the program: install the
	// sender now that it exists (before the event loop starts, so no turn can
	// observe a half-wired spawner).
	sender := hostSender{p: p, updating: &m.inUpdate, stopped: &m.programStopped}
	if t.spawner != nil {
		t.spawner.setSender(func() programSender { return sender }, &m.programStopped)
	}
	if m.spawner != nil && m.spawner != t.spawner {
		m.spawner.setSender(func() programSender { return sender }, &m.programStopped)
	}
	// Startup notices must never hit the terminal as raw stderr writes: the
	// inline renderer owns every row of the terminal-height frame, and an
	// untracked line above it forces unaccounted scrolls that desync the
	// cell diff from the real screen (ghost cursors, stuck columns).
	// Println sends on the program's unbuffered message channel, so this
	// goroutine delivers the notices in order as soon as Run's event loop
	// starts consuming; if the program exits first, process teardown
	// reclaims the blocked send.
	if len(t.startupNotices) > 0 {
		notes := t.startupNotices
		go func() {
			for _, n := range notes {
				p.Println(n)
			}
		}()
	}
	// Job-completion notices are focus-aware: a focused root session gets
	// the normal delivery turn (deliveryRequestMsg on the tea loop); a
	// backgrounded one buffers an attributed condensed note that
	// switchToLive surfaces when focus returns. The delivery names the ROOT
	// slot (the hook's session, captured here on the Update thread), so the
	// notice lands on its own queue even if focus moved before tea delivers
	// the message.
	if m.lives != nil {
		rootID := m.lives.Active().id
		t.installJobNoticeHook(m, func(summary string) {
			p.Send(deliveryRequestMsg{sid: rootID, text: summary})
		})
	}
	// A validation finishing before Run started would have missed the
	// program; drain the pending flag so the first render already reflects
	// the change.
	if t.modelChanged.Swap(false) {
		p.Send(modelChangedMsg{})
	}

	_, runErr := p.Run()
	// The loop is gone: host posts from the teardown sweep below would block
	// forever on the program's channel.
	m.programStopped.Store(true)
	// The renderer has released the terminal, so restore the real stderr
	// before anything else reports: a fatal TUI error and the teardown sweep
	// below belong on the shell, not in the diversion sink.
	restoreTerminalWrites()
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", runErr)
	}
	// The event loop has stopped: either an in-app quit (flushAndQuit already
	// swept every session) or a context cancellation from SIGINT/SIGTERM/
	// SIGHUP, which does NOT run flushAndQuit. Sweep here so a signal-driven
	// exit persists dirty sessions too — including background live sessions
	// that main's deferred FlushPending (which only covers the default agent)
	// cannot reach. FlushPending is idempotent and writes only dirty
	// sessions, so the in-app quit path is unaffected.
	m.flushAllSessions()
}
