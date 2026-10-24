package server

import (
	"context"
	"fmt"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	sesspkg "gogen/internal/session"
	"gogen/internal/subagents"
)

// Web host half of the shared continuable-subagent lifecycle
// (internal/subagents). The lifecycle owns admission, the per-parent caps,
// the retention/release policy, the exit classification, and the control
// tools; this file supplies what only the web server can do: build a child
// session runtime, run one turn through the per-session turn machinery,
// broadcast the sidebar events, deliver into the parent's delivery service
// (live, or queued for the session's next registration), and resolve a
// released child's persisted outcome.
//
// Host rules that stay here and are NOT negotiable:
//   - a child runtime is HELD (newSessionRuntimeWithHold), so orphan eviction
//     never collects it while it is alive or awaiting release;
//   - the post-turn orphan sweep runs on EVERY exit path: a child write that
//     lands after a concurrent DELETE of the parent must not resurrect the
//     child as an invisible orphan (see sweepOrphanedChild);
//   - delete approvals for a headless child route to the PARENT's clients,
//     resolved by id at approval time (routeApprovalsTo) — the runtime
//     pointer captured at spawn goes stale when the parent is orphan-evicted
//     and reopened;
//   - releasing a child is an explicit teardown of its session: it fires the
//     evict hook so the child's own children (grandchildren) are cancelled
//     and released, and re-checks the parent for orphan eviction.

// --- subagents.Host implementation ---

// NewChild implements subagents.Host: it builds and REGISTERS the child
// runtime (see newChildRuntime) without announcing it — the lifecycle
// announces an admitted child through Started.
func (sp *subagentSpawner) NewChild(ctx context.Context, parent *agent.Agent, job, model string, depth int) (subagents.NewChild[*sessionRuntime], error) {
	rt, label, _, err := sp.newChildRuntime(ctx, parent, job, model, depth)
	if err != nil {
		return subagents.NewChild[*sessionRuntime]{}, err
	}
	// Held: orphan eviction must never collect a continuable child while it
	// is alive or awaiting release. Only the BACKGROUND path builds children
	// through this method (the foreground Spawn/Fork call newChildRuntime
	// directly and are unregistered when their turn ends), so the hold is
	// scoped exactly to the children whose lifecycle outlives a turn. The
	// discard/release paths clear it.
	rt.held.Store(true)
	return subagents.NewChild[*sessionRuntime]{Handle: rt, Agent: rt.agent, Label: label}, nil
}

// RunTurn implements subagents.Host. The orphan sweep runs on every exit
// path; a runtime released while the turn ran reports subagents.ErrEvicted so
// the lifecycle marks the child interrupted and notifies nobody.
func (sp *subagentSpawner) RunTurn(ctx context.Context, c *subagents.Child[*sessionRuntime], prompt string) (string, error) {
	report, err := sp.runChildTurn(ctx, c.Handle, prompt)
	sp.sweepOrphanedChild(c.Handle, c.ParentID, c.ID)
	if c.Handle.evicted.Load() {
		return report, subagents.ErrEvicted
	}
	return report, err
}

// CancelTurn implements subagents.Host: stop the child's in-flight turn (main
// or follow-up) without tearing its session down. Queued deliveries are
// dropped by the release/eviction path, not here.
func (sp *subagentSpawner) CancelTurn(c *subagents.Child[*sessionRuntime]) {
	c.Handle.stream.cancelInFlight()
}

// DeliverChildMessage implements subagents.Host: the message is delivered
// through the child's own delivery service, queued when the child is
// mid-turn. deliverChildMessage is the dedicated source — it is the only
// system delivery that arms the reply capture.
func (sp *subagentSpawner) DeliverChildMessage(c *subagents.Child[*sessionRuntime], text string) (ok, full bool) {
	return c.Handle.deliverChildMessage(text)
}

// ArmReplyCapture implements subagents.Host: reply capture is armed exactly
// when a send_message delivery starts, and completed with that invocation's
// exact output/error. No transcript scan or shared turn-end hook is involved.
func (sp *subagentSpawner) ArmReplyCapture(c *subagents.Child[*sessionRuntime]) {
	c.Handle.deliverStartHook = func() {
		c.BeginReply()
	}
	c.Handle.deliverResultFunc = func(reply string, replyErr error) {
		// An evicted runtime can no longer deliver the work the capture
		// protects: dropping it keeps a released child's record from
		// re-arming the retention timer forever.
		if c.Handle.evicted.Load() {
			return
		}
		c.OnTurnResult(sp, reply, replyErr)
	}
}

// Release implements subagents.Host: tear the child's runtime down. The
// lifecycle has already removed the child from its registry and marked it
// released, so the host half only unwinds the runtime.
func (sp *subagentSpawner) Release(c *subagents.Child[*sessionRuntime], cancelledMidRun bool) {
	rt := c.Handle
	// A child released while its main job is running was cancelled (parent
	// teardown / cap): record the failed outcome BEFORE the drain, so the
	// cancelled turn's final flush persists it with the transcript. Finished
	// children keep the outcome already written at completion;
	// interrupted-idle children leave it unset (still continuable). This
	// write is why a STARTED child's release differs from a refused spawn's
	// discard, which must leave no session file at all.
	if cancelledMidRun {
		c.MarkInterrupted()
		rt.agent.SetSubagentOutcome("failed", "cancelled")
		// Force the write: the cancelled turn's final flush may have already
		// run (turn end racing the teardown), in which case the eviction's
		// FlushPending would find the session clean and the outcome would be
		// lost — the sidebar would render the cancelled child as done.
		rt.agent.FlushSession()
	}
	if cancel := c.RunCancel(); cancel != nil {
		cancel()
	}
	rt.stream.cancelInFlight() // active delivery turns unwind; queued ones are discarded by eviction
	rt.held.Store(false)
	sp.s.registry.evictRuntime(rt)
	// Releasing a child is an explicit teardown of its session: its own
	// children (grandchildren) are cancelled and released in turn.
	sp.s.registry.fireEvictHook(c.ID)
	sp.recheckParent(c.ParentID)
}

// Deliver implements subagents.Host: route one system message to the parent
// session, or queue it for the session's next registration when the parent is
// not live (orphan-evicted, closed). A producer must not lose a completion
// notice to a parent that merely went idle with no viewers.
func (sp *subagentSpawner) Deliver(parentID, text string) {
	if sp == nil || sp.s == nil || sp.s.registry == nil || parentID == "" {
		return
	}
	sp.s.registry.deliverToParent(parentID, text)
}

// Started implements subagents.Host: announce an ADMITTED child — the sidebar
// row's attach target.
func (sp *subagentSpawner) Started(c *subagents.Child[*sessionRuntime], job string) {
	parentRt, ok := sp.s.registry.get(c.ParentID)
	if !ok {
		return
	}
	parentRt.broadcast(WSMessage{
		Type:           "subagent_started",
		SessionID:      c.ParentID,
		SubagentID:     c.ID,
		SubagentLabel:  c.Label,
		SubagentJob:    truncateJob(job),
		SubagentParent: c.ParentID,
	})
}

// Finished implements subagents.Host: announce the child's terminal state.
func (sp *subagentSpawner) Finished(c *subagents.Child[*sessionRuntime], report string, err error) {
	parentRt, ok := sp.s.registry.get(c.ParentID)
	if !ok {
		return
	}
	parentRt.broadcast(WSMessage{
		Type:            "subagent_finished",
		SessionID:       c.ParentID,
		SubagentID:      c.ID,
		SubagentLabel:   c.Label,
		SubagentParent:  c.ParentID,
		SubagentSuccess: err == nil,
		SubagentSummary: truncateReport(report, err),
	})
}

// PersistedResult implements subagents.Host: resolve a released child from the
// session store. The ParentID check is the authorization boundary — a model
// must not retrieve a nested session belonging to another parent.
func (sp *subagentSpawner) PersistedResult(caller *agent.Agent, agentID string) (string, bool) {
	if caller == nil || agentID == "" || sp.s == nil || sp.s.ws == nil || sp.s.ws.Store == nil {
		return "", false
	}
	info := sp.s.ws.Store.Info(sp.s.ws.GetWorkingDir(), agentID)
	if info == nil || info.ParentID != caller.SessionID {
		return "", false
	}
	switch info.SubagentStatus {
	case "success":
		return fmt.Sprintf("Subagent %s finished.\n\n%s", agentID, info.SubagentSummary), true
	case "failed":
		return fmt.Sprintf("Subagent %s failed.\n\n%s", agentID, info.SubagentSummary), true
	default:
		return fmt.Sprintf("Subagent %s has no persisted terminal result yet; it may have been interrupted before completion.", agentID), true
	}
}

// TurnActive implements subagents.Host: a follow-up delivery turn (for
// example a send_message reply) reads as "running".
func (sp *subagentSpawner) TurnActive(c *subagents.Child[*sessionRuntime]) bool {
	active, _ := c.Handle.turnState()
	return active
}

// Busy implements subagents.Host: lifecycle work a release would cut short.
// An evicted runtime can no longer run the work these checks protect, so it
// reports quiet — an exact-result capture whose callback will never run must
// not pin the child record and re-arm its retention timer forever.
func (sp *subagentSpawner) Busy(c *subagents.Child[*sessionRuntime]) bool {
	if c.Handle.evicted.Load() {
		return false
	}
	if active, _ := c.Handle.turnState(); active {
		return true
	}
	if c.Handle.hasPendingDeliveries() {
		return true
	}
	return c.PendingReply()
}

// Policy implements subagents.Host with the server's live knobs (test
// overrides first, then the shared defaults).
func (sp *subagentSpawner) Policy() subagents.Policy {
	return subagents.Policy{
		Retain:       sp.retain,
		MaxFinished:  sp.maxFinished,
		MaxLiveGuard: sp.maxLiveGuard,
		Concurrent:   sp.concurrentLimit,
	}
}

// --- host helpers ---

// retainWindow resolves the retention window (the test knob included).
func (sp *subagentSpawner) retainWindow() time.Duration {
	if sp.retain > 0 {
		return sp.retain
	}
	return subagents.DefaultRetain
}

// concurrentLimit returns the user-facing per-parent cap on ACTIVE
// subagents (running background children + in-flight foreground children;
// an interrupted idle child holds no slot): the test override (maxLive)
// when set, otherwise the live workspace value (config default when unset).
// The parent id is part of the subagents.Policy shape; one server-wide limit
// applies today.
func (sp *subagentSpawner) concurrentLimit(string) int {
	if sp.maxLive > 0 {
		return sp.maxLive
	}
	if n := sp.s.ws.GetSubagentMaxConcurrent(); n > 0 {
		return n
	}
	return config.DefaultSubagentMaxConcurrent
}

// Fork implements agent.ContinuableSubagentSpawner: a foreground child
// seeded with a deep copy of the parent's messages (via ForkMessages, the
// same orphaned-tool-call stripping as the web fork UI) plus the parent's
// model/mode/thinking. The parent transcript is untouched.
func (sp *subagentSpawner) Fork(ctx context.Context, parent *agent.Agent, job string, depth int) (string, error) {
	s := sp.s
	if parent == nil {
		return "", fmt.Errorf("subagent_fork: parent agent is nil")
	}
	parentRt, ok := s.registry.get(parent.SessionID)
	if !ok {
		return "", fmt.Errorf("subagent_fork: parent session is not live")
	}
	// The fork's foreground child holds a slot of the concurrent limit for
	// the whole turn, exactly like Spawn: AcquireForeground reserves the
	// slot atomically with the cap check, and the deferred release frees
	// it on every exit path.
	ok, guardHit := sp.AcquireForeground(parent.SessionID)
	if !ok {
		return "", fmt.Errorf("subagent_fork: %w", sp.SpawnCapError(parent.SessionID, guardHit))
	}
	defer sp.ReleaseForeground(parent.SessionID)
	forkedMsgs, err := agent.ForkMessages(parent.SnapshotMessages(), "last")
	if err != nil {
		return "", fmt.Errorf("subagent_fork: %w", err)
	}
	mode, thinking := parent.ModeAndThinkingLevel()
	label := "fork: " + parent.SessionLabelSnapshot()
	if job == "" {
		job = "Continue this session from the fork point."
	}
	newID := sesspkg.NewID()
	// Re-point the forked transcript's spill locators at the child's spill
	// dir (hardlinks) so retrieval keeps working after the parent session
	// is deleted (delete cascade removes the parent's spill files).
	parent.RepointSpillLocators(forkedMsgs, newID)
	forkRef := parent.CurrentModelRef()
	snap := &agent.SessionSnapshot{
		WorkingDir:    parent.WorkingDir,
		Model:         forkRef.WireID(),
		ModelProvider: forkRef.ProfileName(),
		Mode:          mode.String(),
		ThinkingLevel: string(thinking),
		Label:         label,
		LabelRenamed:  true,
		Messages:      forkedMsgs,
		ParentID:      parent.SessionID,
	}
	child := s.ws.NewSessionAgent(snap, newID)
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	childRt := newSessionRuntimeWithHold(child, sp.approvalHold())
	childRt.parentID = parent.SessionID
	childRt.nested = true
	// Resolved by id at approval time: the parent pointer captured here goes
	// stale when the parent is orphan-evicted and reopened (routeApprovalsTo).
	childRt.routeApprovalsTo(s.registry, parent.SessionID)
	s.registry.register(newID, childRt)
	parentRt.broadcast(WSMessage{
		Type:           "subagent_started",
		SessionID:      parent.SessionID,
		SubagentID:     newID,
		SubagentLabel:  label,
		SubagentJob:    truncateJob(job),
		SubagentParent: parent.SessionID,
	})

	report, err := sp.runChildTurn(ctx, childRt, job)
	report = agent.TruncateSubagentReport(report)
	sp.finalizeForegroundChild(childRt, parentRt, label, report, err)
	if err != nil {
		return report, fmt.Errorf("subagent_fork %s: %w", newID, err)
	}
	return report, nil
}

// recheckParent re-runs the orphan check for a parent session that may have
// been kept resident ONLY by the background work just released. The eviction
// paths refuse to evict a session that still owns background work
// (sessionRegistry.keepsBackgroundWork, which counts live background
// children), so without this a parent whose last child just finished would
// stay registered until the next unrelated detach — a residency leak
// disguised as a deferred cleanup.
//
// Best-effort and re-entrant: it resolves the parent's live runtime, and
// evictOrphanedIfPossible no-ops for an evicted, viewed, busy, or
// still-working one.
func (sp *subagentSpawner) recheckParent(parentID string) {
	if parentID == "" {
		return
	}
	if rt, ok := sp.s.registry.get(parentID); ok {
		rt.evictOrphanedIfPossible()
	}
}
