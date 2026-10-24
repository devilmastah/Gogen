package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm.land/bubbles/v2/textarea"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/session"
	"gogen/internal/subagents"
)

// childHarness wires a Model, a host spawner and a recording sender: the
// Update-thread halves are called directly (the tests own the "event loop").
type childHarness struct {
	m      *Model
	sp     *tuiSubagentSpawner
	sender *syncSender
	cfg    *config.Config
}

func newChildHarness(t *testing.T, content string) *childHarness {
	t.Helper()
	// The whole harness runs on MOCK providers, including the root agent: the
	// child's completion notice starts a delivery turn on the parent, and a
	// real provider (dragModel's dead endpoint) would stall that turn.
	parentProv := llmtest.NewMockProvider()
	parentProv.Model = "default-model"
	parentProv.Models = []llm.ModelInfo{{ID: "default-model", ContextLimit: 128000}}
	parentProv.StreamResults = []*llm.StreamResult{{Content: "ack"}}
	dir := t.TempDir()
	root := agent.NewAgent(parentProv, agent.NewExecutor(dir), contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000}))
	root.SetSessionID("parent-session")
	vp := NewViewport(80, 20)
	ta := textarea.New()
	ta.SetHeight(3)
	m := &Model{
		agent:          root,
		lives:          newLiveSessions(root),
		viewport:       vp,
		textarea:       ta,
		width:          100,
		height:         30,
		sidebarVisible: true,
		sidebarWidth:   26,
	}
	cfg := &config.Config{OpenAIModel: "default-model"}
	sp := newTUISubagentSpawner(cfg, nil)
	// Mock children: the real provider factory would dial the catalog.
	sp.providerFactory = func(*config.Config, *agent.Agent) llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = []llm.ModelInfo{{ID: "default-model", ContextLimit: 128000}}
		p.StreamResults = []*llm.StreamResult{{Content: content}}
		return p
	}
	sender := &syncSender{m: m}
	sp.setSender(func() programSender { return sender }, nil)
	m.spawner = sp
	m.cfg = cfg
	return &childHarness{m: m, sp: sp, sender: sender, cfg: cfg}
}

// newChild builds a child agent + handle pair the way the lifecycle would,
// returning the lifecycle child record bound to the handle.
func (h *childHarness) newChild(t *testing.T, parent *agent.Agent, content, label string) (*agent.Agent, *childHandle, *subagents.Child[*childHandle]) {
	t.Helper()
	prov := llmtest.NewMockProvider()
	prov.Model = "child-model"
	prov.StreamResults = []*llm.StreamResult{{Content: content}}
	child := agent.NewSessionAgent(agent.SessionAgentOptions{
		Provider: prov,
		Executor: parent.Executor,
		Config:   h.cfg,
	}, nil, session.NewID())
	child.SetParentID(parent.SessionID)
	handle := newChildHandle(child)
	c := &subagents.Child[*childHandle]{
		ID:       child.SessionID,
		ParentID: parent.SessionID,
		Label:    label,
		Depth:    1,
		Agent:    child,
		Handle:   handle,
	}
	h.sp.ArmReplyCapture(c)
	return child, handle, c
}

// admit starts the child's turn the way the lifecycle does (childSpawnMsg), and
// returns the started turn command.
func (h *childHarness) admit(t *testing.T, handle *childHandle, label, text string) tea.Cmd {
	t.Helper()
	_, cmd := h.m.handleChildSpawnMsg(childSpawnMsg{handle: handle, label: label, text: text, ready: handle.ready})
	if err := <-handle.ready; err != nil {
		t.Fatalf("child admission failed: %v", err)
	}
	return cmd
}

// TestChildSpawnAdmitsLiveSlot pins the handoff: the Update thread creates a
// slot whose routing id IS the child's session id (so every stream event routes
// without a new id space), and the turn's exact output resolves the handle.
func TestChildSpawnAdmitsLiveSlot(t *testing.T) {
	hh := newChildHarness(t, "child report")
	child, handle, _ := hh.newChild(t, hh.m.agent, "child report", "subagent: job")

	cmd := hh.admit(t, handle, "subagent: job", "job prompt")
	if cmd == nil {
		t.Fatal("admission returned no turn command")
	}
	slot := hh.m.lives.ByID(child.SessionID)
	if slot == nil {
		t.Fatalf("no live slot for child %s", child.SessionID)
	}
	if slot.parentID != hh.m.agent.SessionID {
		t.Fatalf("slot parentID = %q, want %q", slot.parentID, hh.m.agent.SessionID)
	}
	if slot.child != handle {
		t.Fatal("slot is not bound to the child handle")
	}

	// Run the turn: the output reaches the handle (RunTurn's side of the
	// handoff) and the terminal finalizes the slot.
	if msg := cmd(); msg != nil {
		hh.m.handleMsg(msg)
	}
	select {
	case res := <-handle.result:
		if res.out != "child report" {
			t.Fatalf("turn output = %q, want the child's report", res.out)
		}
		if res.err != nil {
			t.Fatalf("turn error = %v", res.err)
		}
	default:
		t.Fatal("the turn output never resolved the handle")
	}
	if handle.TurnActive() {
		t.Fatal("the child handle still reads an in-flight turn")
	}
}

// TestChildSpawnRejectsDuplicateSlot pins that a second admission for the same
// child reports an error instead of leaking a second slot.
func TestChildSpawnRejectsDuplicateSlot(t *testing.T) {
	hh := newChildHarness(t, "report")
	_, handle, _ := hh.newChild(t, hh.m.agent, "report", "subagent: job")
	hh.admit(t, handle, "subagent: job", "job")

	_, _ = hh.m.handleChildSpawnMsg(childSpawnMsg{handle: handle, label: "subagent: job", text: "job", ready: handle.ready})
	if err := <-handle.ready; err == nil || !strings.Contains(err.Error(), "already live") {
		t.Fatalf("duplicate admission = %v, want an already-live error", err)
	}
}

// TestRunTurnWithoutProgramReportsInsteadOfBlocking pins the headless guard: a
// background spawn with no running program fails the tool call rather than
// parking the parent's goroutine forever.
func TestRunTurnWithoutProgramReportsInsteadOfBlocking(t *testing.T) {
	m := dragModel(t)
	cfg := &config.Config{OpenAIModel: "default-model"}
	sp := newTUISubagentSpawner(cfg, nil)
	_ = m
	c := &subagents.Child[*childHandle]{Handle: newChildHandle(m.agent)}
	if _, err := sp.RunTurn(context.Background(), c, "job"); err == nil ||
		!strings.Contains(err.Error(), "terminal UI is not running") {
		t.Fatalf("RunTurn without a program = %v, want the not-running error", err)
	}
}

// TestChildMessageQueuesAndRefusesWhenFull pins the targeted-delivery contract:
// a send_message queues on the child's slot (never batched with notices), and a
// full queue is REFUSED with full=true instead of silently dropping a message
// whose reply the parent waits on.
func TestChildMessageQueuesAndRefusesWhenFull(t *testing.T) {
	hh := newChildHarness(t, "child reply")
	child, handle, _ := hh.newChild(t, hh.m.agent, "child reply", "subagent: job")
	slot := hh.m.lives.AddChild(child, "subagent: job", handle)
	handle.setSlot(slot)

	// A busy child (turn in flight) keeps the delivery queued instead of
	// draining it immediately.
	slot.streaming = true
	ack := make(chan childMessageAck, 1)
	_, _ = hh.m.handleChildMessageMsg(childMessageMsg{handle: handle, text: "please elaborate", ack: ack})
	if res := <-ack; !res.ok || res.full {
		t.Fatalf("first send_message = %+v, want queued", res)
	}
	if len(slot.steerQueue) != 1 || slot.steerQueue[0].child != handle {
		t.Fatal("the targeted delivery is not queued on the child's slot")
	}
	if got := handle.queueDepth; got != 1 {
		t.Fatalf("queue depth mirror = %d, want 1", got)
	}

	// Fill the system queue with ordinary notices: a targeted delivery must
	// still be refused (not silently dropped) once the queue is at its cap.
	slot.steerQueue = nil
	for i := 0; i < maxPendingDeliveries; i++ {
		slot.steerQueue = append(slot.steerQueue, steerItem{id: newSteerItemID(), text: "notice", system: true})
	}
	ack = make(chan childMessageAck, 1)
	_, _ = hh.m.handleChildMessageMsg(childMessageMsg{handle: handle, text: "again", ack: ack})
	if res := <-ack; res.ok || !res.full {
		t.Fatalf("send_message at a full queue = %+v, want refused as full", res)
	}
}

// TestChildReplyReachesParent pins the single return channel per send_message:
// the child's exact turn output is delivered to the parent session (by SESSION
// id, so a focus switch cannot misroute it), and the reply capture is armed
// only for a targeted delivery.
func TestChildReplyReachesParent(t *testing.T) {
	hh := newChildHarness(t, "the exact reply")
	child, handle, c := hh.newChild(t, hh.m.agent, "the exact reply", "subagent: job")
	slot := hh.m.lives.AddChild(child, "subagent: job", handle)
	handle.setSlot(slot)

	// A notice on the child must NOT produce a parent reply.
	slot.steerQueue = append(slot.steerQueue, steerItem{id: newSteerItemID(), text: "notice", system: true})
	if cmd := hh.m.drainSessionQueue(slot); cmd == nil {
		t.Fatal("notice drain returned no turn")
	} else if msg := cmd(); msg != nil {
		hh.m.handleMsg(msg)
	}
	hh.sender.take()
	if c.PendingReply() {
		t.Fatal("a plain notice armed the reply capture")
	}

	// A targeted delivery does: its exact output becomes the injected reply.
	ack := make(chan childMessageAck, 1)
	_, cmd := hh.m.handleChildMessageMsg(childMessageMsg{handle: handle, text: "please elaborate", ack: ack})
	if res := <-ack; !res.ok {
		t.Fatalf("send_message refused: %+v", res)
	}
	if !c.PendingReply() {
		t.Fatal("the targeted delivery did not arm the reply capture")
	}
	if cmd == nil {
		t.Fatalf("the targeted delivery did not start a turn (queue = %d)", len(slot.steerQueue))
	}
	if msg := cmd(); msg != nil {
		hh.m.handleMsg(msg)
	}

	// The host posts asynchronously (tea.Program.Send is an unbuffered handoff
	// to the event loop), so wait for the delivery instead of racing it.
	var delivered []deliveryRequestMsg
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range hh.sender.take() {
			if d, ok := msg.(deliveryRequestMsg); ok {
				delivered = append(delivered, d)
			}
		}
		if len(delivered) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(delivered) != 1 {
		t.Fatalf("parent deliveries = %v, want exactly one reply", delivered)
	}
	if delivered[0].sid != hh.m.agent.SessionID {
		t.Fatalf("reply routed to %q, want the parent session %q", delivered[0].sid, hh.m.agent.SessionID)
	}
	if !strings.Contains(delivered[0].text, "the exact reply") {
		t.Fatalf("reply text = %q", delivered[0].text)
	}
	if c.PendingReply() {
		t.Fatal("the reply capture stayed armed after delivery")
	}
}

// TestChildReleaseRemovesSlot pins the teardown: a released child's slot leaves
// the registry (its saved session stays on disk) and the focus falls back to
// the root session when the child was focused.
func TestChildReleaseRemovesSlot(t *testing.T) {
	hh := newChildHarness(t, "report")
	child, handle, c := hh.newChild(t, hh.m.agent, "report", "subagent: job")
	slot := hh.m.lives.AddChild(child, "subagent: job", handle)
	handle.setSlot(slot)

	hh.m.handleChildReleaseMsg(childReleaseMsg{handle: handle, cancelledMidRun: false})
	if hh.m.lives.ByID(child.SessionID) != nil {
		t.Fatal("the released child's slot is still registered")
	}
	if handle.slotOf() != nil {
		t.Fatal("the handle still points at the removed slot")
	}
	if handle.busy() {
		t.Fatal("a released handle must read quiet")
	}
	_ = c
}

// TestChildPolicyCapsFollowConfig pins that the TUI host's policy exposes the
// configured concurrency limit and the finished-children cap.
func TestChildPolicyCapsFollowConfig(t *testing.T) {
	cfg := &config.Config{OpenAIModel: "default-model", SubagentMaxConcurrent: 2}
	sp := newTUISubagentSpawner(cfg, nil)
	pol := sp.Policy()
	if got := pol.Concurrent("any"); got != cfg.SubagentLimit() {
		t.Fatalf("policy concurrent = %d, want %d", got, cfg.SubagentLimit())
	}
	if pol.MaxFinished != maxRecordedSubagents {
		t.Fatalf("policy MaxFinished = %d, want %d", pol.MaxFinished, maxRecordedSubagents)
	}
}

// TestBusyIncludesArmedReply pins the retention trap on the TUI host: an armed
// exact-reply capture keeps the child busy, but a RELEASED child reports quiet
// (so its retention timer cannot re-arm forever).
func TestBusyIncludesArmedReply(t *testing.T) {
	hh := newChildHarness(t, "report")
	_, _, c := hh.newChild(t, hh.m.agent, "report", "subagent: job")
	c.BeginReply()
	if !hh.sp.Busy(c) {
		t.Fatal("an armed reply capture must read busy")
	}
	c.SetPendingReply(false)
	if hh.sp.Busy(c) {
		t.Fatal("a quiet child must not read busy")
	}
}

// blockingSender blocks inside Send until released, recording what arrived.
type blockingSender struct {
	release chan struct{}
	got     chan tea.Msg
}

func (b *blockingSender) Send(msg tea.Msg) {
	<-b.release
	b.got <- msg
}

// TestHostSenderNeverBlocksTheUpdateLoop pins the deadlock fix: a host post
// made ON the Update goroutine must not wait for the program's unbuffered
// handoff (the quit sweep and the sidebar's close action post from there),
// while a post from any other goroutine stays inline.
func TestHostSenderNeverBlocksTheUpdateLoop(t *testing.T) {
	inner := &blockingSender{release: make(chan struct{}), got: make(chan tea.Msg, 2)}
	var updating atomic.Bool

	t.Run("on the update goroutine", func(t *testing.T) {
		updating.Store(true)
		defer updating.Store(false)
		s := hostSender{p: inner, updating: &updating}
		done := make(chan struct{})
		go func() { s.Send("on-loop"); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Send blocked on the Update goroutine (deadlock at quit/close)")
		}
		close(inner.release)
		select {
		case msg := <-inner.got:
			if msg != "on-loop" {
				t.Fatalf("delivered %v", msg)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the asynchronous post never reached the program")
		}
	})

	t.Run("after the loop stopped", func(t *testing.T) {
		var stopped atomic.Bool
		stopped.Store(true)
		inner := &blockingSender{release: make(chan struct{}), got: make(chan tea.Msg, 1)}
		s := hostSender{p: inner, updating: &updating, stopped: &stopped}
		done := make(chan struct{})
		go func() { s.Send("after-quit"); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Send blocked after the program stopped (quit sweep would hang)")
		}
		select {
		case msg := <-inner.got:
			t.Fatalf("a stopped program received %v", msg)
		default:
		}
	})

	t.Run("off the update goroutine stays inline", func(t *testing.T) {
		inner := &blockingSender{release: make(chan struct{}), got: make(chan tea.Msg, 1)}
		s := hostSender{p: inner, updating: &updating}
		done := make(chan struct{})
		go func() { s.Send("off-loop"); close(done) }()
		select {
		case <-done:
			t.Fatal("Send returned while the program was still blocked")
		case <-time.After(50 * time.Millisecond):
		}
		close(inner.release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("inline Send never completed")
		}
	})
}

// hhExecutor builds a throwaway executor root for a synthetic child agent.
func hhExecutor(t *testing.T) *agent.Executor {
	t.Helper()
	return agent.NewExecutor(t.TempDir())
}

// TestHostSendFailsAfterTheLoopExits pins the liveness guarantee behind the
// control tools: once the event loop is gone (or was never running), a host post
// must FAIL instead of parking the caller. A send_message or RunTurn waiting on
// an ack nobody will ever send would hang the agent's tool call forever.
func TestHostSendFailsAfterTheLoopExits(t *testing.T) {
	inner := &blockingSender{release: make(chan struct{}), got: make(chan tea.Msg, 1)}
	var stopped atomic.Bool

	t.Run("no sender installed", func(t *testing.T) {
		sp := newTUISubagentSpawner(&config.Config{OpenAIModel: "default-model"}, nil)
		if err := sp.send("x"); err == nil || !strings.Contains(err.Error(), "not running") {
			t.Fatalf("send without a program = %v, want a not-running error", err)
		}
	})

	t.Run("loop exited", func(t *testing.T) {
		sp := newTUISubagentSpawner(&config.Config{OpenAIModel: "default-model"}, nil)
		sp.setSender(func() programSender { return inner }, &stopped)
		stopped.Store(true)
		if err := sp.send("x"); err == nil || !strings.Contains(err.Error(), "has exited") {
			t.Fatalf("send after the loop exited = %v, want an exited error", err)
		}
	})

	t.Run("control tools report instead of blocking", func(t *testing.T) {
		sp := newTUISubagentSpawner(&config.Config{OpenAIModel: "default-model"}, nil)
		sp.setSender(func() programSender { return inner }, &stopped)
		stopped.Store(true)
		prov := llmtest.NewMockProvider()
		prov.Model = "child-model"
		childAgent := agent.NewAgent(prov, hhExecutor(t), contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
		childAgent.SetSessionID("child-session")
		handle := newChildHandle(childAgent)
		if ok, full := sp.DeliverChildMessage(&subagents.Child[*childHandle]{Handle: handle}, "hi"); ok || full {
			t.Fatalf("DeliverChildMessage = %v/%v, want a refusal", ok, full)
		}
		done := make(chan struct{})
		go func() {
			_, _ = sp.RunTurn(context.Background(), &subagents.Child[*childHandle]{Handle: handle}, "job")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunTurn blocked after the loop exited")
		}
	})
}

// TestNoticeOverflowNeverEvictsATargetedDelivery pins the delivery priority the
// web host enforces: a burst of notices evicts the OLDEST NOTICE, never a queued
// send_message whose reply the parent is waiting on — and when every slot holds
// a targeted delivery, the incoming notice is the one dropped.
func TestNoticeOverflowNeverEvictsATargetedDelivery(t *testing.T) {
	hh := newChildHarness(t, "reply")
	child, handle, _ := hh.newChild(t, hh.m.agent, "reply", "subagent: job")
	slot := hh.m.lives.AddChild(child, "subagent: job", handle)
	handle.setSlot(slot)
	slot.streaming = true // keep the queue from draining

	// One targeted delivery, then fill every remaining slot with notices.
	ack := make(chan childMessageAck, 1)
	if _, _ = hh.m.handleChildMessageMsg(childMessageMsg{handle: handle, text: "please elaborate", ack: ack}); !(<-ack).ok {
		t.Fatal("targeted delivery refused")
	}
	for i := 0; i < maxPendingDeliveries-1; i++ {
		_, _ = hh.m.handleMsg(deliveryRequestMsg{sid: child.SessionID, text: "notice"})
	}
	targetedBefore := countQueuedChildItems(slot.steerQueue, handle)
	if targetedBefore != 1 {
		t.Fatalf("targeted queued = %d, want 1", targetedBefore)
	}

	// One more notice at a full queue: a notice is evicted, the reply survives.
	_, _ = hh.m.handleMsg(deliveryRequestMsg{sid: child.SessionID, text: "late notice"})
	if got := countQueuedChildItems(slot.steerQueue, handle); got != 1 {
		t.Fatalf("targeted queued after overflow = %d, want the reply preserved", got)
	}

	// Now make every system slot targeted: the incoming notice must be dropped.
	for _, it := range append([]steerItem(nil), slot.steerQueue...) {
		if it.child == nil {
			slot.removeSteerItem(it.id)
		}
	}
	for len(slot.steerQueue) < maxPendingDeliveries {
		slot.steerQueue = append(slot.steerQueue, steerItem{
			id: newSteerItemID(), text: "targeted", system: true, child: handle,
		})
	}
	dropsBefore := hh.m.deliveryDrops
	_, _ = hh.m.handleMsg(deliveryRequestMsg{sid: child.SessionID, text: "notice at a full targeted queue"})
	if got := len(slot.steerQueue); got != maxPendingDeliveries {
		t.Fatalf("queue length = %d, want %d (no targeted delivery evicted)", got, maxPendingDeliveries)
	}
	if hh.m.deliveryDrops != dropsBefore+1 {
		t.Fatal("the dropped notice was not reported")
	}
}
