package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/llm"
	"gogen/internal/pty"
	"gogen/internal/randhex"
)

// terminalState groups the session-owned persistent terminal sessions and
// terminal background jobs. Terminals are owner-scoped to the session (the
// Agent is the owner): they are created by terminal action=open and killed when
// the session closes (Agent.Close → closeTerminals), so a closed web pane or
// TUI quit never orphans a PTY.
type terminalState struct {
	// termMu guards terms, termJobs, and pshell. Leaf lock: it is never held
	// while blocking on a terminal send or shell command, only for the brief
	// map/pointer updates.
	termMu   sync.Mutex
	terms    map[string]*terminalSession
	termJobs map[string]*terminalJob
	// pshell is the per-session persistent shell used by bash_persistent. It
	// is created lazily on first use and closed on session close; reset=true
	// (or an unexpected shell exit) drops it.
	pshell *persistentShell
}

// terminalSession is one live owner-scoped PTY session.
type terminalSession struct {
	ID      string
	Name    string
	Dir     string
	Started time.Time
	Session *pty.Session
}

// terminalJob tracks a background terminal send (terminal action=send with
// run_in_background=true) so the shared background_job tool can poll or cancel
// it: job ids are session-scoped and BackgroundJobStatus/CancelBackgroundJob
// consult this registry first.
type terminalJob struct {
	ID        string
	SessionID string
	Text      string
	Start     time.Time
	cancelled atomic.Bool

	done   chan struct{}
	sendMu sync.Mutex
	result pty.SendResult
	err    error
}

const (
	// terminalDefaultSendTimeout is how long terminal action=send waits for
	// output to settle when the caller does not pass timeout_ms.
	terminalDefaultSendTimeout = 10 * time.Second
	// terminalMaxSendTimeout caps the caller-supplied wait.
	terminalMaxSendTimeout = 120 * time.Second
)

// newTerminalID returns a random terminal session id.
func newTerminalID() string { return randhex.ID(4, "term-") }

// newTerminalJobID returns a random terminal-job id.
func newTerminalJobID() string { return randhex.ID(4, "tjob-") }

// envWith returns env with key=val replacing any existing key (appended when
// absent), so an interactive shell gets a usable TERM even when the server was
// started from a service rather than a terminal.
func envWith(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			if !replaced {
				out = append(out, prefix+val)
				replaced = true
			}
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, prefix+val)
	}
	return out
}

// terminalByID returns the live terminal for id, or nil.
func (a *Agent) terminalByID(id string) *terminalSession {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	return a.terms[id]
}

// terminalJob returns the terminal job for id, or nil.
func (a *Agent) terminalJob(id string) *terminalJob {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	return a.termJobs[id]
}

// hasTerminalWork is HasBackgroundWork's terminal half: a live persistent
// terminal (PTY), a running background terminal send, or a live persistent
// shell. An EXITED persistent shell is not work — its process is already
// gone, so counting it would keep a host resident with nothing running.
// Leaf lock (termMu), never blocks.
func (a *Agent) hasTerminalWork() bool {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	if len(a.terms) > 0 {
		return true
	}
	if a.pshell != nil && !a.pshell.Exited() {
		return true
	}
	for _, job := range a.termJobs {
		select {
		case <-job.done:
			continue // finished: still pollable for the retention window, not work
		default:
			return true
		}
	}
	return false
}

// closeTerminals kills every terminal session and background job owned by this
// session. Called from Agent.Close so no PTY is orphaned. Idempotent.
func (a *Agent) closeTerminals() {
	a.termMu.Lock()
	terms := a.terms
	a.terms = nil
	jobs := a.termJobs
	a.termJobs = nil
	shell := a.pshell
	a.pshell = nil
	a.termMu.Unlock()

	for _, job := range jobs {
		job.cancelled.Store(true)
	}
	for _, ts := range terms {
		_ = ts.Session.Close()
	}
	if shell != nil {
		_ = shell.Close()
	}
}

// resolveTerminalDir resolves an optional terminal working directory against
// the session working dir; an empty dir means the session working dir.
//
// A supplied dir goes through SecurePath like every other agent path: it must
// stay inside the executor's boundary and its symlinks are resolved. Accepting
// a bare absolute path (or one containing "..") let the tool open a shell
// outside the workspace — an escape from the file boundary and a second route
// around the command sandbox.
func (a *Agent) resolveTerminalDir(dir string) (string, error) {
	base := a.Executor.GetWorkingDir()
	if strings.TrimSpace(dir) == "" {
		return base, nil
	}
	secure, err := a.Executor.SecurePath(dir)
	if err != nil {
		return "", err
	}
	return secure, nil
}

// openTerminal starts a PTY session and registers it under a fresh id.
func (a *Agent) openTerminal(name, dir, shell string) (*terminalSession, error) {
	wd, err := a.resolveTerminalDir(dir)
	if err != nil {
		return nil, err
	}
	sess, err := pty.Start(pty.Options{
		Shell: shell,
		Dir:   wd,
		Env:   envWith(os.Environ(), "TERM", "xterm-256color"),
		Rows:  24,
		Cols:  80,
	})
	if err != nil {
		return nil, err
	}
	ts := &terminalSession{
		ID:      newTerminalID(),
		Name:    name,
		Dir:     wd,
		Started: time.Now(),
		Session: sess,
	}
	a.termMu.Lock()
	if a.terms == nil {
		a.terms = make(map[string]*terminalSession)
	}
	a.terms[ts.ID] = ts
	a.termMu.Unlock()
	return ts, nil
}

// startTerminalSendBackground runs a terminal send off the turn and registers
// it as a job so background_job can poll or cancel it. The returned job id is
// pollable until the session closes.
func (a *Agent) startTerminalSendBackground(ts *terminalSession, text string, submit bool, timeout time.Duration) string {
	job := &terminalJob{
		ID:        newTerminalJobID(),
		SessionID: ts.ID,
		Text:      text,
		Start:     time.Now(),
		done:      make(chan struct{}),
	}
	a.termMu.Lock()
	if a.termJobs == nil {
		a.termJobs = make(map[string]*terminalJob)
	}
	a.termJobs[job.ID] = job
	a.termMu.Unlock()

	go func() {
		res, err := ts.Session.Send(text, submit, timeout)
		job.sendMu.Lock()
		job.result = res
		job.err = err
		job.sendMu.Unlock()
		close(job.done)
		// Keep the finished job pollable for the standard retention window,
		// then drop it so a long session cannot accumulate stale sends.
		time.AfterFunc(defaultBackgroundJobRetain, func() { a.removeTerminalJob(job.ID) })
		a.maybeFireBackgroundIdle()
	}()
	return job.ID
}

// terminalJobStatus renders a background terminal send's state.
func (a *Agent) terminalJobStatus(jobID string) (string, error) {
	job := a.terminalJob(jobID)
	if job == nil {
		return "", fmt.Errorf("unknown background job %q (jobs are scoped to this session; the session may have been closed)", jobID)
	}
	elapsed := time.Since(job.Start).Round(time.Millisecond)
	select {
	case <-job.done:
	default:
		return fmt.Sprintf("Job %s is RUNNING (%s elapsed).\nTerminal: %s\nInput: %s", job.ID, elapsed, job.SessionID, job.Text), nil
	}
	job.sendMu.Lock()
	res := job.result
	err := job.err
	job.sendMu.Unlock()
	if job.cancelled.Load() {
		return fmt.Sprintf("Job %s was CANCELLED after %s.\nTerminal: %s\nOutput:\n%s", job.ID, elapsed, job.SessionID, renderTerminalDelta(res)), nil
	}
	if err != nil {
		return fmt.Sprintf("Job %s FAILED after %s: %v\nTerminal: %s\nOutput:\n%s", job.ID, elapsed, err, job.SessionID, renderTerminalDelta(res)), nil
	}
	return fmt.Sprintf("Job %s FINISHED in %s.\nTerminal: %s\nOutput:\n%s", job.ID, elapsed, job.SessionID, renderTerminalDelta(res)), nil
}

// terminalJobState renders a background terminal send's one-word state for
// the background_job list view: RUNNING, CANCELLED, FAILED, or FINISHED.
func terminalJobState(job *terminalJob) string {
	select {
	case <-job.done:
	default:
		return "RUNNING"
	}
	if job.cancelled.Load() {
		return "CANCELLED"
	}
	job.sendMu.Lock()
	err := job.err
	job.sendMu.Unlock()
	if err != nil {
		return "FAILED"
	}
	return "FINISHED"
}

// cancelTerminalJob interrupts a background terminal send with SIGINT.
func (a *Agent) cancelTerminalJob(jobID string) (string, error) {
	job := a.terminalJob(jobID)
	if job == nil {
		return "", fmt.Errorf("unknown background job %q", jobID)
	}
	select {
	case <-job.done:
		return "", fmt.Errorf("background job %s already finished", jobID)
	default:
	}
	job.cancelled.Store(true)
	if ts := a.terminalByID(job.SessionID); ts != nil {
		_ = ts.Session.Signal("SIGINT")
	}
	return fmt.Sprintf("Cancelled background terminal send %s (terminal %s).", jobID, job.SessionID), nil
}

// removeTerminalJob drops a finished terminal job's registration.
func (a *Agent) removeTerminalJob(id string) {
	a.termMu.Lock()
	delete(a.termJobs, id)
	a.termMu.Unlock()
}

// renderTerminalDelta renders a send/read result for the model.
func renderTerminalDelta(res pty.SendResult) string {
	var b strings.Builder
	if res.Output == "" {
		b.WriteString("(no new output)")
	} else {
		b.WriteString(res.Output)
	}
	if res.Truncated {
		b.WriteString("\n[older output was dropped from the scrollback buffer]")
	}
	if res.Exited {
		fmt.Fprintf(&b, "\n[terminal exited with code %d]", res.ExitCode)
	}
	return b.String()
}

func handleTerminalOpen(_ context.Context, a *Agent, args map[string]any) (string, error) {
	if a.Executor == nil {
		return "", fmt.Errorf("no executor configured")
	}
	// The terminal is a raw shell: with a sandbox configured it would run
	// outside the sandbox, so refuse it exactly like bash_persistent does.
	if mode := strings.ToLower(strings.TrimSpace(a.Executor.sandbox())); mode != "" && mode != "off" {
		return "", fmt.Errorf("terminal is not supported with command_sandbox=%s; use execute_command", mode)
	}
	name, err := stringArgOptional(args, "name")
	if err != nil {
		return "", err
	}
	dir, err := stringArgOptional(args, "dir")
	if err != nil {
		return "", err
	}
	shell, err := stringArgOptional(args, "shell")
	if err != nil {
		return "", err
	}
	ts, err := a.openTerminal(name, dir, shell)
	if err != nil {
		return "", err
	}
	label := ts.ID
	if ts.Name != "" {
		label = fmt.Sprintf("%s (%s)", ts.ID, ts.Name)
	}
	return fmt.Sprintf("Opened terminal %s in %s (pid %d, shell %s). Send input with terminal (action=send, session_id=%q), read incremental output with action=read, and close it with action=close when done.",
		label, ts.Dir, ts.Session.PID(), ts.Session.Title(), ts.ID), nil
}

func handleTerminalSend(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	id, err := stringArg(args, "session_id")
	if err != nil {
		return "", err
	}
	text, err := stringArg(args, "text")
	if err != nil {
		return "", err
	}
	submit, err := boolArg(args, "submit", true)
	if err != nil {
		return "", err
	}
	runInBackground, err := boolArg(args, "run_in_background", false)
	if err != nil {
		return "", err
	}
	timeout, err := terminalSendTimeout(args)
	if err != nil {
		return "", err
	}
	// Submitted input is a shell command line, so it goes through the same
	// guard bash_persistent applies — otherwise a command the guard blocks
	// (and execute_command therefore refuses) could simply be typed into the
	// terminal instead. Raw keystrokes (submit=false, e.g. a REPL or editor)
	// are not command lines and are left alone.
	if submit {
		if a.Executor == nil {
			return "", fmt.Errorf("no executor configured")
		}
		if err := a.Executor.enforceCommandGuard(ctx, text); err != nil {
			return "", err
		}
	}
	ts := a.terminalByID(id)
	if ts == nil {
		return "", fmt.Errorf("unknown terminal session %q (use action=list or action=open)", id)
	}
	if runInBackground {
		jobID := a.startTerminalSendBackground(ts, text, submit, timeout)
		return fmt.Sprintf("Started background terminal send %s on terminal %s.\nWait for it with background_job (action=wait, job_id: %q), poll with action=status, or cancel with action=cancel.", jobID, id, jobID), nil
	}
	res, err := ts.Session.Send(text, submit, timeout)
	if err != nil {
		return renderTerminalDelta(res), err
	}
	return renderTerminalDelta(res), nil
}

// terminalSendTimeout resolves the optional timeout_ms argument.
func terminalSendTimeout(args map[string]any) (time.Duration, error) {
	ms, err := intArgOptional(args, "timeout_ms")
	if err != nil {
		return 0, err
	}
	if ms <= 0 {
		return terminalDefaultSendTimeout, nil
	}
	d := time.Duration(ms) * time.Millisecond
	if d > terminalMaxSendTimeout {
		d = terminalMaxSendTimeout
	}
	return d, nil
}

func handleTerminalRead(_ context.Context, a *Agent, args map[string]any) (string, error) {
	id, err := stringArg(args, "session_id")
	if err != nil {
		return "", err
	}
	ts := a.terminalByID(id)
	if ts == nil {
		return "", fmt.Errorf("unknown terminal session %q (use action=list or action=open)", id)
	}
	out, truncated := ts.Session.Read()
	res := pty.SendResult{Output: out, Truncated: truncated}
	if ts.Session.Exited() {
		res.Exited = true
		res.ExitCode = ts.Session.ExitCode()
	}
	return renderTerminalDelta(res), nil
}

func handleTerminalSignal(_ context.Context, a *Agent, args map[string]any) (string, error) {
	id, err := stringArg(args, "session_id")
	if err != nil {
		return "", err
	}
	sig, err := stringArg(args, "signal")
	if err != nil {
		return "", err
	}
	ts := a.terminalByID(id)
	if ts == nil {
		return "", fmt.Errorf("unknown terminal session %q (use action=list or action=open)", id)
	}
	if err := ts.Session.Signal(sig); err != nil {
		return "", err
	}
	return fmt.Sprintf("Sent %s to terminal %s.", sig, id), nil
}

func handleTerminalList(_ context.Context, a *Agent, _ map[string]any) (string, error) {
	a.termMu.Lock()
	ids := make([]string, 0, len(a.terms))
	for id := range a.terms {
		ids = append(ids, id)
	}
	terms := make([]*terminalSession, 0, len(ids))
	for _, id := range ids {
		terms = append(terms, a.terms[id])
	}
	a.termMu.Unlock()

	if len(terms) == 0 {
		return "No terminal sessions are open.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d terminal session(s):\n", len(terms))
	for _, ts := range terms {
		status := ts.Session.Status()
		state := "running"
		if !status.Running {
			state = fmt.Sprintf("exited (code %d)", status.ExitCode)
		}
		name := ""
		if ts.Name != "" {
			name = " name=" + ts.Name
		}
		fmt.Fprintf(&b, "- %s%s [%s] pid=%d dir=%s age=%s\n", ts.ID, name, state, ts.Session.PID(), ts.Dir, time.Since(ts.Started).Round(time.Second))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func handleTerminalClose(_ context.Context, a *Agent, args map[string]any) (string, error) {
	id, err := stringArg(args, "session_id")
	if err != nil {
		return "", err
	}
	a.termMu.Lock()
	ts := a.terms[id]
	delete(a.terms, id)
	a.termMu.Unlock()
	if ts == nil {
		return "", fmt.Errorf("unknown terminal session %q (use action=list)", id)
	}
	if err := ts.Session.Close(); err != nil {
		return "", fmt.Errorf("closing terminal %s: %w", id, err)
	}
	// Closing the last PTY can drain the session's background work; let a
	// host that kept the session resident for it re-check now.
	a.maybeFireBackgroundIdle()
	return fmt.Sprintf("Closed terminal %s.", id), nil
}

// --- Tool definition --------------------------------------------------------

// handleTerminal is the single entry point for the terminal tool: it dispatches
// on action to the per-operation handlers. One tool with an action enum mirrors
// the todo/background_job/git tools and keeps six near-identical schemas out of
// the model-facing tool list.
func handleTerminal(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	action, err := stringArg(args, "action")
	if err != nil {
		return "", err
	}
	switch action {
	case "open":
		return handleTerminalOpen(ctx, a, args)
	case "send":
		return handleTerminalSend(ctx, a, args)
	case "read":
		return handleTerminalRead(ctx, a, args)
	case "signal":
		return handleTerminalSignal(ctx, a, args)
	case "list":
		return handleTerminalList(ctx, a, args)
	case "close":
		return handleTerminalClose(ctx, a, args)
	default:
		return "", fmt.Errorf("unknown terminal action %q (want open, send, read, signal, list, or close)", action)
	}
}

func terminalToolDef() llm.Tool {
	return toolDef("terminal", "Drive a persistent, session-scoped terminal (PTY) for interactive commands or multi-step shell work. action=open starts a session and returns its id; send writes input and waits a short quiet window for output to settle (use read to fetch output that lands later, or set run_in_background=true for a job id); read returns output since the previous send/read; signal sends a POSIX signal (e.g. SIGINT to interrupt); list lists sessions; close kills the session's process group. Prefer execute_command for one-shot commands and bash_persistent for multi-step shell state.",
		toolSchema(map[string]any{
			"action":            toolPropEnum("string", []string{"open", "send", "read", "signal", "list", "close"}, "Operation to perform"),
			"session_id":        toolProp("string", "Terminal session id (from action=open; required for send/read/signal/close)"),
			"text":              toolProp("string", "Text to write to the terminal (action=send)"),
			"submit":            toolProp("boolean", "Append Enter after the text (action=send; default true)"),
			"run_in_background": toolProp("boolean", "Return a job id immediately instead of waiting (action=send; default false)"),
			"timeout_ms":        toolProp("integer", "Max wait for output to settle (action=send; default 10000, max 120000)"),
			"signal":            toolPropEnum("string", []string{"SIGINT", "SIGTERM", "SIGKILL", "SIGTSTP", "SIGHUP"}, "Signal to deliver (action=signal)"),
			"name":              toolProp("string", "Optional display name (action=open)"),
			"dir":               toolProp("string", "Initial working directory (action=open; default session working dir)"),
			"shell":             toolProp("string", "Shell/executable to run (action=open; default $SHELL, then /bin/sh)"),
		}, "action"))
}
