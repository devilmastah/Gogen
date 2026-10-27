package server

import (
	"context"
	"errors"
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
	"gogen/internal/subagents"
)

// newContinuableServer builds a server with a quick mock for the parent and
// a configurable child provider factory.
func newContinuableServer(t *testing.T, childProvider func() llm.LLMProvider) (*Server, *agent.Agent) {
	t.Helper()
	dir := t.TempDir()
	stub := newBlockingStub()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	// The parent's delivery turns (terminal events, replies) need a
	// provider that answers.
	p := llmtest.NewMockProvider()
	p.StreamResults = []*llm.StreamResult{{Content: "ack"}, {Content: "ack2"}}
	a.Provider = p
	// newTestServer registers the server drain (stop automations, cancel and
	// flush the sessions, evict the runtimes) ahead of the two settles below,
	// so LIFO order runs them first and the drain last, right before the
	// caller's t.TempDir() removal.
	s := newTestServer(t, a, &config.Config{})
	s.ws.ProviderFactory = childProvider
	a.SetSubagentsEnabled(true)
	// A delivered message is appended to the parent transcript BEFORE the
	// delivery turn's final FlushSession lands (the write that persists
	// the reply). Tests return as soon as they observe the message, so
	// that write would race the test's TempDir removal ("directory not
	// empty"); wait for the delivery machinery to go quiet first.
	t.Cleanup(func() { waitForParentDeliveriesSettled(t, s, a) })
	// The SpawnBackground goroutine's post-turn orphan sweep (Store.Delete
	// → index rewrite) runs AFTER the child is unregistered, so a test can
	// return — and its TempDir start being removed — while that write is
	// still in flight (macOS CI: "TempDir RemoveAll cleanup: ... directory
	// not empty"). Wait for every spawn goroutine to exit first.
	t.Cleanup(func() { waitForChildSpawnsSettled(t, s) })
	return s, a
}

// waitForChildSpawnsSettled waits (bounded) until every SpawnBackground
// goroutine has fully exited — including its post-turn orphan sweep, the
// last disk write the lifecycle performs. A plain WaitGroup wait would
// hang the whole package if a test ever leaves a child blocked in a
// provider without cancelling it, so poll with a deadline instead.
func waitForChildSpawnsSettled(t *testing.T, s *Server) {
	t.Helper()
	sp := continuableSpawner(t, s)
	waitFor(t, 5*time.Second, func() bool {
		done := make(chan struct{})
		go func() {
			sp.Wait()
			close(done)
		}()
		select {
		case <-done:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	})
}

func continuableSpawner(t *testing.T, s *Server) *subagentSpawner {
	t.Helper()
	sp, ok := s.ws.SubagentSpawner.(*subagentSpawner)
	if !ok {
		t.Fatal("workspace spawner is not a *subagentSpawner")
	}
	return sp
}

// waitForParentDeliveriesSettled waits until the parent session's delivery
// machinery is quiescent (sessionRuntimeQuiescent): no turn running, nothing
// queued, the delivery worker idle, and the turn lock free. The delivery turn
// appends its message to the transcript before its final flush completes, so
// observing the message is not enough — the disk write must land before the
// test's TempDir is removed, or the cleanup sporadically fails with
// "directory not empty".
func waitForParentDeliveriesSettled(t *testing.T, s *Server, a *agent.Agent) {
	t.Helper()
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		// The runtime was evicted, which only happens after its turn ended
		// (and flushed): nothing can be in flight.
		return
	}
	waitFor(t, 5*time.Second, func() bool { return sessionRuntimeQuiescent(rt) })
}

// sessionRuntimeQuiescent reports whether rt can no longer start work that
// writes to disk: no turn running, no queued delivery, the delivery worker
// idle — AND the turn lock free.
//
// The lock check is the handoff the other three signals miss. deliverLoop
// pops an item while HOLDING turnMu and passes ownership to the turn
// goroutine startTurnWithResult spawns, but it empties the queue and clears
// deliverWorker BEFORE spawning it. A probe that stops at the first three
// signals therefore reads the runtime as quiet during the pop→start window —
// while the worker still owns the lock and the turn has not yet set
// turnActive — and the test's TempDir removal then races the turn's own
// flush, which recreates .gogen/sessions through the atomic writer's
// MkdirAll ("TempDir RemoveAll cleanup: ... directory not empty"). This is
// the same signal production asks for the same question: evictOrphaned
// decides "this runtime is idle" with rt.turnMu.TryLock, which cannot win
// during the handoff.
func sessionRuntimeQuiescent(rt *sessionRuntime) bool {
	if active, _ := rt.turnState(); active {
		return false
	}
	if rt.hasPendingDeliveries() {
		return false
	}
	if rt.deliverWorker.Load() {
		return false
	}
	if !rt.turnMu.TryLock() {
		return false // a turn holds the lock: it runs, or the delivery worker is mid-handoff
	}
	// Release through the production helper, not turnMu.Unlock: it also
	// broadcasts the release, so a delivery worker parked on the release
	// signal re-probes now instead of waiting for the next wake-up.
	rt.releaseTurn()
	return true
}

// TestBackgroundSpawnCompletionNotice drives the full background path: the
// child runs to completion, the parent receives a compact terminal event, and
// the child stays registered (held) until release.
func TestBackgroundSpawnCompletionNotice(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "background report"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour // keep registered for the assertions

	id, err := sp.SpawnBackground(context.Background(), a, "do the thing", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected a child id")
	}
	// The child is registered and held (orphan eviction exempt).
	child := sp.Get(id)
	if child == nil {
		t.Fatal("child not registered")
	}
	if !child.Handle.held.Load() {
		t.Fatal("child must be held")
	}
	// The parent receives a compact, correlated terminal event; the full
	// report is pulled explicitly through get_agent_result.
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "[subagent") && deliveredMessages(a, "get_agent_result")
	})
	result, err := sp.GetAgentResult(a, id)
	if err != nil || !strings.Contains(result, "background report") {
		t.Fatalf("GetAgentResult = %q, %v", result, err)
	}
	if child.StatusOf(sp) != "finished" {
		t.Fatalf("child status = %q, want finished", child.StatusOf(sp))
	}
	// The terminal-event delivery turn on the parent also completed.
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range a.SnapshotMessages() {
			if m.Role == "assistant" && strings.Contains(m.Content, "ack") {
				return true
			}
		}
		return false
	})
}

// TestSendMessageReplyCapture delivers a message to a finished child and
// verifies the child's reply is injected into the parent. The parent pane
// stays attached (a real user watching the parent), so the parent runtime
// is not orphan-evicted between the notice and the reply.
func TestSendMessageReplyCapture(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		// First call: the main job. Second call: the send_message turn.
		p.StreamResults = []*llm.StreamResult{
			{Content: "main job report"},
			{Content: "reply to message"},
		}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	id, err := sp.SpawnBackground(context.Background(), a, "main job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "finished" })

	if err := sp.SendMessage(a, id, "please elaborate"); err != nil {
		t.Fatal(err)
	}
	// The child's reply lands in the parent conversation.
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "reply to message")
	})
}

// TestSendMessageFailedReplyNotifiesParent pins the failure half of the
// one-channel-per-run contract: when the delivered reply turn produces no
// assistant message (provider failure, or an interrupt before any completed
// round), the parent must still receive a notice. The send_message tool
// result already told the parent the message was delivered, so silence
// would leave it waiting for a reply that never comes.
func TestSendMessageFailedReplyNotifiesParent(t *testing.T) {
	// Call 1 = the child's main job (report); call 2 = the send_message
	// reply turn, which fails outright. The counter and OnStream keep the
	// failure deterministic and race-free (a bare field write after the
	// spawn races the turn's locked read).
	var calls atomic.Int32
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.OnStream = func(_ context.Context, _ []llm.Message, h *llm.StreamHandlers) (*llm.StreamResult, error) {
			if calls.Add(1) > 1 {
				return nil, errFailedReply
			}
			if h.OnStart != nil {
				h.OnStart()
			}
			if h.OnToken != nil {
				h.OnToken("main job report")
			}
			return &llm.StreamResult{Content: "main job report"}, nil
		}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	// The parent pane stays attached (a real user watching the parent), so
	// the parent runtime is not orphan-evicted between the notice and the
	// reply — an evicted parent would have the notice queued for a next
	// registration that never comes (the TestSendMessageReplyCapture
	// contract).
	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	id, err := sp.SpawnBackground(context.Background(), a, "main job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "finished" })

	if err := sp.SendMessage(a, id, "please elaborate"); err != nil {
		t.Fatal(err)
	}
	// The parent must learn the reply never came — silence would leave it
	// waiting forever on the "reply is injected when the turn finishes"
	// contract.
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "no reply") })
}

// TestSendMessageQueueFullRefuses pins the targeted refuse-at-submit policy:
// when a busy child's system queue is full of targeted deliveries, a further
// send_message fails with an actionable error instead of silently dropping
// an older queued message whose reply the parent is waiting on.
func TestSendMessageQueueFullRefuses(t *testing.T) {
	// Every provider call is one started turn: the main job (1) and — after
	// the release below — anything the delivery worker manages to start.
	var streamCalls atomic.Int32
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		// The child's main job never finishes.
		return &blockingProvider{onStream: func() { streamCalls.Add(1) }}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	id, err := sp.SpawnBackground(context.Background(), a, "main job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	// Wait until the main job actually holds the turn lock, so the
	// deliveries queue instead of draining between sends.
	waitFor(t, 5*time.Second, func() bool {
		active, _ := child.Handle.turnState()
		return active
	})
	for i := 0; i < maxSystemDeliveryQueue; i++ {
		if err := sp.SendMessage(a, id, fmt.Sprintf("queued-%d", i)); err != nil {
			t.Fatalf("send %d refused before the cap: %v", i, err)
		}
	}
	err = sp.SendMessage(a, id, "one too many")
	if err == nil || !strings.Contains(err.Error(), "delivery queue is full") {
		t.Fatalf("send past a full targeted queue = %v, want a delivery-queue-full error", err)
	}
	// Tear the blocked child down: release evicts its runtime and drops the
	// queued deliveries, so the spawn goroutine can exit (the provider
	// unwinds on cancel) instead of leaking into the package teardown.
	sp.ReleaseChild(child)
	// ... and the drop must happen in the eviction's critical section, not by
	// racing it: the delivery worker is parked on the turn lock the main job
	// is about to release, and a worker that wins that handoff starts a
	// delivered turn whose own start-flush lands in .gogen/sessions after the
	// runtime left the registry ("TempDir RemoveAll cleanup: ...
	// directory not empty" in CI). Release claims the runtime before its
	// outcome flush and drains the turn lock before evicting, so no further
	// turn may start on the released child.
	requireNever(t, 200*time.Millisecond,
		"a queued send_message delivery started a turn on the released child",
		func() bool { return streamCalls.Load() > 1 })
}

var errFailedReply = errors.New("provider exploded")

// TestSessionRuntimeQuiescentCoversDeliveryHandoff pins the quiescence
// probe the TempDir cleanups rely on. Simulating the delivery worker's
// pop→start handoff (item popped, queue empty, deliverWorker cleared, turn
// goroutine not yet running — so turnActive is still false) the runtime must
// NOT read quiescent: the worker still owns the turn lock and the turn's
// flush is about to write. A probe that stopped at the queue/worker/turn
// signals let that write race the test's TempDir removal.
func TestSessionRuntimeQuiescentCoversDeliveryHandoff(t *testing.T) {
	rt := &sessionRuntime{}

	// Quiet runtime: nothing running, nothing queued, no worker.
	if !sessionRuntimeQuiescent(rt) {
		t.Fatal("an untouched runtime must read quiescent")
	}

	// A queued delivery alone is enough to hold the answer at "no".
	seedDeliveries(rt, deliverItem{ID: newQueueItemID(), Text: "notice", Kind: deliverSystem, Source: deliverFromSystem})
	if sessionRuntimeQuiescent(rt) {
		t.Fatal("a runtime with a queued delivery must not read quiescent")
	}

	// The handoff window: the worker popped the item (queue empty) and
	// cleared deliverWorker, but still holds turnMu while the turn
	// goroutine it just spawned has not started yet.
	seedDeliveries(rt)
	rt.deliverWorker.Store(true)
	rt.turnMu.Lock()
	rt.deliverWorker.Store(false)
	if sessionRuntimeQuiescent(rt) {
		rt.turnMu.Unlock()
		t.Fatal("the delivery worker's pop→start handoff must not read quiescent")
	}

	// A running turn: turnActive with the lock free is still busy.
	rt.turnMu.Unlock()
	rt.setTurnActive(true, time.Now(), nil)
	if sessionRuntimeQuiescent(rt) {
		rt.setTurnActive(false, time.Time{}, nil)
		t.Fatal("a runtime with a running turn must not read quiescent")
	}
	rt.setTurnActive(false, time.Time{}, nil)

	// Lock handed back: quiescent again (and the probe's probe-and-release
	// must not leave the lock held).
	if !sessionRuntimeQuiescent(rt) {
		t.Fatal("an idle runtime must read quiescent again once the handoff ends")
	}
	if !rt.turnMu.TryLock() {
		t.Fatal("the quiescence probe left the turn lock held")
	}
	rt.releaseTurn()
}

// TestDrainTurnLockWaitsOutHandoff pins the eviction-side drain
// (sessionRuntime.drainTurnLock) that Release and the test teardown use: a
// delivery worker can win the turn lock in the pop→start handoff before the
// evicted flag lands, and the turn it starts persists its user message before
// the provider call. Evicting without waiting let that flush land after the
// runtime left the registry — a session file re-created post-teardown, which
// the suite sees as "TempDir RemoveAll cleanup: ... directory not empty".
func TestDrainTurnLockWaitsOutHandoff(t *testing.T) {
	rt := &sessionRuntime{stream: &wsConnStream{}}
	rt.evicted.Store(true) // callers latch first, so no new turn can start

	// The handoff: the delivery worker owns the turn lock while the turn
	// goroutine it just spawned is starting.
	rt.turnMu.Lock()
	drained := make(chan bool, 1)
	go func() { drained <- rt.drainTurnLock(5 * time.Second) }()
	select {
	case ok := <-drained:
		t.Fatalf("drain reported %v while a turn still held the lock", ok)
	case <-time.After(50 * time.Millisecond):
	}

	// The blocked turn reaches its end and releases: the drain must report the
	// runtime quiet, not give up.
	rt.releaseTurn()
	select {
	case ok := <-drained:
		if !ok {
			t.Fatal("drain gave up instead of waiting out the handoff")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after the turn released the lock")
	}

	// It must hand the lock back: the worker's own drop path needs it to see
	// the evicted flag and discard the queue.
	if !rt.turnMu.TryLock() {
		t.Fatal("drain left the turn lock held")
	}
	rt.releaseTurn()
}

// TestDrainTurnLockGivesUpOnStuckTurn pins the bound: a turn that ignores
// cancellation (a stuck tool) must cost the window once and let the eviction
// proceed — the sticky evicted flag still refuses new turns.
func TestDrainTurnLockGivesUpOnStuckTurn(t *testing.T) {
	rt := &sessionRuntime{stream: &wsConnStream{}}
	rt.evicted.Store(true)
	rt.turnMu.Lock()
	defer rt.releaseTurn()

	start := time.Now()
	if rt.drainTurnLock(100 * time.Millisecond) {
		t.Fatal("drain reported a held turn lock as quiet")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("drain took %v, want it bounded by the ~100ms budget", elapsed)
	}
}

// TestBackgroundChildBusy pins the release-safety predicate: a quiet child
// is not busy; a send_message delivery queued for the delivery worker (not
// yet past the turn acquire — the turn state reads idle) is busy; a
// captured reply not yet delivered to the parent is busy. The last two are
// the handoff windows where maybeReleaseChild used to read the child as
// idle and release it mid-delivery.
func TestBackgroundChildBusy(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{
			{Content: "main job report"}, // the main job
			{Content: "reply"},           // the queued delivery's turn
		}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	id, err := sp.SpawnBackground(context.Background(), a, "main job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "finished" })
	if child.Busy(sp) {
		t.Fatal("a quiet finished child must not be busy")
	}

	// Queued delivery: hold the child's turn lock so the delivery worker
	// cannot get past the acquire step — the message stays queued and no
	// turn has started.
	child.Handle.turnMu.Lock()
	unlocked := false
	unlockOnce := func() {
		if !unlocked {
			unlocked = true
			// Release through the production helper so the parked delivery
			// worker wakes on the release broadcast (the test simulates a
			// held turn lock).
			child.Handle.releaseTurn()
		}
	}
	t.Cleanup(unlockOnce)
	if err := sp.SendMessage(a, id, "queued question"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return child.Handle.hasPendingDeliveries() })
	if !child.Busy(sp) {
		t.Fatal("a send_message delivery queued for the worker must read busy")
	}

	// Captured result: the delivery turn acquired the lock and the exact
	// callback is armed, but setTurnActive(true) has not run yet — the
	// acquire→start handoff. Simulated by arming the flag directly.
	child.SetPendingReply(true)
	if !child.Busy(sp) {
		t.Fatal("an armed exact-result callback must read busy")
	}
	child.SetPendingReply(false)

	// Neutralize the queued delivery before returning: the t.Cleanup unlock
	// (LIFO — it runs before TempDir RemoveAll) would otherwise let the
	// delivery worker acquire the lock and START the queued turn, whose
	// session writes race the temp-dir cleanup (flake: "directory not
	// empty"). Emptying the queue makes the worker's post-acquire re-check
	// drop the item and exit without a turn.
	seedDeliveries(child.Handle)
}

// TestRetentionReleasesAfterQueuedDelivery pins the retention/queue
// interaction end to end: a send_message delivery queued for the child's
// delivery worker (not yet past the turn acquire, so the turn state reads
// idle) must DEFER the retention release. The pre-fix maybeReleaseChild
// checked only the turn state, read the child as idle mid-window, released
// it, and the queued message was silently dropped — the parent got no reply
// and no error. After the delivery completes and the child goes quiet, the
// next timer fire releases it as designed.
func TestRetentionReleasesAfterQueuedDelivery(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{
			{Content: "main job report"}, // the main job
			{Content: "queued reply"},    // the queued delivery's turn
		}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = 500 * time.Millisecond

	// The parent pane stays attached (a real user watching the parent), so
	// the parent runtime is not orphan-evicted between the notice and the
	// reply — the reply must land in the parent transcript for the
	// assertion.
	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	id, err := sp.SpawnBackground(context.Background(), a, "main job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "finished" })

	// Hold the child's turn lock so the delivery worker cannot get past
	// the acquire step: the message stays queued and the turn state reads
	// idle — exactly the state the timer must not misread.
	child.Handle.turnMu.Lock()
	unlocked := false
	unlockOnce := func() {
		if !unlocked {
			unlocked = true
			// Release through the production helper so the parked delivery
			// worker wakes on the release broadcast (the test simulates a
			// held turn lock).
			child.Handle.releaseTurn()
		}
	}
	t.Cleanup(unlockOnce)
	if err := sp.SendMessage(a, id, "queued question"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return child.Handle.hasPendingDeliveries() })

	// Outlive the retention window: the timer fires (and re-fires) while
	// the message is still queued and must re-arm instead of releasing.
	time.Sleep(3 * sp.retainWindow())
	if sp.Get(id) == nil {
		t.Fatal("a queued send_message delivery must defer the retention release")
	}

	// Deliver: the worker acquires the turn, the reply runs, and the
	// parent receives it.
	unlockOnce()
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "queued reply")
	})

	// Back to quiet: the next timer fire releases the child as designed.
	waitFor(t, 5*time.Second, func() bool { return sp.Get(id) == nil })
}

// TestInterruptAgent cancels the current turn only: the child stays
// registered and continuable, and no terminal event is delivered.
func TestInterruptAgent(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{} // blocks until cancelled
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	id, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	// Deliberately interrupt immediately: runCancel is installed before the
	// child is published, so cancellation must not depend on the provider
	// having registered its stream handle yet.
	if err := sp.InterruptAgent(a, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "idle" })
	if deliveredMessages(a, "[subagent") {
		t.Fatal("an interrupted child must not deliver a terminal event")
	}
	waited, err := sp.WaitAgent(t.Context(), a, id, time.Second)
	if err != nil || !strings.Contains(waited, "interrupted before completion") {
		t.Fatalf("WaitAgent after immediate interrupt = %q, %v", waited, err)
	}
	result, err := sp.GetAgentResult(a, id)
	if err != nil || !strings.Contains(result, "interrupted before completion") {
		t.Fatalf("GetAgentResult after interrupt = %q, %v", result, err)
	}
	// Still live: messaging it works.
	if err := sp.SendMessage(a, id, "wake up"); err != nil {
		t.Fatalf("child should remain continuable after interrupt: %v", err)
	}
	// Authorization: a foreign caller cannot retrieve/message/interrupt the
	// child.
	if _, err := sp.GetAgentResult(&agent.Agent{SessionID: "someone-else"}, id); err == nil {
		t.Fatal("foreign caller must not retrieve the child")
	}
	if err := sp.SendMessage(&agent.Agent{SessionID: "someone-else"}, id, "hi"); err == nil {
		t.Fatal("foreign caller must not message the child")
	}
	// Cleanup: the wake-up reply turn blocks on the provider — cancel and
	// release the child so no goroutine outlives the test's temp dir.
	sp.CancelAll(a.SessionID)
	waitFor(t, 5*time.Second, func() bool { return sp.Get(id) == nil })
}

// TestSubagentForkCopiesMessages pins the fork semantics: the child starts
// with a copy of the parent's history and the parent transcript is
// untouched.
func TestSubagentForkCopiesMessages(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "fork reply"}}
		return p
	})
	sp := continuableSpawner(t, s)

	a.Messages = []llm.Message{
		{Role: "user", Content: "original question"},
		{Role: "assistant", Content: "original answer"},
	}
	before := a.MessageCount()

	report, err := sp.Fork(context.Background(), a, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "fork reply") {
		t.Fatalf("fork report = %q", report)
	}
	if a.MessageCount() != before {
		t.Fatalf("parent message count changed: %d → %d", before, a.MessageCount())
	}
	// The fork's session carries the copied history (2) + job (1) + reply (1).
	forkID := sp.Get("") // forks are foreground: no child record
	_ = forkID
	// Find the fork via the saved sessions: it persisted with ParentID.
	list, err := s.ws.Store.List(a.WorkingDir)
	if err != nil {
		t.Fatal(err)
	}
	var fork *agent.SessionInfo
	for i := range list {
		if list[i].ParentID == a.SessionID {
			fork = &list[i]
			break
		}
	}
	if fork == nil {
		t.Fatal("fork session not persisted under the parent")
	}
}

// TestForkPersistsFailedOutcome verifies a cancelled fork child is saved
// with the failed outcome, so the sidebar renders the true result after a
// reload/restart (same contract as Spawn/SpawnBackground).
func TestForkPersistsFailedOutcome(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{}
	}
	sp := continuableSpawner(t, s)
	// Fork requires a non-empty transcript (with an assistant message to
	// fork from) to copy.
	a.Messages = []llm.Message{
		{Role: "user", Content: "original question"},
		{Role: "assistant", Content: "original answer"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sp.Fork(ctx, a, "", 0)
		done <- err
	}()
	// Wait for the fork child to register, then cancel mid-turn.
	var forkID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID {
				forkID = id
				return true
			}
		}
		return false
	})
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not return after cancel")
	}
	// The fork's session is saved with the failed outcome.
	info := store.Info(dir, forkID)
	if info == nil {
		t.Fatal("fork session not persisted")
	}
	if info.SubagentStatus != "failed" {
		t.Fatalf("fork outcome = %q, want failed", info.SubagentStatus)
	}
}

// TestChildRetentionRelease pins the retention window: a finished child is
// released (runtime evicted, child record dropped) after the window; the
// saved session stays.
func TestChildRetentionRelease(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = 150 * time.Millisecond

	id, err := sp.SpawnBackground(context.Background(), a, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "finished" })
	waitFor(t, 5*time.Second, func() bool {
		if _, ok := s.registry.get(id); ok {
			return false
		}
		if sp.Get(id) != nil {
			return false
		}
		return !child.Handle.held.Load()
	})
	// Retention removes only the live runtime/record. The persisted terminal
	// result remains available to the original parent.
	result, err := sp.GetAgentResult(a, id)
	if err != nil || !strings.Contains(result, "done") {
		t.Fatalf("GetAgentResult after retention = %q, %v", result, err)
	}
	if _, err := sp.GetAgentResult(&agent.Agent{SessionID: "someone-else"}, id); err == nil {
		t.Fatal("a foreign parent must not retrieve a released child's result")
	}
}

// TestParentEvictionCancelsChildren pins the parent-close policy: evicting
// the parent runtime cancels and releases its background children.
func TestParentEvictionCancelsChildren(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{} // keep the child running
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	id, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "running" })

	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("parent runtime missing")
	}
	// session_close is the explicit teardown path the policy covers.
	s.registry.closeRuntime(parentRt)

	waitFor(t, 5*time.Second, func() bool {
		if _, ok := s.registry.get(id); ok {
			return false
		}
		return sp.Get(id) == nil
	})
}

// TestChildProgressNeverPushesToParent pins the return contract: a child
// cannot push interim messages into the parent conversation. The parent gets
// one compact terminal event and pulls the bounded report through
// get_agent_result; progress remains available via list_agents. The child here ATTEMPTS a
// report tool call mid-turn: the tool no longer exists for children, so the
// attempt is rejected as an unknown tool inside the child, and nothing
// reaches the parent.
func TestChildProgressNeverPushesToParent(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{
			{Content: "checking the parser now", ToolCalls: []llm.ToolCall{{
				ID: "t1", Name: "report", Args: map[string]any{"message": "should not be pushed"},
			}}},
			{Content: "final outcome text"},
		}
		return p
	})
	s.ws.SetSubagentEnabled(true)
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	id, err := sp.SpawnBackground(context.Background(), a, "check the parser", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.IsFinished() })
	waitForParentDeliveriesSettled(t, s, a)
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "get_agent_result")
	})
	// The report tool is gone for children: the attempt is an unknown tool
	// (visible in the child's own transcript), and the parent receives one
	// compact terminal event for the run.
	rejected := false
	for _, m := range child.Handle.agent.SnapshotMessages() {
		if m.Role == "tool" && strings.Contains(m.Content, "unknown tool: report") {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("a child's report attempt must be rejected as an unknown tool")
	}
	if deliveredMessages(a, "should not be pushed") {
		t.Fatal("a child must not be able to push interim messages into the parent conversation")
	}
	if n := deliveredCount(a, "[subagent"); n != 1 {
		t.Fatalf("parent received %d child terminal events, want exactly 1", n)
	}
	result, err := sp.GetAgentResult(a, id)
	if err != nil || !strings.Contains(result, "final outcome text") {
		t.Fatalf("get_agent_result = %q, %v", result, err)
	}
	out, err := sp.ListAgents(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "final outcome text") {
		t.Fatalf("list_agents must expose the child's latest progress: %q", out)
	}
}

// TestChildCapReleasesOldest pins the per-parent finished cap: spawning
// beyond the cap releases the oldest finished children.
func TestChildCapReleasesOldest(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	sp.maxFinished = 2

	for i := 0; i < 4; i++ {
		if _, err := sp.SpawnBackground(context.Background(), a, "job", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	// Children finish CONCURRENTLY (spawn order is not finish order), so
	// the per-id release order is not deterministic: the deterministic
	// contract is that the cap leaves exactly maxFinished registered
	// children and every survivor is finished. (Which two are released
	// depends on finish timing — the implementation releases the oldest
	// FINISHED children.)
	waitFor(t, 5*time.Second, func() bool {
		return len(sp.ChildrenOf(a.SessionID)) == sp.maxFinished
	})
	for _, c := range sp.ChildrenOf(a.SessionID) {
		if !c.IsFinished() {
			t.Fatalf("surviving child %s is not finished", c.ID)
		}
	}
	// Settle: each child's compact terminal event is enqueued/delivered only
	// after its final flush lands. Four completions may legitimately be
	// coalesced into fewer parent delivery turns, so verify the durable child
	// sessions/outcomes directly; the terminal-event text is asserted in the
	// dedicated completion tests.
	waitForParentDeliveriesSettled(t, s, a)
	entries, err := s.ws.Store.List(s.ws.GetWorkingDir())
	if err != nil {
		t.Fatal(err)
	}
	finishedStored := 0
	for _, entry := range entries {
		if entry.ParentID == a.SessionID && entry.SubagentStatus == "success" {
			finishedStored++
		}
	}
	if finishedStored != 4 {
		t.Fatalf("persisted successful children = %d, want 4", finishedStored)
	}
}

// TestLiveChildCapRefuses pins the live-child bound: spawning beyond
// maxLive is refused with an error (never silently released mid-task) and
// the refused child's runtime is released while earlier children stay
// registered.
func TestLiveChildCapRefuses(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{} // keeps the first child running
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	sp.maxLive = 1

	id1, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return sp.Get(id1) != nil })

	// A second live child would exceed the cap: refused.
	if _, err := sp.SpawnBackground(context.Background(), a, "second job", "", 0); err == nil {
		t.Fatal("spawning beyond the live cap must be refused")
	}
	// The refused child was discarded (never announced, nothing
	// persisted); the first stays registered.
	if sp.Get(id1) == nil {
		t.Fatal("the first child must stay registered")
	}
	// Cleanup: cancel the running child so its turn goroutine exits.
	sp.CancelAll(a.SessionID)
	waitFor(t, 5*time.Second, func() bool { return sp.Get(id1) == nil })
}

// TestRefusedSpawnLeavesNoSessionFile pins the cap-refusal hygiene: a
// background spawn refused by either cap must leave NO artifact. The child
// never started, so stamping its outcome "failed" and forcing a flush (the
// pre-fix releaseChild path) wrote an empty session file + index entry per
// refused spawn — a phantom failed child row in the sidebar for a session
// that never existed, stacked one per refusal. The refusal error (the tool
// result the parent model sees) is the only trace a refused spawn may
// leave.
func TestRefusedSpawnLeavesNoSessionFile(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{} // keeps the accepted child running
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(1)

	// A live client observes the announcements: the admitted child is
	// announced exactly once; the refused children are never announced (an
	// announced-but-refused child would leave a "running" sidebar row no
	// event ever resolves).
	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// drainStarted reads frames for d and fails if a subagent_started
	// arrives — a refused child must never be announced.
	drainStarted := func(d time.Duration) {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			_ = conn.SetReadDeadline(deadline)
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return // quiet window elapsed: nothing announced
			}
			if msg.Type == "subagent_started" {
				t.Fatalf("refused spawn must not be announced, got subagent_started for %s", msg.SubagentID)
			}
		}
	}

	// failedChildIDs lists the persisted children of the parent stamped
	// failed — the phantom rows at stake.
	failedChildIDs := func() []string {
		t.Helper()
		list, err := s.ws.Store.List(s.ws.GetWorkingDir())
		if err != nil {
			t.Fatalf("store list: %v", err)
		}
		var out []string
		for _, e := range list {
			if e.ParentID == a.SessionID && e.SubagentStatus == "failed" {
				out = append(out, e.ID)
			}
		}
		return out
	}

	id1, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	started := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "subagent_started" })
	if started.SubagentID != id1 {
		t.Fatalf("announced child = %q, want the admitted %q", started.SubagentID, id1)
	}
	waitFor(t, 5*time.Second, func() bool { return sp.Get(id1) != nil })

	// Refused by the user-facing concurrent limit.
	if id, err := sp.SpawnBackground(context.Background(), a, "refused job", "", 0); err == nil ||
		id != "" || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("spawn beyond the cap must be refused with a limit error and no id, got id %q err %v", id, err)
	}
	drainStarted(400 * time.Millisecond)
	if got := failedChildIDs(); len(got) != 0 {
		t.Fatalf("refused spawn must not persist a failed child session, got %v", got)
	}

	// Refused by the internal live guard (raise the user-facing limit so
	// the guard is the only cap in play).
	s.ws.SetSubagentMaxConcurrent(4)
	sp.maxLiveGuard = 1
	if id, err := sp.SpawnBackground(context.Background(), a, "guard-refused job", "", 0); err == nil ||
		id != "" || !strings.Contains(err.Error(), "too many live subagents") {
		t.Fatalf("spawn at the live guard must be refused with a guard error and no id, got id %q err %v", id, err)
	}
	drainStarted(400 * time.Millisecond)
	if got := failedChildIDs(); len(got) != 0 {
		t.Fatalf("guard-refused spawn must not persist a failed child session, got %v", got)
	}

	// The accepted child is untouched by both refusals.
	if sp.Get(id1) == nil {
		t.Fatal("the accepted child must stay registered")
	}

	// Cleanup: cancel the running child so its turn goroutine exits.
	sp.CancelAll(a.SessionID)
	waitFor(t, 5*time.Second, func() bool { return sp.Get(id1) == nil })
}

// TestConcurrentLimitFromConfig pins the user-facing concurrent-subagent
// limit (subagent_max_concurrent): with the workspace value set, background
// spawning beyond it is refused (the refused child's runtime is released),
// and foreground Spawn / Fork are refused while the cap is full.
func TestConcurrentLimitFromConfig(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &gateProvider{release: gate}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(2)

	id1, err := sp.SpawnBackground(context.Background(), a, "job one", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := sp.SpawnBackground(context.Background(), a, "job two", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return sp.Get(id1) != nil && sp.Get(id2) != nil
	})

	// A third background child exceeds the limit: refused and released.
	if _, err := sp.SpawnBackground(context.Background(), a, "job three", "", 0); err == nil ||
		!strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("background spawn beyond the concurrent limit must be refused with a limit error, got %v", err)
	}
	// Foreground spawn and fork are refused while the cap is full.
	if _, err := sp.Spawn(context.Background(), a, "foreground job", "", 0); err == nil ||
		!strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("foreground spawn at a full cap must be refused with a limit error, got %v", err)
	}
	if _, err := sp.Fork(context.Background(), a, "", 0); err == nil ||
		!strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("fork at a full cap must be refused with a limit error, got %v", err)
	}
	// The two accepted children stay registered.
	if sp.Get(id1) == nil || sp.Get(id2) == nil {
		t.Fatal("accepted children must stay registered")
	}
	// Cleanup: cancel the running children so their turn goroutines exit.
	sp.CancelAll(a.SessionID)
}

// TestConcurrentLimitCountsOnlyLiveChildren pins that FINISHED children do
// not count toward the concurrent limit: with a limit of 1, a second spawn
// is allowed once the first child has finished.
func TestConcurrentLimitCountsOnlyLiveChildren(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(1)
	// Held: the parent has no attached clients, so the orphan re-check after
	// the completion-notice turn would evict it while we wait for the first
	// child to finish.
	if parentRt, ok := s.registry.get(a.SessionID); ok {
		parentRt.held.Store(true)
	}

	id1, err := sp.SpawnBackground(context.Background(), a, "first job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		c := sp.Get(id1)
		return c != nil && c.IsFinished()
	})
	// The finished child no longer counts: the second spawn is allowed.
	id2, err := sp.SpawnBackground(context.Background(), a, "second job", "", 0)
	if err != nil {
		t.Fatalf("spawn after the first child finished must be allowed: %v", err)
	}
	if id2 == id1 {
		t.Fatal("expected a fresh child id")
	}
	// Both completion pipelines must be absorbed before the test returns.
	// The notice delivery happens AFTER the child's outcome FlushSession,
	// so observing BOTH notices in the parent transcript proves both child
	// sessions are persisted; the waitForParentDeliveriesSettled cleanup
	// then bounds the parent ack turns' final flushes. Without this, a
	// child goroutine starved until after cleanup (Windows CI) writes its
	// session file while t.TempDir() is being removed ("TempDir RemoveAll
	// cleanup: ... The directory is not empty").
	waitFor(t, 5*time.Second, func() bool {
		return deliveredCount(a, "[subagent") >= 2
	})
}

// TestInterruptFreesConcurrentSlot pins that the concurrent limit counts
// ACTIVE children: interrupting a child frees its slot immediately (the
// refusal error promises interrupt_agent makes room) while the interrupted
// child stays registered and continuable until its retention window.
func TestInterruptFreesConcurrentSlot(t *testing.T) {
	n := 0
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		n++
		if n == 2 {
			p := llmtest.NewMockProvider()
			p.StreamResults = []*llm.StreamResult{{Content: "done"}}
			return p
		}
		return &blockingProvider{}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(1)
	// Held: the parent has no attached clients, so the orphan re-check
	// after id2's completion-notice turn would evict it before the
	// foreground spawn below.
	if parentRt, ok := s.registry.get(a.SessionID); ok {
		parentRt.held.Store(true)
	}

	id1, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id1)
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "running" })
	// Wait until the turn is actually in flight: cancelInFlight is a
	// no-op before the turn registers its cancel handle, so an interrupt
	// fired at mere "running" status (set at registration) could be lost.
	waitFor(t, 5*time.Second, func() bool {
		active, _ := child.Handle.turnState()
		return active
	})

	if err := sp.InterruptAgent(a, id1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return child.StatusOf(sp) == "idle" })

	// The slot is free: a new background spawn is allowed while the
	// interrupted child is still registered.
	id2, err := sp.SpawnBackground(context.Background(), a, "second job", "", 0)
	if err != nil {
		t.Fatalf("spawn after interrupt must be allowed (an idle child holds no slot): %v", err)
	}
	if sp.Get(id2) == nil {
		t.Fatal("the second child must stay registered")
	}
	// Let it finish so the foreground admission below is deterministic
	// (with the cap at 1 a still-running id2 would refuse it).
	waitFor(t, 5*time.Second, func() bool {
		c := sp.Get(id2)
		return c != nil && c.StatusOf(sp) == "finished"
	})

	// A foreground spawn is admitted at the free slot (its slot is
	// released when the turn is cancelled).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sp.Spawn(ctx, a, "foreground job", "", 0)
		done <- err
	}()
	var fgID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID && id != id1 && id != id2 {
				fgID = id
				return true
			}
		}
		return false
	})
	if fgID == "" {
		t.Fatal("foreground spawn at a free slot must be admitted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("foreground spawn did not return after cancel")
	}
	sp.CancelAll(a.SessionID)
	waitFor(t, 5*time.Second, func() bool {
		return sp.Get(id1) == nil && sp.Get(id2) == nil
	})
}

// TestForegroundCountsTowardConcurrentLimit pins that an in-flight
// foreground child holds a slot of the concurrent limit: with the limit at
// 2 and one background + one foreground child running, a second background
// spawn is refused — and allowed again once the foreground child is gone.
func TestForegroundCountsTowardConcurrentLimit(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(2)

	id1, err := sp.SpawnBackground(context.Background(), a, "background job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		c := sp.Get(id1)
		return c != nil && c.StatusOf(sp) == "running"
	})

	// A foreground child in flight (blocking provider, cancelled below).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sp.Spawn(ctx, a, "foreground job", "", 0)
		done <- err
	}()
	var fgID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID && id != id1 {
				fgID = id
				return true
			}
		}
		return false
	})
	if fgID == "" {
		t.Fatal("foreground child not registered")
	}

	// Active = 2 (background + foreground): the cap is full.
	if _, err := sp.SpawnBackground(context.Background(), a, "second background", "", 0); err == nil ||
		!strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("background spawn with an in-flight foreground child at the cap must be refused, got %v", err)
	}

	// Cancel the foreground child: its slot is released when Spawn returns.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("foreground spawn did not return after cancel")
	}
	// Active = 1 again: the background spawn is allowed.
	id3, err := sp.SpawnBackground(context.Background(), a, "second background", "", 0)
	if err != nil {
		t.Fatalf("background spawn after the foreground child finished must be allowed: %v", err)
	}
	if sp.Get(id3) == nil {
		t.Fatal("the second background child must stay registered")
	}
	sp.CancelAll(a.SessionID)
}

// TestForegroundSlotReleasedOnFailure pins the deferred foreground release:
// a foreground spawn that fails BEFORE its turn starts (model selection)
// must free its slot, or the parent would lose a slot of the concurrent
// limit for nothing.
func TestForegroundSlotReleasedOnFailure(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(1)

	// Fails in newChildRuntime (unknown model), after the slot was taken.
	if _, err := sp.Spawn(context.Background(), a, "job", "no/such-model", 0); err == nil ||
		!strings.Contains(err.Error(), "subagent model") {
		t.Fatalf("foreground spawn with an unknown model must fail with a model error, got %v", err)
	}
	// The slot is free: a background spawn at the cap is allowed.
	id, err := sp.SpawnBackground(context.Background(), a, "job", "", 0)
	if err != nil {
		t.Fatalf("background spawn after a failed foreground spawn must be allowed: %v", err)
	}
	if sp.Get(id) == nil {
		t.Fatal("the background child must stay registered")
	}
	sp.CancelAll(a.SessionID)
}

// TestLiveGuardBoundsIdleChildren pins the internal non-finished guard:
// interrupted (idle) children hold no slot of the user-facing concurrent
// limit, but they still count against the memory guard, so a
// spawn+interrupt loop cannot accumulate unbounded runtimes.
func TestLiveGuardBoundsIdleChildren(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(4)
	sp.maxLiveGuard = 2

	var ids []string
	for _, job := range []string{"job one", "job two"} {
		id, err := sp.SpawnBackground(context.Background(), a, job, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		c := sp.Get(id)
		waitFor(t, 5*time.Second, func() bool { return c.StatusOf(sp) == "running" })
		// The turn must be in flight before the interrupt: cancelInFlight
		// is a no-op before the turn registers its cancel handle.
		waitFor(t, 5*time.Second, func() bool {
			active, _ := c.Handle.turnState()
			return active
		})
		if err := sp.InterruptAgent(a, id); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 5*time.Second, func() bool { return c.StatusOf(sp) == "idle" })
	}
	// Active = 0 (well under the limit of 4), but non-finished = 2 = the
	// guard: the next spawn is refused by the guard, not the user limit.
	if _, err := sp.SpawnBackground(context.Background(), a, "third job", "", 0); err == nil ||
		!strings.Contains(err.Error(), "too many live subagents") {
		t.Fatalf("spawn at the live guard must be refused by the guard, got %v", err)
	}
	sp.CancelAll(a.SessionID)
}

// TestOnTurnResultUsesExactOutput pins that reply delivery consumes the
// current turn's returned output, never a transcript heuristic: an older
// assistant message cannot be re-delivered after an interrupted turn.
func TestOnTurnResultUsesExactOutput(t *testing.T) {
	replyEvent := subagents.Event{
		Kind:     subagents.EventReply,
		ChildID:  "child-1",
		ParentID: "parent-1",
		Label:    "subagent: test",
		Text:     strings.Repeat("r", agent.MaxSubagentReportBytes+1000),
	}
	if got := replyEvent.ParentMessage(); len(got) > agent.MaxSubagentReportBytes+len("… (truncated)")+len(replyEvent.Label)+32 {
		t.Fatalf("typed reply event escaped the report cap: %d bytes", len(got))
	}
	if got := (subagents.Event{
		Kind:     subagents.EventReply,
		ChildID:  "child-1",
		ParentID: "parent-1",
		Label:    "subagent: test",
		Err:      context.Canceled,
	}).ParentMessage(); !strings.Contains(got, "context canceled") {
		t.Fatalf("typed no-reply event = %q", got)
	}

	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "ack"}}
		return p
	})
	sp := continuableSpawner(t, s)
	// Keep the parent registered so the synthetic child's delivery can run.
	if rt, ok := s.registry.get(a.SessionID); ok {
		rt.held.Store(true)
	}
	t.Cleanup(func() {
		if rt, ok := s.registry.get(a.SessionID); ok {
			rt.held.Store(false)
		}
	})
	id, err := sp.SpawnBackground(context.Background(), a, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	c := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return c.IsFinished() })

	c.SetPendingReply(true)
	c.OnTurnResult(sp, "new reply", nil)
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "new reply") })
	waitForParentDeliveriesSettled(t, s, a)

	c.SetPendingReply(true)
	c.OnTurnResult(sp, "", context.Canceled)
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "no reply") })
	if deliveredMessages(a, "ack") {
		t.Fatal("an interrupted reply turn must not re-deliver an older assistant message")
	}
	waitForParentDeliveriesSettled(t, s, a)
}

// gateProvider streams one result only after release is closed, so a test
// can deterministically finish a child turn AFTER a parent eviction.
type gateProvider struct {
	release chan struct{}
}

func (p *gateProvider) Name() string { return "gate" }
func (p *gateProvider) ModelName() string {
	return "gate-model"
}
func (p *gateProvider) SetModel(string) error { return nil }
func (p *gateProvider) SetThinkingLevel(string) {
}
func (p *gateProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (p *gateProvider) GenerateResponse(context.Context, []llm.Message, map[string]struct{}, []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}
func (p *gateProvider) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	if h != nil && h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if h != nil && h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
	return &llm.StreamResult{Content: "gated report"}, nil
}
func (p *gateProvider) ModelContextLimit(context.Context) (int, error) { return 1000, nil }

// TestBackgroundSpawnCompletionQueuedUntilParentLive pins the deferred
// terminal-event delivery: a parent that is orphan-evicted (idle, no viewers)
// while its background child runs receives the compact event when the session
// is next registered; the event is queued, not dropped.
func TestBackgroundSpawnCompletionQueuedUntilParentLive(t *testing.T) {
	gate := make(chan struct{})
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &gateProvider{release: gate}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour // keep the child registered for the assertions

	id, err := sp.SpawnBackground(context.Background(), a, "gated job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	if child == nil {
		t.Fatal("child not registered")
	}
	// Orphan-evict the parent while the child is mid-turn: the evict hook
	// deliberately does NOT fire for orphan eviction, so the child keeps
	// running (its completion is queued instead).
	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("parent runtime missing")
	}
	s.registry.evictRuntime(parentRt)
	close(gate) // let the child finish

	// The child finishes; the parent is not live, so the compact terminal
	// event is queued in the registry rather than delivered.
	waitFor(t, 5*time.Second, func() bool {
		s.registry.parentDeliverMu.Lock()
		defer s.registry.parentDeliverMu.Unlock()
		return len(s.registry.parentDeliveries[a.SessionID]) == 1
	})
	time.Sleep(200 * time.Millisecond)
	if deliveredMessages(a, "[subagent") {
		t.Fatal("completion must not be delivered while the parent is not live")
	}

	// The parent session is reopened: registering a fresh runtime flushes
	// the queued notice into it.
	s.registry.register(a.SessionID, newSessionRuntime(a))
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "[subagent") && deliveredMessages(a, "get_agent_result")
	})
	result, err := sp.GetAgentResult(a, id)
	if err != nil || !strings.Contains(result, "gated report") {
		t.Fatalf("GetAgentResult = %q, %v", result, err)
	}
}

// --- subagent return-path fixes ---

// keepParentPaneAttached starts a WS server and dials a client, waiting for
// the attach sequence. The parent runtime then has a viewer and cannot be
// orphan-evicted while a test asserts on its transcript.
func keepParentPaneAttached(t *testing.T, s *Server) {
	t.Helper()
	srv := startWSServer(t, s)
	t.Cleanup(srv.Close)
	conn := dialWS(t, srv, "/ws") // its own cleanup closes the conn
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
}

// TestChildSystemNoticeDoesNotArmReplyCapture pins the reply-capture source
// gate end to end: a system delivery into a background child that is NOT a
// send_message (here the child's own job-notice shape — job_notices: on)
// must not have the child's reaction injected into the parent as a reply.
// The child still runs the turn; only the capture is withheld.
func TestChildSystemNoticeDoesNotArmReplyCapture(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{
			{Content: "main job report"},     // the main job
			{Content: "reaction to notice"},  // the child's turn on the job notice
			{Content: "reaction to message"}, // the send_message control case
		}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	keepParentPaneAttached(t, s)

	id, err := sp.SpawnBackground(context.Background(), a, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.IsFinished() })

	// A plain system delivery (the job notice shape; the real producer is
	// the registry-backed jobNoticeDeliverer) runs a turn on the child — but
	// its reaction must NOT be injected into the parent.
	if !child.Handle.deliverToSession("[job] job-1 exited with code 0 — echo hi") {
		t.Fatal("system delivery refused")
	}
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range child.Handle.agent.SnapshotMessages() {
			if m.Role == "assistant" && strings.Contains(m.Content, "reaction to notice") {
				return true
			}
		}
		return false
	})
	waitForParentDeliveriesSettled(t, s, a)
	if deliveredMessages(a, "reaction to notice") {
		t.Fatal("a non-send_message system delivery must not arm the reply capture")
	}

	// Control: a send_message still captures the child's reply.
	if err := sp.SendMessage(a, id, "please elaborate"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "reaction to message") })
}

// TestListAgentsProgressLineBounded pins the pull-side progress view: each
// child row carries a one-line, bounded snippet of its latest assistant
// output (the full transcript stays in the child's pane), and a finished
// child stays listed with its outcome.
func TestListAgentsProgressLineBounded(t *testing.T) {
	long := strings.Repeat("x", 400) + "\nsecond line"
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: long}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	// Keep the parent pane attached: the second spawn below needs the parent
	// runtime live (deliveries would otherwise orphan-evict it).
	keepParentPaneAttached(t, s)

	id, err := sp.SpawnBackground(context.Background(), a, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.IsFinished() })

	out, err := sp.ListAgents(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, id) || !strings.Contains(out, "finished") {
		t.Fatalf("listing must name the child and its status: %q", out)
	}
	if !strings.Contains(out, "last: ") {
		t.Fatalf("listing must carry a progress line: %q", out)
	}
	// Bounded and single-line: neither the full body nor the embedded
	// newline survives.
	if strings.Contains(out, strings.Repeat("x", 161)) {
		t.Fatal("progress line is not bounded")
	}
	if strings.Contains(out, "second line") {
		t.Fatal("progress line must collapse to one line")
	}

	waited, err := sp.WaitAgent(t.Context(), a, id, time.Second)
	if err != nil || !strings.Contains(waited, "second line") {
		t.Fatalf("WaitAgent on finished child = %q, %v", waited, err)
	}
	// A second, running child is listed alongside (newest last).
	blocking := make(chan struct{})
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.OnStream = func(ctx context.Context, _ []llm.Message, h *llm.StreamHandlers) (*llm.StreamResult, error) {
			if h != nil && h.OnStreamOpened != nil {
				h.OnStreamOpened()
			}
			select {
			case <-blocking:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &llm.StreamResult{Content: "late"}, nil
		}
		return p
	}
	id2, err := sp.SpawnBackground(context.Background(), a, "second job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return sp.Get(id2).StatusOf(sp) == "running" })
	out, err = sp.ListAgents(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, id) || !strings.Contains(out, id2) {
		t.Fatalf("both children must be listed: %q", out)
	}
	close(blocking)
	sp.CancelAll(a.SessionID)
	waitFor(t, 5*time.Second, func() bool { return len(sp.ChildrenOf(a.SessionID)) == 0 })
}

// TestEvictedChildWithArmedCaptureReleases pins the release predicate: an
// evicted runtime is never "busy" — an exact-result capture armed by a
// delivery whose turn never started must not pin the child record and re-arm
// its retention timer forever.
func TestEvictedChildWithArmedCaptureReleases(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	id, err := sp.SpawnBackground(context.Background(), a, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, func() bool { return child.IsFinished() })

	// Simulate the evicted early return: the start hook armed the exact
	// callback but the turn never ran, so its callback never consumed it.
	child.SetPendingReply(true)
	s.registry.evictRuntime(child.Handle)

	if child.Busy(sp) {
		t.Fatal("an evicted child must not read busy (armed capture or not)")
	}
	sp.ReleaseChildIfQuiet(child)
	if sp.Get(id) != nil {
		t.Fatal("an evicted child must be released, not pinned by the armed capture")
	}
}
