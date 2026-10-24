//go:build unix

package agent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// configureCancelableCmd makes context cancel terminate the whole process
// group. Plain CommandContext only kills the direct child (usually sh), so
// pipelines and `echo; sleep` leave grandchildren holding stdout/stderr and
// CombinedOutput hangs — which keeps the web turn lock held after Cancel.
func configureCancelableCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil {
			_ = cmd.Process.Kill()
		}
		return err
	}
	// Unblock Wait if anything still holds pipes after the group kill.
	cmd.WaitDelay = 500 * time.Millisecond
}

// BuildCommand validates the command against the command guard and returns a
// prepared *exec.Cmd for the given context: shell wrapper, sandbox wrapper
// (bwrap), working directory, and process-group cancellation are configured
// exactly as runCommand configures them. Background job execution uses the
// same path so the guard and sandbox rules apply identically to foreground
// and background commands. Unix-only: Windows runs commands through the
// embedded shell interpreter (executor_run_windows.go) instead.
func (e *Executor) BuildCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	if err := e.enforceCommandGuard(ctx, command); err != nil {
		return nil, err
	}
	cmd, err := e.buildShellCommand(ctx, command)
	if err != nil {
		return nil, err
	}
	configureCancelableCmd(cmd)
	cmd.Dir = e.GetWorkingDir()
	return cmd, nil
}

func (e *Executor) buildShellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	sandbox := strings.ToLower(strings.TrimSpace(e.sandbox()))
	switch sandbox {
	case "", "off":
		return exec.CommandContext(ctx, "sh", "-c", command), nil
	case "bwrap":
		path, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, fmt.Errorf("command_sandbox=bwrap but bwrap not found on PATH: %w", err)
		}
		wd := e.GetWorkingDir()
		if wd == "" {
			wd = "."
		}
		// Resolve symlinks so the --bind/--chdir target matches what the
		// child process will see after its own symlink traversal. Without
		// this, a working directory that is itself a symlink (common on
		// macOS /home -> /Users) gets bind-mounted under one path while
		// the child ends up in the other, so writes/reads don't line up.
		if resolved, err := filepath.EvalSymlinks(wd); err == nil {
			wd = resolved
		}
		// Restrict filesystem to the working directory; keep network and
		// basic devices so builds/tests still work. Not a full container.
		return exec.CommandContext(ctx, path,
			"--die-with-parent",
			"--unshare-pid",
			"--dev", "/dev",
			"--proc", "/proc",
			"--ro-bind", "/usr", "/usr",
			"--ro-bind", "/bin", "/bin",
			"--ro-bind", "/lib", "/lib",
			"--ro-bind-try", "/lib64", "/lib64",
			"--ro-bind-try", "/etc", "/etc",
			"--bind", wd, wd,
			"--chdir", wd,
			"sh", "-c", command,
		), nil
	default:
		return nil, fmt.Errorf("unknown command_sandbox %q (use \"off\" or \"bwrap\")", e.sandbox())
	}
}

// runCommand executes a command string through the system shell (sh -c) and
// returns the final error: guard/sandbox failures raw, execution failures
// wrapped with "execution error:" — the exact shapes ExecuteCommand has
// always produced. runCommand is the platform seam: Windows executes the
// same command strings through the embedded POSIX interpreter instead.
func (e *Executor) runCommand(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd, err := e.BuildCommand(ctx, command)
	if err != nil {
		return err
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("execution error: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("execution error: %w", err)
	}
	return nil
}

// launchBackground starts a command detached from the turn and returns the
// writer for feeding its stdin (an OS pipe: buffered, EPIPE semantics) plus
// a wait function that returns the process's exit error. The command guard
// and sandbox apply exactly as they do to foreground commands.
func (e *Executor) launchBackground(ctx context.Context, command string, stdout, stderr io.Writer) (io.WriteCloser, func() error, error) {
	cmd, err := e.BuildCommand(ctx, command)
	if err != nil {
		return nil, nil, err
	}
	// Create the stdin pipe BEFORE cmd.Start (exec requires it): with the
	// pipe in place, background_job action=input can feed interactive
	// programs (REPLs, psql, dev servers). Without it cmd.Stdin is nil and
	// the child reads from /dev/null.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, fmt.Errorf("execution error: %w", err)
	}
	return stdin, cmd.Wait, nil
}
