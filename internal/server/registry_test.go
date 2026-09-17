package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/session"
)

func newTestRuntime(t *testing.T) *sessionRuntime {
	t.Helper()
	return newSessionRuntime(agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(t.TempDir()), nil))
}

// newFakeWSConn returns a wsConn whose writeJSON succeeds (buffered send
// queue, open quit/done channels) so broadcast can deliver to it without
// triggering the write-failure detach path — tests can then assert both what
// was broadcast and whether the socket stayed attached.
//
// The queue must absorb everything a scenario writes BEFORE the test reads
// anything: a real client drains continuously, this fake one only drains when
// a helper reads. The attach payload alone is session_state + history + two
// configs (+ the reasoning probe), and a turn adds its own frames on top, so
// the buffer has to cover a full attach + turn with margin — a tight size
// silently drops the frames a later assertion waits for ("websocket send
// queue full").
func newFakeWSConn() *wsConn {
	return &wsConn{
		conn:  &websocket.Conn{},
		sendQ: make(chan WSMessage, 64),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// TestRegistryEvictionEnforcesCap verifies the D6/E11 registry cap: when
// registrations exceed maxActive, the least-recently-active idle session is
// evicted (flushed + session_detached) while the default session and the
// pane's current session survive.
func TestRegistryEvictionEnforcesCap(t *testing.T) {
	r := newSessionRegistry(2)
	rtA := newTestRuntime(t)
	rtB := newTestRuntime(t)
	r.register("a", rtA)
	r.register("b", rtB)
	r.setDefault("b") // b is the default (front of order)

	// Registering a third session must evict the oldest idle non-default (a).
	rtC := newTestRuntime(t)
	r.register("c", rtC)

	ids := r.activeIDs()
	if len(ids) != 2 {
		t.Fatalf("active sessions after eviction = %v, want 2 (cap)", ids)
	}
	if _, ok := r.get("a"); ok {
		t.Fatal("idle non-default session was not evicted")
	}
	if _, ok := r.get("b"); !ok {
		t.Fatal("default session was evicted")
	}
	if _, ok := r.get("c"); !ok {
		t.Fatal("newly registered session missing")
	}
}

// TestRegistryEvictionSkipsInFlight verifies E11: a session with a running
// (including headless) turn is never evicted — the cap is exceeded instead
// of killing the turn.
func TestRegistryEvictionSkipsInFlight(t *testing.T) {
	r := newSessionRegistry(2)
	rtA := newTestRuntime(t)
	rtB := newTestRuntime(t)
	r.register("a", rtA)
	r.register("b", rtB)
	rtB.setTurnActive(true, time.Now(), nil) // b has an in-flight turn

	rtC := newTestRuntime(t)
	r.register("c", rtC)

	if _, ok := r.get("b"); !ok {
		t.Fatal("in-flight session was evicted")
	}
	if len(r.activeIDs()) != 3 {
		t.Fatalf("active sessions = %v, want 3 (cap exceeded, in-flight protected)", r.activeIDs())
	}
}

// TestRegistryEvictionSkipsAcquiringTurn pins the TOCTOU fix: a session whose
// turn is ABOUT to start (the message handler resolved the runtime and holds
// turnMu, but turnActive is not set yet) must not be evicted. Pre-fix, the
// eviction only checked turnState, so a session in that window was evicted
// and the turn then started on an unregistered runtime — invisible to the UI
// and to delete/prune/shutdown. The eviction now also tries the victim's
// turnMu (TryLock, never blocking): a held lock means a turn is running or
// starting, so the session is skipped and the cap is exceeded instead.
func TestRegistryEvictionSkipsAcquiringTurn(t *testing.T) {
	r := newSessionRegistry(2)
	rtA := newTestRuntime(t)
	rtB := newTestRuntime(t)
	r.register("a", rtA)
	r.register("b", rtB)
	r.setDefault("b") // b is the default (front of order)

	// Simulate a message handler that resolved rtA and is about to start a
	// turn: turnMu is held, turnActive is not yet set (startTurn sets it).
	rtA.turnMu.Lock()
	defer rtA.turnMu.Unlock()

	rtC := newTestRuntime(t)
	r.register("c", rtC)

	if _, ok := r.get("a"); !ok {
		t.Fatal("session with an acquiring turn was evicted (TOCTOU)")
	}
	if _, ok := r.get("b"); !ok {
		t.Fatal("default session was evicted")
	}
	if _, ok := r.get("c"); !ok {
		t.Fatal("newly registered session missing")
	}
}

func TestRegistryRegisterGetRemove(t *testing.T) {
	r := newSessionRegistry(0)
	newRT := func() *sessionRuntime {
		return &sessionRuntime{agent: agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(t.TempDir()), nil)}
	}
	rt1 := newRT()
	rt2 := newRT()
	r.register("a", rt1)
	r.register("b", rt2)
	// Duplicate registration is a no-op (E9 dedupe).
	r.register("a", rt2)

	if got, ok := r.get("a"); !ok || got != rt1 {
		t.Fatalf("get(a) = %v, %v; want rt1, true", got, ok)
	}
	if got := r.first(); got != rt1 {
		t.Fatalf("first() = %v; want rt1 (registration order)", got)
	}
	got := r.activeIDs()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("activeIDs = %v; want [a b]", got)
	}
	r.remove("a")
	if _, ok := r.get("a"); ok {
		t.Fatal("get(a) still ok after remove")
	}
	if got := r.first(); got != rt2 {
		t.Fatalf("first() after remove = %v; want rt2", got)
	}
}

// TestSessionTurnLocksArePerSession verifies the per-session turn
// lock: holding one session's turnMu does not block another session's turn
// (the old global turnMu serialized all sessions).
func TestSessionTurnLocksArePerSession(t *testing.T) {
	a1 := agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(t.TempDir()), nil)
	a2 := agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(t.TempDir()), nil)
	rt1 := &sessionRuntime{agent: a1}
	rt2 := &sessionRuntime{agent: a2}

	rt1.turnMu.Lock()
	done := make(chan struct{})
	go func() {
		// Acquiring rt2's turn lock while rt1's is held must not block: turn
		// locks are per-session. If the old global-turnMu behavior
		// regressed, the Lock above would block until rt1 releases its lock
		// and the select below would time out. close(done) runs inside the
		// critical section so the acquisition is proven before the release.
		rt2.turnMu.Lock()
		defer rt2.turnMu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("session 2's turn lock blocked by session 1's lock")
	}
	rt1.turnMu.Unlock()
}

// TestWorkspaceNewSessionAgentFactory verifies the session agent factory:
// a fresh agent per call with its own provider, sharing the workspace
// executor/store, with restored snapshot state and fsMu-wrapped handlers.
func TestWorkspaceNewSessionAgentFactory(t *testing.T) {
	dir := t.TempDir()
	exec := agent.NewExecutor(dir)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	cfg := &config.Config{WorkingDir: dir}
	ws := &Workspace{
		Exec:          exec,
		Store:         store,
		Config:        cfg,
		WorkingDir:    dir,
		GlobalMode:    true,
		Model:         "m1",
		ThinkingLevel: "high",
		// The provider factory seeds the workspace default model + thinking
		// level, mirroring newWorkspaceFromAgent.
		ProviderFactory: func() llm.LLMProvider {
			p := llmtest.NewMockProvider()
			_ = p.SetModel("m1")
			p.SetThinkingLevel("high")
			return p
		},
	}
	id := session.NewID()
	snap := agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}
	a := ws.NewSessionAgent(&snap, id)

	if a.SessionID != id {
		t.Fatalf("SessionID = %q, want %q", a.SessionID, id)
	}
	if a.Executor != exec {
		t.Fatal("session agent does not share the workspace executor")
	}
	if a.SessionStore != store {
		t.Fatal("session agent does not share the workspace store")
	}
	if !a.GlobalMode {
		t.Fatal("GlobalMode not propagated")
	}
	if len(a.Messages) != 1 || a.Messages[0].Content != "hello" {
		t.Fatalf("restored messages = %+v", a.Messages)
	}
	if a.CurrentModel() != "m1" {
		t.Fatalf("provider model = %q, want m1 (workspace default seeding, D1)", a.CurrentModel())
	}
	if string(a.ThinkingLevel) != "high" {
		t.Fatalf("ThinkingLevel = %q, want high", a.ThinkingLevel)
	}

	// The tool handlers must be wrapped so mutating tools take the workspace
	// fsMu. Verify write_file blocks while fsMu is held and runs after release.
	handlers := wrapToolHandlers(agent.BuiltinToolHandlers(), &ws.fsMu)
	writeFile, ok := handlers["write_file"]
	if !ok {
		t.Fatal("write_file handler missing")
	}
	ws.fsMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = writeFile(context.Background(), a, map[string]any{"path": dir + "/f.txt", "content": "x"})
	}()
	select {
	case <-done:
		t.Fatal("write_file completed while fsMu held")
	case <-time.After(50 * time.Millisecond):
	}
	ws.fsMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("write_file did not complete after fsMu release")
	}
}

// TestRegistryEvictionMarksRuntime verifies the evicted flag: a runtime the
// registry cap evicts is marked while the eviction holds its turnMu, so a
// message handler that resolved the runtime just before the eviction and
// acquires turnMu AFTER it completes will see the flag and drop the message
// instead of starting a turn on an unregistered runtime (invisible to
// cancel/prune/shutdown).
func TestRegistryEvictionMarksRuntime(t *testing.T) {
	r := newSessionRegistry(2)
	rtA := newTestRuntime(t)
	rtB := newTestRuntime(t)
	r.register("a", rtA)
	r.register("b", rtB)
	r.setDefault("b")

	evicted := r.register("c", newTestRuntime(t))
	if len(evicted) != 1 || evicted[0] != "a" {
		t.Fatalf("register evicted = %v, want [a]", evicted)
	}
	if _, ok := r.get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if !rtA.evicted.Load() {
		t.Fatal("evicted runtime not marked")
	}
	// The flag must be visible to a handler that acquires turnMu after the
	// eviction completed (the server checks it under the lock).
	rtA.turnMu.Lock()
	defer rtA.turnMu.Unlock()
	if !rtA.evicted.Load() {
		t.Fatal("evicted flag not visible under turnMu after eviction")
	}
}

// TestRegistryEvictRuntimeUnregistersBeforeNotify pins fix B: evictRuntime
// must remove the runtime from the registry BEFORE notifying clients, so a
// concurrent session_attach/resume of the same id can never resolve a
// dying-but-registered runtime — the lookup fails and the caller loads fresh
// from the store instead. The notification itself still reaches attached
// clients (broadcast writes to the clients set, not the registry), and the
// evicted runtime's attachments are swept so no socket lingers in a runtime
// teardown's detachAll can never reach.
func TestRegistryEvictRuntimeUnregistersBeforeNotify(t *testing.T) {
	r := newSessionRegistry(0)
	rt := newTestRuntime(t)
	// Real flows always have an id: NewServer assigns one, and the registry
	// keys runtimes by agent.SessionID (evictRuntime removes by it).
	rt.agent.SessionID = "a"
	r.register("a", rt)

	ws := newFakeWSConn()
	rt.attach(ws)

	r.evictRuntime(rt)

	// Unregistered and marked immediately — no dying-but-registered state.
	if _, ok := r.get("a"); ok {
		t.Fatal("runtime still registered after evictRuntime")
	}
	if !rt.evicted.Load() {
		t.Fatal("evicted runtime not marked")
	}
	// The attached client still received session_detached (notification
	// happens after removal but writes to the clients set).
	select {
	case m := <-ws.sendQ:
		if m.Type != "session_detached" {
			t.Fatalf("broadcast type = %q, want session_detached", m.Type)
		}
	default:
		t.Fatal("attached client did not receive session_detached after eviction")
	}
	// Attachments are swept: the removed runtime holds no sockets.
	if rt.clientCount() != 0 {
		t.Fatal("evicted runtime still has attached clients")
	}
	// A fresh runtime for the same id registers cleanly (no zombie dedupe).
	rt2 := newTestRuntime(t)
	r.register("a", rt2)
	if got, ok := r.get("a"); !ok || got != rt2 {
		t.Fatalf("fresh register after eviction: get(a) = %v, %v; want rt2", got, ok)
	}
}

// TestRegistryCapEvictionDetachesClients pins fix A in the cap-eviction path:
// a victim evicted by the registry cap may have attached clients (another tab
// watching it), and those sockets must be detached after the session_detached
// notification — pre-fix they lingered in the evicted runtime's clients set
// until connection teardown, which can never reach an unregistered runtime.
func TestRegistryCapEvictionDetachesClients(t *testing.T) {
	r := newSessionRegistry(2)
	rtA := newTestRuntime(t)
	rtB := newTestRuntime(t)
	r.register("a", rtA)
	r.register("b", rtB)
	r.setDefault("b") // b is the default (front of order)

	// rtA is watched by a client when the cap evicts it.
	ws := newFakeWSConn()
	rtA.attach(ws)

	r.register("c", newTestRuntime(t))

	if _, ok := r.get("a"); ok {
		t.Fatal("idle session was not evicted")
	}
	select {
	case m := <-ws.sendQ:
		if m.Type != "session_detached" {
			t.Fatalf("broadcast type = %q, want session_detached", m.Type)
		}
	default:
		t.Fatal("attached client did not receive session_detached")
	}
	if rtA.clientCount() != 0 {
		t.Fatal("evicted victim still has attached clients")
	}
}

// TestSessionRuntimeDetachAllClients verifies the detachAllClients sweep:
// every attached socket is removed, and the sweep is idempotent.
func TestSessionRuntimeDetachAllClients(t *testing.T) {
	rt := newTestRuntime(t)
	ws1 := newFakeWSConn()
	ws2 := newFakeWSConn()
	rt.attach(ws1)
	rt.attach(ws2)
	if rt.clientCount() != 2 {
		t.Fatalf("clients attached = %d, want 2", rt.clientCount())
	}
	rt.detachAllClients()
	if rt.clientCount() != 0 {
		t.Fatalf("clients after detachAllClients = %d, want 0", rt.clientCount())
	}
	// Idempotent.
	rt.detachAllClients()
	if rt.clientCount() != 0 {
		t.Fatalf("clients after second detachAllClients = %d, want 0", rt.clientCount())
	}
}

// TestPassiveAttachNotAViewer pins the board-start attach role: a passive
// (approval-only) socket counts for broadcast/approval purposes
// (clientCount) but NOT for the live-session signal (viewerCount) — the
// orphan eviction and the sessions payload's active flag must treat a
// passively-attached idle runtime as a plain saved session, not a stale
// "resume to continue" row. A viewer attach upgrades a passive socket;
// detach clears the role.
func TestPassiveAttachNotAViewer(t *testing.T) {
	r := newSessionRegistry(0)
	rt := newTestRuntime(t)
	rt.agent.SessionID = "a"
	r.register("a", rt)

	ws := newFakeWSConn()
	rt.attachPassive(ws)
	if rt.clientCount() != 1 {
		t.Fatalf("passive attach clientCount = %d, want 1 (approval machinery must see it)", rt.clientCount())
	}
	if rt.viewerCount() != 0 {
		t.Fatalf("passive attach viewerCount = %d, want 0 (not a viewer)", rt.viewerCount())
	}
	// Not a viewer → the idle runtime is orphan-evictable.
	rt.evictOrphanedIfPossible()
	if _, ok := r.get("a"); ok {
		t.Fatal("passively-attached idle runtime was not orphan-evicted")
	}

	// A viewer attach upgrades a passive socket back to a viewer.
	rt2 := newTestRuntime(t)
	rt2.agent.SessionID = "a"
	r.register("a", rt2)
	rt2.attachPassive(ws)
	rt2.attach(ws)
	if rt2.viewerCount() != 1 {
		t.Fatalf("viewer attach after passive: viewerCount = %d, want 1 (upgraded)", rt2.viewerCount())
	}
	rt2.detach(ws)
	if rt2.clientCount() != 0 || rt2.viewerCount() != 0 {
		t.Fatalf("after detach: clients=%d viewers=%d, want 0/0", rt2.clientCount(), rt2.viewerCount())
	}
}

// TestFSMutatingToolsConsistency guards the server's fsMu wrapper set against
// drift from the agent tool registry. fsMutatingTools must equal the
// registry's MutatesFS flag set exactly: a mutating tool that isn't flagged
// (or a flag that isn't wrapped) would bypass the workspace filesystem lock.
func TestFSMutatingToolsConsistency(t *testing.T) {
	want := make(map[string]bool)
	for _, name := range agent.FSMutatingToolNames() {
		want[name] = true
	}
	if len(fsMutatingTools) != len(want) {
		t.Fatalf("fsMutatingTools has %d entries, registry flags %d: %v vs %v", len(fsMutatingTools), len(want), fsMutatingTools, want)
	}
	for name := range want {
		if !fsMutatingTools[name] {
			t.Errorf("fsMutatingTools missing registry-flagged tool %q", name)
		}
	}
	for name := range fsMutatingTools {
		if !want[name] {
			t.Errorf("fsMutatingTools wraps tool %q that the registry does not flag MutatesFS", name)
		}
	}
}

// TestCapEvictionKillsBackgroundJobs pins the background-job half of the
// cap-eviction teardown contract. Every eviction path except register's cap
// tail goes through evictRuntime, which kills the session's execute_command
// background=true jobs ("kill them rather than leak orphan processes");
// pre-fix the cap tail flushed and detached the victim but skipped
// agent.Close, so the job's process kept running with no owner left to poll
// it, and its eventual completion notice was silently dropped (the
// registry-backed deliverer resolves the live runtime and finds none).
func TestCapEvictionKillsBackgroundJobs(t *testing.T) {
	dir := t.TempDir()
	r := newSessionRegistry(2)
	rtA := newSessionRuntime(agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(dir), nil))
	rtB := newTestRuntime(t)
	r.register("a", rtA)
	r.register("b", rtB)
	r.setDefault("b") // b is the default (front of order): a is the LRU victim

	// A long-running background job on the soon-to-be-evicted session:
	// sleep 30, then write the marker. If the eviction fails to kill the
	// job, the process outlives the runtime and completes its work. (The
	// BackgroundJob handle is agent-internal; the test observes the job
	// through the exported status/hook API only.)
	marker := filepath.Join(dir, "marker")
	id, err := rtA.agent.StartBackgroundCommand(context.Background(), "sleep 30; echo done > "+marker)
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	if st, err := rtA.agent.BackgroundJobStatus(id); err != nil || !strings.Contains(st, "RUNNING") {
		t.Fatalf("status before eviction: %q, %v; want RUNNING", st, err)
	}
	// Record any completion notice: after the fix the eviction CANCELS the
	// job, so onJobFinished never fires the hook — and a notice from a
	// legitimately finished job would be dropped anyway, because the
	// deliverer resolves the runtime through the registry the victim just
	// left.
	notices := make(chan string, 1)
	rtA.agent.SetJobNoticeHook(func(summary string) { notices <- summary })

	// The third registration exceeds the cap → a is cap-evicted.
	r.register("c", newTestRuntime(t))

	if _, ok := r.get("a"); ok {
		t.Fatal("cap eviction did not unregister the victim")
	}
	if !rtA.evicted.Load() {
		t.Fatal("evicted runtime not marked")
	}
	// The eviction tail must have run agent.Close: the job registry is
	// cleared, so a later poll reports the job unknown. Pre-fix the job
	// stayed registered — RUNNING, then FINISHED for the whole retention
	// window, and never unknown.
	waitFor(t, 5*time.Second, func() bool {
		_, err := rtA.agent.BackgroundJobStatus(id)
		return err != nil && strings.Contains(err.Error(), "unknown background job")
	})
	// The kill must have cut the sleep short: the marker is never written.
	requireNever(t, 750*time.Millisecond, "background job survived cap eviction and completed its work", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
	// No completion notice fires for the cancelled job.
	requireNever(t, 250*time.Millisecond, "completion notice fired for a cap-evicted session's killed job", func() bool {
		select {
		case <-notices:
			return true
		default:
			return false
		}
	})
	// No pollable residue: the evicted session's job registry is empty.
	if _, err := rtA.agent.BackgroundJobStatus(id); err == nil || !strings.Contains(err.Error(), "unknown background job") {
		t.Fatalf("status after cap eviction: %v, want unknown background job", err)
	}
}

// TestTryAcquireTurnWakesOnRelease pins the release-broadcast handoff: a
// waiter parked in tryAcquireTurn acquires the instant releaseTurn fires,
// rather than after the next polling interval (the old loop slept 5 ms).
func TestTryAcquireTurnWakesOnRelease(t *testing.T) {
	rt := &sessionRuntime{}
	rt.turnMu.Lock()

	acquired := make(chan bool, 1)
	go func() {
		acquired <- rt.tryAcquireTurn(2 * time.Second)
	}()
	// Give the waiter time to park on the release broadcast before releasing.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	rt.releaseTurn()
	select {
	case ok := <-acquired:
		if !ok {
			t.Fatal("tryAcquireTurn returned false after a release")
		}
	case <-time.After(time.Second):
		t.Fatal("tryAcquireTurn did not wake on the release broadcast")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("woke %v after release; want an immediate handoff", elapsed)
	}
	rt.turnMu.Unlock()
}

// TestTryAcquireTurnTimesOut pins the bounded wait: with no release the waiter
// returns false at the deadline instead of blocking forever.
func TestTryAcquireTurnTimesOut(t *testing.T) {
	rt := &sessionRuntime{}
	rt.turnMu.Lock()
	defer rt.turnMu.Unlock()
	start := time.Now()
	if rt.tryAcquireTurn(80 * time.Millisecond) {
		t.Fatal("tryAcquireTurn acquired a held lock")
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("returned after %v; the deadline was 80ms", elapsed)
	}
}

// TestTryAcquireTurnNonPositiveIsProbe pins the wait<=0 fast path: a single
// non-blocking probe with no timer.
func TestTryAcquireTurnNonPositiveIsProbe(t *testing.T) {
	rt := &sessionRuntime{}
	rt.turnMu.Lock()
	if rt.tryAcquireTurn(0) {
		t.Fatal("tryAcquireTurn(0) acquired a held lock")
	}
	rt.turnMu.Unlock()
	if !rt.tryAcquireTurn(0) {
		t.Fatal("tryAcquireTurn(0) must acquire a free lock")
	}
	rt.turnMu.Unlock()
}
