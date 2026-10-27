//go:build windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// forceInterpreter makes every command in the test run through the embedded
// interpreter, even on CI runners where git-bash puts a native sh on PATH —
// the fallback must be exercised, not just the native path.
func forceInterpreter(t *testing.T) {
	t.Helper()
	old := shellInterpreterForced
	shellInterpreterForced = true
	t.Cleanup(func() { shellInterpreterForced = old })
}

// TestWindowsShellGatePreference verifies the native-vs-interpreter gate:
// forcing the interpreter disables the native shell, and without the force
// the decision tracks whether sh is actually on PATH.
func TestWindowsShellGatePreference(t *testing.T) {
	forceInterpreter(t)
	if nativeShellAvailable() {
		t.Fatal("forced interpreter must disable the native shell")
	}
	shellInterpreterForced = false
	_, err := exec.LookPath("sh")
	if want := err == nil; nativeShellAvailable() != want {
		t.Fatalf("nativeShellAvailable() = %v, want %v (sh on PATH: %v)", nativeShellAvailable(), want, err == nil)
	}
}

// TestWindowsShellNativePath verifies the native sh path is used when one is
// on PATH (skipped on a stock Windows box without git-bash/msys).
func TestWindowsShellNativePath(t *testing.T) {
	if !nativeShellAvailable() {
		t.Skip("no native sh on PATH")
	}
	exec := NewExecutor(t.TempDir())
	out, err := exec.ExecuteCommand(context.Background(), "echo native-ok")
	if err != nil || !strings.Contains(out, "native-ok") {
		t.Fatalf("native sh: out=%q err=%v", out, err)
	}
}

// TestWindowsShellBuiltins verifies the interpreter's builtins cover the
// shell constructs the agent's commands rely on.
func TestWindowsShellBuiltins(t *testing.T) {
	forceInterpreter(t)
	exec := NewExecutor(t.TempDir())
	out, err := exec.ExecuteCommand(context.Background(), "echo hello && printf 'world\n'")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("output = %q, want hello + world", out)
	}
}

// TestWindowsShellExternalCommand verifies external binaries are resolved
// via PATH and exec'd natively (cmd.exe ships with every Windows install).
func TestWindowsShellExternalCommand(t *testing.T) {
	forceInterpreter(t)
	exec := NewExecutor(t.TempDir())
	out, err := exec.ExecuteCommand(context.Background(), "cmd /c echo external-ok")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "external-ok") {
		t.Fatalf("output = %q, want external-ok", out)
	}
}

// TestWindowsShellExitStatus verifies non-zero exits surface as
// "execution error: exit status N", matching the Unix sh path.
func TestWindowsShellExitStatus(t *testing.T) {
	forceInterpreter(t)
	exec := NewExecutor(t.TempDir())
	_, err := exec.ExecuteCommand(context.Background(), "exit 3")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v, want exit status 3", err)
	}
}

// TestWindowsShellWideExitCode verifies a 32-bit Windows exit code whose
// low byte is 0 is reported as a command failure and the rest of the script
// does not run. Truncating it into the interpreter's 8-bit status used to
// panic the process (exit code 2). The skip is mvdan.cc/sh/v3 v3.14.1:
// fromHandlerError treats a non-ExitStatus error as fatal.
func TestWindowsShellWideExitCode(t *testing.T) {
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("powershell is not on PATH")
	}
	forceInterpreter(t)
	ex := NewExecutor(t.TempDir())
	out, err := ex.ExecuteCommand(context.Background(), `powershell -NoProfile -Command "exit 256"; echo still-running`)
	if err == nil || !strings.Contains(err.Error(), "exit code 256") {
		t.Fatalf("err = %v, want exit code 256", err)
	}
	if strings.Contains(out, "still-running") {
		t.Fatalf("output = %q, want the rest of the script skipped", out)
	}
}

// TestWindowsShellCommandNotFound verifies a missing binary reports the
// shell's 127 exit status (like sh).
func TestWindowsShellCommandNotFound(t *testing.T) {
	forceInterpreter(t)
	exec := NewExecutor(t.TempDir())
	_, err := exec.ExecuteCommand(context.Background(), "definitely-not-a-real-command-xyz")
	if err == nil || !strings.Contains(err.Error(), "exit status 127") {
		t.Fatalf("err = %v, want exit status 127", err)
	}
}

// TestWindowsShellTimeoutKillsTree verifies the timeout path terminates the
// command promptly instead of waiting it out, and that the kill reaches the
// process tree (taskkill /T).
func TestWindowsShellTimeoutKillsTree(t *testing.T) {
	forceInterpreter(t)
	dir := t.TempDir()
	exec := NewExecutor(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	// cmd /c ping -n 30 would otherwise run ~30s.
	_, err := exec.ExecuteCommand(ctx, "cmd /c ping -n 30 127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout kill took %s, want prompt tree kill", elapsed)
	}
	// taskkill has returned, but Windows can keep the killed console
	// host's working-directory handle for a moment. TempDir's RemoveAll
	// then fails with "being used by another process".
	waitUntilDirUnlocked(t, dir)
}

// waitUntilDirUnlocked removes dir once no process still has it as its
// working directory. A missing directory counts as unlocked: TempDir's
// own cleanup treats that as success.
func waitUntilDirUnlocked(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for {
		err = os.Remove(dir)
		if err == nil || os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("working dir still locked after tree kill: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWindowsBackgroundJobInterp verifies background jobs run through the
// embedded interpreter and stdin input works end-to-end (in-memory pipe).
func TestWindowsBackgroundJobInterp(t *testing.T) {
	forceInterpreter(t)
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand(context.Background(), "while read line; do echo \"got: $line\"; done")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := a.BackgroundJobInput(context.Background(), id, "hello", true); err != nil {
		t.Fatalf("input: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, err := a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if strings.Contains(s, "got: hello") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("echoed line never appeared in job output")
}
