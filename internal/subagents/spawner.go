package subagents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
)

// Spawner owns one host's continuable children: admission, the caps, the
// retention/release policy, and the control-tool implementations. It
// implements the body of agent.ContinuableSubagentSpawner; a host embeds it
// and keeps only its own foreground (Spawn/Fork) wiring.
//
// Lifecycle:
//   - SpawnBackground builds the child through the host, registers it, then
//     runs the turn in a goroutine. The parent tool call returns the child id
//     immediately and the child keeps running after it.
//   - A refused spawn (cap exceeded) is DISCARDED, not released: it never
//     started, so it must leave no session artifact (a release would stamp a
//     failed outcome and write an empty session file — a phantom failed child
//     row for a child that never existed). The refusal error is its only
//     trace.
//   - On natural completion a compact terminal event is delivered to the
//     parent conversation. The authoritative bounded report is pulled with
//     get_agent_result and stays persisted after live release.
//   - Finished children stay registered for a retention window, then are
//     released. A child with work in flight (active turn, queued delivery,
//     armed reply) is never released — the timer re-arms instead.
//   - An interrupted main run is marked idle and stays continuable: no
//     completion event, no error.
//   - When the parent session goes away the host cancels and releases all of
//     its children through CancelAll (a child whose parent is gone cannot
//     deliver anyway).
type Spawner[H any] struct {
	host     Host[H]
	children *registry[H]
	// wg tracks in-flight spawn goroutines. A goroutine's post-turn host work
	// can outlive "child gone from the registry", so tests wait on this
	// before tearing down a host.
	wg sync.WaitGroup
}

// New returns a spawner driving host with an empty child registry. The host
// must be non-nil.
func New[H any](host Host[H]) *Spawner[H] {
	return &Spawner[H]{host: host, children: newRegistry[H]()}
}

// Wait blocks until every in-flight spawn goroutine has returned. Hosts use
// it at shutdown; tests use it before removing temp dirs.
func (sp *Spawner[H]) Wait() {
	if sp == nil {
		return
	}
	sp.wg.Wait()
}

// Get resolves a live child by id (nil when unknown or released).
func (sp *Spawner[H]) Get(id string) *Child[H] {
	if sp == nil {
		return nil
	}
	return sp.children.get(id)
}

// ChildrenOf returns a parent's live children, oldest first (registration
// order): the order list_agents and the host's UI render.
func (sp *Spawner[H]) ChildrenOf(parentID string) []*Child[H] {
	if sp == nil {
		return nil
	}
	return sp.children.childrenOf(parentID)
}

// HasLiveChildren reports whether parentID still owns live child work (the
// host's "not idle, do not evict" check).
func (sp *Spawner[H]) HasLiveChildren(parentID string) bool {
	if sp == nil {
		return false
	}
	return sp.children.hasLiveChildren(parentID)
}

// AcquireForeground reserves a concurrency slot for a FOREGROUND child
// (Spawn/Fork) for the whole turn. The check and the reservation are one
// critical section, so concurrent spawns cannot slip past either cap.
// hitGuard reports a refusal by the internal non-finished guard rather than
// the user-facing active limit.
func (sp *Spawner[H]) AcquireForeground(parentID string) (ok, hitGuard bool) {
	pol := sp.host.Policy()
	return sp.children.acquireForeground(parentID, sp.host, pol.activeLimit(parentID), pol.guardLimit())
}

// ReleaseForeground returns a slot reserved by AcquireForeground.
func (sp *Spawner[H]) ReleaseForeground(parentID string) {
	sp.children.releaseForeground(parentID)
}

// SpawnCapError builds the refusal error for a FOREGROUND spawn (Spawn/Fork)
// rejected by either cap. hitGuard reports the refusal came from the internal
// non-finished guard rather than the user-facing active limit.
func (sp *Spawner[H]) SpawnCapError(parentID string, hitGuard bool) error {
	pol := sp.host.Policy()
	return spawnCapError(pol.activeLimit(parentID), pol.guardLimit(), hitGuard)
}

// CancelAll cancels and releases every live child of parentID. Wired to the
// host's parent-eviction hook so a parent leaving the host (close, cap
// eviction, delete, shutdown) takes its children with it.
func (sp *Spawner[H]) CancelAll(parentID string) {
	if sp == nil || sp.children == nil {
		return
	}
	for _, c := range sp.children.allChildren() {
		if c.ParentID != parentID {
			continue
		}
		if cancel := c.RunCancel(); cancel != nil {
			cancel()
		}
		sp.host.CancelTurn(c)
		sp.ReleaseChild(c)
	}
}

// CancelAllChildren cancels and releases every live child of this host — the
// shutdown path (the process is leaving, so no child may outlive it). A parent
// going away uses CancelAll(parentID) instead.
func (sp *Spawner[H]) CancelAllChildren() {
	if sp == nil || sp.children == nil {
		return
	}
	for _, c := range sp.children.allChildren() {
		if cancel := c.RunCancel(); cancel != nil {
			cancel()
		}
		sp.host.CancelTurn(c)
		sp.ReleaseChild(c)
	}
}

// SpawnBackground starts a nested session that keeps running after this call
// returns. Implements agent.ContinuableSubagentSpawner.
func (sp *Spawner[H]) SpawnBackground(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	if parent == nil {
		return "", fmt.Errorf("subagent: parent agent is nil")
	}
	nc, err := sp.host.NewChild(ctx, parent, job, model, depth)
	if err != nil {
		return "", err
	}
	if nc.Agent == nil {
		return "", fmt.Errorf("subagent: host returned no child agent")
	}
	// Own the main run from the moment the child record exists: a cancel
	// requested before the goroutine is scheduled is observed when the run
	// begins, instead of being lost because no stream handle is registered
	// yet.
	runCtx, runCancel := context.WithCancel(context.Background())
	child := &Child[H]{
		ID:        nc.Agent.SessionID,
		ParentID:  parent.SessionID,
		Label:     nc.Label,
		Depth:     depth + 1,
		CreatedAt: time.Now(),
		Agent:     nc.Agent,
		Handle:    nc.Handle,
		done:      make(chan struct{}),
		status:    "running",
	}
	child.SetRunCancel(runCancel)
	sp.children.register(child)
	// Reply capture is armed once per background child; the host's delivery
	// hooks arm it per targeted delivery (Child.BeginReply).
	sp.host.ArmReplyCapture(child)
	// Enforce the per-parent finished cap at spawn time; fast jobs that
	// finish after their siblings spawned re-enforce it at completion.
	sp.enforceCap(parent.SessionID)

	// Spawn caps are checked AFTER registration so concurrent spawns cannot
	// slip past them: the just-registered child counts itself in both, so
	// strictly-greater means we are one over.
	pol := sp.host.Policy()
	activeOver, guardOver := sp.children.spawnCapExceeded(parent.SessionID, sp.host, pol.activeLimit(parent.SessionID), pol.guardLimit())
	if activeOver || guardOver {
		sp.discardUnstartedChild(child)
		return "", spawnCapError(pol.activeLimit(parent.SessionID), pol.guardLimit(), guardOver)
	}

	// Announce the ADMITTED child (its pane/sidebar row's attach target) —
	// only now, after the caps passed: a refused child is discarded without
	// ever being announced.
	sp.host.Started(child, job)

	sp.wg.Add(1)
	go func() {
		defer sp.wg.Done()
		defer runCancel()
		// The job wrapper applies here, AFTER label derivation: labels and
		// the started event keep the original job the parent wrote; only the
		// child's first message carries the wrapped job.
		report, runErr := sp.host.RunTurn(runCtx, child, agent.FormatSubagentJob(job))
		if errors.Is(runErr, ErrEvicted) {
			// The runtime was released while the turn ran (parent teardown /
			// retention): nothing to notify, nobody left to deliver to.
			child.MarkInterrupted()
			return
		}
		if runErr != nil && errors.Is(runErr, context.Canceled) {
			// Interrupted (interrupt_agent / child-pane cancel): the child
			// stays alive and continuable; no completion notice. Arm the
			// retention timer so an idle interrupted child cannot linger in
			// memory forever.
			child.MarkInterrupted()
			time.AfterFunc(sp.host.Policy().retain(), func() { sp.ReleaseChildIfQuiet(child) })
			return
		}
		child.Finish(report, runErr)
		// Persist the final outcome on the child's snapshot: the events are
		// not replayed after a reload/restart, so the saved session is what
		// get_agent_result and the host's UI fall back to.
		child.Agent.FinishSubagentOutcome(report, runErr)
		// Enforce the finished cap NOW: spawn-time enforcement misses fast
		// jobs that finish after their siblings spawned.
		sp.enforceCap(parent.SessionID)
		sp.host.Finished(child, report, runErr)
		verb := "finished"
		if runErr != nil {
			verb = "failed"
		}
		// Queue a compact, correlated terminal event. The potentially large
		// authoritative report is pulled with get_agent_result; keeping it
		// out of the automatic parent turn prevents completion bursts from
		// flooding the parent's context. The child id makes duplicate or
		// truncated labels resolvable without guessing.
		notice := fmt.Sprintf("[subagent %s] %s (%s). Use get_agent_result(agent_id=%q) for the full result.",
			child.ID, child.Label, verb, child.ID)
		child.DeliverEvent(sp.host, Event{Kind: EventTerminal, Text: notice, Err: runErr})
		// Retention: release the runtime after the window so finished
		// children cannot accumulate in memory.
		time.AfterFunc(sp.host.Policy().retain(), func() { sp.ReleaseChildIfQuiet(child) })
	}()
	return child.ID, nil
}

// enforceCap releases the oldest quiet finished children of parentID beyond
// the per-parent cap.
func (sp *Spawner[H]) enforceCap(parentID string) {
	if sp == nil || sp.children == nil {
		return
	}
	finished := sp.children.finishedChildren(parentID)
	overflow := len(finished) - sp.host.Policy().maxFinished()
	if overflow <= 0 {
		return
	}
	// Oldest-first, but never a child whose exact-reply work is still in
	// flight (an active send_message turn, a delivery queued for the host's
	// worker, or an armed reply result): releasing it would cancel the turn
	// and drop the reply silently — the same trap the retention timer's busy
	// check guards. A spared child is released by its own retention timer
	// once it goes quiet, and the next spawn/finish re-runs this cap, so
	// sparing cannot strand it.
	var victims []*Child[H]
	for _, c := range finished {
		if len(victims) >= overflow {
			break
		}
		if c.Busy(sp.host) {
			continue
		}
		victims = append(victims, c)
	}
	for _, c := range victims {
		sp.ReleaseChild(c)
	}
}

// ReleaseChildIfQuiet runs when a child's retention window elapses. A child
// with lifecycle work in flight is NOT released — the timer re-arms so the
// work completes and the child is released after the next quiet window. Quiet
// children are released. Hosts call it for their own retention scheduling;
// the lifecycle arms the same path on completion and interruption.
func (sp *Spawner[H]) ReleaseChildIfQuiet(c *Child[H]) {
	if c.Busy(sp.host) {
		time.AfterFunc(sp.host.Policy().retain(), func() { sp.ReleaseChildIfQuiet(c) })
		return
	}
	sp.ReleaseChild(c)
}

// ReleaseChild forces a child's teardown (host-initiated close of a child
// pane, or the finished-cap sweep). Idempotent: the registry claims the
// release atomically, so concurrent release requests reach the host exactly
// once.
func (sp *Spawner[H]) ReleaseChild(c *Child[H]) {
	if !sp.children.claimRelease(c) {
		return
	}
	sp.host.Release(c, c.IsRunning())
}

// discardUnstartedChild tears down a child refused BEFORE its turn ever
// started (the spawn-cap refusals). Unlike releaseChild it reports a
// non-cancelled release, so the host stamps no outcome and forces no flush:
// the child never ran, and only a turn's persist ever writes its session
// file — so a refused spawn leaves no record at all.
func (sp *Spawner[H]) discardUnstartedChild(c *Child[H]) {
	if !sp.children.claimRelease(c) {
		return
	}
	sp.host.Release(c, false)
}

// ListAgents lists the caller's live children (id, label, status, depth, and
// latest progress line). Implements agent.ContinuableSubagentSpawner.
func (sp *Spawner[H]) ListAgents(caller *agent.Agent) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("list_agents: caller agent is nil")
	}
	if sp == nil || sp.children == nil {
		return "", fmt.Errorf("list_agents: background subagents are not available in this mode")
	}
	children := sp.children.childrenOf(caller.SessionID)
	if len(children) == 0 {
		return "No live subagents.", nil
	}
	var b strings.Builder
	for _, c := range children {
		fmt.Fprintf(&b, "%s — %s — %s (depth %d)\n", c.ID, c.Label, c.StatusOf(sp.host), c.Depth)
		if line := c.LatestProgress(); line != "" {
			fmt.Fprintf(&b, "  last: %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ownedChild resolves a live child and enforces direct-parent ownership.
// Every result/wait/control operation uses this same guard.
func (sp *Spawner[H]) ownedChild(caller *agent.Agent, agentID, operation string) (*Child[H], error) {
	if caller == nil {
		return nil, fmt.Errorf("%s: caller agent is nil", operation)
	}
	if sp == nil || sp.children == nil {
		return nil, fmt.Errorf("%s: background subagents are not available in this mode", operation)
	}
	child := sp.children.get(agentID)
	if child == nil {
		return nil, fmt.Errorf("%s: subagent %s is not live (use get_agent_result for a released child's persisted result)", operation, agentID)
	}
	if child.ParentID != caller.SessionID {
		return nil, fmt.Errorf("%s: subagent %s is not a child of this session", operation, agentID)
	}
	return child, nil
}

// persistedResult resolves a released child from the host's session store.
func (sp *Spawner[H]) persistedResult(caller *agent.Agent, agentID string) (string, bool) {
	if caller == nil || agentID == "" || sp == nil || sp.host == nil {
		return "", false
	}
	return sp.host.PersistedResult(caller, agentID)
}

// GetAgentResult returns one child's recorded status or terminal result; a
// released child falls back to its persisted outcome. Implements
// agent.ContinuableSubagentSpawner.
func (sp *Spawner[H]) GetAgentResult(caller *agent.Agent, agentID string) (string, error) {
	child, err := sp.ownedChild(caller, agentID, "get_agent_result")
	if err != nil {
		if persisted, ok := sp.persistedResult(caller, agentID); ok {
			return persisted, nil
		}
		return "", err
	}
	status := child.Status()
	result, runErr := child.Result()
	switch status {
	case "running":
		if line := child.LatestProgress(); line != "" {
			return fmt.Sprintf("Subagent %s is still running. Latest progress: %s", agentID, line), nil
		}
		return fmt.Sprintf("Subagent %s is still running.", agentID), nil
	case "idle":
		return fmt.Sprintf("Subagent %s was interrupted before completion and remains available for a follow-up message.", agentID), nil
	}
	if runErr != nil {
		return fmt.Sprintf("Subagent %s failed: %v", agentID, runErr), nil
	}
	return fmt.Sprintf("Subagent %s finished.\n\n%s", agentID, agent.TruncateSubagentReport(result)), nil
}

// WaitAgent blocks until the child's main run reaches a terminal or
// interrupted state, or the timeout/the caller's context expires. A timeout is
// reported as current status, not an error. Implements
// agent.ContinuableSubagentSpawner.
func (sp *Spawner[H]) WaitAgent(ctx context.Context, caller *agent.Agent, agentID string, timeout time.Duration) (string, error) {
	child, err := sp.ownedChild(caller, agentID, "wait_agent")
	if err != nil {
		if persisted, ok := sp.persistedResult(caller, agentID); ok {
			return persisted, nil
		}
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	timedOut := false
	select {
	case <-child.Done():
	case <-timer:
		timedOut = true
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for subagent %s interrupted: %w (the child keeps running; wait again or interrupt it)", agentID, ctx.Err())
	}
	status, err := sp.GetAgentResult(caller, agentID)
	if err != nil {
		return "", err
	}
	if timedOut {
		return status + fmt.Sprintf("\nWait timed out after %s; wait again or interrupt the child.", timeout), nil
	}
	return status, nil
}

// SendMessage delivers text to a live child of caller as a new user message
// (queued when the child is mid-turn). Implements
// agent.ContinuableSubagentSpawner.
func (sp *Spawner[H]) SendMessage(caller *agent.Agent, agentID, text string) error {
	child, err := sp.ownedChild(caller, agentID, "send_message")
	if err != nil {
		return err
	}
	// The message itself is delivered through the child's delivery service
	// (queued when the child is mid-turn); reply capture is armed by the
	// host's delivery-start hook when the delivered turn actually begins.
	ok, full := sp.host.DeliverChildMessage(child, text)
	if !ok {
		if full {
			// Every slot of the child's system queue holds a targeted
			// delivery: refuse rather than silently dropping a message whose
			// reply the parent is waiting on. The error is actionable.
			return fmt.Errorf("send_message: subagent %s is busy and its delivery queue is full (%d queued messages); wait for it to finish its current turn or reply, then retry",
				agentID, sp.host.Policy().systemQueueCap())
		}
		// The child's runtime was evicted between the registry lookup and the
		// queue append (e.g. the retention release racing this send): report
		// the loss instead of dropping the message silently.
		return fmt.Errorf("send_message: subagent %s is no longer live", agentID)
	}
	return nil
}

// InterruptAgent cancels a live child's in-flight turn (no-op when idle); the
// child session stays alive. Implements agent.ContinuableSubagentSpawner.
func (sp *Spawner[H]) InterruptAgent(caller *agent.Agent, agentID string) error {
	child, err := sp.ownedChild(caller, agentID, "interrupt_agent")
	if err != nil {
		return err
	}
	// RunCancel closes the main run's pre-start registration window; the
	// host's CancelTurn covers a follow-up delivery turn once it started.
	if cancel := child.RunCancel(); cancel != nil {
		cancel()
	}
	sp.host.CancelTurn(child)
	return nil
}

// spawnCapError builds the refusal error for a spawn rejected by the internal
// non-finished guard (guardHit) or, by default, by the user-facing concurrent
// limit.
func spawnCapError(limit, guard int, guardHit bool) error {
	if guardHit {
		return fmt.Errorf("subagent limit reached: too many live subagents (running or interrupted); wait for them to finish or be released")
	}
	return fmt.Errorf("subagent limit reached (%d concurrent): wait for running subagents to finish or interrupt_agent them", limit)
}
