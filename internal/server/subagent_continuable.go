package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	sesspkg "gogen/internal/session"
)

// Continuable (background) subagents: children that keep running after the
// parent's tool call returns, can be messaged (send_message), interrupted
// (interrupt_agent), listed (list_agents), waited on (wait_agent), and queried
// for their authoritative result (get_agent_result). Web host only in v1
// (the TUI spawner stays foreground-only, so its agents never see the
// continuable tools or the run_in_background parameter).
//
// Lifecycle:
//   - SpawnBackground creates and registers the child exactly like the
//     foreground Spawn, then runs the turn in a goroutine instead of
//     blocking. The runtime is marked held, so orphan eviction (no clients,
//     no turn) never collects it mid-lifecycle.
//   - On natural completion a compact terminal event is delivered to the
//     parent conversation (queued for the session's next registration when
//     the parent runtime is not live). The authoritative bounded report is
//     pulled with get_agent_result and remains persisted after live release.
//   - Finished children stay registered for a retention window (bounded
//     memory), then are released: held cleared, runtime evicted (the saved
//     session stays attachable via the sidebar).
//   - A per-parent cap bounds how many finished children accumulate.
//   - When the PARENT session is evicted (closed/cap/orphan), all of its
//     children are cancelled and released (fail-safe: a child whose parent
//     is gone cannot deliver anyway).
//   - interrupt_agent cancels the current main or follow-up turn; the child
//     stays alive and continuable (no terminal event for an interruption).

const (
	// defaultSubagentRetain is how long a finished continuable child stays
	// registered after its main job completes before its runtime is
	// released (the saved session remains attachable). Mirrors the
	// background-jobs retention philosophy.
	defaultSubagentRetain = 10 * time.Minute
	// defaultMaxFinishedSubagents caps finished continuable children per
	// parent; beyond it the oldest finished children are released.
	defaultMaxFinishedSubagents = 32
	// defaultMaxLiveSubagents is the internal per-parent cap on
	// NON-finished children (running + idle background + in-flight
	// foreground). The user-facing concurrent limit counts only ACTIVE
	// children, so without this guard a spawn+interrupt loop could
	// accumulate unbounded runtimes. Spawning beyond it is refused with
	// an error — never silently released mid-task.
	defaultMaxLiveSubagents = 64
)

// backgroundChild is one live continuable subagent.
type backgroundChild struct {
	id       string
	parentID string
	label    string
	depth    int
	// createdAt stamps registration order; childrenOf sorts by it so the
	// listing shows oldest first (newest last).
	createdAt time.Time
	rt        *sessionRuntime
	sp        *subagentSpawner // owning spawner (for parent delivery)
	// runCancel is installed before the child is published and remains valid
	// for the main run's whole lifetime. InterruptAgent can therefore cancel
	// a just-spawned child even before its turn goroutine registers a stream
	// handle, closing the old registration race.
	runCancel context.CancelFunc
	// done is closed exactly once when the main run reaches a terminal or
	// interrupted state. WaitAgent waits on this one-shot signal and reads the
	// authoritative result from the fields above after it closes.
	done     chan struct{}
	doneOnce sync.Once

	mu           sync.Mutex
	status       string // "running" | "idle" | "finished"
	result       string // final report (status finished)
	err          error  // final error (status finished)
	finishedAt   time.Time
	released     bool
	pendingReply bool // a send_message delivery is in flight; capture its exact result
}

type subagentEventKind uint8

const (
	subagentEventTerminal subagentEventKind = iota
	subagentEventReply
)

// subagentEvent is the typed, host-mediated child-to-parent event. Text is
// produced only at the delivery boundary; identity, kind, and result remain
// structured for routing, tests, and future non-text consumers. A raw Go
// channel is deliberately not exposed to the model/parent Agent: transcript
// writes still belong to the serialized parent delivery worker.
type subagentEvent struct {
	Kind     subagentEventKind
	ChildID  string
	ParentID string
	Label    string
	Text     string
	Err      error
}

func (e subagentEvent) parentMessage() string {
	switch e.Kind {
	case subagentEventTerminal:
		return e.Text
	case subagentEventReply:
		if e.Text == "" {
			msg := fmt.Sprintf("[subagent %s] no reply (the turn failed or was interrupted before producing one)", e.Label)
			if e.Err != nil {
				msg = fmt.Sprintf("[subagent %s] no reply (the turn failed: %v)", e.Label, e.Err)
			}
			return msg
		}
		return fmt.Sprintf("[subagent %s] %s", e.Label, agent.TruncateSubagentReport(e.Text))
	default:
		return e.Text
	}
}

func (c *backgroundChild) deliverEvent(event subagentEvent) {
	event.ChildID = c.id
	event.ParentID = c.parentID
	event.Label = c.label
	c.sp.deliverEvent(event)
}

// childRegistry owns the live continuable children of a server. A nil
// registry means the foreground-only test path; continuable methods report
// unsupported rather than panicking.
type childRegistry struct {
	mu       sync.Mutex
	children map[string]*backgroundChild
	// foreground counts in-flight foreground (Spawn/Fork) children per
	// parent session id. Foreground children are not backgroundChild
	// records, but they hold a slot of the concurrent limit for the whole
	// turn; guarded by mu.
	foreground map[string]int
}

func newChildRegistry() *childRegistry {
	return &childRegistry{
		children:   make(map[string]*backgroundChild),
		foreground: make(map[string]int),
	}
}

func (cr *childRegistry) register(c *backgroundChild) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.children[c.id] = c
}

// hasLiveChildren reports whether parentID still has a continuable child
// registered — i.e. background subagent work that a registry eviction would
// destroy, because the eviction hook (cancelAll) cancels every live child of
// an evicted parent. Wired into sessionRegistry.bgWorkHook so the eviction
// paths treat such a parent as NOT idle (see keepsBackgroundWork), exactly
// like a running shell job or terminal.
//
// A released child is already out of the map (releaseChild deletes under mu),
// so only genuinely live work counts. Leaf lock (mu): every critical section
// releases it before calling into the registry, so the registry's cap
// eviction may consult this while holding its own lock.
func (cr *childRegistry) hasLiveChildren(parentID string) bool {
	if cr == nil {
		return false
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	for _, c := range cr.children {
		if c.parentID == parentID {
			return true
		}
	}
	return false
}

func (cr *childRegistry) get(id string) *backgroundChild {
	if cr == nil {
		return nil
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.children[id]
}

// childrenOf returns the live children of parentID (id → child), newest
// last (registration order, by createdAt).
func (cr *childRegistry) childrenOf(parentID string) []*backgroundChild {
	if cr == nil {
		return nil
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	var out []*backgroundChild
	for _, c := range cr.children {
		if c.parentID == parentID {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *backgroundChild) int { return a.createdAt.Compare(b.createdAt) })
	return out
}

// activeCountLocked counts the ACTIVE children of parentID: background
// children whose statusOf() reports "running" (main job in flight, or
// mid-turn replying to a send_message) plus in-flight foreground children.
// An interrupted (idle) child holds no slot — interrupt_agent frees it, as
// the spawn-refusal error promises. Caller holds mu. Lock order:
// mu → c.mu → rt.stateMu (statusOf); c.mu and stateMu are leaf locks, never
// held when acquiring mu.
func (cr *childRegistry) activeCountLocked(parentID string) int {
	n := cr.foreground[parentID]
	for _, c := range cr.children {
		if c.parentID == parentID && c.statusOf() == "running" {
			n++
		}
	}
	return n
}

// liveCountLocked counts the NON-finished children of parentID (running +
// idle background + in-flight foreground) — the memory guard's metric: an
// idle child holds no slot of the concurrent limit, but its runtime still
// lives until the retention window, so it is bounded separately. Caller
// holds mu.
func (cr *childRegistry) liveCountLocked(parentID string) int {
	n := cr.foreground[parentID]
	for _, c := range cr.children {
		if c.parentID == parentID && !c.isFinished() {
			n++
		}
	}
	return n
}

// acquireForeground atomically checks both spawn caps and, when allowed,
// reserves a foreground slot for parentID: the user-facing limit against
// active children (the new child counts itself via the increment, so >=
// refuses at a full cap) and the internal guard against non-finished
// children. hitGuard reports a refusal by the guard rather than the
// user-facing limit. Check and increment are one critical section, so
// concurrent spawns cannot slip past either cap. A nil registry (test
// spawners that only exercise the foreground path) is unbounded.
func (cr *childRegistry) acquireForeground(parentID string, limit, guard int) (ok bool, hitGuard bool) {
	if cr == nil {
		return true, false
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.activeCountLocked(parentID) >= limit {
		return false, false
	}
	if cr.liveCountLocked(parentID) >= guard {
		return false, true
	}
	cr.foreground[parentID]++
	return true, false
}

// releaseForeground returns a slot reserved by acquireForeground. The key
// is deleted at zero so a long-running server does not accumulate one
// entry per session.
func (cr *childRegistry) releaseForeground(parentID string) {
	if cr == nil {
		return
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if n := cr.foreground[parentID] - 1; n > 0 {
		cr.foreground[parentID] = n
	} else {
		delete(cr.foreground, parentID)
	}
}

// spawnCapExceeded reports, in one snapshot, whether the spawn the caller
// has ALREADY committed (the child is registered) exceeds either cap: the
// user-facing active limit or the internal non-finished guard. Strictly-
// greater because the committed child counts itself in both.
func (cr *childRegistry) spawnCapExceeded(parentID string, limit, guard int) (activeOver, guardOver bool) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.activeCountLocked(parentID) > limit, cr.liveCountLocked(parentID) > guard
}

// finish records the main-job outcome and closes done.
func (c *backgroundChild) finish(report string, err error) {
	c.mu.Lock()
	c.status = "finished"
	c.result = report
	c.err = err
	c.finishedAt = time.Now()
	c.mu.Unlock()
	c.doneOnce.Do(func() { close(c.done) })
}

// markInterrupted records the main run's intentional cancellation and wakes
// waiters without manufacturing a terminal result. The child remains
// continuable through send_message.
func (c *backgroundChild) markInterrupted() {
	c.mu.Lock()
	if c.status == "running" {
		c.status = "idle"
	}
	c.mu.Unlock()
	c.doneOnce.Do(func() { close(c.done) })
}

func (c *backgroundChild) isFinished() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status == "finished"
}

// isRunning reports whether the child's main job has not completed or been
// interrupted yet. Takes c.mu: callers that already hold children.mu must
// read the status through this helper (lock order children.mu → c.mu, same
// as liveCountLocked's isFinished call).
func (c *backgroundChild) isRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status == "running"
}

// busy reports whether the child has lifecycle work a release would cut
// short: an active turn, a send_message delivery queued for the delivery
// worker but not yet turned into one, or an exact reply result armed but not
// yet delivered to the parent. Takes no locks on entry; c.mu, rt.stateMu and
// rt.deliverMu are leaf locks (the documented children.mu → c.mu order,
// never the reverse).
func (c *backgroundChild) busy() bool {
	// An evicted runtime can no longer run the work the checks below
	// protect. An exact-result capture whose callback will never run must not
	// pin the child record and re-arm its retention timer forever.
	if c.rt.evicted.Load() {
		return false
	}
	if active, _ := c.rt.turnState(); active {
		return true
	}
	if c.rt.hasPendingDeliveries() {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingReply
}

// latestProgress returns a one-line, bounded snippet of the child's most
// recent assistant output: the PULL-side view of a background child's
// progress (list_agents). Children never push progress into the parent
// conversation — the pull path and the child's own pane are how the parent
// model and the user see what a child is doing. Empty when the child has
// produced no assistant content yet.
func (c *backgroundChild) latestProgress() string {
	msgs := c.rt.agent.SnapshotMessages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" || msgs[i].Content == "" {
			continue
		}
		line := strings.Join(strings.Fields(msgs[i].Content), " ")
		return contextmgr.Truncate(line, maxAgentProgressLine, contextmgr.TruncateOptions{Marker: "…"})
	}
	return ""
}

// maxAgentProgressLine bounds a child's progress snippet in list_agents (a
// listing line, not content: the full transcript stays in the child's pane).
const maxAgentProgressLine = 160

// onTurnResult receives the exact output and error returned by one
// send_message delivery turn. Using the turn's return value avoids inferring
// a reply by scanning (and potentially misidentifying) transcript messages
// after tool rounds or auto-compaction.
func (c *backgroundChild) onTurnResult(reply string, replyErr error) {
	c.mu.Lock()
	if !c.pendingReply {
		c.mu.Unlock()
		return
	}
	c.pendingReply = false
	c.mu.Unlock()

	if c.rt.evicted.Load() {
		return
	}
	c.deliverEvent(subagentEvent{Kind: subagentEventReply, Text: reply, Err: replyErr})
}

// deliverEvent routes one typed child event through the parent's serialized
// delivery service. The method is currently a narrow seam around the existing
// string delivery API; keeping identity/kind here prevents a future producer
// from bypassing parent routing and gives tests a structured contract.
func (sp *subagentSpawner) deliverEvent(event subagentEvent) {
	if sp == nil || sp.s == nil || sp.s.registry == nil || event.ParentID == "" {
		return
	}
	sp.s.registry.deliverToParent(event.ParentID, event.parentMessage())
}

// --- spawner implementation ---

func (sp *subagentSpawner) retainWindow() time.Duration {
	if sp.retain > 0 {
		return sp.retain
	}
	return defaultSubagentRetain
}

func (sp *subagentSpawner) maxFinishedLimit() int {
	if sp.maxFinished > 0 {
		return sp.maxFinished
	}
	return defaultMaxFinishedSubagents
}

// concurrentLimit returns the user-facing per-parent cap on ACTIVE
// subagents (running background children + in-flight foreground children;
// an interrupted idle child holds no slot): the test override (maxLive)
// when set, otherwise the live workspace value (config default when unset).
func (sp *subagentSpawner) concurrentLimit() int {
	if sp.maxLive > 0 {
		return sp.maxLive
	}
	if n := sp.s.ws.GetSubagentMaxConcurrent(); n > 0 {
		return n
	}
	return config.DefaultSubagentMaxConcurrent
}

// liveGuardLimit returns the internal per-parent cap on NON-finished
// children (running + idle background + in-flight foreground). The user-
// facing limit counts only active children, so without this guard a
// spawn+interrupt loop could accumulate unbounded runtimes. Test override
// (maxLiveGuard) when set.
func (sp *subagentSpawner) liveGuardLimit() int {
	if sp.maxLiveGuard > 0 {
		return sp.maxLiveGuard
	}
	return defaultMaxLiveSubagents
}

// spawnCapError builds the refusal error for a spawn rejected by the
// internal non-finished guard (guardHit) or, by default, by the user-facing
// concurrent limit.
func spawnCapError(limit int, guardHit bool) error {
	if guardHit {
		return fmt.Errorf("subagent limit reached: too many live subagents (running or interrupted); wait for them to finish or be released")
	}
	return fmt.Errorf("subagent limit reached (%d concurrent): wait for running subagents to finish or interrupt_agent them", limit)
}

// SpawnBackground implements agent.ContinuableSubagentSpawner: the child is
// created exactly like the foreground Spawn, then its turn runs in a
// goroutine. The parent tool call returns the child id immediately; the
// parent is notified when the child finishes.
func (sp *subagentSpawner) SpawnBackground(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	s := sp.s
	childRt, label, rawJob, err := sp.newChildRuntime(ctx, parent, job, model, depth)
	if err != nil {
		return "", err
	}
	// Own the main run from the moment the child record is created. A cancel
	// requested before the goroutine gets scheduled is observed when the run
	// begins, rather than being lost because no stream handle exists yet.
	runCtx, runCancel := context.WithCancel(context.Background())
	child := &backgroundChild{
		id:        childRt.agent.SessionID,
		parentID:  parent.SessionID,
		label:     label,
		depth:     depth + 1,
		createdAt: time.Now(),
		rt:        childRt,
		sp:        sp,
		runCancel: runCancel,
		done:      make(chan struct{}),
		status:    "running",
	}
	sp.children.register(child)
	// Held: orphan eviction must never collect a continuable child while
	// it is alive or awaiting release.
	childRt.held.Store(true)
	// Reply capture is armed exactly when a send_message delivery starts,
	// and completed with that invocation's exact output/error. No transcript
	// scan or shared turn-end hook is involved.
	childRt.deliverStartHook = func() {
		child.mu.Lock()
		child.pendingReply = true
		child.mu.Unlock()
	}
	childRt.deliverResultFunc = child.onTurnResult
	sp.enforceChildCap(parent.SessionID)
	// Spawn caps (checked after registration so concurrent spawns cannot
	// slip past them): the just-registered child counts itself in both, so
	// strictly-greater means we are one over.
	//   - the user-facing limit bounds ACTIVE children (running background
	//     + in-flight foreground); an interrupted (idle) child holds no
	//     slot — interrupt_agent frees it, as the refusal error says.
	//   - the internal guard bounds NON-finished children (running + idle
	//     + foreground) so interrupted children cannot accumulate
	//     unbounded runtimes.
	activeOver, guardOver := sp.children.spawnCapExceeded(parent.SessionID, sp.concurrentLimit(), sp.liveGuardLimit())
	if activeOver || guardOver {
		// The child is DISCARDED, not released: it never started — no turn,
		// nothing persisted — so it must leave no artifact. releaseChild
		// would stamp the outcome "failed"/"cancelled" and force a flush,
		// writing an empty session file + index entry per refused spawn: a
		// phantom failed child row in the sidebar for a child that never
		// existed (discardUnstartedChild's flush-free eviction writes
		// nothing). The refusal error below — the tool result the parent
		// model sees — is the refused child's only trace.
		sp.discardUnstartedChild(child)
		return "", spawnCapError(sp.concurrentLimit(), guardOver)
	}

	// Announce the ADMITTED child (the sidebar row's attach target) — only
	// now, after the caps passed: a refused child is discarded without ever
	// being announced, so no client ever learns of it (an announced-but-
	// refused child would leave a "running" row no event ever resolves).
	if parentRt, ok := s.registry.get(parent.SessionID); ok {
		parentRt.broadcast(WSMessage{
			Type:           "subagent_started",
			SessionID:      parent.SessionID,
			SubagentID:     child.id,
			SubagentLabel:  label,
			SubagentJob:    truncateJob(rawJob),
			SubagentParent: parent.SessionID,
		})
	}

	sp.spawnWg.Add(1)
	go func() {
		defer sp.spawnWg.Done()
		defer runCancel()
		report, runErr := sp.runChildTurn(runCtx, childRt, agent.FormatSubagentJob(rawJob))
		// Sweep the orphan on EVERY exit path: a concurrent DELETE of the
		// parent cascades the child's file away and a write that lands
		// after that delete (the cancelled turn's final flush, or the
		// release path's outcome flush) resurrects the child as an
		// invisible orphan. The old finish-path-only check left interrupted
		// and released children un-swept. (The eviction loop's own sweep
		// covers the window before this check runs.)
		sp.sweepOrphanedChild(childRt, parent.SessionID, child.id)
		if childRt.evicted.Load() {
			child.markInterrupted()
			return // released while running (parent close / retention) — nothing to notify
		}
		if runErr != nil && errors.Is(runErr, context.Canceled) {
			// Interrupted (interrupt_agent / child-pane cancel): the child
			// stays alive and continuable; no completion notice. Arm the
			// retention timer so an idle interrupted child cannot linger in
			// memory forever (the parent's lifetime no longer bounds it:
			// orphan eviction does not cancel children).
			child.markInterrupted()
			time.AfterFunc(sp.retainWindow(), func() { sp.maybeReleaseChild(child) })
			return
		}
		child.finish(report, runErr)
		// Persist the final outcome on the child's snapshot (the events
		// are not replayed after a reload/restart, so the saved session is
		// what get_agent_result and the sidebar fall back to).
		success := runErr == nil
		childRt.agent.FinishSubagentOutcome(report, runErr)
		// Enforce the per-parent finished cap NOW (spawn-time enforcement
		// misses fast jobs that finish after their siblings spawned):
		// release the oldest finished children beyond the cap.
		sp.enforceChildCap(parent.SessionID)
		summary := truncateReport(report, runErr)
		verb := "finished"
		if !success {
			verb = "failed"
		}
		if parentRt, ok := s.registry.get(parent.SessionID); ok {
			parentRt.broadcast(WSMessage{
				Type:            "subagent_finished",
				SessionID:       parent.SessionID,
				SubagentID:      child.id,
				SubagentLabel:   label,
				SubagentParent:  parent.SessionID,
				SubagentSuccess: success,
				SubagentSummary: summary,
			})
		}
		// Queue a compact, correlated terminal event. The potentially large
		// authoritative report is pulled with get_agent_result; keeping it out
		// of the automatic parent turn prevents completion bursts from flooding
		// the parent's context. The child id makes duplicate/truncated labels
		// resolvable without guessing.
		notice := fmt.Sprintf("[subagent %s] %s (%s). Use get_agent_result(agent_id=%q) for the full result.", child.id, label, verb, child.id)
		child.deliverEvent(subagentEvent{Kind: subagentEventTerminal, Text: notice, Err: runErr})
		// Retention: release the runtime after the window so finished
		// children cannot accumulate in memory. The saved session stays
		// attachable; release is idempotent (child.released).
		time.AfterFunc(sp.retainWindow(), func() { sp.maybeReleaseChild(child) })
	}()
	return child.id, nil
}

// enforceChildCap releases the oldest finished children of parentID beyond
// the per-parent cap.
func (sp *subagentSpawner) enforceChildCap(parentID string) {
	sp.children.mu.Lock()
	var finished []*backgroundChild
	for _, c := range sp.children.children {
		if c.parentID == parentID && c.isFinished() {
			finished = append(finished, c)
		}
	}
	slices.SortFunc(finished, func(a, b *backgroundChild) int { return a.finishedAt.Compare(b.finishedAt) })
	overflow := len(finished) - sp.maxFinishedLimit()
	var victims []*backgroundChild
	if overflow > 0 {
		// Oldest-first, but never a child whose exact-reply work is still
		// in flight (an active send_message turn, a delivery queued for the
		// worker, or an armed reply result): releaseChild would cancel the
		// turn and drop the reply silently — the same trap the retention
		// timer's busy check guards. A spared child is released by its own
		// retention timer once it goes quiet (armed at finish), and the
		// next spawn/finish re-runs this cap, so sparing cannot strand it.
		for _, c := range finished {
			if len(victims) >= overflow {
				break
			}
			if c.busy() {
				continue
			}
			victims = append(victims, c)
		}
	}
	sp.children.mu.Unlock()
	for _, c := range victims {
		sp.releaseChild(c)
	}
}

// maybeReleaseChild runs when a child's retention window elapses. A child
// with lifecycle work in flight is NOT released — the timer re-arms so the
// work completes and the child is released after the next quiet window:
//   - an active turn (e.g. a send_message reply): releasing mid-turn would
//     cancel the reply;
//   - a send_message delivery queued but not yet past the acquire step:
//     the delivery worker has not acquired the turn lock, so the turn state
//     reads idle — checking only the turn state released the child here and
//     silently dropped the queued message (the parent got no reply and no
//     error);
//   - an exact reply result armed by a delivery but not yet delivered to
//     the parent (the acquire→start handoff and the turn-result callback).
//
// Quiet children are released.
func (sp *subagentSpawner) maybeReleaseChild(c *backgroundChild) {
	if c.busy() {
		time.AfterFunc(sp.retainWindow(), func() { sp.maybeReleaseChild(c) })
		return
	}
	sp.releaseChild(c)
}

// releaseChild unregisters and evicts a child's runtime. Idempotent. The
// saved session stays on disk and reopens via the sidebar. An in-flight
// turn (e.g. a send_message reply) is cancelled first so it cannot keep
// running on an evicted runtime. Only for children whose lifecycle started
// (running, interrupted-idle, or finished) — a spawn refused before its
// turn must go through discardUnstartedChild instead, or it would leave a
// phantom empty "failed" session file behind.
func (sp *subagentSpawner) releaseChild(c *backgroundChild) {
	sp.children.mu.Lock()
	if c.released {
		sp.children.mu.Unlock()
		return
	}
	c.released = true
	releasedMidRun := c.isRunning()
	delete(sp.children.children, c.id)
	sp.children.mu.Unlock()
	// A child released while its main job is running was cancelled
	// (parent teardown / cap): record the failed outcome BEFORE the drain,
	// so the cancelled turn's final flush persists it with the transcript.
	// Finished children keep the outcome already written at completion;
	// interrupted-idle children leave the status unset (still continuable).
	if releasedMidRun {
		c.markInterrupted()
		c.rt.agent.SetSubagentOutcome("failed", "cancelled")
		// Force the write: the cancelled turn's final flush may have
		// already run (turn end racing the teardown), in which case the
		// eviction's FlushPending would find the session clean and the
		// outcome would be lost — the sidebar would render the cancelled
		// child as done instead of failed.
		c.rt.agent.FlushSession()
	}
	if c.runCancel != nil {
		c.runCancel()
	}
	c.rt.stream.cancelInFlight() // active delivery turns unwind; queued ones are discarded by eviction
	c.rt.held.Store(false)
	sp.s.registry.evictRuntime(c.rt)
	// Releasing a child is an explicit teardown of its session: its own
	// children (grandchildren) are cancelled and released in turn.
	sp.s.registry.fireEvictHook(c.id)
	sp.recheckParent(c.parentID)
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

// discardUnstartedChild tears down a child refused BEFORE its turn ever
// started (the spawn-cap refusals in SpawnBackground). Unlike releaseChild
// it stamps no outcome and forces no flush: the child never ran, and a
// turn's persist is the only thing that ever writes its session file — so
// the flush-free eviction's FlushPending finds a clean session and writes
// NOTHING. That is the point: a refused spawn must not leave an empty
// "failed" session file + index entry behind, or the sidebar grows one
// phantom failed child row per refused spawn, for children that never
// existed (the pre-fix behavior — releaseChild's mid-run flush). The
// refusal error returned to the parent model is the refused child's only
// trace. Idempotent with releaseChild via the same released flag. Runs
// before any turn goroutine exists: there is no in-flight turn to cancel,
// and the same synchronous call that evicts the runtime is the only one
// that could have started a turn on it.
func (sp *subagentSpawner) discardUnstartedChild(c *backgroundChild) {
	sp.children.mu.Lock()
	if c.released {
		sp.children.mu.Unlock()
		return
	}
	c.released = true
	delete(sp.children.children, c.id)
	sp.children.mu.Unlock()
	if c.runCancel != nil {
		c.runCancel()
	}
	c.rt.held.Store(false)
	sp.s.registry.evictRuntime(c.rt)
	// Same teardown symmetry as releaseChild: the refused child cannot have
	// children of its own, but the hook keeps the "every child teardown
	// fires the evict hook" invariant future-proof.
	sp.s.registry.fireEvictHook(c.id)
	sp.recheckParent(c.parentID)
}

// cancelAll cancels and releases every live child of parentID. Wired to the
// registry eviction hook so a parent session leaving the registry (close,
// cap eviction, delete, shutdown) takes its children with it — a child
// whose parent is gone cannot deliver report/completion anyway.
func (sp *subagentSpawner) cancelAll(parentID string) {
	sp.children.mu.Lock()
	var victims []*backgroundChild
	for _, c := range sp.children.children {
		if c.parentID == parentID {
			victims = append(victims, c)
		}
	}
	sp.children.mu.Unlock()
	for _, c := range victims {
		if c.runCancel != nil {
			c.runCancel()
		}
		c.rt.stream.cancelInFlight() // stop an active turn; release/eviction drops queued turns
		sp.releaseChild(c)
	}
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
	// the whole turn, exactly like Spawn: acquireForeground reserves the
	// slot atomically with the cap check, and the deferred release frees
	// it on every exit path.
	ok, guardHit := sp.children.acquireForeground(parent.SessionID, sp.concurrentLimit(), sp.liveGuardLimit())
	if !ok {
		return "", fmt.Errorf("subagent_fork: %w", spawnCapError(sp.concurrentLimit(), guardHit))
	}
	defer sp.children.releaseForeground(parent.SessionID)
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

// ListAgents implements agent.ContinuableSubagentSpawner. Each child row
// carries its latest progress snippet (bounded, one line): the PULL path
// for a child's progress — nothing is pushed into the parent conversation
// between the spawn result and the run's completion.
func (sp *subagentSpawner) ListAgents(caller *agent.Agent) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("list_agents: caller agent is nil")
	}
	if sp.children == nil {
		return "", fmt.Errorf("list_agents: background subagents are not available in this mode")
	}
	children := sp.children.childrenOf(caller.SessionID)
	if len(children) == 0 {
		return "No live subagents.", nil
	}
	var b strings.Builder
	for _, c := range children {
		fmt.Fprintf(&b, "%s — %s — %s (depth %d)\n", c.id, c.label, c.statusOf(), c.depth)
		if line := c.latestProgress(); line != "" {
			fmt.Fprintf(&b, "  last: %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ownedBackgroundChild resolves a live child and enforces direct-parent
// ownership. Every result/wait/control operation uses this same guard.
func (sp *subagentSpawner) ownedBackgroundChild(caller *agent.Agent, agentID, operation string) (*backgroundChild, error) {
	if caller == nil {
		return nil, fmt.Errorf("%s: caller agent is nil", operation)
	}
	if sp.children == nil {
		return nil, fmt.Errorf("%s: background subagents are not available in this mode", operation)
	}
	child := sp.children.get(agentID)
	if child == nil {
		return nil, fmt.Errorf("%s: subagent %s is not live (use get_agent_result for a released child's persisted result)", operation, agentID)
	}
	if child.parentID != caller.SessionID {
		return nil, fmt.Errorf("%s: subagent %s is not a child of this session", operation, agentID)
	}
	return child, nil
}

// persistedAgentResult resolves a released child from the session store. The
// ParentID check is the authorization boundary: a model must not retrieve a
// nested session belonging to another parent.
func (sp *subagentSpawner) persistedAgentResult(caller *agent.Agent, agentID string) (string, bool) {
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

// GetAgentResult implements agent.ContinuableSubagentSpawner. Live children
// return their current status; terminal children return the in-memory result.
// After retention release, the persisted child outcome remains authoritative.
func (sp *subagentSpawner) GetAgentResult(caller *agent.Agent, agentID string) (string, error) {
	child, err := sp.ownedBackgroundChild(caller, agentID, "get_agent_result")
	if err != nil {
		if persisted, ok := sp.persistedAgentResult(caller, agentID); ok {
			return persisted, nil
		}
		return "", err
	}
	child.mu.Lock()
	status, result, runErr := child.status, child.result, child.err
	child.mu.Unlock()
	if status == "running" {
		line := child.latestProgress()
		if line != "" {
			return fmt.Sprintf("Subagent %s is still running. Latest progress: %s", agentID, line), nil
		}
		return fmt.Sprintf("Subagent %s is still running.", agentID), nil
	}
	if status == "idle" {
		return fmt.Sprintf("Subagent %s was interrupted before completion and remains available for a follow-up message.", agentID), nil
	}
	if runErr != nil {
		return fmt.Sprintf("Subagent %s failed: %v", agentID, runErr), nil
	}
	return fmt.Sprintf("Subagent %s finished.\n\n%s", agentID, agent.TruncateSubagentReport(result)), nil
}

// WaitAgent blocks on the child's main-run done channel, then delegates to
// GetAgentResult. The channel is a one-shot internal signal, not a payload
// transport: all authoritative data remains in the guarded child/store state.
func (sp *subagentSpawner) WaitAgent(ctx context.Context, caller *agent.Agent, agentID string, timeout time.Duration) (string, error) {
	child, err := sp.ownedBackgroundChild(caller, agentID, "wait_agent")
	if err != nil {
		if persisted, ok := sp.persistedAgentResult(caller, agentID); ok {
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
	case <-child.done:
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

func (c *backgroundChild) statusOf() string {
	c.mu.Lock()
	status := c.status
	c.mu.Unlock()
	if active, _ := c.rt.turnState(); active {
		return "running"
	}
	return status
}

// SendMessage implements agent.ContinuableSubagentSpawner: the message is
// delivered through the child's own delivery service (queued when the child
// is mid-turn); the exact reply result is delivered to the parent.
func (sp *subagentSpawner) SendMessage(caller *agent.Agent, agentID, text string) error {
	child, err := sp.ownedBackgroundChild(caller, agentID, "send_message")
	if err != nil {
		return err
	}
	// The message itself is delivered through the child's delivery service
	// (queued when the child is mid-turn); reply capture is armed by the
	// delivery-start hook when the delivered turn actually begins.
	// deliverChildMessage is the dedicated source: it is the only system
	// delivery that arms the capture (deliverFromChildMessage).
	ok, full := child.rt.deliverChildMessage(text)
	if !ok {
		if full {
			// Every slot of the child's system queue holds a targeted
			// delivery: refuse rather than silently dropping a message
			// whose reply the parent is waiting on. The error is
			// actionable — the model can retry after the child drains.
			return fmt.Errorf("send_message: subagent %s is busy and its delivery queue is full (%d queued messages); wait for it to finish its current turn or reply, then retry", agentID, maxSystemDeliveryQueue)
		}
		// The child's runtime was evicted between the registry lookup and
		// the queue append (e.g. the retention release racing this send):
		// report the loss instead of dropping the message silently — the
		// model can re-send or re-spawn.
		return fmt.Errorf("send_message: subagent %s is no longer live", agentID)
	}
	return nil
}

// InterruptAgent implements agent.ContinuableSubagentSpawner: cancels the
// child's current main or follow-up turn (no-op when idle); the child stays
// alive. runCancel closes the main run's pre-start registration window;
// the stream handle covers a follow-up turn once it has started.
func (sp *subagentSpawner) InterruptAgent(caller *agent.Agent, agentID string) error {
	child, err := sp.ownedBackgroundChild(caller, agentID, "interrupt_agent")
	if err != nil {
		return err
	}
	if child.runCancel != nil {
		child.runCancel()
	}
	child.rt.stream.cancelInFlight()
	return nil
}
