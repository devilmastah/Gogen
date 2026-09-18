package server

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/session"
)

// deliveredCount reports how many user messages in the agent's snapshot
// contain needle.
func deliveredCount(a *agent.Agent, needle string) int {
	n := 0
	for _, m := range a.SnapshotMessages() {
		if m.Role == "user" && strings.Contains(m.Content, needle) {
			n++
		}
	}
	return n
}

// deliveredMessages reports whether any user message in the agent's
// snapshot contains needle.
func deliveredMessages(a *agent.Agent, needle string) bool {
	for _, m := range a.SnapshotMessages() {
		if m.Role == "user" && strings.Contains(m.Content, needle) {
			return true
		}
	}
	return false
}

// TestDeliverToSessionIdle delivers a notice to an idle session: the
// message is appended as a user message and a turn runs against the mock
// provider.
func TestDeliverToSessionIdle(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	p := llmtest.NewMockProvider()
	p.StreamResults = []*llm.StreamResult{{Content: "ack"}}
	a.Provider = p

	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	rt.deliverToSession("background notice here")

	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "background notice here")
	})
	// The delivered turn completed: the assistant ack is in history.
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range a.SnapshotMessages() {
			if m.Role == "assistant" && strings.Contains(m.Content, "ack") {
				return true
			}
		}
		return false
	})
	// The turn's final flush must land before the test returns, or it races
	// t.TempDir()'s removal (Windows flakes with "TempDir RemoveAll cleanup:
	// ... directory is not empty"): the ack becomes memory-visible BEFORE the
	// turn goroutine's closing persist. Same store-wait pattern as
	// TestBoardStartStaleAgentLink.
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(dir, a.SessionID)
		if err != nil {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && strings.Contains(m.Content, "ack") {
				return true
			}
		}
		return false
	})
}

// TestDeliverToSessionBusyQueuesUntilIdle delivers while a turn is running:
// the notice must NOT be injected mid-turn; it arrives after the turn ends
// (the worker wakes on setTurnActive(false)).
func TestDeliverToSessionBusyQueuesUntilIdle(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	// The default provider is the blocking stub: the in-flight turn stays
	// active until cancelled.
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}

	// Simulate an in-flight user turn: acquire the turn lock and hand it to
	// startTurn (whose goroutine defers the unlock).
	rt.turnMu.Lock()
	rt.startTurn(nil, "block me", nil)
	time.Sleep(100 * time.Millisecond) // let the turn enter the provider

	rt.deliverToSession("queued notice")
	// Give the worker time to attempt acquisition and fail.
	time.Sleep(300 * time.Millisecond)
	if deliveredMessages(a, "queued notice") {
		t.Fatal("notice must not be delivered while a turn is running")
	}

	// Cancel the blocking turn; the delivery worker wakes at turn end.
	rt.stream.cancelInFlight()
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "queued notice")
	})

	// The queued notice started its OWN turn against the blocking stub
	// (stream call 2), which blocks forever unless released. Release it and
	// wait for the reply to be PERSISTED before returning: a turn still
	// flushing its final state would race t.TempDir()'s removal and flake
	// Windows with "TempDir RemoveAll cleanup: ... The directory is not
	// empty" (same store-wait pattern as TestBoardStartStaleAgentLink).
	stub.releaseN(2)
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(dir, a.SessionID)
		if err != nil {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "done" {
				return true
			}
		}
		return false
	})
}

// TestDeliverQueueOverflow pins the bounded queue: on overflow the OLDEST
// delivery is dropped and the rest are delivered in order.
func TestDeliverQueueOverflow(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	p := llmtest.NewMockProvider()
	// The FIRST stream call (the "block me" turn started below) blocks
	// until cancelled, so the delivery worker's tryAcquireTurn fails while
	// it runs and the burst below cannot be popped mid-append. Later calls
	// (the delivered turns) answer immediately.
	first := true
	entered := make(chan struct{})
	p.OnStream = func(ctx context.Context, _ []llm.Message, h *llm.StreamHandlers) (*llm.StreamResult, error) {
		if first {
			first = false
			if h != nil && h.OnStreamOpened != nil {
				h.OnStreamOpened()
			}
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &llm.StreamResult{Content: "ack"}, nil
	}
	a.Provider = p

	rt, _ := s.registry.get(a.SessionID)
	// The six deliveries must land as one burst: the blocking turn holds
	// turnMu, so the worker cannot pop the first notice mid-append (a
	// worker that delivers notice-1 before the burst completes would leave
	// no overflow to drop). The 6th append overflows the cap and drops the
	// OLDEST (notice-1).
	rt.turnMu.Lock()
	rt.startTurn(nil, "block me", nil)
	// Wait until the blocking turn is actually inside the provider (it
	// holds turnMu for the whole turn): the burst below must land while
	// the delivery worker cannot acquire the lock, or it would pop
	// notice-1 mid-append and the overflow would never happen. A fixed
	// sleep would race the turn's scheduling under load.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking turn never entered the provider")
	}
	for i := 1; i <= 6; i++ {
		rt.deliverToSession("notice-" + string(rune('0'+i)))
	}
	// End the blocking turn: the worker wakes at turn end and drains the
	// queue (2..6 — notice-1 was dropped by the overflow).
	rt.stream.cancelInFlight()
	// The delivered turns complete quickly (mock provider); wait for the
	// last one, then settle.
	waitFor(t, 10*time.Second, func() bool {
		return deliveredMessages(a, "notice-6")
	})
	time.Sleep(200 * time.Millisecond)
	if deliveredMessages(a, "notice-1") {
		t.Fatal("oldest notice must be dropped on overflow")
	}
	for i := 2; i <= 6; i++ {
		if !deliveredMessages(a, "notice-"+string(rune('0'+i))) {
			t.Fatalf("notice-%d missing", i)
		}
	}
}

// TestDeliverToSessionOverflowNoDeadlock pins the overflow-notice ordering:
// the "queue full" broadcast must run OUTSIDE deliverMu — a failing write on
// the last attached client detaches it and funnels into evictOrphaned →
// hasPendingDeliveries, which takes deliverMu. Broadcasting under the lock
// deadlocked that chain on the producer goroutine.
func TestDeliverToSessionOverflowNoDeadlock(t *testing.T) {
	reg := newSessionRegistry(8)
	rt := &sessionRuntime{
		registry:      reg,
		clients:       make(map[*wsConn]struct{}),
		agent:         &agent.Agent{},
		deliverNotify: make(chan struct{}, 1),
	}
	for i := 0; i < defaultDeliverQueueCap; i++ {
		rt.pendingDeliver = append(rt.pendingDeliver, deliverItem{ID: newQueueItemID(), Text: "stale", Kind: deliverSystem})
	}
	// No worker goroutine for this unit test: the queue stays at cap so the
	// overflow branch fires deterministically.
	rt.deliverWorker.Store(true)
	// A nil-conn wsConn fails writeJSON immediately (errWSClosed), so the
	// overflow notice's broadcast detaches it — the exact chain that used to
	// deadlock while deliverMu was held.
	dead := &wsConn{}
	rt.clients[dead] = struct{}{}
	reg.agents["test-session"] = rt

	done := make(chan struct{})
	go func() {
		rt.deliverToSession("overflow notice")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliverToSession deadlocked: overflow notice broadcast ran under deliverMu")
	}

	// The drop-oldest + append invariant still holds after the fix.
	rt.deliverMu.Lock()
	n := len(rt.pendingDeliver)
	last := rt.pendingDeliver[n-1]
	rt.deliverMu.Unlock()
	if n != defaultDeliverQueueCap || last.Text != "overflow notice" {
		t.Fatalf("queue = %d entries, last %q; want cap=%d with the new notice at the tail", n, last.Text, defaultDeliverQueueCap)
	}
}

// TestJobNoticeDeliveredToSession is the end-to-end path: with
// job_notices on, a background command finishing naturally on the initial
// agent delivers its summary into the session as a user message.
func TestJobNoticeDeliveredToSession(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	_ = NewServer(a, &config.Config{JobNotices: "on"})
	// The notice's delivery turn needs a provider that answers.
	p := llmtest.NewMockProvider()
	p.StreamResults = []*llm.StreamResult{{Content: "ack"}}
	a.Provider = p

	if _, err := a.StartBackgroundCommand(context.Background(), "echo job-done"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		return deliveredMessages(a, "[job]") && deliveredMessages(a, "echo job-done")
	})
}

// TestDeliverToSessionEvictedDrop delivers to an evicted runtime: the
// message is dropped silently (no delivery, no panic).
func TestDeliverToSessionEvictedDrop(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	rt, _ := s.registry.get(a.SessionID)

	rt.evicted.Store(true)
	if rt.deliverToSession("dropped notice") {
		t.Fatal("deliverToSession must refuse an evicted runtime")
	}
	time.Sleep(200 * time.Millisecond)
	if deliveredMessages(a, "dropped notice") {
		t.Fatal("notice must not be delivered to an evicted runtime")
	}
	// A nil runtime is a no-op too.
	var nilRt *sessionRuntime
	if nilRt.deliverToSession("nope") {
		t.Fatal("deliverToSession must refuse a nil runtime")
	}
}

// TestDeliverToParentQueuesUntilRegistered pins the deferred-delivery
// contract: a message for a session whose runtime is not live is queued in
// the registry and flushed into the runtime when the session is next
// registered (reopened from the store) — nothing is lost in the gap.
func TestDeliverToParentQueuesUntilRegistered(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	p := llmtest.NewMockProvider()
	p.StreamResults = []*llm.StreamResult{{Content: "ack"}}
	a.Provider = p

	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	// Evict the parent (idle, no viewers) and deliver to it: the message
	// must be queued, not dropped.
	s.registry.evictRuntime(parentRt)
	s.registry.deliverToParent(a.SessionID, "delayed notice")
	time.Sleep(200 * time.Millisecond)
	if deliveredMessages(a, "delayed notice") {
		t.Fatal("message must not deliver while the runtime is not live")
	}
	s.registry.parentDeliverMu.Lock()
	queued := len(s.registry.parentDeliveries[a.SessionID])
	s.registry.parentDeliverMu.Unlock()
	if queued != 1 {
		t.Fatalf("queued = %d, want 1", queued)
	}

	// Reopen the session: registering a fresh runtime flushes the queue.
	s.registry.register(a.SessionID, newSessionRuntime(a))
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "delayed notice")
	})
	// The queue is drained by the flush.
	s.registry.parentDeliverMu.Lock()
	queued = len(s.registry.parentDeliveries[a.SessionID])
	s.registry.parentDeliverMu.Unlock()
	if queued != 0 {
		t.Fatalf("queued after flush = %d, want 0", queued)
	}
}

// TestDeliverStartHookArmsOnlyChildMessages pins the delivery-source gate on
// the per-runtime start hook (the continuable-subagent reply capture): a
// plain system notice must NOT fire it, a send_message delivery
// (deliverChildMessage) must fire it exactly once. Without the gate, a
// child's reaction to any machine message (job notice, subagent report,
// completion notice) is injected into its parent as an unsolicited reply.
func TestDeliverStartHookArmsOnlyChildMessages(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	p := llmtest.NewMockProvider()
	p.StreamResults = []*llm.StreamResult{{Content: "ack"}, {Content: "ack2"}}
	a.Provider = p

	// Keep a client attached so the delivered turns cannot orphan-evict the
	// runtime between the two deliveries.
	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	var fired atomic.Int32
	rt.deliverStartHook = func() { fired.Add(1) }

	// A plain system notice starts its turn without arming the capture (the
	// hook fires BEFORE startTurn, so observing the delivered message proves
	// the decision has been made).
	if !rt.deliverToSession("plain notice") {
		t.Fatal("system notice refused")
	}
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "plain notice") })
	if got := fired.Load(); got != 0 {
		t.Fatalf("start hook fired %d times for a system notice, want 0", got)
	}

	// A send_message delivery arms it exactly once.
	if !rt.deliverChildMessage("child message") {
		t.Fatal("child message refused")
	}
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "child message") })
	if got := fired.Load(); got != 1 {
		t.Fatalf("start hook fired %d times for a child message, want 1", got)
	}
	waitForParentDeliveriesSettled(t, s, a)
}
