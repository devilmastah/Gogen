//go:build unix

package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/llm"
)

// newTerminalTestAgent builds a bare agent with the persistent-terminal tools
// enabled.
func newTerminalTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)
	a.SetTerminalEnabled(true)
	return a
}

// openTestTerminal opens a non-interactive-aware sh PTY and returns its id.
func openTestTerminal(t *testing.T, a *Agent) string {
	t.Helper()
	if _, err := handleTerminal(context.Background(), a, map[string]any{"action": "open", "shell": "/bin/sh"}); err != nil {
		t.Fatalf("terminal open: %v", err)
	}
	a.termMu.Lock()
	defer a.termMu.Unlock()
	for id := range a.terms {
		return id
	}
	t.Fatal("terminal open registered no session")
	return ""
}

func sendText(t *testing.T, a *Agent, id, text string) string {
	t.Helper()
	out, err := handleTerminal(context.Background(), a, map[string]any{
		"action":     "send",
		"session_id": id,
		"text":       text,
		"timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("terminal send %q: %v", text, err)
	}
	return out
}

// TestTerminalToolGating pins the featureTools gating: with the terminal
// feature off the tools appear nowhere (llmTools, AllowedToolNames,
// executeTool); with it on they are registered and executable.
func TestTerminalToolGating(t *testing.T) {
	tools := []string{"terminal", "bash_persistent"}

	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)
	defs := map[string]bool{}
	for _, d := range a.llmTools() {
		defs[d.Name] = true
	}
	for _, name := range tools {
		if defs[name] {
			t.Fatalf("%s must not be registered when terminal is off", name)
		}
		if _, ok := a.AllowedToolNames()[name]; ok {
			t.Fatalf("%s must not be allowed when terminal is off", name)
		}
		if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: name, Args: map[string]any{}}); err == nil {
			t.Fatalf("executeTool(%s) must fail when terminal is off", name)
		}
	}

	a.SetTerminalEnabled(true)
	defs = map[string]bool{}
	for _, d := range a.llmTools() {
		defs[d.Name] = true
	}
	for _, name := range tools {
		if !defs[name] {
			t.Fatalf("%s must be registered when terminal is on", name)
		}
		if _, ok := a.AllowedToolNames()[name]; !ok {
			t.Fatalf("%s must be allowed when terminal is on", name)
		}
	}
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "terminal", Args: map[string]any{"action": "list"}}); err != nil {
		t.Fatalf("terminal action=list must dispatch: %v", err)
	}
}

// TestTerminalLifecycle drives the open/send/read/close lifecycle through the
// tool handlers.
func TestTerminalLifecycle(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()

	id := openTestTerminal(t, a)

	if out := sendText(t, a, id, "echo hello-world"); !strings.Contains(out, "hello-world") {
		t.Fatalf("send output = %q, want hello-world", out)
	}
	// Nothing new: the send consumed the output.
	read, err := handleTerminal(context.Background(), a, map[string]any{"action": "read", "session_id": id})
	if err != nil {
		t.Fatalf("terminal read: %v", err)
	}
	if strings.Contains(read, "hello-world") {
		t.Fatalf("read reprinted consumed output: %q", read)
	}

	list, err := handleTerminal(context.Background(), a, map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("terminal list: %v", err)
	}
	if !strings.Contains(list, id) {
		t.Fatalf("terminal list = %q, want it to mention %s", list, id)
	}

	if _, err := handleTerminal(context.Background(), a, map[string]any{"action": "close", "session_id": id}); err != nil {
		t.Fatalf("terminal close: %v", err)
	}
	list, _ = handleTerminal(context.Background(), a, map[string]any{"action": "list"})
	if strings.Contains(list, id) {
		t.Fatalf("closed terminal still listed: %q", list)
	}
	if _, err := handleTerminal(context.Background(), a, map[string]any{"action": "close", "session_id": id}); err == nil {
		t.Fatal("closing an unknown terminal must fail")
	}
	if _, err := handleTerminal(context.Background(), a, map[string]any{"action": "bogus"}); err == nil {
		t.Fatal("an unknown terminal action must fail")
	}
}

// TestTerminalIncrementalRead verifies the read action returns output produced
// after the previous send. Echo is disabled first so the echoed command text
// cannot be mistaken for command output.
func TestTerminalIncrementalRead(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()

	id := openTestTerminal(t, a)
	sendText(t, a, id, "stty -echo")

	first := sendText(t, a, id, "printf AAAA; sleep 0.6; printf BBBB")
	if !strings.Contains(first, "AAAA") {
		t.Fatalf("first send output = %q, want AAAA", first)
	}
	if strings.Contains(first, "BBBB") {
		t.Fatalf("first send output already contains BBBB (settle window too long): %q", first)
	}

	deadline := time.Now().Add(3 * time.Second)
	var acc string
	for time.Now().Before(deadline) {
		out, err := handleTerminal(context.Background(), a, map[string]any{"action": "read", "session_id": id})
		if err != nil {
			t.Fatalf("terminal read: %v", err)
		}
		acc += out
		if strings.Contains(acc, "BBBB") {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("incremental read never saw BBBB; got %q", acc)
}

// TestTerminalSignal verifies sending SIGTERM ends the session.
func TestTerminalSignal(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()

	id := openTestTerminal(t, a)
	if _, err := handleTerminal(context.Background(), a, map[string]any{"action": "signal", "session_id": id, "signal": "SIGKILL"}); err != nil {
		t.Fatalf("terminal signal: %v", err)
	}
	ts := a.terminalByID(id)
	select {
	case <-ts.Session.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session did not exit after SIGKILL")
	}
	if _, err := handleTerminal(context.Background(), a, map[string]any{"action": "signal", "session_id": id, "signal": "SIGBOGUS"}); err == nil {
		t.Fatal("unknown signal must fail")
	}
}

// TestTerminalBackgroundSend verifies run_in_background registers a pollable
// job that finishes when the send settles.
func TestTerminalBackgroundSend(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()

	id := openTestTerminal(t, a)
	sendText(t, a, id, "stty -echo")

	out, err := handleTerminal(context.Background(), a, map[string]any{
		"action":            "send",
		"session_id":        id,
		"text":              "printf RAN",
		"run_in_background": true,
		"timeout_ms":        8000,
	})
	if err != nil {
		t.Fatalf("background send: %v", err)
	}
	a.termMu.Lock()
	if len(a.termJobs) != 1 {
		a.termMu.Unlock()
		t.Fatal("background send did not register a job")
	}
	var jobID string
	for jid := range a.termJobs {
		jobID = jid
	}
	a.termMu.Unlock()
	if !strings.Contains(out, jobID) {
		t.Fatalf("send result %q does not mention job %s", out, jobID)
	}

	waitForTerminalJob(t, a, jobID)
	status, err := a.BackgroundJobStatus(jobID)
	if err != nil {
		t.Fatalf("status after finish: %v", err)
	}
	if !strings.Contains(status, "FINISHED") || !strings.Contains(status, "RAN") {
		t.Fatalf("finished status = %q, want FINISHED with RAN", status)
	}
}

// TestTerminalBackgroundCancel verifies background_job cancel interrupts a
// long-running background send.
func TestTerminalBackgroundCancel(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()

	id := openTestTerminal(t, a)

	// Continuous output keeps the send from settling, so the job stays RUNNING
	// until we cancel it.
	if _, err := handleTerminal(context.Background(), a, map[string]any{
		"action":            "send",
		"session_id":        id,
		"text":              "i=0; while [ $i -lt 400 ]; do printf x; sleep 0.05; i=$((i+1)); done",
		"run_in_background": true,
		"timeout_ms":        60000,
	}); err != nil {
		t.Fatalf("background send: %v", err)
	}
	a.termMu.Lock()
	var jobID string
	for jid := range a.termJobs {
		jobID = jid
	}
	a.termMu.Unlock()

	time.Sleep(100 * time.Millisecond)
	status, err := a.BackgroundJobStatus(jobID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(status, "RUNNING") {
		t.Fatalf("status = %q, want RUNNING", status)
	}

	if _, err := a.CancelBackgroundJob(jobID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitForTerminalJob(t, a, jobID)
	status, _ = a.BackgroundJobStatus(jobID)
	if !strings.Contains(status, "CANCELLED") {
		t.Fatalf("status = %q, want CANCELLED", status)
	}
}

func waitForTerminalJob(t *testing.T, a *Agent, jobID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job := a.terminalJob(jobID)
		if job == nil {
			return
		}
		select {
		case <-job.done:
			return
		default:
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("terminal job %s never finished", jobID)
}

// TestPersistentShellState verifies cwd and exported env persist between calls,
// reset restarts the shell, and the command guard is applied.
func TestPersistentShellState(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()

	dir := a.Executor.GetWorkingDir()
	run := func(args map[string]any) string {
		t.Helper()
		out, err := handleBashPersistent(context.Background(), a, args)
		if err != nil {
			t.Fatalf("bash_persistent %v: %v", args, err)
		}
		return out
	}

	if out := run(map[string]any{"command": "cd /tmp && pwd"}); !strings.Contains(out, "/tmp") {
		t.Fatalf("cd output = %q, want /tmp", out)
	}
	run(map[string]any{"command": "export GOGEN_TERM_TEST=persisted"})
	if out := run(map[string]any{"command": "echo $GOGEN_TERM_TEST"}); !strings.Contains(out, "persisted") {
		t.Fatalf("exported env did not persist: %q", out)
	}
	if out := run(map[string]any{"command": "pwd"}); !strings.Contains(out, "/tmp") {
		t.Fatalf("cwd did not persist: %q", out)
	}
	// reset starts a fresh shell in the working dir.
	if out := run(map[string]any{"command": "pwd", "reset": true}); !strings.Contains(out, dir) {
		t.Fatalf("reset cwd = %q, want %q", out, dir)
	}
	// guard applies to persistent-shell commands.
	if _, err := handleBashPersistent(context.Background(), a, map[string]any{"command": "sudo rm -rf /"}); err == nil {
		t.Fatal("command guard did not block bash_persistent")
	}
}

// TestPersistentShellClosedOnSessionClose verifies Agent.Close kills the
// persistent shell (no orphan).
func TestPersistentShellClosedOnSessionClose(t *testing.T) {
	a := newTerminalTestAgent(t)
	if out, err := handleBashPersistent(context.Background(), a, map[string]any{"command": "echo up"}); err != nil || !strings.Contains(out, "up") {
		t.Fatalf("bash_persistent: %q %v", out, err)
	}
	sh := a.pshell
	if sh == nil {
		t.Fatal("expected a persistent shell")
	}
	a.Close()
	if !sh.Exited() {
		t.Fatal("persistent shell still alive after Agent.Close")
	}
}

// TestCloseTerminalsKillsSessions verifies Agent.Close tears down every
// owner-scoped PTY.
func TestCloseTerminalsKillsSessions(t *testing.T) {
	a := newTerminalTestAgent(t)
	id1 := openTestTerminal(t, a)
	id2 := openTestTerminal(t, a)
	ts1 := a.terminalByID(id1)
	ts2 := a.terminalByID(id2)

	a.Close()
	for i, ts := range []*terminalSession{ts1, ts2} {
		select {
		case <-ts.Session.Done():
		case <-time.After(3 * time.Second):
			t.Fatalf("terminal %d still alive after Agent.Close", i)
		}
	}
	a.termMu.Lock()
	if len(a.terms) != 0 || len(a.termJobs) != 0 || a.pshell != nil {
		a.termMu.Unlock()
		t.Fatal("terminal state not cleared by Agent.Close")
	}
	a.termMu.Unlock()
}

// TestTerminalConcurrentOpenClose exercises the session registry under
// concurrent open/list/close to catch data races (run with -race).
func TestTerminalConcurrentOpenClose(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				if _, err := handleTerminal(context.Background(), a, map[string]any{"action": "open", "shell": "/bin/sh"}); err != nil {
					return
				}
				_, _ = handleTerminal(context.Background(), a, map[string]any{"action": "list"})
				a.termMu.Lock()
				var id string
				for k := range a.terms {
					id = k
					break
				}
				a.termMu.Unlock()
				if id != "" {
					_, _ = handleTerminal(context.Background(), a, map[string]any{"action": "close", "session_id": id})
				}
			}
		}()
	}
	wg.Wait()
}

// TestTerminalOwnerScoped verifies terminals are scoped to the owning session
// agent: another agent cannot reach them by id.
func TestTerminalOwnerScoped(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()
	b := newTerminalTestAgent(t)
	defer b.Close()

	id := openTestTerminal(t, a)
	if b.terminalByID(id) != nil {
		t.Fatal("a terminal is visible to a non-owning agent")
	}
	if _, err := handleTerminal(context.Background(), b, map[string]any{"action": "read", "session_id": id}); err == nil {
		t.Fatal("terminal read from a non-owning agent must fail")
	}
	if _, err := handleTerminal(context.Background(), b, map[string]any{"action": "close", "session_id": id}); err == nil {
		t.Fatal("terminal close from a non-owning agent must fail")
	}
	// The real owner can still use it.
	if out := sendText(t, a, id, "echo owned"); !strings.Contains(out, "owned") {
		t.Fatalf("owner send failed: %q", out)
	}
}

// TestTerminalUnknownSession pins the error paths for bad session ids.
func TestTerminalUnknownSession(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()
	for _, action := range []string{"read", "signal", "close"} {
		args := map[string]any{"action": action, "session_id": "term-nope"}
		if action == "signal" {
			args["signal"] = "SIGINT"
		}
		if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "terminal", Args: args}); err == nil {
			t.Fatalf("terminal action=%s with an unknown session must fail", action)
		}
	}
	if _, err := a.BackgroundJobStatus("tjob-nope"); err == nil {
		t.Fatal("unknown job status must fail")
	}
}

// TestTerminalBackgroundWait covers the background_job wait action on a
// terminal send: shell jobs and terminal sends share the job-id space, so a
// wait must resolve a tjob- id and the list must show it.
func TestTerminalBackgroundWait(t *testing.T) {
	a := newTerminalTestAgent(t)
	defer a.Close()

	id := openTestTerminal(t, a)
	sendText(t, a, id, "stty -echo")

	out, err := handleTerminal(context.Background(), a, map[string]any{
		"action":            "send",
		"session_id":        id,
		"text":              "printf WAITED",
		"run_in_background": true,
		"timeout_ms":        8000,
	})
	if err != nil {
		t.Fatalf("background send: %v", err)
	}
	a.termMu.Lock()
	var jobID string
	for jid := range a.termJobs {
		jobID = jid
	}
	a.termMu.Unlock()
	if jobID == "" || !strings.Contains(out, jobID) {
		t.Fatalf("send result %q does not mention a registered job id", out)
	}

	status, err := a.BackgroundJobWait(context.Background(), jobID, 15*time.Second)
	if err != nil {
		t.Fatalf("wait on terminal job: %v", err)
	}
	if !strings.Contains(status, "FINISHED") || !strings.Contains(status, "WAITED") {
		t.Fatalf("wait status = %q, want FINISHED with WAITED", status)
	}
	if list := a.BackgroundJobsList(); !strings.Contains(list, jobID) {
		t.Fatalf("list missing terminal job %s:\n%s", jobID, list)
	}
}
