package agent

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// waitForBackground polls cond until it holds or the budget runs out.
func waitForBackground(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// requireNoBackgroundEvent asserts nothing arrives on ch within the window.
func requireNoBackgroundEvent(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s fired within %s", what, within)
	case <-time.After(within):
	}
}

// TestHasBackgroundWorkFollowsJobLifetime pins the liveness query hosts use to
// decide that a session is NOT idle: a running execute_command background=true
// job counts as work, a finished one does not (it stays pollable, but nothing
// is running any more, so deferring a teardown for it would leak).
func TestHasBackgroundWorkFollowsJobLifetime(t *testing.T) {
	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)
	defer a.Close()

	if a.HasBackgroundWork() {
		t.Fatal("a fresh session reports background work")
	}

	id, err := a.StartBackgroundCommand(context.Background(), "sleep 30")
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	if !a.HasBackgroundWork() {
		t.Fatal("a running background job does not count as background work")
	}

	if _, err := a.CancelBackgroundJob(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitForBackground(t, "the cancelled job to leave HasBackgroundWork", func() bool {
		return !a.HasBackgroundWork()
	})
}

// TestBackgroundIdleHookFiresOnlyWhenDrained pins the drain callback's
// contract: it must not fire while work is still running, and it fires once
// the session owns nothing more. A host that defers a teardown while work runs
// (the web registry refusing to evict a session with a live job) relies on
// this to run that teardown afterwards.
func TestBackgroundIdleHookFiresOnlyWhenDrained(t *testing.T) {
	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)
	defer a.Close()

	drained := make(chan struct{}, 4)
	a.SetBackgroundIdleHook(func() {
		// The hook runs on the job's wait goroutine: it must see the
		// session as already drained, and must not hold any agent lock.
		if a.HasBackgroundWork() {
			t.Error("idle hook fired while background work was still running")
		}
		drained <- struct{}{}
	})

	id, err := a.StartBackgroundCommand(context.Background(), "sleep 30")
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	requireNoBackgroundEvent(t, drained, 250*time.Millisecond, "background idle hook")

	if _, err := a.CancelBackgroundJob(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("background idle hook did not fire after the last job drained")
	}
}

// TestBackgroundIdleHookSuppressedAfterClose pins the Close latch: tearing a
// session down cancels its jobs, whose wait goroutines unwind moments later —
// the hook must not fire then, or a host would re-run its teardown decision
// for a session it is already closing.
func TestBackgroundIdleHookSuppressedAfterClose(t *testing.T) {
	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)

	fired := make(chan struct{}, 4)
	a.SetBackgroundIdleHook(func() { fired <- struct{}{} })

	if _, err := a.StartBackgroundCommand(context.Background(), "sleep 30"); err != nil {
		t.Fatalf("start background job: %v", err)
	}
	a.Close()
	requireNoBackgroundEvent(t, fired, 750*time.Millisecond, "background idle hook after Close")
}

// TestBackgroundWorkCountsPersistentTerminal extends the liveness query to
// the terminal tool: an open PTY is background work (a dev server started in
// one keeps running), and closing the last one drains the session.
func TestBackgroundWorkCountsPersistentTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no PTY on Windows")
	}
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
