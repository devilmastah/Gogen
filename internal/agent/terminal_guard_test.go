//go:build unix

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestTerminalSendHonoursCommandGuard pins the guard on the terminal tool:
// submitted input is a command line like any other, so a pattern the guard
// blocks (and execute_command therefore refuses) must not slip through the PTY
// instead. No approver is attached to the context, so the guard fails closed.
func TestTerminalSendHonoursCommandGuard(t *testing.T) {
	a := newTerminalTestAgent(t)
	id := openTestTerminal(t, a)
	defer a.closeTerminals()

	_, err := handleTerminal(context.Background(), a, map[string]any{
		"action":     "send",
		"session_id": id,
		"text":       "su" + "do rm -rf /tmp/gogen-guard-probe",
	})
	if err == nil {
		t.Fatal("a guard-blocked command must be refused on the terminal tool")
	}
	var blocked *CommandBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v (%T), want CommandBlockedError", err, err)
	}
}

// TestTerminalSendRawInputSkipsGuard pins the other side of the same check:
// submit=false is raw keystrokes for a REPL or editor, not a command line, so
// the guard must not inspect it.
func TestTerminalSendRawInputSkipsGuard(t *testing.T) {
	a := newTerminalTestAgent(t)
	id := openTestTerminal(t, a)
	defer a.closeTerminals()

	// "sudo" typed without Enter is just text for the shell to echo.
	if _, err := handleTerminal(context.Background(), a, map[string]any{
		"action":     "send",
		"session_id": id,
		"text":       "su" + "do",
		"submit":     false,
		"timeout_ms": 200,
	}); err != nil {
		t.Fatalf("raw input must not be guarded: %v", err)
	}
}

// TestTerminalRefusedUnderSandbox: a sandboxed executor must refuse to open a
// terminal, exactly like bash_persistent — the PTY would run outside the
// sandbox.
func TestTerminalRefusedUnderSandbox(t *testing.T) {
	a := newTerminalTestAgent(t)
	a.Executor.SetSandbox("bwrap")
	defer a.Executor.SetSandbox("off")

	_, err := handleTerminal(context.Background(), a, map[string]any{"action": "open", "shell": "/bin/sh"})
	if err == nil || !strings.Contains(err.Error(), "command_sandbox") {
		t.Fatalf("open error = %v, want a command_sandbox refusal", err)
	}
}

// TestTerminalDirStaysInsideBoundary: a terminal working directory is resolved
// like every other agent path, so an absolute path outside the workspace (or a
// traversal) is refused instead of opening a shell there.
func TestTerminalDirStaysInsideBoundary(t *testing.T) {
	a := newTerminalTestAgent(t)
	outside := t.TempDir()
	if _, err := handleTerminal(context.Background(), a, map[string]any{
		"action": "open", "shell": "/bin/sh", "dir": outside,
	}); err == nil {
		t.Fatal("a terminal dir outside the workspace must be refused")
	}
	if _, err := handleTerminal(context.Background(), a, map[string]any{
		"action": "open", "shell": "/bin/sh", "dir": "../..",
	}); err == nil {
		t.Fatal("a traversal terminal dir must be refused")
	}
	// A relative dir inside the workspace still works.
	if _, err := handleTerminal(context.Background(), a, map[string]any{
		"action": "open", "shell": "/bin/sh", "dir": ".",
	}); err != nil {
		t.Fatalf("relative dir inside the workspace: %v", err)
	}
	a.closeTerminals()
}
