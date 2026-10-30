package subagents

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	if !sp.children.register(child) {
		// Unreachable while ids come from sesspkg.NewID, and kept honest
		// anyway: the registered record owns its host runtime, so this one
		// (a held runtime nothing would ever release) must go.
		runCancel()
		sp.host.Release(child, false)
		return "", fmt.Errorf("subagent: session id %s is already live", child.ID)
	}
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
	// Record the admission in the child's persisted outcome, BEFORE its first
	// turn writes a snapshot. A child that is interrupted (interrupt_agent, a
	// cancelled parent) or whose process goes away never reaches a terminal
	// state, and an unstamped outcome ("") is indistinguishable from "was
	// never admitted" — the host's UI then renders it as DONE. Marking it
	// "running" makes such a child read as interrupted; the terminal paths
	// overwrite it (FinishSubagentOutcome on completion, "failed"/"cancelled"
	// on a cancelled release). In-memory only: the child's own persist writes
	// it, so a refused spawn still leaves no session artifact.
	child.Agent.SetSubagentOutcome("running", "")

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
		if errors.Is(runErr, ErrYielded) {
			// STEERED: the run stopped at a round boundary with its work kept,
			// so the queued message can run next. Same shape as an interrupt —
			// continuable, no terminal event — and never a completion: recording
			// one here would stamp an empty success and tell the parent the job
			// finished.
			child.MarkInterrupted()
			time.AfterFunc(sp.host.Policy().retain(), func() { sp.ReleaseChildIfQuiet(child) })
			return
		}
		// Persist the final outcome on the child's snapshot BEFORE publishing
		// the terminal state: the events are not replayed after a reload or a
		// restart, so the saved session is what get_agent_result and the
		// host's UI fall back to. Publishing first (Finish, which closes Done
		// and flips IsFinished) let a reader that woke on that signal — a
		// waiter, the listing, a test — read the index while it still carried
		// the admission stamp "running", so a finished child could be reported
		// as interrupted. FinishSubagentOutcome writes synchronously (the
		// snapshot and its index entry), so the terminal state is on disk by
		// the time anything can observe that the run ended.
		child.Agent.FinishSubagentOutcome(report, runErr)
		child.Finish(report, runErr)
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
		notice := Notice(child.ID, fmt.Sprintf("%s (%s). Use get_agent_result(agent_id=%q) for the full result.",
			child.Label, verb, child.ID))
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

// listAgentsRow is one child in the list_agents output, live or saved. The
// two sources are merged into ONE list so the caller reads it in a single
// order (most recent activity first) instead of two sections it has to
// cross-reference.
type listAgentsRow struct {
	ID     string
	Label  string
	Active bool
	// Status is the live status (running/idle/finished) for an active child,
	// or the recorded outcome (success/failed/interrupted) for an inactive
	// one.
	Status string
	// Depth is the live nesting depth (0 for a saved child: depth is not
	// persisted — see SessionSnapshot).
	Depth int
	// Progress is the live one-line snippet of the newest assistant output.
	Progress string
	// Messages is the saved session's message count (inactive rows only).
	Messages int
	// Activity orders the listing, newest first: a running child is active
	// NOW, a finished one ended when it finished, a saved one was last
	// updated when the store says.
	Activity time.Time
}

// ListAgents lists the caller's children as ONE list, most recent activity
// first: the LIVE children (status, depth, latest progress line) and the SAVED
// ones the live registry no longer tracks (recorded outcome, message count).
// Every row carries an active/inactive flag, so the caller can tell at a
// glance what send_message can reach — inactive rows are readable only through
// get_agent_result.
//
// There is deliberately no cap here: the listing is a model-facing tool result,
// and the executor already bounds tool results (spilling the full text when it
// exceeds the configured limit). A cap would silently hide children the caller
// cannot otherwise discover — which is the whole point of listing saved ones.
//
// The saved half is what makes a child survive a restart in the model's view:
// the live registry is process-local, so after a restart (or once retention
// released a child) nothing else would tell the caller that its children — and
// their reports — still exist. Their ids are the handles get_agent_result
// needs. Implements agent.ContinuableSubagentSpawner.
func (sp *Spawner[H]) ListAgents(caller *agent.Agent) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("list_agents: caller agent is nil")
	}
	if sp == nil || sp.children == nil {
		return "", fmt.Errorf("list_agents: background subagents are not available in this mode")
	}
	rows := sp.agentRows(caller)
	if len(rows) == 0 {
		return "No live subagents.", nil
	}
	active := 0
	for _, r := range rows {
		if r.Active {
			active++
		}
	}
	var b strings.Builder
	noun := "child subagents"
	if len(rows) == 1 {
		noun = "child subagent"
	}
	fmt.Fprintf(&b, "%d %s (%d active, %d inactive), most recent activity first:\n", len(rows), noun, active, len(rows)-active)
	for _, r := range rows {
		if r.Active {
			fmt.Fprintf(&b, "%s — %s — active — %s (depth %d)\n", r.ID, r.Label, r.Status, r.Depth)
			if r.Progress != "" {
				fmt.Fprintf(&b, "  last: %s\n", r.Progress)
			}
			continue
		}
		fmt.Fprintf(&b, "%s — %s — inactive — %s — %d msgs", r.ID, r.Label, r.Status, r.Messages)
		if age := shortAge(time.Since(r.Activity)); age != "" {
			fmt.Fprintf(&b, " — %s ago", age)
		}
		b.WriteString("\n")
	}
	if active < len(rows) {
		// send_message revives a saved child (it is the same tool, and the
		// revival is transparent), so the footer must not claim the active
		// rows are the only reachable ones — the model would stop trying.
		b.WriteString("get_agent_result reads any child's report; send_message to an inactive one reopens its saved session.")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// agentRows merges the live children and the caller's saved (non-live) ones
// into one recency-ordered list. An id the registry still tracks is taken from
// the registry only, so a child can never appear twice.
func (sp *Spawner[H]) agentRows(caller *agent.Agent) []listAgentsRow {
	children := sp.children.childrenOf(caller.SessionID)
	now := time.Now()
	rows := make([]listAgentsRow, 0, len(children))
	liveIDs := make(map[string]struct{}, len(children))
	for _, c := range children {
		liveIDs[c.ID] = struct{}{}
		row := listAgentsRow{
			ID:       c.ID,
			Label:    c.Label,
			Active:   true,
			Status:   c.StatusOf(sp.host),
			Depth:    c.Depth,
			Progress: c.LatestProgress(),
			Activity: c.CreatedAt,
		}
		// Recency of a live child: a turn in flight is activity right now,
		// a finished one ended when it finished, otherwise it was created
		// when it was created.
		if row.Status == "running" {
			row.Activity = now
		} else if fin := c.FinishedAt(); !fin.IsZero() {
			row.Activity = fin
		}
		rows = append(rows, row)
	}
	if sp.host != nil {
		for _, pc := range sp.host.PersistedChildren(caller) {
			if pc.ID == "" {
				continue
			}
			if _, ok := liveIDs[pc.ID]; ok {
				continue
			}
			rows = append(rows, listAgentsRow{
				ID:       pc.ID,
				Label:    pc.Label,
				Status:   persistedStatusLabel(pc.Status),
				Messages: pc.Messages,
				Activity: pc.Updated,
			})
		}
	}
	// Newest first; ties (and missing timestamps) break on the id so the
	// listing is deterministic between calls.
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Activity.Equal(rows[j].Activity) {
			return rows[i].Activity.After(rows[j].Activity)
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

// shortAge renders an update age for an inactive row ("3m", "2h", "4d"). Empty
// for an unknown (zero) timestamp or an age under a minute, so the common
// just-finished case stays clean.
func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// persistedStatusLabel renders a stored outcome for list_agents. "" and
// "running" both mean the child never reached a terminal state — it was
// interrupted, cancelled, or its process went away — so they read as
// "interrupted"; nothing here may suggest that unfinished work completed.
func persistedStatusLabel(status string) string {
	switch status {
	case "success":
		return "success"
	case "failed":
		return "failed"
	default:
		return "interrupted"
	}
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

// errNotRestorable reports that the store has no saved child the caller may
// revive (unknown id, or another parent's session). SendMessage keeps its
// original, more descriptive refusal in that case.
var errNotRestorable = errors.New("subagent: no saved session to revive")

// SendMessage delivers text to a child of caller as a new user message
// (queued when the child is mid-turn). A child that is no longer live — its
// retention window elapsed, it was evicted, or this process never spawned it
// (a restart) — is REVIVED from its saved session first, so a conversation can
// continue across restarts instead of ending at the first release. Implements
// agent.ContinuableSubagentSpawner.
func (sp *Spawner[H]) SendMessage(caller *agent.Agent, agentID, text string) error {
	child, err := sp.ownedChild(caller, agentID, "send_message")
	if err != nil {
		revived, reviveErr := sp.reviveChild(caller, agentID)
		if reviveErr != nil {
			if errors.Is(reviveErr, errNotRestorable) {
				// Unknown id, or another parent's session: the ownership
				// refusal above is the right answer, and it already points
				// at the pull path.
				return err
			}
			return reviveErr
		}
		child = revived
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

// reviveChild brings a saved child of caller back into the live registry as an
// idle, continuable child, so the delivery that follows has somewhere to land
// and the reply capture has a parent to report to.
//
// A revived child has NO main run — it is exactly an interrupted child that has
// not been messaged yet — so its status is "idle" and its Done channel is
// closed (WaitAgent must not park on a run that already happened in a previous
// life). Its recorded outcome is left alone: the terminal state belongs to the
// main run, which a follow-up turn does not rewrite. The retention timer is
// armed so the revived runtime cannot linger; a delivery in flight re-arms it
// through the usual Busy check.
func (sp *Spawner[H]) reviveChild(caller *agent.Agent, agentID string) (*Child[H], error) {
	if caller == nil || agentID == "" || sp == nil || sp.host == nil {
		return nil, errNotRestorable
	}
	// A concurrent send_message (or a spawn) may have revived it already:
	// the registry is the single source of truth. The ownership check is NOT
	// optional here — ownedChild already refused this caller, so a live child
	// with the id belongs to somebody else and must stay refused, exactly as
	// if it had never been revived.
	if c := sp.children.get(agentID); c != nil {
		if c.ParentID != caller.SessionID {
			return nil, errNotRestorable
		}
		return c, nil
	}
	pol := sp.host.Policy()
	if sp.children.guardExceeded(caller.SessionID, pol.guardLimit()) {
		return nil, fmt.Errorf("send_message: too many live subagents (%d); wait for them to finish or be released, then retry", pol.guardLimit())
	}
	// Claim the id across the whole reopen. The lookup above and the register
	// below are two critical sections, and between them the host opens a
	// runtime: a second revival racing here would publish a second record over
	// the first one's id (the web host dedupes the runtime by id, so both
	// records would share it), and the displaced record's release would evict
	// the child mid-turn. Unreachable through send_message today — the tool is
	// never parallel-eligible — which is exactly why it must not be what keeps
	// this correct.
	if !sp.children.beginRevive(agentID) {
		return nil, fmt.Errorf("send_message: subagent %s is being reopened; retry in a moment", agentID)
	}
	defer sp.children.endRevive(agentID)
	nc, ok, err := sp.host.ReviveChild(caller, agentID)
	if err != nil {
		return nil, fmt.Errorf("send_message: reopening subagent %s: %w", agentID, err)
	}
	if !ok {
		return nil, errNotRestorable
	}
	if nc.Agent == nil {
		// Contract violation: a host must not open — let alone hold — a child
		// runtime it cannot hand back (see Host.ReviveChild). Both hosts check
		// their own runtime before they take the hold, so this reports the bug
		// instead of pinning a runtime the lifecycle has no record of.
		return nil, fmt.Errorf("send_message: host returned no child agent for subagent %s", agentID)
	}
	child := &Child[H]{
		ID: agentID,
		// The store authorises the parent link; the caller is that parent.
		ParentID: caller.SessionID,
		Label:    nc.Label,
		// Depth is not persisted (SessionSnapshot has no field for it), so
		// it is reconstructed from the caller: one level below it. That is
		// what the spawn contract would have set.
		Depth:     caller.SubagentDepth() + 1,
		CreatedAt: time.Now(),
		Agent:     nc.Agent,
		Handle:    nc.Handle,
		done:      make(chan struct{}),
		status:    "running",
	}
	child.MarkInterrupted() // no main run: idle and continuable
	// Guaranteed to succeed: beginRevive owns the id until this returns. The
	// check is kept so a future second registration path cannot silently
	// displace this record — and because a held runtime with no record is
	// exactly what the Host contract forbids, the loser is torn down instead
	// of leaked (the same unwind SpawnBackground's duplicate-id path does).
	if !sp.children.register(child) {
		sp.host.Release(child, false)
		return nil, fmt.Errorf("send_message: subagent %s was registered while it was being reopened", agentID)
	}
	sp.host.ArmReplyCapture(child)
	time.AfterFunc(pol.retain(), func() { sp.ReleaseChildIfQuiet(child) })
	return child, nil
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

// ErrYielded reports that a child's turn stopped at a ROUND BOUNDARY because a
// host asked it to (a steered send_message), not that its job finished. The
// lifecycle treats it like an interrupt — the child stays continuable, no
// terminal event is sent — except that the work done so far is kept (the turn
// flushed itself) and the queued message runs as the next turn.
var ErrYielded = errors.New("subagent: turn yielded to a queued message")

// Steer delivers text to a child of caller like SendMessage, and additionally
// asks the child's RUNNING turn to stop at its next round boundary so the
// message runs immediately after the model call and tools that round already
// started. That is the difference from SendMessage: a busy child is redirected
// instead of finishing its turn first, without cancelling anything (the
// in-flight model call and its tools complete; only the NEXT model call is
// skipped), so no work is lost.
//
// A child that is answering another targeted delivery is never cut: the reply
// capture is 1:1 with the call, and truncating that turn would hand the other
// parent a bogus answer — the message then simply queues, exactly as
// SendMessage would have left it. A child with no turn in flight is not
// "steered" at all: the delivery starts its own turn.
func (sp *Spawner[H]) Steer(caller *agent.Agent, agentID, text string) error {
	if err := sp.SendMessage(caller, agentID, text); err != nil {
		return err
	}
	child := sp.children.get(agentID)
	if child == nil || child.Agent == nil {
		return nil
	}
	// Read the GENERATION first, then the reply guard — the same order
	// sendNow/CheckSendNow uses, and it matters: the delivery worker arms
	// PendingReply BEFORE it starts the turn (deliverLoop's hook runs ahead of
	// startTurnWithResult), so a turn observed here is a reply turn exactly
	// when the guard read after it says so. Checking only before the read left
	// a window where the child's MAIN turn ended and the queued reply turn
	// started in between: the yield then landed on the reply turn, cutting the
	// answer the other parent is waiting for down to "no reply".
	turn := child.Agent.TurnGeneration()
	if turn == 0 {
		return nil // idle: the delivery starts its own turn
	}
	if child.PendingReply() {
		// Answering another targeted delivery: the capture is 1:1 with that
		// call, so this message waits for the turn to end (the same contract
		// SendNow enforces for a turn that answers a parent).
		return nil
	}
	// Best effort: a turn that ended between the read and this call simply
	// makes RequestYieldFor a no-op (it compares generations).
	child.Agent.RequestYieldFor(turn)
	return nil
}
