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
// (interrupt_agent), and listed (list_agents, including each child's latest
// progress line — the pull path). Web host only in v1 (the TUI spawner
// stays foreground-only, so
// its agents never see the continuable tools or the run_in_background
// parameter).
//
// Lifecycle:
//   - SpawnBackground creates and registers the child exactly like the
//     foreground Spawn, then runs the turn in a goroutine instead of
//     blocking. The runtime is marked held, so orphan eviction (no clients,
//     no turn) never collects it mid-lifecycle.
//   - On natural completion the final report is delivered to the parent
//     conversation (delivery service; queued for the session's next
//     registration when the parent runtime is not live — an
//     orphan-evicted/closed parent must not lose the outcome) and a
//     retention timer is armed.
//   - Finished children stay registered for a retention window (bounded
//     memory), then are released: held cleared, runtime evicted (the saved
//     session stays attachable via the sidebar).
//   - A per-parent cap bounds how many finished children accumulate.
//   - When the PARENT session is evicted (closed/cap/orphan), all of its
//     children are cancelled and released (fail-safe: a child whose parent
//     is gone cannot deliver anyway).
//   - interrupt_agent cancels only the in-flight turn; the child stays
//     alive and continuable (no completion notice for an interruption).

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
	sp        *subagentSpawner // owning spawner (for parent lookup in onTurnEnd)

	mu           sync.Mutex
	status       string // "running" | "idle" | "finished"
	result       string // final report (status finished)
	err          error  // final error (status finished)
	finishedAt   time.Time
	released     bool
	pendingReply bool // a send_message delivery is in flight; capture the next turn's reply
	replyBase    int  // len(SnapshotMessages()) when the delivery turn started
	// turnStart is when the captured delivery turn started (set with
	// pendingReply). The reply scan's index baseline (replyBase) stops
	// bounding the turn when auto-compaction renumbers the history mid-turn;
	// the scan then falls back to the assistant messages appended at or
	// after this timestamp.
	turnStart time.Time
}

// childRegistry owns the live continuable children of a server.
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

func (cr *childRegistry) get(id string) *backgroundChild {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.children[id]
}

// childrenOf returns the live children of parentID (id → child), newest
// last (registration order, by createdAt).
func (cr *childRegistry) childrenOf(parentID string) []*backgroundChild {
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

func (c *backgroundChild) setStatus(s string) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

// finish records the main-job outcome and closes done.
func (c *backgroundChild) finish(report string, err error) {
	c.mu.Lock()
	c.status = "finished"
	c.result = report
	c.err = err
	c.finishedAt = time.Now()
	c.mu.Unlock()
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
// worker but not yet turned into one, or a captured reply not yet delivered
// to the parent. The last two cover the delivery handoffs the turn state
// alone reads as idle: the worker holds the queued item until it has
// ACQUIRED the turn lock (deliverLoop's peek-not-pop contract), and
// deliverStartHook arms pendingReply before startTurn sets the turn active
// — with the mirror-image window at turn end, where setTurnActive(false)
// runs before the turn-end hook consumes the reply. Takes no locks on
// entry; c.mu, rt.stateMu and rt.deliverMu are leaf locks (the documented
// children.mu → c.mu order, never the reverse).
func (c *backgroundChild) busy() bool {
	// An evicted runtime can no longer run the work the checks below
	// protect. In particular, a capture armed by a delivery whose turn
	// never started (the evicted early return in runTurnBody skips the
	// turn-end hook that consumes pendingReply) would otherwise read busy
	// forever and re-arm the retention timer — pinning a dead record.
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

// onTurnEnd is installed as the child runtime's turn-end hook: when a
// send_message delivery's turn has ended, the child's last assistant reply
// is injected into the parent conversation as ONE bounded message (delivery
// service; dropped when the parent is gone) — the single return channel for
// that run.
func (c *backgroundChild) onTurnEnd() {
	c.mu.Lock()
	if !c.pendingReply {
		c.mu.Unlock()
		return
	}
	c.pendingReply = false
	base := c.replyBase
	turnStart := c.turnStart
	c.mu.Unlock()

	msgs := c.rt.agent.SnapshotMessages()
	var reply string
	// Only messages the delivered turn actually produced count as the
	// reply: an interrupted turn (no completed round) appends nothing, so
	// the scan finds nothing and no stale assistant message is re-sent.
	for i := len(msgs) - 1; i >= base; i-- {
		if msgs[i].Role == "assistant" && msgs[i].Content != "" {
			reply = msgs[i].Content
			break
		}
	}
	// The delivered turn may have auto-compacted the child's history,
	// renumbering the messages so the index baseline no longer bounds the
	// turn (the reply can sit BELOW base). Fall back to the timestamp
	// floor: the delivered turn appends its assistant message at or after
	// turnStart. Zero-timestamp fixtures (hand-built records in tests)
	// skip the fallback, exactly like the pre-compaction code.
	if reply == "" && !turnStart.IsZero() {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != "assistant" || msgs[i].Content == "" || msgs[i].CreatedAt.IsZero() {
				continue
			}
			if !msgs[i].CreatedAt.Before(turnStart) {
				reply = msgs[i].Content
				break
			}
		}
	}
	if reply == "" {
		return
	}
	// Deliver to the live parent, or queue for its next registration when
	// the parent runtime is not live (orphan-evicted/closed mid-reply).
	// The reply is bounded like the completion notice: one message per run,
	// never an unbounded transcript dump (OpenHands' context-flood issue;
	// Anthropic's "pass lightweight references").
	c.sp.s.registry.deliverToParent(c.parentID,
		fmt.Sprintf("[subagent %s] %s", c.label, agent.TruncateSubagentReport(reply)))
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
	child := &backgroundChild{
		id:        childRt.agent.SessionID,
		parentID:  parent.SessionID,
		label:     label,
		depth:     depth + 1,
		createdAt: time.Now(),
		rt:        childRt,
		sp:        sp,
		status:    "running",
	}
	sp.children.register(child)
	// Held: orphan eviction must never collect a continuable child while
	// it is alive or awaiting release.
	childRt.held.Store(true)
	// Reply capture: pendingReply is armed by the delivery-start hook (the
	// send_message turn ACTUALLY starting — not when it was queued) and
	// consumed by the turn-end hook, which injects the reply into the
	// parent.
	childRt.turnEndHook = child.onTurnEnd
	childRt.deliverStartHook = func() {
		child.mu.Lock()
		child.pendingReply = true
		// Baseline the child's history at delivery start so reply capture
		// can only pick up messages the delivered turn actually produced —
		// an interrupted turn (no completed round) must not re-deliver an
		// older assistant message as the reply. turnStart is the same
		// baseline as a timestamp, for the scan's compaction fallback.
		child.replyBase = len(child.rt.agent.SnapshotMessages())
		child.turnStart = time.Now().Truncate(time.Millisecond)
		child.mu.Unlock()
	}
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
		report, runErr := sp.runChildTurn(context.Background(), childRt, agent.FormatSubagentJob(rawJob))
		// Sweep the orphan on EVERY exit path: a concurrent DELETE of the
		// parent cascades the child's file away and a write that lands
		// after that delete (the cancelled turn's final flush, or the
		// release path's outcome flush) resurrects the child as an
		// invisible orphan. The old finish-path-only check left interrupted
		// and released children un-swept. (The eviction loop's own sweep
		// covers the window before this check runs.)
		sp.sweepOrphanedChild(childRt, parent.SessionID, child.id)
		if childRt.evicted.Load() {
			return // released while running (parent close / retention) — nothing to notify
		}
		if runErr != nil && errors.Is(runErr, context.Canceled) {
			// Interrupted (interrupt_agent / child-pane cancel): the child
			// stays alive and continuable; no completion notice. Arm the
			// retention timer so an idle interrupted child cannot linger in
			// memory forever (the parent's lifetime no longer bounds it:
			// orphan eviction does not cancel children).
			child.setStatus("idle")
			time.AfterFunc(sp.retainWindow(), func() { sp.maybeReleaseChild(child) })
			return
		}
		child.finish(report, runErr)
		// Persist the final outcome on the child's snapshot (the events
		// are not replayed after a reload/restart, so the saved session is
		// what the sidebar falls back to).
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
		// Deliver the completion notice — immediately when the parent is
		// live, queued for its next registration otherwise (deliverToParent
		// re-resolves the runtime, so this also covers the eviction race
		// between the lookup above and the queue append). This is the
		// child's ONE return channel for the run: state (finished/failed)
		// plus the bounded outcome text. Nothing else pushes into the
		// parent conversation, so there is nothing to dedup.
		notice := fmt.Sprintf("[subagent %s] %s", label, verb)
		if summary != "" {
			notice += ": " + summary
		}
		s.registry.deliverToParent(parent.SessionID, notice)
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
		// Oldest-first, but never a child whose reply work is still in
		// flight (an active send_message turn, a delivery queued for the
		// worker, or a captured reply): releaseChild would cancel the
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
//   - a reply captured but not yet delivered to the parent (the delivery
//     turn's acquire→start handoff and the end→hook window).
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
		c.rt.agent.SetSubagentOutcome("failed", "cancelled")
		// Force the write: the cancelled turn's final flush may have
		// already run (turn end racing the teardown), in which case the
		// eviction's FlushPending would find the session clean and the
		// outcome would be lost — the sidebar would render the cancelled
		// child as done instead of failed.
		c.rt.agent.FlushSession()
	}
	c.rt.stream.cancelInFlight() // no-op when idle
	c.rt.held.Store(false)
	sp.s.registry.evictRuntime(c.rt)
	// Releasing a child is an explicit teardown of its session: its own
	// children (grandchildren) are cancelled and released in turn.
	sp.s.registry.fireEvictHook(c.id)
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
	c.rt.held.Store(false)
	sp.s.registry.evictRuntime(c.rt)
	// Same teardown symmetry as releaseChild: the refused child cannot have
	// children of its own, but the hook keeps the "every child teardown
	// fires the evict hook" invariant future-proof.
	sp.s.registry.fireEvictHook(c.id)
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
		c.rt.stream.cancelInFlight() // stop an in-flight turn
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
	sp.finalizeForegroundChild(childRt, parentRt, label, report, err)
	if err != nil {
		return "", fmt.Errorf("subagent_fork %s: %w", newID, err)
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
// is mid-turn); the child's reply is captured by the turn-end hook and
// injected into the parent.
func (sp *subagentSpawner) SendMessage(caller *agent.Agent, agentID, text string) error {
	if caller == nil {
		return fmt.Errorf("send_message: caller agent is nil")
	}
	child := sp.children.get(agentID)
	if child == nil {
		return fmt.Errorf("send_message: subagent %s is not live (finished children are released after their retention window)", agentID)
	}
	if child.parentID != caller.SessionID {
		return fmt.Errorf("send_message: subagent %s is not a child of this session", agentID)
	}
	// The message itself is delivered through the child's delivery service
	// (queued when the child is mid-turn); reply capture is armed by the
	// delivery-start hook when the delivered turn actually begins.
	// deliverChildMessage is the dedicated source: it is the only system
	// delivery that arms the capture (deliverFromChildMessage).
	if !child.rt.deliverChildMessage(text) {
		// The child's runtime was evicted between the registry lookup and
		// the queue append (e.g. the retention release racing this send):
		// report the loss instead of dropping the message silently — the
		// model can re-send or re-spawn.
		return fmt.Errorf("send_message: subagent %s is no longer live", agentID)
	}
	return nil
}

// InterruptAgent implements agent.ContinuableSubagentSpawner: cancels the
// child's in-flight turn (no-op when idle); the child stays alive.
func (sp *subagentSpawner) InterruptAgent(caller *agent.Agent, agentID string) error {
	if caller == nil {
		return fmt.Errorf("interrupt_agent: caller agent is nil")
	}
	child := sp.children.get(agentID)
	if child == nil {
		return fmt.Errorf("interrupt_agent: subagent %s is not live", agentID)
	}
	if child.parentID != caller.SessionID {
		return fmt.Errorf("interrupt_agent: subagent %s is not a child of this session", agentID)
	}
	child.rt.stream.cancelInFlight()
	return nil
}
