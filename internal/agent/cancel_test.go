//go:build unix

package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPersistentShellRunHonoursCancel pins B12a: the shell's completion poll
// only ended on its own timeout (300 s by default), so pressing Esc left the
// turn waiting for the whole budget. Cancelling must return promptly and kill
// the shared shell (its leftovers would corrupt the next Run's output).
func TestPersistentShellRunHonoursCancel(t *testing.T) {
	s, err := newPersistentShell(t.TempDir(), []string{"TERM=dumb"})
	if err != nil {
		t.Fatalf("start persistent shell: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := s.Run(ctx, "sleep 30", 60*time.Second)
		done <- err
	}()
	// Let the command reach the shell before cancelling.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	if !s.Exited() {
		t.Fatal("the shell survived a cancelled Run; the next command would race its leftovers")
	}
}

// TestBackgroundJobInputHonoursCancelledContext pins B12b's fast path: a
// cancelled turn must not start a write that can block on a full pipe.
func TestBackgroundJobInputHonoursCancelledContext(t *testing.T) {
	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)
	defer a.Close()
	if _, err := a.StartBackgroundCommand(context.Background(), "sleep 30"); err != nil {
		t.Fatalf("start job: %v", err)
	}
	var id string
	a.bgMu.Lock()
	for k := range a.bgJobs {
		id = k
	}
	a.bgMu.Unlock()
	if id == "" {
		t.Fatal("no background job registered")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.BackgroundJobInput(ctx, id, "x", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("input error = %v, want context.Canceled", err)
	}
}
