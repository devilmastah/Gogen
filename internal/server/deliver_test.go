package server

import (
	"context"
	"fmt"
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

// deliveredCount reports how many times needle appears across the agent's
// user messages. Occurrences, not messages: a batched system delivery joins
// several notices into ONE message, so callers counting notices must count
// occurrences.
func deliveredCount(a *agent.Agent, needle string) int {
	n := 0
	for _, m := range a.SnapshotMessages() {
		if m.Role == "user" {
			n += strings.Count(m.Content, needle)
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

// TestDeliverQueueOverflow pins the bounded notice queue and the batched
// drain: on overflow the OLDEST notice is dropped, and the remaining run is
// delivered as ONE joined turn.
func TestDeliverQueueOverflow(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	p := llmtest.NewMockProvider()
	// The FIRST stream call (the "block me" turn started below) blocks
	// until cancelled, so the delivery worker's tryAcquireTurn fails while
	// it runs and the burst below cannot be popped mid-append. Later calls
	// (the delivered turn) answer immediately.
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
	// The burst must land as one atomic append run: the blocking turn holds
	// turnMu, so the worker cannot pop the head mid-append (a worker that
	// delivers notice-1 before the burst completes would leave no overflow
	// to drop). The (cap+1)-th append overflows and drops the OLDEST.
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
	for i := 1; i <= maxSystemDeliveryQueue+1; i++ {
		rt.deliverToSession(fmt.Sprintf("notice-%d", i))
	}
	// End the blocking turn: the worker wakes at turn end and drains the
	// surviving run (2..cap+1 — notice-1 was dropped by the overflow) as
	// ONE joined turn.
	rt.stream.cancelInFlight()
	want := make([]string, 0, maxSystemDeliveryQueue)
	for i := 2; i <= maxSystemDeliveryQueue+1; i++ {
		want = append(want, fmt.Sprintf("notice-%d", i))
	}
	expected := strings.Join(want, "\n\n")
	waitFor(t, 10*time.Second, func() bool {
		return deliveredMessages(a, expected)
	})
	time.Sleep(200 * time.Millisecond)
	// Exactly one notice message must exist, and it is the joined run
	// without notice-1: a per-notice delivery (or a surviving notice-1
	// substring match) would break the equality.
	var got []string
	for _, m := range a.SnapshotMessages() {
		if m.Role == "user" && strings.Contains(m.Content, "notice-") {
			got = append(got, m.Content)
		}
	}
	if len(got) != 1 || got[0] != expected {
		t.Fatalf("notice deliveries = %d messages; want exactly the joined run (dropped oldest): got %#v", len(got), got)
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
	for i := 0; i < maxSystemDeliveryQueue; i++ {
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
	if n != maxSystemDeliveryQueue || last.Text != "overflow notice" {
		t.Fatalf("queue = %d entries, last %q; want cap=%d with the new notice at the tail", n, last.Text, maxSystemDeliveryQueue)
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
	_ = newTestServer(t, a, &config.Config{JobNotices: "on"})
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

// TestDeliverResultCallbackOnlyChildMessages pins the delivery-source gate on
// the per-runtime exact-result callback (the continuable-subagent reply path):
// a plain system notice must not arm it, while a send_message delivery must
// receive that invocation's exact output.
func TestDeliverResultCallbackOnlyChildMessages(t *testing.T) {
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
	var results atomic.Int32
	rt.deliverResultFunc = func(reply string, err error) {
		if reply == "ack2" && err == nil {
			results.Add(1)
		}
	}

	// A plain system notice starts its turn without arming the capture (the
	// hook fires BEFORE startTurn, so observing the delivered message proves
	// the decision has been made).
	if !rt.deliverToSession("plain notice") {
		t.Fatal("system notice refused")
	}
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "plain notice") })
	waitForParentDeliveriesSettled(t, s, a)
	if got := fired.Load(); got != 0 {
		t.Fatalf("start hook fired %d times for a system notice, want 0", got)
	}

	// A send_message delivery arms it exactly once.
	if ok, full := rt.deliverChildMessage("child message"); !ok || full {
		t.Fatalf("child message refused (ok=%v full=%v)", ok, full)
	}
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "child message") })
	waitForParentDeliveriesSettled(t, s, a)
	if got := fired.Load(); got != 1 {
		t.Fatalf("start hook fired %d times for a child message, want 1", got)
	}
	if got := results.Load(); got != 1 {
		t.Fatalf("exact result callback fired %d times with the expected output, want 1", got)
	}
}

// TestDeliverNoticeBatchOneTurn pins the batched drain: a run of contiguous
// system notices delivers as ONE joined user message (one provider call)
// instead of one turn per notice, and a user steering item queued behind the
// run keeps its own turn (FIFO across kinds).
func TestDeliverNoticeBatchOneTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	p := llmtest.NewMockProvider()
	first := true
	var calls atomic.Int32
	entered := make(chan struct{})
	p.OnStream = func(ctx context.Context, _ []llm.Message, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
		if first {
			first = false
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		calls.Add(1)
		return &llm.StreamResult{Content: "ack"}, nil
	}
	a.Provider = p

	rt, _ := s.registry.get(a.SessionID)
	rt.turnMu.Lock()
	rt.startTurn(nil, "block me", nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking turn never entered the provider")
	}
	rt.deliverToSession("batch-a")
	rt.deliverToSession("batch-b")
	rt.deliverToSession("batch-c")
	if !rt.enqueueUserTurn("", "user-steer", nil) {
		t.Fatal("user enqueue refused")
	}
	rt.stream.cancelInFlight()

	// The three notices land as ONE joined user message...
	want := "batch-a\n\nbatch-b\n\nbatch-c"
	waitFor(t, 10*time.Second, func() bool { return deliveredMessages(a, want) })
	// ...and the user item behind them still drains as its own turn.
	waitFor(t, 10*time.Second, func() bool { return deliveredMessages(a, "user-steer") })
	time.Sleep(200 * time.Millisecond)

	if got := calls.Load(); got != 2 {
		t.Fatalf("provider calls after the blocking turn = %d, want 2 (one batched notice turn + one user turn)", got)
	}
	if got := deliveredCount(a, "batch-a"); got != 1 {
		t.Fatalf("batch-a delivered %d times, want 1 (the batch must be one message)", got)
	}
	for _, m := range a.SnapshotMessages() {
		if m.Role == "user" && strings.Contains(m.Content, "batch-") && strings.Contains(m.Content, "user-steer") {
			t.Fatal("a user steering item must not be joined into the notice batch")
		}
	}
}

// TestTargetedDeliveryOverflowPolicy pins the per-source overflow policy: a
// targeted (send_message) delivery evicts the OLDEST NOTICE to make room;
// when every slot holds a targeted delivery it is REFUSED with full=true
// (never silently dropped); and a notice arriving at an all-targeted full
// queue drops itself instead of evicting a pending reply.
func TestTargetedDeliveryOverflowPolicy(t *testing.T) {
	rt := &sessionRuntime{agent: &agent.Agent{}, deliverNotify: make(chan struct{}, 1)}
	rt.deliverWorker.Store(true) // no worker goroutine: the queue stays put

	// Notice slots full: a targeted delivery evicts the oldest notice.
	for i := 0; i < maxSystemDeliveryQueue; i++ {
		rt.deliverToSession(fmt.Sprintf("notice-%d", i))
	}
	if ok, full := rt.deliverChildMessage("targeted-1"); !ok || full {
		t.Fatalf("targeted enqueue refused with notice slots free (ok=%v full=%v)", ok, full)
	}
	rt.deliverMu.Lock()
	nSys, nNotice := 0, 0
	for _, it := range rt.pendingDeliver {
		if it.Kind != deliverSystem {
			continue
		}
		nSys++
		if it.Source == deliverFromSystem {
			nNotice++
		}
	}
	rt.deliverMu.Unlock()
	if nSys != maxSystemDeliveryQueue || nNotice != maxSystemDeliveryQueue-1 {
		t.Fatalf("after targeted enqueue: system=%d notices=%d, want %d/%d (oldest notice evicted)",
			nSys, nNotice, maxSystemDeliveryQueue, maxSystemDeliveryQueue-1)
	}

	// All-targeted full: the next targeted delivery is REFUSED...
	rt2 := &sessionRuntime{agent: &agent.Agent{}, deliverNotify: make(chan struct{}, 1)}
	rt2.deliverWorker.Store(true)
	for i := 0; i < maxSystemDeliveryQueue; i++ {
		if ok, full := rt2.deliverChildMessage(fmt.Sprintf("targeted-%d", i)); !ok || full {
			t.Fatalf("targeted enqueue %d refused before the cap (ok=%v full=%v)", i, ok, full)
		}
	}
	if ok, full := rt2.deliverChildMessage("one-too-many"); ok || !full {
		t.Fatalf("targeted enqueue past a full targeted queue = ok=%v full=%v, want false/true", ok, full)
	}
	// ...while a notice is consumed-and-dropped, keeping the targeted items.
	if !rt2.deliverToSession("notice-at-full") {
		t.Fatal("a notice must be consumed (dropped) rather than refused")
	}
	rt2.deliverMu.Lock()
	n, nTargeted := 0, 0
	for _, it := range rt2.pendingDeliver {
		if it.Kind != deliverSystem {
			continue
		}
		n++
		if it.Source == deliverFromChildMessage {
			nTargeted++
		}
	}
	rt2.deliverMu.Unlock()
	if n != maxSystemDeliveryQueue || nTargeted != maxSystemDeliveryQueue {
		t.Fatalf("queue = %d items (%d targeted), want all %d targeted preserved", n, nTargeted, maxSystemDeliveryQueue)
	}
}
