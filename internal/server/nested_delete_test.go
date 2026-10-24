package server

// Tests for the sidebar close/delete mechanics on nested (subagent)
// sessions: closing a child pane (session_close), or deleting a child
// whose outcome has not reached the parent yet (still running /
// interrupted), reports back to the parent agent via the delivery
// channel. Deleting a FINISHED child is silent: its completion notice
// already delivered the outcome, so no paid parent turn is spent on a
// housekeeping event.

import (
	"context"
	"strings"
	"testing"
	"time"

	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// TestSubagentCloseChildNotifiesParent drives the ✕ close on an open child
// pane: the child's in-flight turn is cancelled (the spawn fails) and the
// parent session receives a "[subagent ...] closed by the user" notice.
func TestSubagentCloseChildNotifiesParent(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	// A provider that blocks until the context is cancelled: the child's
	// turn is still running when the user closes the pane.
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{}
	}
	a.SetSubagentsEnabled(true)
	// Save the parent before spawning (mirroring production, where a
	// parent that spawns a subagent has already persisted its own turn):
	// the spawner's post-turn orphan check deletes the child only when
	// the parent's session is gone — an unsaved parent would read as
	// "gone" and the check could remove the closed child's file.
	a.Messages = []llm.Message{{Role: "user", Content: "hi"}}
	a.FlushSession()
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (newSubagentSpawner(s)).Spawn(ctx, a, "long job", "", 0)
		done <- err
	}()

	// Wait for the child runtime to be registered, then attach a pane to
	// it (the user opens the nested row) and close it (the ✕ on the pane).
	var childID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID {
				childID = id
				return true
			}
		}
		return false
	})
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: childID}); err != nil {
		t.Fatalf("attach child: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == childID
	})
	if err := conn.WriteJSON(WSMessage{Type: "session_close", SessionID: childID}); err != nil {
		t.Fatalf("close child: %v", err)
	}

	// The close cancels the child's turn: the spawn must fail.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("spawn must fail after the child pane was closed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return after the child pane was closed")
	}

	// The parent receives the close notice.
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "closed by the user") })

	// The cancelled turn's final persist lands before the close drain
	// returns, but wait for the file so the test's temp dir is never
	// removed while a late write is still in flight.
	waitFor(t, 5*time.Second, func() bool {
		_, err := store.LoadInWorkingDir(dir, childID)
		return err == nil
	})
	// The notice is delivered as a turn on the parent; it blocks on the
	// stub provider. Release it and wait for the turn to end so its final
	// flush completes before the test's temp dir is removed.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == a.SessionID
	})
}

// TestSubagentDeleteFinishedChildSilent drives the ✕ delete on a CLOSED
// nested row: the child's saved session is deleted by id and the parent
// receives NO notice — the finished child's completion notice already
// delivered its outcome, so the delete is housekeeping and must not spend
// a paid parent turn (a "[subagent ...] deleted by the user" message in
// the parent transcript is the regression this test guards against).
func TestSubagentDeleteFinishedChildSilent(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "child report"}}
		return p
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// A finished foreground child: the spawn completes, the runtime is
	// unregistered, and the session stays saved with ParentID. Spawn
	// returns the REPORT, not the id — find the child session in the store.
	sp := newSubagentSpawner(s)
	if _, err := sp.Spawn(context.Background(), a, "short job", "", 0); err != nil {
		t.Fatal(err)
	}
	var childID string
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, si := range list {
		if si.ParentID == a.SessionID {
			childID = si.ID
			break
		}
	}
	if childID == "" {
		t.Fatal("child session was not persisted with ParentID")
	}
	if _, ok := s.registry.get(childID); ok {
		t.Fatal("finished child must be unregistered")
	}

	// Delete the child by id (the sidebar ✕ on the closed nested row).
	if err := conn.WriteJSON(WSMessage{Type: "session_delete", SessionID: childID}); err != nil {
		t.Fatalf("delete child: %v", err)
	}

	// The child's file is gone (the delete is processed asynchronously on
	// the connection read loop) and the parent receives NO notice: a
	// (buggy) notice would deliver within the settle window below.
	waitFor(t, 5*time.Second, func() bool {
		_, err := store.LoadInWorkingDir(dir, childID)
		return err != nil
	})
	requireNever(t, 500*time.Millisecond,
		"deleting a finished child must not notify the parent (its outcome already landed)",
		func() bool { return deliveredMessages(a, "deleted by the user") })
}

// TestSubagentDeleteFinishedRegisteredChildSilent covers the
// registered-runtime + finished-child case: a background child inside its
// retention window is still REGISTERED when deleted, but its completion
// notice already delivered the outcome — the delete must be silent (no
// paid parent turn), same as the unregistered case.
func TestSubagentDeleteFinishedRegisteredChildSilent(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "bg report"}}
		return p
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	sp := continuableSpawner(t, s)
	sp.retain = time.Hour // keep the finished child registered
	id, err := sp.SpawnBackground(context.Background(), a, "bg job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child != nil && child.IsFinished() })
	if _, ok := s.registry.get(id); !ok {
		t.Fatal("finished child must still be registered within the retention window")
	}

	if err := conn.WriteJSON(WSMessage{Type: "session_delete", SessionID: id}); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := store.LoadInWorkingDir(dir, id)
		return err != nil
	})
	requireNever(t, 500*time.Millisecond,
		"deleting a finished (still-registered) child must not notify the parent",
		func() bool { return deliveredMessages(a, "deleted by the user") })
}

// TestSubagentDeleteChildNotifiesParentRunning covers the registered-runtime
// path of sessionDelete: deleting a child whose runtime is still live (a
// running foreground subagent) cancels its turn and still reports to the
// parent.
func TestSubagentDeleteChildNotifiesParentRunning(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{}
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (newSubagentSpawner(s)).Spawn(ctx, a, "long job", "", 0)
		done <- err
	}()
	var childID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID {
				childID = id
				return true
			}
		}
		return false
	})

	if err := conn.WriteJSON(WSMessage{Type: "session_delete", SessionID: childID}); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "subagent") {
			t.Fatalf("spawn error = %v, want a subagent error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return after the child was deleted")
	}
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "deleted by the user") })
	// The notice is delivered as a turn on the parent; it blocks on the
	// stub provider. Release it and wait for the turn to end so its final
	// flush completes before the test's temp dir is removed.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == a.SessionID
	})
}
