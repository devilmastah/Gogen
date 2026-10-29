package tui

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
	"gogen/internal/steerq"
	"gogen/internal/subagents"
)

// TUI host half of the shared continuable-subagent lifecycle
// (internal/subagents). Children are LIVE SESSIONS: each one gets a slot in
// the same registry that hosts the user's sessions, so it streams through the
// normal adapter, keeps its own replay buffer and round snapshot (a mid-turn
// join renders the in-flight reply), and can be focused as a pane. The
// lifecycle owns admission, caps, retention, release and the control tools;
// this file supplies the tea plumbing.
//
// Threading contract: a child's turn is STARTED by the Update thread (slots
// and their turn state are Update-thread-only, like every other live session)
// but the PARENT's tool goroutine must block for the child's final output, so
// the two halves meet on the handle:
//   - the tool goroutine sends childSpawnMsg and waits on the handle's ready
//     then result channels;
//   - Update admits the slot and starts the turn via startTurnOnWithResult,
//     whose onResult callback (running on the turn's own goroutine) resolves
//     the handle — no slot field is ever touched off the Update thread.

// childTurnResult is one child turn's exact output: the authoritative value
// for RunTurn and for a send_message reply (never a transcript heuristic).
type childTurnResult struct {
	out string
	err error
}

// childHandle is the TUI host's per-child object: the pane's identity plus the
// few handles the lifecycle reads from off the Update thread. Everything the
// Update thread writes into it is guarded by mu, so Busy/InterruptAgent see a
// consistent snapshot from the parent's tool goroutine.
type childHandle struct {
	agent   *agent.Agent
	session string // child session id (also the slot's routing id)

	// ready/result hand the spawn's admission and the main run's output back
	// to the parent's tool goroutine. Both are buffered(1) and written once.
	ready  chan error
	result chan childTurnResult

	mu         sync.Mutex
	slot       *liveSession
	child      *subagents.Child[*childHandle] // filled by ArmReplyCapture
	turnActive bool
	turnCancel context.CancelFunc
	queueDepth int // targeted deliveries queued for the child
	released   bool
}

// newChildHandle builds the host object for one child session.
func newChildHandle(a *agent.Agent) *childHandle {
	return &childHandle{
		agent:   a,
		session: a.SessionID,
		ready:   make(chan error, 1),
		result:  make(chan childTurnResult, 1),
	}
}

// setSlot is called on the Update thread when the child's live slot is
// admitted.
func (h *childHandle) setSlot(s *liveSession) {
	h.mu.Lock()
	h.slot = s
	h.mu.Unlock()
}

// childOf returns the lifecycle child bound to this handle (nil before
// ArmReplyCapture). Immutable after the spawn, so a lock-free read is safe on
// the Update thread's drain path.
func (h *childHandle) childOf() *subagents.Child[*childHandle] {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.child
}

// senderFor returns the program sender, or nil when the UI is not running.
func (sp *tuiSubagentSpawner) senderFor() programSender {
	sp.senderMu.Lock()
	get := sp.sender
	sp.senderMu.Unlock()
	if get == nil {
		return nil
	}
	return get()
}

// send posts one message to the Update thread. It FAILS when no program is
// running (headless host, spawn before Run) or the event loop has already
// exited: every caller then reports an error instead of parking forever on a
// channel (or an ack) nobody will ever serve — which is what a waiting
// send_message / RunTurn would otherwise do.
func (sp *tuiSubagentSpawner) send(msg tea.Msg) error {
	if sp == nil {
		return fmt.Errorf("subagent: no spawner")
	}
	if sp.stopped != nil && sp.stopped.Load() {
		return fmt.Errorf("subagent: the terminal UI has exited")
	}
	p := sp.senderFor()
	if p == nil {
		return fmt.Errorf("subagent: the terminal UI is not running")
	}
	p.Send(msg)
	return nil
}

// hostSender posts host messages to the event loop the way the caller needs:
// INLINE from a turn/tool goroutine (the normal, cheap path), but
// asynchronously when the caller is the Update goroutine itself. The quit sweep
// and the sidebar's close action run on the loop, and tea.Program.Send is an
// unbuffered handoff to that very loop — an inline send there deadlocks the
// program. Every receiver is idempotent (a spawn for a released child is
// refused, a release for a missing slot no-ops, a delivery for a closed session
// is dropped), so an asynchronous post can safely land after its producer.
type hostSender struct {
	p        programSender
	updating *atomic.Bool
	// stopped latches once the event loop has exited: Program.Send writes to a
	// channel nobody reads anymore, so a post must be dropped instead of
	// blocking the teardown sweep forever.
	stopped *atomic.Bool
}

// Send implements programSender.
func (h hostSender) Send(msg tea.Msg) {
	if h.p == nil {
		return
	}
	if h.stopped != nil && h.stopped.Load() {
		return
	}
	if h.updating != nil && h.updating.Load() {
		go h.p.Send(msg)
		return
	}
	h.p.Send(msg)
}

// setSender installs the program-sender accessor and the "loop exited" latch.
// Called once by the runner before the event loop starts (the program pointer
// is written there), never during a turn. stopped may be nil (tests whose
// sender never goes away).
func (sp *tuiSubagentSpawner) setSender(sender func() programSender, stopped *atomic.Bool) {
	if sp == nil {
		return
	}
	sp.senderMu.Lock()
	sp.sender = sender
	sp.stopped = stopped
	sp.senderMu.Unlock()
}

// slotOf returns the child's live slot (nil before admission).
func (h *childHandle) slotOf() *liveSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.slot
}

// setTurn records the child's in-flight turn (Update thread) so an interrupt
// from the parent's tool goroutine can cancel exactly that turn.
func (h *childHandle) setTurn(cancel context.CancelFunc) {
	h.mu.Lock()
	h.turnActive = cancel != nil
	h.turnCancel = cancel
	h.mu.Unlock()
}

// turnDone clears the in-flight turn state.
func (h *childHandle) turnDone() {
	h.setTurn(nil)
}

// TurnActive reports whether a child turn is in flight.
func (h *childHandle) TurnActive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.turnActive
}

// cancelTurn cancels the child's in-flight turn (no-op when idle).
func (h *childHandle) cancelTurn() {
	h.mu.Lock()
	cancel := h.turnCancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// setQueueDepth mirrors the child's queued targeted deliveries (Update thread)
// so Busy can see them off-thread.
func (h *childHandle) setQueueDepth(n int) {
	h.mu.Lock()
	h.queueDepth = n
	h.mu.Unlock()
}

// busy reports lifecycle work a release would cut short: an active turn or a
// queued delivery. The armed exact-reply capture is added by the host's Busy
// (Child.PendingReply), which is where the released/reaped decision lives.
func (h *childHandle) busy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return false
	}
	return h.turnActive || h.queueDepth > 0
}

// isReleased reports whether the teardown has latched.
func (h *childHandle) isReleased() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.released
}

// markReleased latches the teardown so a late signal cannot resurrect work.
func (h *childHandle) markReleased() {
	h.mu.Lock()
	h.released = true
	h.mu.Unlock()
}

// --- messages (any goroutine → Update thread) ---

// childSpawnMsg asks the Update thread to admit a background child: create its
// live slot and start its turn. ready reports admission (buffered, written
// once); the turn's output is resolved through the handle.
type childSpawnMsg struct {
	handle *childHandle
	label  string
	text   string // wrapped job prompt
	ready  chan error
}

// childAdoptMsg asks the Update thread to give a REVIVED child — one loaded
// from the store because a send_message found it no longer live — a live slot,
// WITHOUT starting a turn: the delivery that follows starts one. ack reports
// admission.
type childAdoptMsg struct {
	handle *childHandle
	label  string
	ack    chan error
}

// childMessageMsg delivers a send_message into a child's queue (queued when the
// child is mid-turn). ack reports (queued, full).
type childMessageMsg struct {
	handle *childHandle
	text   string
	ack    chan childMessageAck
}

// childMessageAck reports the outcome of one send_message delivery.
type childMessageAck struct {
	ok   bool
	full bool
}

// childReleaseMsg tears a released child's slot down (Update thread).
type childReleaseMsg struct {
	handle          *childHandle
	cancelledMidRun bool
}

// --- subagents.Host implementation ---

// NewChild implements subagents.Host: build the child agent (workspace factory
// or the single-session fallback) and apply the shared model/thinking cascade.
// The live slot is created later, by the Update thread, when RunTurn starts.
func (sp *tuiSubagentSpawner) NewChild(ctx context.Context, parent *agent.Agent, job, model string, depth int) (subagents.NewChild[*childHandle], error) {
	child := sp.newChildAgent(parent)
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	// Model cascade, shared with the web spawner (ResolveSubagentModel /
	// ApplySubagentModel): explicit tool argument > configured subagent model >
	// the parent's live model (unless it is the default the child is already
	// seeded with). The explicit argument is a hard requirement.
	m, src := agent.ResolveSubagentModel(model, sp.cfg.SubagentModel, parent.CurrentModelRef().String(), sp.cfg.OpenAIModel)
	if err := agent.ApplySubagentModel(ctx, child, m, src); err != nil {
		return subagents.NewChild[*childHandle]{}, err
	}
	agent.ApplySubagentThinkingLevel(child, parent, sp.cfg.SubagentThinkingLevel)
	label := agent.SubagentLabel(job)
	_, _ = child.RenameSession(label)
	return subagents.NewChild[*childHandle]{Handle: newChildHandle(child), Agent: child, Label: label}, nil
}

// RunTurn implements subagents.Host: hand the turn to the Update thread (which
// owns every slot) and block for its exact output. Cancellation through ctx
// (interrupt_agent) returns immediately; the turn itself unwinds on its own
// because CancelTurn cancels the same context.
func (sp *tuiSubagentSpawner) RunTurn(ctx context.Context, c *subagents.Child[*childHandle], prompt string) (string, error) {
	h := c.Handle
	if err := sp.send(childSpawnMsg{handle: h, label: c.Label, text: prompt, ready: h.ready}); err != nil {
		return "", err
	}
	select {
	case err := <-h.ready:
		if err != nil {
			return "", err
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case res := <-h.result:
		return res.out, res.err
	case <-ctx.Done():
		// The interrupted turn keeps unwinding in the background; the turn's
		// own sink clears the mirror when StreamProcessInput actually returns.
		// Clearing it here would drop the cancel handle while the turn still
		// runs, so a release could no longer stop it.
		return "", ctx.Err()
	}
}

// CancelTurn implements subagents.Host: cancel the child's in-flight turn; the
// slot (and the session) stay alive and continuable.
func (sp *tuiSubagentSpawner) CancelTurn(c *subagents.Child[*childHandle]) {
	c.Handle.cancelTurn()
}

// DeliverChildMessage implements subagents.Host: queue the message on the
// child's slot through the Update thread. ok=false means nothing was queued;
// full=true means every queue slot holds a targeted delivery, so the caller
// reports an actionable error instead of dropping a message whose reply the
// parent waits on.
func (sp *tuiSubagentSpawner) DeliverChildMessage(c *subagents.Child[*childHandle], text string) (ok, full bool) {
	ack := make(chan childMessageAck, 1)
	if err := sp.send(childMessageMsg{handle: c.Handle, text: text, ack: ack}); err != nil {
		return false, false
	}
	res := <-ack
	return res.ok, res.full
}

// ArmReplyCapture implements subagents.Host: remember the lifecycle child on
// the handle. The reply itself is captured per targeted delivery in
// drainSessionQueue (the TUI's delivery-start hook), exactly like the web's
// deliverStartHook.
func (sp *tuiSubagentSpawner) ArmReplyCapture(c *subagents.Child[*childHandle]) {
	h := c.Handle
	h.mu.Lock()
	h.child = c
	h.mu.Unlock()
}

// Release implements subagents.Host: tear the child's slot down on the Update
// thread. The cancelled outcome is recorded first (a released running child
// must read failed, not done), then the slot is closed and the session
// flushed. The lifecycle has already removed the child from its registry.
func (sp *tuiSubagentSpawner) Release(c *subagents.Child[*childHandle], cancelledMidRun bool) {
	h := c.Handle
	h.markReleased()
	if cancelledMidRun {
		c.MarkInterrupted()
		// Same discipline as the web host: stamp the cancelled outcome BEFORE
		// the final flush, or the sidebar renders the child as done.
		c.Agent.SetSubagentOutcome("failed", "cancelled")
		c.Agent.FlushSession()
	}
	// Cancel first so the turn cannot keep writing while the slot leaves the
	// registry (the drain/finalize paths tolerate a terminal for a removed
	// slot: ByID misses and they no-op).
	h.cancelTurn()
	// Releasing a child is an explicit teardown of its session: its OWN
	// children (grandchildren) are cancelled and released in turn, the same
	// cascade the web host fires from its eviction hook.
	sp.CancelAll(c.ID)
	_ = sp.send(childReleaseMsg{handle: h, cancelledMidRun: cancelledMidRun})
}

// Deliver implements subagents.Host: queue one child event into the parent
// conversation. It names the parent by SESSION id; the Update thread resolves
// the slot, so a focus switch between the producer and the handler cannot
// re-attribute the notice.
//
// The event is injected as a one-way notice whatever its kind: the TUI keeps
// no offline stash (a delivery for a slot that is not live is dropped), and a
// child's event never arms reply capture — that is armed per targeted
// send_message delivery, on the child's own queue.
func (sp *tuiSubagentSpawner) Deliver(event subagents.Event) {
	_ = sp.send(deliveryRequestMsg{sid: event.ParentID, text: event.ParentMessage()})
}

// Started implements subagents.Host. The child's sidebar row comes from its
// live slot when RunTurn admits it, so there is nothing to announce here.
func (sp *tuiSubagentSpawner) Started(*subagents.Child[*childHandle], string) {}

// Finished implements subagents.Host: append the finished child to the
// /subagents list (live status for running children comes from their slot).
func (sp *tuiSubagentSpawner) Finished(c *subagents.Child[*childHandle], report string, err error) {
	sp.addRecord(subagentRecord{label: c.Label, report: agent.TruncateSubagentReport(report), err: err})
}

// ReviveChild implements subagents.Host: reopen one of the caller's saved
// nested sessions as a live slot so a send_message can reach it again — the
// live slot set is process-local, the session is not, so without this a
// subagent conversation ended for good at its first release or restart.
//
// The agent is loaded here, on the caller's goroutine (the spawn path builds
// child agents off the Update thread too); only the SLOT is admitted on the
// Update thread, through childAdoptMsg, because slots are Update-thread state.
func (sp *tuiSubagentSpawner) ReviveChild(caller *agent.Agent, childID string) (subagents.NewChild[*childHandle], bool, error) {
	if sp == nil || sp.ws == nil || sp.ws.Store == nil || caller == nil || childID == "" {
		return subagents.NewChild[*childHandle]{}, false, nil
	}
	// Ownership from the store index first: it never loads a session the
	// caller does not own (the same boundary PersistedResult uses).
	info := sp.ws.Store.Info(sp.ws.GetWorkingDir(), childID)
	if info == nil || info.ParentID != caller.SessionID {
		return subagents.NewChild[*childHandle]{}, false, nil
	}
	snap, err := sp.ws.Store.LoadInWorkingDir(sp.ws.GetWorkingDir(), childID)
	if err != nil {
		return subagents.NewChild[*childHandle]{}, false, err
	}
	a := sp.ws.NewSessionAgent(&snap, childID)
	h := newChildHandle(a)
	label := info.Label
	if l := a.SessionLabelSnapshot(); l != "" {
		label = l
	}
	ack := make(chan error, 1)
	if err := sp.send(childAdoptMsg{handle: h, label: label, ack: ack}); err != nil {
		return subagents.NewChild[*childHandle]{}, false, err
	}
	if err := <-ack; err != nil {
		return subagents.NewChild[*childHandle]{}, false, err
	}
	return subagents.NewChild[*childHandle]{Handle: h, Agent: a, Label: label}, true, nil
}

// PersistedChildren implements subagents.Host: the caller's saved nested
// (subagent) sessions, newest first, for list_agents. The ParentID filter is
// the same authorization boundary PersistedResult applies — a model must never
// see another parent's children. Best effort: a store error yields no saved
// section rather than failing the tool call.
func (sp *tuiSubagentSpawner) PersistedChildren(caller *agent.Agent) []subagents.PersistedChild {
	if sp.ws == nil || sp.ws.Store == nil || caller == nil {
		return nil
	}
	list, err := sp.ws.Store.List(sp.ws.GetWorkingDir())
	if err != nil {
		return nil
	}
	var out []subagents.PersistedChild
	for _, info := range list {
		if info.ParentID == "" || info.ParentID != caller.SessionID {
			continue
		}
		label := info.Label
		if label == "" {
			label = info.ID
		}
		out = append(out, subagents.PersistedChild{
			ID:       info.ID,
			Label:    label,
			Status:   info.SubagentStatus,
			Messages: info.MessageCount,
			Updated:  info.UpdatedTime(),
		})
	}
	return out
}

// PersistedResult implements subagents.Host: resolve a released child from the
// session store. The ParentID check is the authorization boundary — a model
// must not retrieve a nested session belonging to another parent.
func (sp *tuiSubagentSpawner) PersistedResult(caller *agent.Agent, childID string) (string, bool) {
	if sp.ws == nil || sp.ws.Store == nil || caller == nil || childID == "" {
		return "", false
	}
	info := sp.ws.Store.Info(sp.ws.GetWorkingDir(), childID)
	if info == nil || info.ParentID != caller.SessionID {
		return "", false
	}
	switch info.SubagentStatus {
	case "success":
		return fmt.Sprintf("Subagent %s finished.\n\n%s", childID, info.SubagentSummary), true
	case "failed":
		return fmt.Sprintf("Subagent %s failed.\n\n%s", childID, info.SubagentSummary), true
	default:
		return fmt.Sprintf("Subagent %s has no persisted terminal result yet; it may have been interrupted before completion.", childID), true
	}
}

// TurnActive implements subagents.Host: a follow-up delivery turn reads as
// running.
func (sp *tuiSubagentSpawner) TurnActive(c *subagents.Child[*childHandle]) bool {
	return c.Handle.TurnActive()
}

// Busy implements subagents.Host: an active turn, a queued targeted delivery,
// or an armed exact-reply capture. A released child can no longer run that
// work, so it reports quiet (an armed capture must not pin the retention timer
// forever).
func (sp *tuiSubagentSpawner) Busy(c *subagents.Child[*childHandle]) bool {
	if c.Released() {
		return false
	}
	if c.Handle.busy() {
		return true
	}
	return c.PendingReply()
}

// Policy implements subagents.Host. The TUI's caps match the configured limits;
// retention/finished caps use the shared defaults, and the finished-children
// list is bounded by maxRecordedSubagents.
func (sp *tuiSubagentSpawner) Policy() subagents.Policy {
	limit := 0
	if sp.cfg != nil {
		limit = sp.cfg.SubagentLimit()
	}
	return subagents.Policy{
		MaxFinished: maxRecordedSubagents,
		Concurrent:  func(string) int { return limit },
	}
}

// --- Update-thread handlers ---

// handleChildSpawnMsg admits a background child: it creates the child's live
// slot (child slots carry their own session id as the routing id, so every
// stream event routes without a new id space) and starts its turn.
func (m *Model) handleChildSpawnMsg(msg childSpawnMsg) (tea.Model, tea.Cmd) {
	fail := func(err error) (tea.Model, tea.Cmd) {
		msg.ready <- err
		return m, nil
	}
	if m.lives == nil || msg.handle == nil || msg.handle.agent == nil {
		return fail(fmt.Errorf("subagent: no live session registry"))
	}
	if msg.handle.slotOf() != nil {
		return fail(fmt.Errorf("subagent: child session %s is already live", msg.handle.session))
	}
	// The host posts asynchronously, so a release can overtake its spawn (a cap
	// eviction or the parent's teardown racing the admission). Admitting a
	// released child would leave a slot nothing ever removes.
	if child := msg.handle.childOf(); child != nil && child.Released() {
		// Report it as eviction, not as a run failure: the lifecycle then marks
		// the child interrupted and notifies nobody, instead of recording a
		// failed outcome for a child the host already tore down.
		return fail(subagents.ErrEvicted)
	}
	s := m.lives.AddChild(msg.handle.agent, msg.label, msg.handle)
	msg.handle.setSlot(s)
	// The child's turn result resolves the handle (the parent's tool goroutine
	// is parked in RunTurn) and, for a send_message delivery, the exact reply
	// capture — see childTurnResultSink.
	cmd := m.startTurnOnWithResult(s, msg.text, childTurnResultSink(m.spawner, msg.handle, true))
	msg.ready <- nil
	return m, cmd
}

// handleChildAdoptMsg admits the slot a revived child needs. A child that is
// ALREADY live as a session (the user has it open as a pane) is adopted rather
// than duplicated: two slots for one session id would both persist to the same
// file, and the older one's pending delta would be dropped by the newer
// snapshot — the same rule spawnSavedSession states for resume.
func (m *Model) handleChildAdoptMsg(msg childAdoptMsg) (tea.Model, tea.Cmd) {
	ack := msg.ack
	fail := func(err error) (tea.Model, tea.Cmd) {
		if ack != nil {
			ack <- err
		}
		return m, nil
	}
	if m.lives == nil || msg.handle == nil || msg.handle.agent == nil {
		return fail(fmt.Errorf("subagent: no live session registry"))
	}
	// A release can overtake the revival (parent teardown, cap eviction):
	// admitting a released child would leave a slot nothing ever removes.
	if child := msg.handle.childOf(); child != nil && child.Released() {
		return fail(subagents.ErrEvicted)
	}
	if i := m.lives.indexBySessionID(msg.handle.session); i >= 0 {
		s := m.lives.sessions[i]
		s.agent = msg.handle.agent
		s.child = msg.handle
		msg.handle.setSlot(s)
		if ack != nil {
			ack <- nil
		}
		return m, nil
	}
	s := m.lives.AddChild(msg.handle.agent, msg.label, msg.handle)
	msg.handle.setSlot(s)
	if ack != nil {
		ack <- nil
	}
	return m, nil
}

// childTurnResultSink builds the onResult callback for one child turn: it
// clears the handle's turn state and either resolves the main run
// (deliverResult) or delivers the exact send_message reply to the parent.
func childTurnResultSink(sp *tuiSubagentSpawner, h *childHandle, deliverResult bool) func(string, error) {
	return func(out string, err error) {
		h.turnDone()
		if deliverResult {
			select {
			case h.result <- childTurnResult{out: out, err: err}:
			default:
				// A previous result is already pending: the superseded turn
				// (cancel + respawn) must not block its goroutine.
			}
			return
		}
		h.mu.Lock()
		child := h.child
		h.mu.Unlock()
		if child == nil {
			return
		}
		child.OnTurnResult(sp, out, err)
	}
}

// handleChildMessageMsg queues one send_message delivery on the child's slot
// and drains it when the child is idle.
func (m *Model) handleChildMessageMsg(msg childMessageMsg) (tea.Model, tea.Cmd) {
	h := msg.handle
	s := h.slotOf()
	if s == nil {
		msg.ack <- childMessageAck{}
		return m, nil
	}
	// A release can be in flight (async post): accepting the message would run
	// a turn whose reply the lifecycle already decided to drop.
	if h.isReleased() {
		msg.ack <- childMessageAck{}
		return m, nil
	}
	// Targeted deliveries are never silently dropped: refuse when the system
	// queue is full, exactly like the web host's enqueueSystem.
	targeted := countQueuedChildItems(s.queue(), h)
	// A Reply is never the eviction victim (the parent is waiting on it), so
	// the shared queue refuses only when the system cap is already full of
	// replies.
	if outcome, evicted := s.queue().Append(steerq.Item{
		ID: steerq.NewID(), Text: msg.text, Kind: steerq.Reply, Tag: h,
	}); outcome != steerq.Accepted {
		msg.ack <- childMessageAck{ok: false, full: true}
		return m, nil
	} else if evicted != nil {
		m.deliveryDrops++
	}
	h.setQueueDepth(targeted + 1)
	msg.ack <- childMessageAck{ok: true}
	if m.lives.Active() == s {
		return m, m.drainDeliveries()
	}
	return m, m.drainSessionQueue(s)
}

// handleChildReleaseMsg tears a released child's slot down: cancel its turn,
// flush the session, and drop the slot (the saved session stays on disk).
func (m *Model) handleChildReleaseMsg(msg childReleaseMsg) (tea.Model, tea.Cmd) {
	h := msg.handle
	s := h.slotOf()
	if s == nil || m.lives == nil {
		return m, nil
	}
	h.cancelTurn()
	if s.agent != nil {
		// FlushPending, not FlushSession: a clean session must not be
		// re-stamped (it would jump to the top of the saved list). A
		// cancelled child already forced its outcome write.
		s.agent.FlushPending()
	}
	idx := m.lives.indexOf(s)
	if idx < 0 {
		return m, nil
	}
	var cmd tea.Cmd
	if idx == m.lives.active {
		// A released child that was focused hands focus back to the root
		// slot: its pane no longer exists.
		cmd = m.switchToLive(0)
	}
	m.lives.remove(idx)
	h.mu.Lock()
	h.slot = nil
	h.mu.Unlock()
	m.sidebarCursor = max(0, min(m.sidebarCursor, len(m.buildSidebarRows())-1))
	return m, tea.Batch(cmd, m.requestSavedSessions())
}
