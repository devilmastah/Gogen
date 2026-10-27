package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/randhex"
)

// persistentShellMaxBuffer bounds the retained output of the persistent shell
// between reads. Output beyond the bound is dropped from the front so a chatty
// command cannot grow memory without bound.
const persistentShellMaxBuffer = 4 << 20

// errPersistentShellTimeout is returned when a command produces no completion
// marker within the timeout. The shell is killed on timeout (its stdin/out may
// hold a command that never returns), so the next call starts a fresh shell.
var errPersistentShellTimeout = errors.New("persistent shell command timed out")

// errPersistentShellClosed is returned by Run once the shell has been closed.
var errPersistentShellClosed = errors.New("persistent shell is closed")

// shellProcess is the platform shell process handle: stdin, merged
// stdout+stderr, plus wait/kill closures.
type shellProcess struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	wait   func() error
	kill   func()
}

// persistentShell is one long-lived shell whose cwd and exported environment
// persist across Run calls. Commands are serialized (runMu); completion is
// detected with a random marker printed after the command, so Run returns only
// once the command has finished (or the timeout fires).
type persistentShell struct {
	proc *shellProcess

	runMu sync.Mutex

	outMu   sync.Mutex
	buf     bytes.Buffer
	readErr error
	dropped bool

	closed    atomic.Bool
	closeOnce sync.Once
	reapOnce  sync.Once
}

// newPersistentShell starts the platform shell in dir.
func newPersistentShell(dir string, env []string) (*persistentShell, error) {
	proc, err := startShellProcess(dir, env)
	if err != nil {
		return nil, err
	}
	s := &persistentShell{proc: proc}
	go s.readLoop()
	return s, nil
}

// Exited reports whether the shell process has ended (naturally or via Close).
func (s *persistentShell) Exited() bool {
	if s.closed.Load() {
		return true
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.readErr != nil
}

// readLoop appends the shell's output to the buffer until the stream ends.
func (s *persistentShell) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := s.proc.stdout.Read(buf)
		if n > 0 {
			s.outMu.Lock()
			s.buf.Write(buf[:n])
			if s.buf.Len() > persistentShellMaxBuffer {
				start := contextmgr.RuneSafeTailStart(s.buf.Bytes(), persistentShellMaxBuffer)
				if start > 0 {
					s.buf.Next(start)
					s.dropped = true
				}
			}
			s.outMu.Unlock()
		}
		if err != nil {
			s.outMu.Lock()
			s.readErr = err
			s.outMu.Unlock()
			return
		}
	}
}

// Run executes one command in the persistent shell and returns its output, exit
// code, and the shell's post-command working directory. It blocks until the
// command's completion marker is seen or timeout elapses (in which case the
// shell is killed and a fresh one is started on the next call).
//
// Because the command runs in the shared shell, a command that reads stdin can
// consume the next command line; that limitation is documented on the tool.
func (s *persistentShell) Run(ctx context.Context, command string, timeout time.Duration) (string, int, string, error) {
	if timeout <= 0 {
		timeout = defaultCommandIdleTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if s.Exited() {
		return "", -1, "", errPersistentShellClosed
	}
	if err := ctx.Err(); err != nil {
		return "", -1, "", err
	}

	marker := "__GOGEN_" + randhex.ID(8, "") + "__"
	// The command is followed by a printf that prints the completion marker,
	// the exit status, and the current directory. A leading newline keeps the
	// marker on its own line even when the command's output has no trailing
	// newline.
	if _, err := fmt.Fprintf(s.proc.stdin, "%s\nprintf '\\n%s:%%d:%%s\\n' \"$?\" \"$PWD\"\n", command, marker); err != nil {
		return "", -1, "", fmt.Errorf("write to persistent shell: %w", err)
	}

	markerBytes := []byte(marker)
	deadline := time.Now().Add(timeout)
	// scanFrom tracks how much of the buffer the previous poll already searched,
	// so each 20 ms tick scans only the new bytes (the buffer can hold 4 MB, and
	// rescanning it 50 times a second was measurable). A front trim moves the
	// content, so the offset is rebased whenever the buffer shrank.
	scanFrom := 0
	prevLen := 0
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.outMu.Lock()
		data := s.buf.Bytes()
		if len(data) < prevLen || scanFrom > len(data) {
			scanFrom = 0 // trimmed/rewritten: rescan
		}
		idx := -1
		if found := bytes.Index(data[scanFrom:], markerBytes); found >= 0 {
			idx = scanFrom + found
		}
		if idx >= 0 {
			if out, code, pwd, consumed, ok := parseShellMarker(data, idx, len(marker)); ok {
				s.buf.Next(consumed)
				s.outMu.Unlock()
				return out, code, pwd, nil
			}
		}
		// Keep a marker-length overlap so a marker split across two polls is
		// still found.
		nextScan := len(data) - (len(markerBytes) - 1)
		if nextScan < 0 {
			nextScan = 0
		}
		prevLen = len(data)
		scanFrom = nextScan
		readErr := s.readErr
		s.outMu.Unlock()

		if readErr != nil {
			if s.closed.Load() {
				return "", -1, "", errPersistentShellClosed
			}
			return "", -1, "", fmt.Errorf("persistent shell exited: %w", readErr)
		}
		select {
		case <-ctx.Done():
			// The caller gave up (Esc / turn cancelled). The command may still be
			// running in the shared shell, so it must die with the shell or the
			// next Run's output would be corrupted by its leftovers.
			s.killAndReap()
			return "", -1, "", ctx.Err()
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			// Kill the shell: the timed-out command may still be running and
			// would corrupt the next Run's output. The next call recreates it.
			s.killAndReap()
			return "", -1, "", errPersistentShellTimeout
		}
	}
}

// killAndReap kills the shell and reaps it exactly once, so a cancelled or
// timed-out Run leaves no zombie behind (kill alone leaves one until the
// process is waited for).
func (s *persistentShell) killAndReap() {
	s.kill()
	s.reap()
}

// reap waits for the shell process once, whoever gets there first (a cancelled
// Run, Close, or the shell exiting on its own).
func (s *persistentShell) reap() {
	if s.proc == nil || s.proc.wait == nil {
		return
	}
	s.reapOnce.Do(func() {
		go func() { _ = s.proc.wait() }()
	})
}

// parseShellMarker parses "<marker>:<code>:<pwd>\n" starting at idx. It reports
// ok=false while the marker line is still incomplete (the read loop appends
// output in chunks, so a poll can observe a partial line).
func parseShellMarker(data []byte, idx, mlen int) (out string, code int, pwd string, consumed int, ok bool) {
	rest := data[idx+mlen:]
	if len(rest) == 0 || rest[0] != ':' {
		return "", 0, "", 0, false
	}
	rest = rest[1:]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(rest) || rest[i] != ':' {
		return "", 0, "", 0, false
	}
	n, err := strconv.Atoi(string(rest[:i]))
	if err != nil {
		return "", 0, "", 0, false
	}
	rest = rest[i+1:]
	j := bytes.IndexByte(rest, '\n')
	if j < 0 {
		return "", 0, "", 0, false
	}
	pwd = string(rest[:j])
	consumed = idx + mlen + 1 + i + 1 + j + 1
	// Strip the single leading newline the marker printf added.
	end := idx
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	return string(data[:end]), n, pwd, consumed, true
}

// kill kills the shell process group and marks the shell closed. Safe to call
// multiple times.
func (s *persistentShell) kill() {
	s.closed.Store(true)
	if s.proc.kill != nil {
		s.proc.kill()
	}
	if s.proc.stdin != nil {
		_ = s.proc.stdin.Close()
	}
	if s.proc.stdout != nil {
		_ = s.proc.stdout.Close()
	}
}

// Close kills the shell and reaps it without blocking the caller.
func (s *persistentShell) Close() error {
	s.closeOnce.Do(func() {
		s.kill()
		s.reap()
	})
	return nil
}

// persistentShellFor returns the session's persistent shell, creating it lazily
// or recreating it when missing/dead (or when reset is requested).
func (a *Agent) persistentShellFor(reset bool) (*persistentShell, error) {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	if a.pshell != nil && (reset || a.pshell.Exited()) {
		_ = a.pshell.Close()
		a.pshell = nil
	}
	if a.pshell == nil {
		sh, err := newPersistentShell(a.Executor.GetWorkingDir(), envWith(os.Environ(), "TERM", "dumb"))
		if err != nil {
			return nil, err
		}
		a.pshell = sh
	}
	return a.pshell, nil
}

// bashPersistentTimeout resolves the optional timeout_ms argument for
// bash_persistent. Unlike a terminal send, a persistent-shell command is
// expected to run to completion, so the default is the executor idle timeout
// (300s) rather than the short terminal-send settle window.
func bashPersistentTimeout(args map[string]any) (time.Duration, error) {
	ms, err := intArgOptional(args, "timeout_ms")
	if err != nil {
		return 0, err
	}
	if ms <= 0 {
		return defaultCommandIdleTimeout, nil
	}
	d := time.Duration(ms) * time.Millisecond
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d, nil
}

func handleBashPersistent(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	command, err := stringArg(args, "command")
	if err != nil {
		return "", err
	}
	reset, err := boolArg(args, "reset", false)
	if err != nil {
		return "", err
	}
	timeout, err := bashPersistentTimeout(args)
	if err != nil {
		return "", err
	}
	if a.Executor == nil {
		return "", fmt.Errorf("no executor configured")
	}
	if mode := strings.ToLower(strings.TrimSpace(a.Executor.sandbox())); mode != "" && mode != "off" {
		return "", fmt.Errorf("bash_persistent is not supported with command_sandbox=%s; use execute_command", mode)
	}
	if err := a.Executor.enforceCommandGuard(ctx, command); err != nil {
		return "", err
	}
	shell, err := a.persistentShellFor(reset)
	if err != nil {
		return "", err
	}
	out, code, pwd, err := shell.Run(ctx, command, timeout)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if strings.TrimSpace(out) == "" {
		b.WriteString("(no output)")
	} else {
		b.WriteString(strings.TrimRight(out, "\n"))
	}
	fmt.Fprintf(&b, "\n[exit code %d, cwd %s]", code, pwd)
	return b.String(), nil
}

func bashPersistentToolDef() llm.Tool {
	return toolDef("bash_persistent", "Run a command in a per-session persistent shell whose working directory and exported environment persist across calls. This bypasses the fresh-process model of execute_command: state leaks between commands, and a command that reads stdin can consume the shell's next command. The command guard applies. Use reset=true to restart the shell. Prefer execute_command for one-shot commands.",
		toolSchema(map[string]any{
			"command":    toolProp("string", "Shell command to run in the persistent shell"),
			"reset":      toolProp("boolean", "Restart the shell first, discarding cwd/env (default false)"),
			"timeout_ms": toolProp("integer", "Max wait for the command to finish (default 300000)"),
		}, "command"))
}
