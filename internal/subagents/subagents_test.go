package subagents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/llm/llmtest"
)

// stubHandle is a host runtime stand-in; the lifecycle never inspects it.
type stubHandle struct {
	turnActive        bool
	pendingDeliveries bool
	evicted           bool
	releaseCalls      int
	releaseCancelled  []bool
}

// stubHost implements Host for the lifecycle tests. Every callback records
// what the lifecycle asked the host to do, so the tests can assert on the
// host contract rather than on internal state.
type stubHost struct {
	mu sync.Mutex

	policy   Policy
	newChild func(parent *agent.Agent, job, model string, depth int) error
	run      func(ctx context.Context, c *Child[*stubHandle], prompt string) (string, error)

	handles   map[string]*stubHandle
	delivered []string // parent deliveries (terminal events and replies)
	started   []string
	finished  []string
	prompts   []string
	persisted map[string]string
}

func newStubHost() *stubHost {
	return &stubHost{
		handles:   make(map[string]*stubHandle),
		persisted: make(map[string]string),
	}
}

func (h *stubHost) Policy() Policy { return h.policy }

func (h *stubHost) NewChild(_ context.Context, parent *agent.Agent, job, model string, depth int) (NewChild[*stubHandle], error) {
	if h.newChild != nil {
		if err := h.newChild(parent, job, model, depth); err != nil {
			return NewChild[*stubHandle]{}, err
		}
	}
	handle := &stubHandle{}
	seq := len(h.handles) + 1
	child := testAgent(fmt.Sprintf("child-%d", seq), "mock-model")
	h.mu.Lock()
	h.handles[child.SessionID] = handle
	h.mu.Unlock()
	return NewChild[*stubHandle]{Handle: handle, Agent: child, Label: agent.SubagentLabel(job)}, nil
}

func (h *stubHost) RunTurn(ctx context.Context, c *Child[*stubHandle], prompt string) (string, error) {
	h.mu.Lock()
	h.prompts = append(h.prompts, prompt)
	run := h.run
	h.mu.Unlock()
	if run == nil {
		return "report", nil
	}
	return run(ctx, c, prompt)
}

func (h *stubHost) CancelTurn(c *Child[*stubHandle]) {}

func (h *stubHost) DeliverChildMessage(c *Child[*stubHandle], text string) (ok, full bool) {
	return true, false
}

func (h *stubHost) ArmReplyCapture(c *Child[*stubHandle]) {}

func (h *stubHost) Release(c *Child[*stubHandle], cancelledMidRun bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hnd := h.handles[c.ID]; hnd != nil {
		hnd.releaseCalls++
		hnd.releaseCancelled = append(hnd.releaseCancelled, cancelledMidRun)
	}
}

func (h *stubHost) Deliver(parentID, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.delivered = append(h.delivered, parentID+": "+text)
}

func (h *stubHost) Started(c *Child[*stubHandle], job string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = append(h.started, c.ID)
}

func (h *stubHost) Finished(c *Child[*stubHandle], report string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finished = append(h.finished, c.ID)
}

func (h *stubHost) PersistedResult(caller *agent.Agent, childID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.persisted[childID]
	return v, ok
}

func (h *stubHost) TurnActive(c *Child[*stubHandle]) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.handles[c.ID] != nil && h.handles[c.ID].turnActive
}

func (h *stubHost) Busy(c *Child[*stubHandle]) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	handle := h.handles[c.ID]
	if handle == nil || handle.evicted {
		return false
	}
	if handle.turnActive || handle.pendingDeliveries {
		return true
	}
	return c.PendingReply()
}

func (h *stubHost) deliverTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.delivered...)
}

func (h *stubHost) releaseCount(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hnd := h.handles[id]; hnd != nil {
		return hnd.releaseCalls
	}
	return 0
}

func (h *stubHost) runPromptCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.prompts)
}

// testAgent builds a parent/child agent around a mock provider.
func testAgent(id, model string) *agent.Agent {
	prov := llmtest.NewMockProvider()
	prov.Model = model
	a := agent.NewAgent(prov, agent.NewExecutor(""), contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	a.SetSessionID(id)
	return a
}

// waitFor polls cond until it holds or the deadline expires.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSpawnBackgroundCompletionContract pins the one-return-path contract: the
// child is announced only when admitted, the parent receives ONE compact
// terminal event (not the report), and the finished child stays registered for
// the retention window.
func TestSpawnBackgroundCompletionContract(t *testing.T) {
	host := newStubHost()
	host.policy = Policy{Retain: time.Hour, MaxFinished: 2}
	sp := New[*stubHandle](host)
	parent := testAgent("parent", "parent-model")
	parent.SetSessionID("parent")

	id, err := sp.SpawnBackground(context.Background(), parent, "first job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	// IsFinished flips inside child.Finish, before Finished and before the
	// parent notice is posted. The notice is the last step of that path, so
	// waiting for it is what makes the announcement counts stable.
	waitFor(t, 5*time.Second, "terminal event", func() bool {
		return child.IsFinished() && len(host.deliverTexts()) == 1
	})

	host.mu.Lock()
	started, finished := len(host.started), len(host.finished)
	host.mu.Unlock()
	if started != 1 || finished != 1 {
		t.Fatalf("announcements = %d started / %d finished, want 1/1", started, finished)
	}
	delivered := host.deliverTexts()
	if len(delivered) != 1 {
		t.Fatalf("parent deliveries = %v, want exactly one terminal event", delivered)
	}
	notice := delivered[0]
	if strings.Contains(notice, "report") {
		t.Fatalf("terminal event leaked the report: %q", notice)
	}
	if !strings.Contains(notice, "(finished)") || !strings.Contains(notice, "get_agent_result") {
		t.Fatalf("terminal event missing its verb/pull hint: %q", notice)
	}
	// Retention: still registered, and its authoritative result is the report.
	if sp.Get(id) == nil {
		t.Fatal("a finished child must stay registered for the retention window")
	}
	got, err := sp.GetAgentResult(parent, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "finished") || !strings.Contains(got, "report") {
		t.Fatalf("GetAgentResult = %q", got)
	}
}

// TestSpawnRefusedLeavesNoTrace pins the discard contract: a spawn over the
// active cap is torn down with cancelledMidRun=false (no outcome stamp, no
// session write), never announced, and reported as the active-limit refusal.
func TestSpawnRefusedLeavesNoTrace(t *testing.T) {
	release := make(chan struct{})
	host := newStubHost()
	host.policy = Policy{Retain: time.Hour, Concurrent: func(string) int { return 1 }}
	host.run = func(ctx context.Context, _ *Child[*stubHandle], _ string) (string, error) {
		<-release
		return "done", nil
	}
	sp := New[*stubHandle](host)
	parent := testAgent("parent", "m")
	parent.SetSessionID("parent")

	first, err := sp.SpawnBackground(context.Background(), parent, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "first turn started", func() bool { return host.runPromptCount() == 1 })
	if _, err := sp.SpawnBackground(context.Background(), parent, "refused job", "", 0); err == nil ||
		!strings.Contains(err.Error(), "subagent limit reached (1 concurrent)") {
		t.Fatalf("spawn over the active cap = %v, want the concurrent-limit refusal", err)
	}
	host.mu.Lock()
	announced := len(host.started)
	var refusedReleased []bool
	for id, hnd := range host.handles {
		if id != first {
			refusedReleased = hnd.releaseCancelled
		}
	}
	host.mu.Unlock()
	if announced != 1 {
		t.Fatalf("announced children = %d, want only the admitted one", announced)
	}
	if len(refusedReleased) != 1 || refusedReleased[0] {
		t.Fatalf("refused child release flags = %v, want exactly one non-cancelled release", refusedReleased)
	}
	close(release)
	waitFor(t, 5*time.Second, "first child finished", func() bool { return sp.Get(first).IsFinished() })
}

// TestInterruptKeepsChildContinuable pins the interruption contract: the main
// run is marked idle, the child stays registered and messageable, and NO
// completion event reaches the parent.
func TestInterruptKeepsChildContinuable(t *testing.T) {
	started := make(chan struct{})
	host := newStubHost()
	host.policy = Policy{Retain: time.Hour}
	host.run = func(ctx context.Context, _ *Child[*stubHandle], _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	sp := New[*stubHandle](host)
	parent := testAgent("parent", "m")
	parent.SetSessionID("parent")

	id, err := sp.SpawnBackground(context.Background(), parent, "blocking job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := sp.InterruptAgent(parent, id); err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, "child idle", func() bool { return child.StatusOf(host) == "idle" })
	if child.IsFinished() {
		t.Fatal("an interrupted main run must not read finished")
	}
	if got := host.deliverTexts(); len(got) != 0 {
		t.Fatalf("interruption delivered %v, want nothing", got)
	}
	waitFor(t, 5*time.Second, "wait_agent released by interruption", func() bool {
		status, err := sp.WaitAgent(context.Background(), parent, id, 50*time.Millisecond)
		return err == nil && strings.Contains(status, "interrupted before completion")
	})
	if sp.Get(id) == nil {
		t.Fatal("an interrupted child must stay continuable (registered)")
	}
}

// TestReleaseChildIfQuietDefersWhileBusy pins the retention trap: a child with
// reply work in flight must not be released (the reply would be dropped
// silently), and the deferred release happens once it goes quiet.
func TestReleaseChildIfQuietDefersWhileBusy(t *testing.T) {
	host := newStubHost()
	host.policy = Policy{Retain: 20 * time.Millisecond}
	sp := New[*stubHandle](host)
	parent := testAgent("parent", "m")
	parent.SetSessionID("parent")

	id, err := sp.SpawnBackground(context.Background(), parent, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, "child finished", func() bool { return child.IsFinished() })

	// An armed exact-reply capture must pin the child.
	child.SetPendingReply(true)
	sp.ReleaseChildIfQuiet(child)
	if sp.Get(id) == nil {
		t.Fatal("an armed reply capture was released (the reply would be lost)")
	}
	// Quiet: the re-armed timer releases it. Unpublish happens before the
	// host Release call, so wait for that call too — reading the counter
	// off the handle races the timer's write.
	child.SetPendingReply(false)
	waitFor(t, 5*time.Second, "quiet child released", func() bool {
		return sp.Get(id) == nil && host.releaseCount(id) == 1
	})
}

// TestReleaseIsIdempotent pins that concurrent release requests reach the host
// once (the registry claims the release atomically).
func TestReleaseIsIdempotent(t *testing.T) {
	host := newStubHost()
	host.policy = Policy{Retain: time.Hour}
	sp := New[*stubHandle](host)
	parent := testAgent("parent", "m")
	parent.SetSessionID("parent")
	id, err := sp.SpawnBackground(context.Background(), parent, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.Get(id)
	waitFor(t, 5*time.Second, "child finished", func() bool { return child.IsFinished() })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sp.ReleaseChild(child)
		}()
	}
	wg.Wait()
	if got := host.releaseCount(id); got != 1 {
		t.Fatalf("host release calls = %d, want exactly 1", got)
	}
}

// TestOwnershipGuard pins the direct-parent authorization boundary shared by
// every control tool, and the persisted fallback for a released child.
func TestOwnershipGuard(t *testing.T) {
	host := newStubHost()
	host.policy = Policy{Retain: time.Hour}
	sp := New[*stubHandle](host)
	parent := testAgent("parent", "m")
	parent.SetSessionID("parent")
	id, err := sp.SpawnBackground(context.Background(), parent, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "child finished", func() bool { return sp.Get(id).IsFinished() })

	other := testAgent("other", "m")
	other.SetSessionID("other")
	if _, err := sp.GetAgentResult(other, id); err == nil ||
		!strings.Contains(err.Error(), "is not a child of this session") {
		t.Fatalf("cross-parent result = %v, want the ownership refusal", err)
	}
	if err := sp.SendMessage(other, id, "hi"); err == nil {
		t.Fatal("cross-parent send_message must be refused")
	}
	if _, err := sp.ListAgents(other); err != nil {
		t.Fatalf("ListAgents for a childless session = %v", err)
	}

	// Released child: the control tools fall back to the persisted outcome.
	sp.ReleaseChild(sp.Get(id))
	host.mu.Lock()
	host.persisted[id] = "persisted report"
	host.mu.Unlock()
	got, err := sp.GetAgentResult(parent, id)
	if err != nil || got != "persisted report" {
		t.Fatalf("persisted fallback = %q, %v", got, err)
	}
}

// TestEventParentMessage pins the host-neutral event text: a reply is prefixed
// with the label and bounded by the report cap; an empty reply reports the
// failure instead of delivering an empty message.
func TestEventParentMessage(t *testing.T) {
	long := strings.Repeat("r", agent.MaxSubagentReportBytes+1000)
	tests := []struct {
		name string
		in   Event
		want func(string) bool
	}{
		{
			name: "terminal passes the notice through",
			in:   Event{Kind: EventTerminal, Text: "[subagent x] done"},
			want: func(s string) bool { return s == "[subagent x] done" },
		},
		{
			name: "reply is labeled and truncated",
			in:   Event{Kind: EventReply, Label: "subagent: job", Text: long},
			want: func(s string) bool {
				return strings.HasPrefix(s, "[subagent subagent: job] ") &&
					len(s) <= agent.MaxSubagentReportBytes+len("… (truncated)")+64
			},
		},
		{
			name: "empty reply with error reports the failure",
			in:   Event{Kind: EventReply, Label: "subagent: job", Err: errors.New("boom")},
			want: func(s string) bool { return strings.Contains(s, "no reply") && strings.Contains(s, "boom") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.ParentMessage(); !tc.want(got) {
				t.Fatalf("ParentMessage() = %q", got)
			}
		})
	}
}

// TestLiveGuardRefusesSpawnBeyondNonFinished pins the internal memory guard:
// interrupted (idle) children hold no active slot but still count against the
// non-finished bound, so a spawn+interrupt loop cannot accumulate runtimes.
func TestLiveGuardRefusesSpawnBeyondNonFinished(t *testing.T) {
	started := make(chan struct{}, 4)
	host := newStubHost()
	host.policy = Policy{Retain: time.Hour, MaxLiveGuard: 1}
	host.run = func(ctx context.Context, _ *Child[*stubHandle], _ string) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	sp := New[*stubHandle](host)
	parent := testAgent("parent", "m")
	parent.SetSessionID("parent")

	id, err := sp.SpawnBackground(context.Background(), parent, "job one", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := sp.InterruptAgent(parent, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "child idle", func() bool { return sp.Get(id).StatusOf(host) == "idle" })

	if _, err := sp.SpawnBackground(context.Background(), parent, "job two", "", 0); err == nil ||
		!strings.Contains(err.Error(), "too many live subagents") {
		t.Fatalf("spawn beyond the live guard = %v, want the guard refusal", err)
	}
}
