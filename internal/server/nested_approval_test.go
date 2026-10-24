package server

// Tests for nested (subagent) delete-approval routing (D6): an approval must
// reach a LIVE runtime resolved at approval time — the spawn-time parent
// pointer goes stale when the parent is orphan-evicted while its background
// child keeps running and later reopened as a fresh runtime under the same
// id — and an approval nobody can answer must be DENIED, never parked on a
// runtime with no attached clients (a parked approval would hang the child's
// turn forever: the auto-deny machinery is armed only by client detach, and
// a headless child has no client; recovery would only be interrupt_agent).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

// parkedApprovalID polls rt's pending-approval map until a request is parked
// and returns its id. Fails the test on timeout.
func parkedApprovalID(t *testing.T, rt *sessionRuntime) string {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return parkedApprovalCount(rt) > 0 })
	rt.approvalMu.Lock()
	defer rt.approvalMu.Unlock()
	for id := range rt.approvals {
		return id
	}
	return ""
}

// parkedApprovalCount returns the number of delete approvals parked on rt.
func parkedApprovalCount(rt *sessionRuntime) int {
	rt.approvalMu.Lock()
	defer rt.approvalMu.Unlock()
	return len(rt.approvals)
}

// broadcastApprovalReceived drains ws's send queue looking for a
// delete_approval frame with the given approvalId (destructive: frames read
// before the match are dropped). Use it inside waitFor-style polling.
func broadcastApprovalReceived(ws *wsConn, approvalID string) bool {
	for {
		select {
		case m := <-ws.sendQ:
			if m.Type == "delete_approval" && m.ApprovalID == approvalID {
				return true
			}
		default:
			return false
		}
	}
}

// TestNestedDeleteApproverRouting covers the branches of the shared D6
// override, including the stale-pointer regression: an approver closure
// captured before the parent is evicted must follow the parent's REOPENED
// runtime (same id, fresh runtime), and it must deny — never park — when
// nobody can answer.
func TestNestedDeleteApproverRouting(t *testing.T) {
	r := newSessionRegistry(100)
	parent := newTestRuntime(t)
	parent.agent.SessionID = "parent"
	r.register("parent", parent)
	child := newTestRuntime(t)
	child.agent.SessionID = "child"
	// The override is captured ONCE, up front — exactly like the spawner
	// installs it at spawn time; every sub-test below calls this same
	// closure while the parent's runtime is replaced underneath it.
	child.approverOverride = r.nestedDeleteApprover(child, "parent")
	approver := child.approverOverride
	ctx := context.Background()
	req := agent.DeleteRequest{Paths: []string{"a.txt"}, Reason: "test"}

	t.Run("routes to the live parent's clients when the child is headless", func(t *testing.T) {
		ws := newFakeWSConn()
		parent.attach(ws)
		defer parent.detach(ws)

		outcome := make(chan bool, 1)
		go func() {
			approved, err := approver(ctx, req)
			if err != nil {
				t.Errorf("approver error: %v", err)
			}
			outcome <- approved
		}()
		id := parkedApprovalID(t, parent)
		if parkedApprovalCount(child) != 0 {
			t.Fatal("the approval must not be parked on the child")
		}
		waitFor(t, time.Second, func() bool { return broadcastApprovalReceived(ws, id) })
		parent.completeApproval(id, true)
		if !<-outcome {
			t.Fatal("approved response must return true")
		}
	})

	t.Run("prefers the child's own attached clients", func(t *testing.T) {
		ws := newFakeWSConn()
		child.attach(ws)
		defer child.detach(ws)

		outcome := make(chan bool, 1)
		go func() {
			approved, err := approver(ctx, req)
			if err != nil {
				t.Errorf("approver error: %v", err)
			}
			outcome <- approved
		}()
		id := parkedApprovalID(t, child)
		if parkedApprovalCount(parent) != 0 {
			t.Fatal("the approval must not be routed to the parent while the child has clients")
		}
		child.completeApproval(id, true)
		if !<-outcome {
			t.Fatal("approved response must return true")
		}
	})

	t.Run("denies when the live parent has no clients", func(t *testing.T) {
		approved, err := approver(ctx, req)
		if err != nil {
			t.Fatalf("deny must not error: %v", err)
		}
		if approved {
			t.Fatal("an approval nobody can answer must be denied")
		}
		if parkedApprovalCount(parent) != 0 || parkedApprovalCount(child) != 0 {
			t.Fatal("a denied approval must not be parked anywhere")
		}
	})

	t.Run("follows the parent's reopened runtime", func(t *testing.T) {
		// Orphan-evict the parent and reopen it as a FRESH runtime under the
		// same id: the closure captured at the top must route to the fresh
		// runtime, not to the dead pointer it was spawned with.
		r.evictRuntime(parent)
		fresh := newTestRuntime(t)
		fresh.agent.SessionID = "parent"
		r.register("parent", fresh)
		ws := newFakeWSConn()
		fresh.attach(ws)

		outcome := make(chan bool, 1)
		go func() {
			approved, err := approver(ctx, req)
			if err != nil {
				t.Errorf("approver error: %v", err)
			}
			outcome <- approved
		}()
		id := parkedApprovalID(t, fresh)
		if parkedApprovalCount(parent) != 0 {
			t.Fatal("the approval must not be parked on the evicted runtime")
		}
		waitFor(t, time.Second, func() bool { return broadcastApprovalReceived(ws, id) })
		fresh.completeApproval(id, true)
		if !<-outcome {
			t.Fatal("approved response must return true")
		}
		parent = fresh // later sub-tests resolve "parent" to the live runtime
	})

	t.Run("denies when the parent runtime is gone", func(t *testing.T) {
		r.evictRuntime(parent)
		approved, err := approver(ctx, req)
		if err != nil {
			t.Fatalf("deny must not error: %v", err)
		}
		if approved {
			t.Fatal("an approval with no live parent must be denied")
		}
		if parkedApprovalCount(child) != 0 {
			t.Fatal("a denied approval must not be parked on the child")
		}
	})
}

// TestSubagentApprovalRoutesToReopenedParent drives the reported hang end to
// end: a background child's delete approval, requested after its parent was
// orphan-evicted and reopened (fresh runtime, same id, client attached), must
// reach the reopened runtime's clients — pre-fix it was routed to the dead
// spawn-time pointer and waited forever.
func TestSubagentApprovalRoutesToReopenedParent(t *testing.T) {
	dir := t.TempDir()
	// delete Lstats the target before asking for approval, so the file must
	// exist for the approval request to fire.
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_del",
		Name: "delete",
		Args: map[string]any{"path": "victim.txt"},
	}}
	s, a, store := newContinuationServer(t, stub, dir)
	a.SetSubagentsEnabled(true)
	// Persist the parent so the reopen below can rebuild it from the store
	// (a message-less agent would be skipped by the store's empty-save guard).
	if err := store.Save(a.SessionID, agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "parent"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadInWorkingDir(dir, a.SessionID); err != nil {
		t.Fatalf("parent must be persisted before the eviction: %v", err)
	}
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour // keep the finished child registered for the assertions
	// Cleanups run LIFO, and the spawn goroutine is what ENQUEUES the
	// completion notice: wait for the spawns to settle first, THEN for the
	// delivery turn it queued. Registering the parent-delivery wait last
	// (so it runs first) let the notice be enqueued after that wait had
	// already observed an idle parent — the delivery turn then wrote the
	// session concurrently with t.TempDir's RemoveAll ("directory not
	// empty", Windows). This mirrors newContinuableServer's ordering.
	t.Cleanup(func() { waitForParentDeliveriesSettled(t, s, a) })
	t.Cleanup(func() { waitForChildSpawnsSettled(t, s) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childID, err := sp.SpawnBackground(ctx, a, "delete the victim", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(childID)
		return ok
	})
	stub.waitBlocked(1) // the child's turn is parked in the provider

	// Evict the parent (no clients, idle) while its background child keeps
	// running. NOTE: this uses the explicit eviction tail, NOT
	// evictOrphaned: an unwatched parent that still owns a live background
	// child is no longer an orphan (keepsBackgroundWork), which is the
	// policy TestSwitchKeepsSessionWithBackgroundSubagent pins. The tail
	// here is the one that fires on an end-of-session (close/delete/
	// shutdown) and on cap eviction, and — like orphan eviction — it does
	// NOT cancel children, so the child's approval keeps flowing.
	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("parent runtime not registered")
	}
	s.registry.evictRuntime(parentRt)
	if _, ok := s.registry.get(a.SessionID); ok {
		t.Fatal("parent must be unregistered after eviction")
	}

	// Reopen the parent (the sidebar attach path) and attach a client: the
	// approval must reach THIS runtime, not the dead spawn-time pointer.
	freshRt, err := s.loadOrCreateRuntime(a.SessionID)
	if err != nil {
		t.Fatalf("reopen parent: %v", err)
	}
	ws := newFakeWSConn()
	freshRt.attach(ws)

	// Release the child's blocked round: the delete tool runs and requests
	// approval.
	stub.releaseN(1)
	approvalID := parkedApprovalID(t, freshRt)
	if parkedApprovalCount(parentRt) != 0 {
		t.Fatal("the approval must not be parked on the evicted runtime")
	}
	if !broadcastApprovalReceived(ws, approvalID) {
		t.Fatal("no delete_approval broadcast on the reopened parent's client")
	}

	// Approve from the reopened pane: the delete executes and the child
	// finishes (round 2 is released so the turn can complete).
	freshRt.completeApproval(approvalID, true)
	stub.releaseN(2)
	waitFor(t, 5*time.Second, func() bool {
		_, statErr := os.Stat(victim)
		return os.IsNotExist(statErr)
	})
	waitFor(t, 5*time.Second, func() bool {
		c := sp.Get(childID)
		return c != nil && c.IsFinished()
	})
	stub.releaseN(3) // the completion notice's delivery turn on the reopened parent
}

// TestSubagentApprovalDeniedWhenNobodyCanAnswer covers the fail-closed half:
// with the parent orphan-evicted and NOT reopened, no runtime can answer the
// headless child's approval — it must be denied immediately (turn continues
// with the "not approved" tool result), not parked on a dead runtime with no
// auto-deny armed.
func TestSubagentApprovalDeniedWhenNobodyCanAnswer(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_del",
		Name: "delete",
		Args: map[string]any{"path": "victim.txt"},
	}}
	s, a, _ := newContinuationServer(t, stub, dir)
	a.SetSubagentsEnabled(true)
	sp := continuableSpawner(t, s)
	t.Cleanup(func() { waitForChildSpawnsSettled(t, s) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childID, err := sp.SpawnBackground(ctx, a, "delete the victim", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	childRt, ok := s.registry.get(childID)
	if !ok {
		t.Fatal("child runtime not registered")
	}
	stub.waitBlocked(1)

	// Orphan-evict the parent: nobody is left to answer the child's approval.
	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("parent runtime not registered")
	}
	s.registry.evictOrphaned(parentRt)

	// The denied delete must not hang the turn: round 2 is released and the
	// child completes with the "not approved" tool result.
	stub.releaseN(1)
	stub.releaseN(2)
	waitFor(t, 10*time.Second, func() bool {
		for _, m := range childRt.agent.SnapshotMessages() {
			if m.Role == "tool" && m.ToolCallID == "call_del" && strings.Contains(m.Content, "denied by user") {
				return true
			}
		}
		return false
	})
	waitFor(t, 5*time.Second, func() bool {
		c := sp.Get(childID)
		return c != nil && c.IsFinished()
	})
	// The denial is fail-closed: the victim file must still exist, and
	// nothing may be parked on the dead parent runtime.
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("denied delete must not remove the victim file: %v", err)
	}
	if parkedApprovalCount(parentRt) != 0 || parkedApprovalCount(childRt) != 0 {
		t.Fatal("a denied approval must not be parked anywhere")
	}
}

// TestEvictRuntimeDeniesParkedApproval pins the eviction tail: a runtime
// leaving the registry can never answer an approval parked on it, so
// evictRuntime must deny it — otherwise a background child whose approval was
// routed to the runtime before its eviction would wait forever on a dead
// runtime with no auto-deny armed (that machinery is armed by client detach,
// and the runtime already has none).
func TestEvictRuntimeDeniesParkedApproval(t *testing.T) {
	r := newSessionRegistry(10)
	rt := newTestRuntime(t)
	rt.agent.SessionID = "s"
	r.register("s", rt)

	// Park an approval directly on the runtime (as a routed child approval
	// would sit before the eviction). No client is attached, so the
	// last-detach auto-deny never arms.
	done := make(chan bool, 1)
	go func() {
		approved, err := rt.deleteApprover()(context.Background(), agent.DeleteRequest{Paths: []string{"x"}, Reason: "test"})
		if err != nil {
			t.Errorf("approver error: %v", err)
		}
		done <- approved
	}()
	parkedApprovalID(t, rt)

	r.evictRuntime(rt)
	select {
	case approved := <-done:
		if approved {
			t.Fatal("parked approval must be denied on eviction")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter still parked after eviction")
	}
	if parkedApprovalCount(rt) != 0 {
		t.Fatal("approvals still parked on the evicted runtime")
	}
}
