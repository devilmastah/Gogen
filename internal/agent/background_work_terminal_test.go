//go:build unix

package agent

import (
	"context"
	"testing"
	"time"
)

// TestBackgroundWorkCountsPersistentTerminal extends the liveness query to
// the terminal tool: an open PTY is background work (a dev server started in
// one keeps running), and closing the last one drains the session.
//
// Unix-only: it uses openTestTerminal, which lives in the unix-gated
// terminal_test.go (the terminal tool needs a PTY), so the test cannot
// compile on Windows — a runtime GOOS skip is not enough. It therefore lives
// behind the same build tag instead of in the cross-platform
// background_work_test.go.
func TestBackgroundWorkCountsPersistentTerminal(t *testing.T) {
	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)
	a.SetTerminalEnabled(true)
	defer a.Close()

	drained := make(chan struct{}, 4)
	a.SetBackgroundIdleHook(func() { drained <- struct{}{} })

	id := openTestTerminal(t, a)
	if !a.HasBackgroundWork() {
		t.Fatal("an open persistent terminal does not count as background work")
	}
	if _, err := handleTerminal(context.Background(), a, map[string]any{"action": "close", "session_id": id}); err != nil {
		t.Fatalf("terminal close: %v", err)
	}
	waitForBackground(t, "the closed terminal to leave HasBackgroundWork", func() bool {
		return !a.HasBackgroundWork()
	})
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("background idle hook did not fire after the last terminal closed")
	}
}
