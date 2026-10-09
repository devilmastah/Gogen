package tui

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureRealStderr swaps os.Stderr for a pipe and points the standard logger
// at it, reproducing the production wiring (log defaults to os.Stderr) so a
// test can observe anything that would actually reach the terminal. The
// returned read func drains the pipe and restores both writers.
func captureRealStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	prevStderr := os.Stderr
	prevLogWriter := log.Writer()
	os.Stderr = w
	log.SetOutput(os.Stderr)

	var out string
	return func() string {
		// Restore first so a late write cannot block on the closed pipe.
		log.SetOutput(prevLogWriter)
		os.Stderr = prevStderr
		w.Close()
		b, err := io.ReadAll(r)
		if err != nil && !errors.Is(err, os.ErrClosed) {
			t.Fatalf("read pipe: %v", err)
		}
		out = string(b)
		return out
	}
}

// TestDivertTerminalWritesKeepsStderrClean is the regression for the layout
// corruption this exists to prevent: while the inline renderer owns the
// terminal, a raw line on stderr is an unaccounted scroll that desyncs the
// renderer's cell diff from the real screen (ghost cursors, stale columns).
//
// Both writer kinds are exercised because the tree uses both: the standard
// logger (agent.persistDeltaSnapshot's "session delta save failed",
// agent.persistFullSnapshot's "session save failed", internal/session's save
// warnings, …) and direct os.Stderr writes (agent.normalizeFetchMode's
// web_fetch_mode warning). Diverting only one would leave the other
// corrupting the frame.
func TestDivertTerminalWritesKeepsStderrClean(t *testing.T) {
	read := captureRealStderr(t)

	restore := divertTerminalWrites()

	// Standard logger, as the per-turn persistence failures emit it.
	log.Printf("session delta save failed (id=%s): %v", "abc123", errors.New("disk full"))
	log.Printf("auto-compaction failed (backing off): %v", errors.New("no API key"))

	// A direct os.Stderr write, as agent.normalizeFetchMode emits it.
	fmt.Fprintf(os.Stderr, "Warning: unknown web_fetch_mode %q; using \"https\"\n", "htps")

	restore()

	if got := read(); got != "" {
		t.Fatalf("terminal-unsafe output reached stderr while the renderer owned the screen:\n%q", got)
	}
}

// TestDivertTerminalWritesRestoresWriters checks the restore contract: after
// it returns, the logger and os.Stderr are back to what they were, so
// teardown diagnostics (and the "TUI error" report in Run) reach the shell
// instead of the diversion sink.
func TestDivertTerminalWritesRestoresWriters(t *testing.T) {
	read := captureRealStderr(t)

	prevStderr := os.Stderr
	prevLogWriter := log.Writer()

	restore := divertTerminalWrites()
	if os.Stderr == prevStderr {
		t.Error("os.Stderr not diverted while the renderer owns the screen")
	}
	if log.Writer() == prevLogWriter {
		t.Error("standard logger not diverted while the renderer owns the screen")
	}

	restore()

	if os.Stderr != prevStderr {
		t.Error("os.Stderr not restored")
	}
	if log.Writer() != prevLogWriter {
		t.Error("standard logger not restored")
	}

	// And the restored writers actually reach stderr again.
	log.Printf("after restore")
	fmt.Fprint(os.Stderr, "after restore\n")

	if got := read(); !strings.Contains(got, "after restore") {
		t.Fatalf("restored writers did not reach stderr; got %q", got)
	}
}

// TestDivertTerminalWritesRestoreIsIdempotent: Run restores explicitly and
// then again via defer, so a double restore must not resurrect a closed sink.
func TestDivertTerminalWritesRestoreIsIdempotent(t *testing.T) {
	read := captureRealStderr(t)

	prevStderr := os.Stderr
	prevLogWriter := log.Writer()

	restore := divertTerminalWrites()
	restore()
	restore()

	if os.Stderr != prevStderr {
		t.Error("first restore did not take")
	}
	if log.Writer() != prevLogWriter {
		t.Error("first restore did not take")
	}
	log.Print("still fine")
	if got := read(); !strings.Contains(got, "still fine") {
		t.Fatalf("third restore corrupted the writers; got %q", got)
	}
}

// TestDivertTerminalWritesToFileWhenConfigured: the default discard is the
// safe choice, but it must not be the only one — GOGEN_TUI_LOG keeps the
// diagnostics that a user debugging a failed turn actually needs.
func TestDivertTerminalWritesToFileWhenConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "tui.log")
	t.Setenv(tuiLogEnv, path)

	read := captureRealStderr(t)

	restore := divertTerminalWrites()
	log.Printf("session save failed (id=%s): %v", "abc123", errors.New("disk full"))
	restore()

	if got := read(); got != "" {
		t.Fatalf("log reached stderr even with %s set:\n%q", tuiLogEnv, got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	if !strings.Contains(string(data), "session save failed") {
		t.Fatalf("log file missing the entry; got %q", data)
	}
}

// TestDivertTerminalWritesFallsBackToDiscardNotStderr: an unopenable
// GOGEN_TUI_LOG must not fall back to stderr — that would reintroduce the
// very corruption this function prevents.
func TestDivertTerminalWritesFallsBackToDiscardNotStderr(t *testing.T) {
	// A path under a regular file can never be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv(tuiLogEnv, filepath.Join(blocker, "tui.log"))

	read := captureRealStderr(t)

	restore := divertTerminalWrites()
	log.Printf("must not reach the terminal")
	restore()

	if got := read(); got != "" {
		t.Fatalf("unopenable log path fell back to stderr:\n%q", got)
	}
}
