//go:build windows

package pty

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// process is the Windows platform handle. There is no usable PTY here, so the
// child runs over stdio pipes; interactive full-screen programs will not behave
// like a terminal, but line-oriented REPLs still work.
type process struct {
	handle io.ReadWriteCloser
	cmd    *exec.Cmd
	pid    int
	title  string
}

// winHandle merges the child's stdout and stderr into one read stream and
// writes stdin through a pipe.
type winHandle struct {
	in  *os.File // parent write end to the child's stdin
	out *os.File // parent read end from the child's merged stdout/stderr
}

func (w *winHandle) Read(p []byte) (int, error)  { return w.out.Read(p) }
func (w *winHandle) Write(p []byte) (int, error) { return w.in.Write(p) }

func (w *winHandle) Close() error {
	err := w.in.Close()
	if cerr := w.out.Close(); err == nil {
		err = cerr
	}
	return err
}

// startProcess spawns the shell over stdio pipes (Windows has no PTY).
func startProcess(opts Options) (*process, error) {
	shell := opts.Shell
	if shell == "" {
		shell = os.Getenv("COMSPEC")
		if shell == "" {
			shell = "cmd.exe"
		}
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	cmd := exec.Command(shell, opts.Args...)
	cmd.Dir = opts.Dir
	if len(opts.Env) > 0 {
		cmd.Env = opts.Env
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = outW
	if err := cmd.Start(); err != nil {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
		return nil, fmt.Errorf("start terminal %s: %w", shell, err)
	}
	// The child owns these ends now; close the parent's copies so EOF is
	// observed on the read side when the child exits.
	_ = inR.Close()
	_ = outW.Close()
	name := strings.TrimSuffix(filepath.Base(shell), ".exe")
	return &process{
		handle: &winHandle{in: inW, out: outR},
		cmd:    cmd,
		pid:    cmd.Process.Pid,
		title:  name,
	}, nil
}

// signalProcess maps the accepted signal names onto Windows kill semantics.
// SIGKILL/SIGTERM terminate the process; SIGINT is best-effort (unsupported by
// many Windows programs). There are no process groups here.
func signalProcess(cmd *exec.Cmd, name string) error {
	if cmd == nil || cmd.Process == nil {
		return ErrExited
	}
	switch name {
	case "SIGINT":
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	case "SIGTERM", "SIGKILL", "SIGHUP":
		return cmd.Process.Kill()
	default:
		return fmt.Errorf("unsupported signal %q on windows (use the terminal tool's close action)", name)
	}
}

// controlByteFor: there is no terminal driver raising signals from control
// bytes on Windows, so every signal takes the direct process path (see
// signalProcess).
func controlByteFor(string) (byte, bool) { return 0, false }

// killProcess terminates the child.
func killProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
