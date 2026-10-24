//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// shellInterpreterForced is a test hook: when true, runCommand and
// launchBackground always use the embedded interpreter, even when a native
// sh is on PATH (Windows CI bundles git-bash, so the fallback would
// otherwise never be exercised there).
var shellInterpreterForced bool

// nativeShellAvailable reports whether a real POSIX sh (git-bash, msys,
// cygwin — which also ship the coreutils) is on PATH. When it is, commands
// run through it natively (real fork/exec, OS pipes, full coreutils); the
// embedded interpreter is only used when the native shell is actually
// missing, so a stock Windows box keeps working with zero requirements.
func nativeShellAvailable() bool {
	if shellInterpreterForced {
		return false
	}
	_, err := exec.LookPath("sh")
	return err == nil
}

// Windows ships no POSIX shell. When none is on PATH, command strings are
// executed by the embedded pure-Go shell interpreter (mvdan.cc/sh, BSD-3):
// it implements the shell semantics the agent's commands rely on — pipes,
// redirects, && / || / ;, loops, and the echo/printf/cd/test/read/exit
// builtins — in-process, and external binaries (go, git, node, cmd.exe,
// ...) are resolved via PATH and exec'd natively. Only this file and the
// mvdan.cc/sh dependency are compiled into the Windows binary; Unix keeps
// its system sh.

// runCommand executes a command string and returns the final error,
// mirroring the Unix path's shapes: guard/sandbox failures raw, execution
// failures wrapped with "execution error:". Prefers a native sh when one is
// on PATH (full coreutils), falling back to the embedded interpreter.
func (e *Executor) runCommand(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := e.checkCommandConfig(ctx, command); err != nil {
		return err
	}
	if nativeShellAvailable() {
		return e.runNativeCommand(ctx, command, stdin, stdout, stderr)
	}
	return e.runInterpreterCommand(ctx, command, stdin, stdout, stderr)
}

// launchBackground starts a command detached from the turn and returns the
// writer for feeding its stdin plus a wait function returning the exit
// error. Native sh uses an OS stdin pipe (buffered, EPIPE semantics); the
// interpreter uses an in-memory pipe. The command guard and sandbox apply
// exactly as they do to foreground commands.
func (e *Executor) launchBackground(ctx context.Context, command string, stdout, stderr io.Writer) (io.WriteCloser, func() error, error) {
	if err := e.checkCommandConfig(ctx, command); err != nil {
		return nil, nil, err
	}
	if nativeShellAvailable() {
		return e.launchNativeBackground(ctx, command, stdout, stderr)
	}
	return e.launchInterpreterBackground(ctx, command, stdout, stderr)
}

// runNativeCommand executes the command through the system sh (git-bash,
// msys, cygwin): native fork/exec, OS pipes, full coreutils. Cancellation
// still kills the whole process tree via taskkill /T /F (os/exec on
// Windows can only terminate the direct child).
func (e *Executor) runNativeCommand(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = e.GetWorkingDir()
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	taskkillOnCancel(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("execution error: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("execution error: %w", err)
	}
	return nil
}

// launchNativeBackground starts a native sh detached from the turn, with an
// OS stdin pipe (buffered, EPIPE semantics).
func (e *Executor) launchNativeBackground(ctx context.Context, command string, stdout, stderr io.Writer) (io.WriteCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = e.GetWorkingDir()
	// Create the stdin pipe BEFORE cmd.Start (exec requires it): with the
	// pipe in place, background_job action=input can feed interactive
	// programs (REPLs, psql, dev servers).
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	taskkillOnCancel(cmd)
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, fmt.Errorf("execution error: %w", err)
	}
	return stdin, cmd.Wait, nil
}

// taskkillOnCancel replaces CommandContext's plain Process.Kill cancel with
// a whole-tree kill: taskkill ships with every Windows install.
func taskkillOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
	}
	cmd.WaitDelay = 500 * time.Millisecond
}

// checkCommandConfig applies the command guard and sandbox validation that
// BuildCommand performs on Unix (the Windows runner has no *exec.Cmd to
// configure).
func (e *Executor) checkCommandConfig(ctx context.Context, command string) error {
	if err := e.enforceCommandGuard(ctx, command); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(e.sandbox())) {
	case "", "off":
		return nil
	case "bwrap":
		return fmt.Errorf("command_sandbox=bwrap is not supported on Windows")
	default:
		return fmt.Errorf("unknown command_sandbox %q (use \"off\" or \"bwrap\")", e.sandbox())
	}
}

// runInterpreterCommand executes the command string through the embedded
// interpreter (the no-native-sh fallback).
func (e *Executor) runInterpreterCommand(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	runner, prog, err := e.newShellRunner(ctx, command, stdin, stdout, stderr)
	if err != nil {
		return err
	}
	if err := runner.Run(ctx, prog); err != nil {
		if ctx.Err() != nil {
			// Cancelled or timed out: ExecuteCommand maps this via ctx.Err()
			// ("command cancelled"/"command timed out"); the error itself is
			// kept for background jobs' exitErr.
			return err
		}
		return fmt.Errorf("execution error: %w", err)
	}
	return nil
}

// launchInterpreterBackground runs the command through the embedded
// interpreter in a goroutine, with an in-memory stdin pipe.
func (e *Executor) launchInterpreterBackground(ctx context.Context, command string, stdout, stderr io.Writer) (io.WriteCloser, func() error, error) {
	pr, pw := io.Pipe()
	runner, prog, err := e.newShellRunner(ctx, command, pr, stdout, stderr)
	if err != nil {
		_ = pw.Close()
		return nil, nil, err
	}
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = runner.Run(ctx, prog)
		close(done)
	}()
	wait := func() error {
		<-done
		return runErr
	}
	return pw, wait, nil
}

// newShellRunner parses the command and builds a configured interpreter
// runner. stdin may be nil (foreground commands: nothing to read).
func (e *Executor) newShellRunner(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) (*interp.Runner, *syntax.File, error) {
	prog, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, nil, fmt.Errorf("command parse error: %w", err)
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	wd := e.GetWorkingDir()
	if wd == "" {
		wd = "."
	}
	runner, err := interp.New(
		interp.Dir(wd),
		interp.StdIO(stdin, stdout, stderr),
		interp.Env(expand.ListEnviron(os.Environ()...)),
		interp.ExecHandler(execHandler),
	)
	if err != nil {
		return nil, nil, err
	}
	return runner, prog, nil
}

// execHandler runs each external command spawned by the interpreter.
// Cancellation kills the whole process tree via taskkill /T /F.
func execHandler(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.NewExitStatus(127)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Path = path
	cmd.Env = execEnvFromShell(hc.Env)
	cmd.Dir = hc.Dir
	cmd.Stdin = hc.Stdin
	cmd.Stdout = hc.Stdout
	cmd.Stderr = hc.Stderr
	taskkillOnCancel(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return interp.NewExitStatus(uint8(ee.ExitCode()))
		}
		return err
	}
	return nil
}

// execEnvFromShell converts the interpreter's environment to exec format,
// mirroring mvdan's internal conversion: exported string variables only.
func execEnvFromShell(env expand.Environ) []string {
	list := make([]string, 0, 64)
	for name, vr := range env.Each {
		if vr.Exported && vr.Kind == expand.String {
			list = append(list, name+"="+vr.String())
		}
	}
	return list
}
