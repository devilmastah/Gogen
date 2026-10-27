package procutil

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
)

func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"plain error", errors.New("boom"), UnknownExit},
		{"exit 3", exec.Command("sh", "-c", "exit 3").Run(), 3},
		{"wrapped exit 7", wrap(exec.Command("sh", "-c", "exit 7").Run()), 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Fatalf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestWasSignaled(t *testing.T) {
	if runtime.GOOS == "windows" {
		// TerminateProcess gives the child a plain exit code (1), never the
		// negative "signaled" status Wait reports on POSIX.
		t.Skip("Windows terminates processes without POSIX signal semantics")
	}
	// A child killed by SIGKILL reports a negative exit code from exec.
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = cmd.Process.Kill()
	err := cmd.Wait()
	if !WasSignaled(err) {
		t.Fatalf("killed child: WasSignaled(%v) = false", err)
	}
	if got := ExitCode(err); got != UnknownExit {
		t.Fatalf("killed child ExitCode = %d, want %d", got, UnknownExit)
	}
	if WasSignaled(nil) || WasSignaled(errors.New("boom")) {
		t.Fatal("WasSignaled must be false without an ExitError")
	}
}

func wrap(err error) error {
	if err == nil {
		return nil
	}
	return &wrapped{err}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
