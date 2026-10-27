// Package pty provides an owner-agnostic interactive process session backed
// by a pseudo-terminal on Unix (stdio pipes on Windows). It powers the
// agent-facing persistent terminal tools: open a session, send input, read
// incremental output, signal the process, and close it.
//
// A Session is a single child process (typically an interactive shell) with a
// bounded scrollback buffer. Output is appended by a read goroutine; Send
// waits for the output to settle after writing and returns the delta produced
// since the send, while Read returns the delta produced since the previous
// read. A Session is safe for concurrent use; Send serializes writes so two
// sends never interleave on the wire.
package pty

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/procutil"
)

// DefaultMaxScroll bounds the retained scrollback of a Session (bytes). Older
// output is dropped from the front so a chatty interactive program cannot grow
// memory without bound.
const DefaultMaxScroll = 512 * 1024

const (
	// defaultSettle is how long the output must be quiet after a send before
	// Send returns. It is also the minimum time Send waits, so a command that
	// produces no output (cd) still gives the shell a moment to run it.
	defaultSettle = 200 * time.Millisecond
	// defaultSendTimeout caps how long Send waits for output to settle.
	defaultSendTimeout = 10 * time.Second
	// sendPollInterval is the settle-probe tick.
	sendPollInterval = 20 * time.Millisecond
)

// ErrExited is returned when an operation is attempted on an exited Session.
var ErrExited = errors.New("terminal session has exited")

// Signals lists the signal names the terminal tools accept. It is the single
// source of truth for the tool schema's enum and for Signal validation.
var Signals = []string{"SIGINT", "SIGTERM", "SIGKILL", "SIGTSTP", "SIGHUP"}

// Options configure a new Session.
type Options struct {
	// Shell is the executable to run. Empty means $SHELL, then /bin/sh.
	Shell string
	// Args are extra arguments passed before any shell invocation.
	Args []string
	// Dir is the initial working directory (empty = the current process's).
	Dir string
	// Env is the child environment. Empty means os.Environ().
	Env []string
	// Rows and Cols set the initial PTY window size (0 = 24x80).
	Rows uint16
	Cols uint16
	// MaxScroll overrides DefaultMaxScroll for this session.
	MaxScroll int
}

// Status reports whether a Session's process is still running and, once
// exited, its exit code.
type Status struct {
	Running  bool
	ExitCode int
}

// SendResult is the outcome of one Send: the output produced since the send
// began, whether older output was dropped, and the session status.
type SendResult struct {
	Output    string
	Truncated bool
	Exited    bool
	ExitCode  int
}

// Session is one interactive child process with a bounded output buffer.
type Session struct {
	handle io.ReadWriteCloser
	cmd    *exec.Cmd
	pid    int
	title  string

	maxScroll int

	// outMu guards the scrollback buffer and the read cursor. append (read
	// goroutine) and take (Send/Read) are the only touchers.
	outMu      sync.Mutex
	scroll     bytes.Buffer
	bufStart   int64 // absolute byte offset of scroll[0]
	writeTotal int64 // absolute total bytes ever written
	readCursor int64 // absolute offset consumed through
	dropped    bool  // true once a front trim has dropped bytes

	// lastOutput is the unix-nano timestamp of the most recent output chunk
	// (or session start). Atomic: written by the read goroutine, read by Send.
	lastOutput atomic.Int64

	writeMu sync.Mutex // serializes raw writes to the pty
	sendMu  sync.Mutex // serializes Send calls

	done     chan struct{}
	exitMu   sync.Mutex
	exitCode int
	waitErr  error

	handleOnce sync.Once
	closeOnce  sync.Once
}

// Start launches the child process and returns a live Session. The caller must
// Close it; a session whose process has exited still holds the pty until Close
// (or until the read goroutine observes EOF and the wait goroutine runs).
func Start(opts Options) (*Session, error) {
	proc, err := startProcess(opts)
	if err != nil {
		return nil, err
	}
	max := opts.MaxScroll
	if max <= 0 {
		max = DefaultMaxScroll
	}
	s := &Session{
		handle:    proc.handle,
		cmd:       proc.cmd,
		pid:       proc.pid,
		title:     proc.title,
		maxScroll: max,
		done:      make(chan struct{}),
	}
	if s.pid <= 0 && proc.cmd != nil && proc.cmd.Process != nil {
		s.pid = proc.cmd.Process.Pid
	}
	s.lastOutput.Store(time.Now().UnixNano())
	go s.readLoop()
	go s.waitLoop()
	return s, nil
}

// PID returns the child process id (0 when unknown).
func (s *Session) PID() int { return s.pid }

// Title returns a short display name for the session (e.g. "bash").
func (s *Session) Title() string { return s.title }

// Done is closed when the child process has exited and the read goroutine has
// drained. It never closes twice and is safe to use after Close.
func (s *Session) Done() <-chan struct{} { return s.done }

// Exited reports whether the child process has exited (non-blocking).
func (s *Session) Exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// ExitCode returns the process exit code once Done is closed; -1 when killed
// by a signal or before the process was observed to exit.
func (s *Session) ExitCode() int {
	select {
	case <-s.done:
	default:
		return -1
	}
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	return s.exitCode
}

// WaitErr returns the process wait error once Done is closed (nil on a clean
// exit).
func (s *Session) WaitErr() error {
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	return s.waitErr
}

// Status reports the process state.
func (s *Session) Status() Status {
	if s.Exited() {
		return Status{Running: false, ExitCode: s.ExitCode()}
	}
	return Status{Running: true}
}

// Write sends raw bytes to the child's stdin (no newline translation).
func (s *Session) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.Exited() {
		return 0, ErrExited
	}
	n, err := s.handle.Write(p)
	if err != nil {
		return n, fmt.Errorf("write to terminal: %w", err)
	}
	return n, nil
}

// Send writes text (submitting a carriage return when submit is true) and
// waits until the output has been quiet for the settle interval or the timeout
// elapses, then returns the output produced since the send began. A slow
// command that is still producing output when the timeout fires returns what
// it has; callers poll Read for the rest.
func (s *Session) Send(text string, submit bool, timeout time.Duration) (SendResult, error) {
	if timeout <= 0 {
		timeout = defaultSendTimeout
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	if s.Exited() {
		return s.snapshotSince(s.cursor()), ErrExited
	}

	s.outMu.Lock()
	start := s.writeTotal
	s.outMu.Unlock()

	payload := text
	if submit {
		payload += "\r"
	}
	if payload != "" {
		if _, err := s.Write([]byte(payload)); err != nil {
			return s.snapshotSince(start), err
		}
	}

	started := time.Now()
	deadline := started.Add(timeout)
	for {
		if s.Exited() {
			break
		}
		now := time.Now()
		idle := now.Sub(time.Unix(0, s.lastOutput.Load()))
		if now.Sub(started) >= defaultSettle && idle >= defaultSettle {
			break
		}
		if now.After(deadline) {
			break
		}
		time.Sleep(sendPollInterval)
	}
	return s.snapshotSince(start), nil
}

// Read returns the output produced since the previous Read or Send and whether
// any output was dropped by the scrollback bound before being returned.
func (s *Session) Read() (string, bool) {
	res := s.snapshotSince(s.cursor())
	return res.Output, res.Truncated
}

// Scrollback returns the full retained output and whether older output was
// dropped by the bound.
func (s *Session) Scrollback() (string, bool) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.scroll.String(), s.dropped
}

// Signal delivers name to the running session. The name must be one of
// Signals; unsupported names (and SIGKILL/SIGTSTP on platforms without process
// groups) return an error.
//
// SIGINT and SIGTSTP go THROUGH the pty (a control byte) rather than to the
// shell's process group: an interactive shell runs each job in its own group
// (job control), so signalling the shell's group misses the job that is
// actually running — bash ignores the SIGINT and `npm test` keeps going. The
// terminal driver delivers the byte to the FOREGROUND group, which is exactly
// what a ^C keystroke does. SIGTERM, SIGHUP and SIGKILL stay process-group
// signals (they are not keystroke-generated).
func (s *Session) Signal(name string) error {
	if s.Exited() {
		return ErrExited
	}
	if !validSignal(name) {
		return fmt.Errorf("unknown signal %q (want one of %v)", name, Signals)
	}
	if b, ok := controlByteFor(name); ok {
		return s.writeControl(b)
	}
	return signalProcess(s.cmd, name)
}

// writeControl writes one control byte to the pty master, which raises the
// corresponding signal in the terminal's foreground process group.
func (s *Session) writeControl(b byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.Exited() {
		return ErrExited
	}
	_, err := s.handle.Write([]byte{b})
	return err
}

// Close terminates the child's process group and releases the pty. It is
// idempotent and safe to call concurrently with Send/Read; subsequent
// operations fail with ErrExited.
//
// SIGHUP goes first: an interactive bash forwards it to its jobs on exit, so
// `&` jobs die with the shell. SIGKILL alone gave bash no chance to do that,
// which left every background job running as an orphan. The group is SIGKILLed
// after a short grace period (or immediately when the child is already gone).
func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		_ = signalProcess(s.cmd, "SIGHUP")
		select {
		case <-s.done:
		case <-time.After(closeHangupGrace):
		}
		killProcess(s.cmd)
		err = s.closeHandle()
	})
	return err
}

// closeHangupGrace bounds how long Close waits for the child to honour SIGHUP
// before the process group is killed.
//
// It costs nothing in the common case — Close waits on the child's own exit
// signal and returns as soon as the shell is reaped — so the window only elapses
// for a shell that IGNORES the hangup, and lengthening it is nearly free. It is
// a variable rather than a constant so tests can widen it: 250 ms proved too
// tight under load, where an interactive bash may be descheduled before it can
// forward SIGHUP to its jobs, which is exactly how `&` jobs were orphaned.
var closeHangupGrace = time.Second

// validSignal reports whether name is in the accepted set.
func validSignal(name string) bool {
	for _, s := range Signals {
		if s == name {
			return true
		}
	}
	return false
}

// cursor returns the current read cursor (the absolute offset of the first
// byte not yet consumed by Send/Read).
func (s *Session) cursor() int64 {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.readCursor
}

// snapshotSince returns the retained output in the absolute range
// [from, writeTotal), clamped to the read cursor and the retained buffer, and
// advances the read cursor to the end. dropped reports whether any output in
// the requested range had already been trimmed from the front.
func (s *Session) snapshotSince(from int64) SendResult {
	s.outMu.Lock()
	defer s.outMu.Unlock()

	if from < s.readCursor {
		from = s.readCursor
	}
	dropped := from < s.bufStart
	if from < s.bufStart {
		from = s.bufStart
	}
	rel := from - s.bufStart
	if rel < 0 {
		rel = 0
	}
	data := s.scroll.Bytes()
	if rel > int64(len(data)) {
		rel = int64(len(data))
	}
	out := string(data[rel:])
	s.readCursor = s.writeTotal

	res := SendResult{
		Output:    out,
		Truncated: dropped,
		Exited:    s.Exited(),
	}
	if res.Exited {
		res.ExitCode = s.ExitCode()
	}
	return res
}

// append records a new output chunk: it grows the scrollback, trims the front
// when over the bound (rune-safe), and refreshes the activity timestamp.
func (s *Session) append(p []byte) {
	if len(p) == 0 {
		return
	}
	s.outMu.Lock()
	s.scroll.Write(p)
	s.writeTotal += int64(len(p))
	if s.scroll.Len() > s.maxScroll {
		start := contextmgr.RuneSafeTailStart(s.scroll.Bytes(), s.maxScroll)
		if start > 0 {
			s.scroll.Next(start)
			s.bufStart += int64(start)
			s.dropped = true
		}
	}
	s.outMu.Unlock()
	s.lastOutput.Store(time.Now().UnixNano())
}

// readLoop copies the child's output into the scrollback until the stream ends.
func (s *Session) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := s.handle.Read(buf)
		if n > 0 {
			s.append(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// waitLoop waits for the process to exit, records its status, and releases the
// pty so the read goroutine unblocks. It closes done last.
func (s *Session) waitLoop() {
	err := s.cmd.Wait()
	// Give the read goroutine a brief moment to drain final output before the
	// master is severed (mirrors the web user terminal).
	time.Sleep(30 * time.Millisecond)
	s.exitMu.Lock()
	s.waitErr = err
	s.exitCode = procutil.ExitCode(err)
	s.exitMu.Unlock()
	_ = s.closeHandle()
	close(s.done)
	// Touch lastOutput so a Send blocked on settle wakes promptly.
	s.lastOutput.Store(time.Now().UnixNano())
}

// closeHandle closes the platform handle at most once.
func (s *Session) closeHandle() error {
	var err error
	s.handleOnce.Do(func() {
		err = s.handle.Close()
	})
	return err
}

// exitCodeOf extracts the exit code from a Wait error: 0 for a clean exit, the
// real code for an *exec.ExitError, and -1 for a signal kill or unusual error.
