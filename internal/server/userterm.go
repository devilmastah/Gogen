package server

import (
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"gogen/internal/streamutil"
)

// userTermOutputBatch is the coalescing window for the interactive user
// shell's output. A pty hands the read loop ~4 KB at a time, so forwarding
// every read as its own user_term_output frame turned a busy command (yes,
// ls -R, a build log) into thousands of small JSON frames — one marshal, one
// (deflate,) one queue slot and one browser wakeup each. 16 ms keeps terminal
// echo responsive (a single frame interval) while collapsing a burst into a
// handful of frames.
//
// A var, not a const, so tests can widen the window and assert the batching
// without depending on the scheduler (a loaded box can deliver two reads more
// than 16 ms apart, which would flush them separately). It is read once per
// terminal spawn, so a test that stretches it before user_term_request gets
// it.
var userTermOutputBatch = 16 * time.Millisecond

// userTermReadDrainWait bounds how long the exit path waits for the read
// goroutine to hand over its final chunk before flushing (see waitLoop). The
// master is closed first, which unblocks Read, so the normal wait is
// instantaneous; the bound exists only so a stuck pty cannot delay the exit
// report.
const userTermReadDrainWait = 2 * time.Second

// ptyHandle is the platform-specific interactive channel for a user shell.
type ptyHandle interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
}

// UserTerminal is an interactive user shell attached to a WebSocket
// connection. It is safe for concurrent use; Write/Resize become no-ops once
// the shell exits or Close is called.
type UserTerminal struct {
	mu  sync.Mutex
	pty ptyHandle
	cmd *exec.Cmd
	// batch coalesces the read loop's chunks into fewer, larger user_term_output
	// frames; its send callback is the caller's output sink. Nil when the
	// caller passed no sink. The batcher is mutex-protected, so FlushOutput
	// may run on the exit/teardown goroutine while the read goroutine feeds it.
	batch *streamutil.TokenBatcher
	// readDone is closed when the read goroutine returns, so the exit path can
	// flush AFTER the last chunk was handed to the batcher — never before
	// (flushing early would leave that chunk to a later timer, putting it
	// behind user_term_exit).
	readDone chan struct{}
	done     chan struct{}
	code     int
	title    string
	closed   bool
}

// FlushOutput pushes pending batched output to the sink immediately. It is the
// "output ended" hook: the batcher lags the pty by up to userTermOutputBatch,
// so a caller that is about to close the socket (or report the shell's exit)
// must flush first or the last bytes are never sent. No-op when nothing is
// pending or no sink was supplied.
func (u *UserTerminal) FlushOutput() {
	u.mu.Lock()
	batch := u.batch
	u.mu.Unlock()
	if batch != nil {
		batch.Flush()
	}
}

// Title returns a short display name for the terminal tab (e.g. "bash").
func (u *UserTerminal) Title() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.title
}

// Done is closed when the shell process exits.
func (u *UserTerminal) Done() <-chan struct{} { return u.done }

// ExitCode returns the shell's exit code once Done is closed.
func (u *UserTerminal) ExitCode() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.code
}

// Write sends input (keystrokes) to the shell's stdin.
func (u *UserTerminal) Write(p []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.pty == nil {
		return io.ErrClosedPipe
	}
	_, err := u.pty.Write(p)
	return err
}

// Resize updates the terminal window size reported to the shell.
func (u *UserTerminal) Resize(cols, rows uint16) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.pty == nil {
		return io.ErrClosedPipe
	}
	return u.pty.Resize(cols, rows)
}

// Close kills the shell (process group on unix) and releases the pty.
func (u *UserTerminal) Close() error {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return nil
	}
	u.closed = true
	p := u.pty
	u.pty = nil
	cmd := u.cmd
	u.mu.Unlock()
	if cmd != nil {
		killProcessGroup(cmd)
	}
	if p != nil {
		return p.Close()
	}
	return nil
}

// startUserTerminal spawns an interactive shell in dir, forwarding its output
// to output. The output callback is invoked from a background goroutine (the
// read goroutine or the batcher's flush timer) and must not call back into the
// UserTerminal. Reads are coalesced for userTermOutputBatch, so one busy
// command produces a few large frames instead of one per ~4 KB read; callers
// that are about to stop accepting output must FlushOutput first.
func startUserTerminal(dir string, output func(string)) (*UserTerminal, error) {
	p, cmd, title, err := openUserPTY(dir)
	if err != nil {
		return nil, err
	}
	u := &UserTerminal{
		pty:      p,
		cmd:      cmd,
		readDone: make(chan struct{}),
		done:     make(chan struct{}),
		title:    title,
	}
	if output != nil {
		// The think flag is meaningless here: a pty read is always output.
		u.batch = streamutil.NewTokenBatcher(func(_ bool, text string) {
			output(text)
		}, userTermOutputBatch)
	}
	go u.readLoop(p)
	go u.waitLoop(cmd)
	return u, nil
}

func (u *UserTerminal) readLoop(p ptyHandle) {
	defer close(u.readDone)
	// batch is assigned before this goroutine starts and never reassigned, so
	// one read is enough (the batcher itself is mutex-protected).
	u.mu.Lock()
	batch := u.batch
	u.mu.Unlock()
	buf := make([]byte, 4096)
	for {
		n, err := p.Read(buf)
		if n > 0 && batch != nil {
			// FIFO: one batcher with a single segment kind, so frames leave in
			// read order and no byte is dropped or reordered.
			batch.StreamToken(string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func (u *UserTerminal) waitLoop(cmd *exec.Cmd) {
	code := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	// Give the read goroutine a moment to drain final output (e.g. the shell's
	// own "exit" line) before we sever the master.
	time.Sleep(50 * time.Millisecond)
	u.mu.Lock()
	u.code = code
	u.closed = true
	if u.pty != nil {
		_ = u.pty.Close() // unblocks a read still parked in the master
		u.pty = nil
	}
	batch, readDone := u.batch, u.readDone
	u.mu.Unlock()
	// Wait for the read goroutine to hand over its last chunk before flushing:
	// it is the only producer, so flushing earlier could strand that chunk in
	// a later timer flush (i.e. behind user_term_exit) or lose it to the
	// socket teardown.
	select {
	case <-readDone:
	case <-time.After(userTermReadDrainWait):
	}
	if batch != nil {
		// Flush, then close: the flush delivers what the read loop produced,
		// the close stops the flush timer so no frame can be emitted after the
		// terminal ended (a late append is dropped — output is over).
		batch.Flush()
		batch.Close()
	}
	close(u.done)
}

// withEnv returns env with any existing key replaced by key=val (appended if
// absent), so interactive shells get a usable TERM even when the server was
// started from a service rather than a terminal.
func withEnv(env []string, key, val string) []string {
	prefix := key + "="
	res := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			if !replaced {
				res = append(res, prefix+val)
				replaced = true
			}
			continue
		}
		res = append(res, e)
	}
	if !replaced {
		res = append(res, prefix+val)
	}
	return res
}

// userTermHolder tracks the live user terminal for one WebSocket connection.
// A single terminal per connection matches the "one UI, one shell" model; the
// shell is respawned on request after it exits.
type userTermHolder struct {
	mu sync.Mutex
	ut *UserTerminal
}

func (h *userTermHolder) get() *UserTerminal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ut
}

// trySet stores ut as the connection's terminal, failing when a terminal is
// already held (live or dead). It serializes spawns so two concurrent
// user_term_request messages cannot both spawn a shell — without it, the
// holder's get-then-set in spawnUserTerminal had a TOCTOU window in which
// the second spawn leaked a pty (its read goroutine kept writing to the
// socket after the holder was overwritten). The caller closes ut when this
// returns false.
func (h *userTermHolder) trySet(ut *UserTerminal) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ut != nil {
		return false
	}
	h.ut = ut
	return true
}

// clear removes ut from the holder if it is still the current terminal,
// reporting whether it was removed (false when a newer shell replaced it).
func (h *userTermHolder) clear(ut *UserTerminal) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ut == ut {
		h.ut = nil
		return true
	}
	return false
}
