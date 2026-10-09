package tui

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// tuiLogEnv names the file that receives diverted diagnostic output while
// the TUI owns the terminal. Unset (the default) diverts to the null device;
// pointing it at /dev/stderr opts back into raw terminal logging (useful only
// when debugging the layout itself).
const tuiLogEnv = "GOGEN_TUI_LOG"

// divertTerminalWrites moves every diagnostic write that would otherwise land
// on the terminal — the standard logger and direct os.Stderr writes — to a
// sink the renderer does not own, and returns a func that restores both.
//
// Why this exists: the TUI runs Bubble Tea's INLINE renderer (alt-screen is
// off so scrollback and Shift+click selection keep working), so the renderer
// owns every row of a terminal-height frame. A raw line written above or
// inside that frame is an unaccounted scroll: the renderer's cell diff no
// longer matches the real screen, and the frame desyncs into ghost cursors
// and stale columns. Startup notices already avoid this by routing through
// tea's managed above-frame channel (see TUI.SetStartupNotices and Run), but
// the rest of the tree logs directly — ~90 log.Printf calls across
// internal/agent, internal/session, and internal/server, plus a direct
// os.Stderr write in agent.normalizeFetchMode. Several of those sit on the
// hot turn path and can fire mid-frame: a failed delta save after a tool
// round (agent.persistDeltaSnapshot), a failed todo write, an auto-compaction
// abort. Redirecting both writers at the process level covers every one of
// them without touching the call sites, and keeps the web/oneshot hosts —
// which never call this — logging to stderr exactly as before.
//
// The returned func is safe to call more than once. Callers must invoke it
// once the renderer has released the terminal, so that teardown diagnostics
// (and any panic) reach the real stderr again.
func divertTerminalWrites() (restore func()) {
	prevLogWriter := log.Writer()
	prevStderr := os.Stderr

	sink := newDivertSink()
	log.SetOutput(sink.w)
	os.Stderr = sink.stderr

	restored := false
	return func() {
		if restored {
			return
		}
		restored = true
		log.SetOutput(prevLogWriter)
		os.Stderr = prevStderr
		sink.close()
	}
}

// divertSink is where diverted diagnostic writes go. stderr is kept separate
// from w because os.Stderr is typed *os.File: a discard destination has to be
// a real file handle, not an io.Writer.
type divertSink struct {
	// w receives the standard logger's output.
	w io.Writer
	// stderr backs the os.Stderr replacement, so direct os.Stderr writes
	// (agent.normalizeFetchMode) land in the same place as log.Printf.
	stderr *os.File
	// close releases stderr. It does not close w separately: the log package
	// holds no handle of its own, so w and stderr are the same file when a
	// path is configured and one handle covers both.
	close func()
}

// newDivertSink builds the diversion destination from GOGEN_TUI_LOG.
//
// The fallback is always something that is not the terminal: if GOGEN_TUI_LOG
// is set but cannot be opened, output goes to the null device rather than
// falling back to stderr, because a stderr fallback would reintroduce exactly
// the corruption this exists to prevent.
func newDivertSink() divertSink {
	if path := os.Getenv(tuiLogEnv); path != "" {
		if sink, ok := fileDivertSink(path); ok {
			return sink
		}
	}
	return nullDivertSink()
}

// fileDivertSink opens path for append, creating its directory. ok is false
// when the file cannot be opened.
func fileDivertSink(path string) (divertSink, bool) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// Best effort; a failure here resurfaces as the OpenFile error.
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return divertSink{}, false
	}
	return divertSink{w: f, stderr: f, close: func() { _ = f.Close() }}, true
}

// nullDivertSink sends diverted output to the null device. os.Stderr is typed
// *os.File, so the discard must be a real file handle rather than io.Discard;
// os.DevNull resolves to /dev/null or NUL as appropriate.
func nullDivertSink() divertSink {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		// Opening the null device cannot fail on any supported platform. If
		// it somehow does, prefer a handle that drops writes over one that
		// reaches the terminal — see discardableFile.
		return divertSink{w: io.Discard, stderr: discardableFile(), close: func() {}}
	}
	return divertSink{w: f, stderr: f, close: func() { _ = f.Close() }}
}

// discardableFile returns an *os.File whose writes are dropped: the null
// device, closed immediately, or nil when even that cannot be opened. Both
// forms are safe to write to — a closed file returns EBADF and a nil *os.File
// returns ErrInvalid — so output is discarded either way, and neither can
// reach the renderer's frame.
func discardableFile() *os.File {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil
	}
	_ = f.Close()
	return f
}
