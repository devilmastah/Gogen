package server

import (
	"context"
	"testing"
	"time"
)

// TestNoFSLockWriteOpSkipsLockAndLoop pins the git_push fix: a network-only
// write op must not hold the global workspace lock (it stalled every session's
// file edits for the whole push) and must not block the connection's read loop.
func TestNoFSLockWriteOpSkipsLockAndLoop(t *testing.T) {
	conn := newFakeWSConn()
	s := &Server{ws: &Workspace{}}

	entered := make(chan struct{})
	release := make(chan struct{})
	fsWriteOps["test_offlock"] = fsOp{
		resultType: "test_offlock_result",
		noFSLock:   true,
		run: func(*Server, context.Context, WSMessage, *WSMessage) error {
			close(entered)
			<-release
			return nil
		},
	}
	t.Cleanup(func() { delete(fsWriteOps, "test_offlock") })

	done := make(chan struct{})
	go func() {
		s.handleFSWriteMessage(conn, context.Background(), WSMessage{Type: "test_offlock"})
		close(done)
	}()

	<-entered // the op is running
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleFSWriteMessage blocked on an off-lock op")
	}

	// The workspace lock must still be free while the op runs.
	locked := make(chan struct{})
	go func() {
		s.ws.fsMu.Lock()
		close(locked)
		s.ws.fsMu.Unlock()
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("an off-lock write op is holding the workspace filesystem lock")
	}
	close(release)
}

// TestGitPushIsMarkedOffLock is the table guard: the flag is what keeps a slow
// push from blocking every session's edits, so it must not be dropped.
func TestGitPushIsMarkedOffLock(t *testing.T) {
	op, ok := fsWriteOps["git_push"]
	if !ok {
		t.Fatal("git_push is not in the write dispatch table")
	}
	if !op.noFSLock {
		t.Fatal("git_push must run off the workspace lock (noFSLock)")
	}
	if op.resultType != "git_push_result" {
		t.Fatalf("resultType = %q", op.resultType)
	}
}
