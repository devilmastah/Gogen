//go:build unix

package pty

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/creack/pty"
)

// process is the Unix platform handle: a real PTY master plus the child.
type process struct {
	handle io.ReadWriteCloser
	cmd    *exec.Cmd
	pid    int
	title  string
}

// startProcess spawns shell attached to a real PTY so interactive programs
// (REPLs, editors, prompts) behave like a local terminal. It prefers a full
// session with a controlling terminal (job control works) and falls back to a
// plain process group in sandboxes that block setsid/setctty.
func startProcess(opts Options) (*process, error) {
	shell := opts.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
	}
	size := &pty.Winsize{Rows: 24, Cols: 80}
	if opts.Rows > 0 {
		size.Rows = opts.Rows
	}
	if opts.Cols > 0 {
		size.Cols = opts.Cols
	}
	cmd := newShellCmd(shell, opts)
	f, err := pty.StartWithAttrs(cmd, size, &syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		cmd = newShellCmd(shell, opts)
		f, err = pty.StartWithAttrs(cmd, size, &syscall.SysProcAttr{Setpgid: true})
		if err != nil {
			return nil, fmt.Errorf("start terminal %s: %w", shell, err)
		}
	}
	return &process{handle: f, cmd: cmd, pid: cmd.Process.Pid, title: filepath.Base(shell)}, nil
}

// newShellCmd builds the child command; Env defaults to the parent environment
// when the caller supplies none.
func newShellCmd(shell string, opts Options) *exec.Cmd {
	cmd := exec.Command(shell, opts.Args...)
	cmd.Dir = opts.Dir
	if len(opts.Env) > 0 {
		cmd.Env = opts.Env
	}
	return cmd
}

// signalProcess delivers name to the child's process group (the child is a
// session/group leader), falling back to the direct process.
func signalProcess(cmd *exec.Cmd, name string) error {
	if cmd == nil || cmd.Process == nil {
		return ErrExited
	}
	var sig syscall.Signal
	switch name {
	case "SIGINT":
		sig = syscall.SIGINT
	case "SIGTERM":
		sig = syscall.SIGTERM
	case "SIGKILL":
		sig = syscall.SIGKILL
	case "SIGTSTP":
		sig = syscall.SIGTSTP
	case "SIGHUP":
		sig = syscall.SIGHUP
	default:
		return fmt.Errorf("unsupported signal %q", name)
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		return cmd.Process.Signal(sig)
	}
	return nil
}

// controlByteFor maps a keystroke-generated signal to the control byte the
// terminal driver turns back into that signal. An interactive shell runs each
// job in its own process group (job control), so SIGINT/SIGTSTP must go through
// the tty — the driver delivers them to the FOREGROUND group — or the running
// job never sees them.
func controlByteFor(name string) (byte, bool) {
	switch name {
	case "SIGINT":
		return 0x03, true // ^C
	case "SIGTSTP":
		return 0x1a, true // ^Z
	}
	return 0, false
}

// killProcess kills the child and everything it spawned (process group).
func killProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
