package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	sesspkg "gogen/internal/session"
	"gogen/internal/subagents"
)

// subagentSpawner implements agent.SubagentSpawner for the web server:
// children are FULL session runtimes — attachable panes with live streaming
// (D2), registered under their parent, exempt from cap eviction, with delete
// approvals routed to the parent's clients when no child client is attached
// (D6). The child runs with its own turn lock and context window; the parent
// tool call blocks until the child finishes (or the parent turn is
// cancelled, which propagates).
//
// When the spawner implements agent.ContinuableSubagentSpawner (it does) the
// subagent tool gains run_in_background and the control tools; children then
// stay registered past their main turn (held, retention-bounded) for
// messaging.
//
// The embedded Spawner owns that continuable lifecycle (admission, caps,
// retention, release, and the control tools — internal/subagents); this type
// supplies the host-specific halves through subagents.Host: runtime
// construction, turn execution, the sidebar broadcasts, and parent delivery
// (see subagent_continuable.go).
type subagentSpawner struct {
	*subagents.Spawner[*sessionRuntime]
	s *Server

	// retain bounds how long a finished continuable child stays registered
	// (0 = subagents.DefaultRetain). Tests shorten it.
	retain time.Duration
	// maxFinished caps finished continuable children per parent
	// (0 = subagents.DefaultMaxFinished).
	maxFinished int
	// maxLive overrides the user-facing per-parent cap on ACTIVE subagents
	// (0 = the configured subagent_max_concurrent limit — see
	// concurrentLimit).
	maxLive int
	// maxLiveGuard overrides the internal per-parent cap on non-finished
	// children (0 = subagents.DefaultMaxLive).
	maxLiveGuard int
}

// newSubagentSpawner wires the web host to the shared continuable lifecycle.
// The host pointer is required up front (the lifecycle calls back into it),
// so the two are constructed in this one place — NewServer and tests both use
// it, and a spawner built any other way has no lifecycle to drive.
func newSubagentSpawner(s *Server) *subagentSpawner {
	sp := &subagentSpawner{s: s}
	sp.Spawner = subagents.New[*sessionRuntime](sp)
	return sp
}

// truncateReport bounds the tool result returned to the parent agent.
func truncateReport(report string, err error) string {
	if err != nil {
		return err.Error()
	}
	return agent.TruncateSubagentReport(report)
}

// newChildRuntime creates and registers a nested child runtime for parent:
// fresh session agent (model selection, label, depth, parent link), D6
// approval forwarding, registry registration. Returns the child runtime,
// its label, and the RAW (unwrapped) job. Shared by the foreground Spawn
// and the background SpawnBackground so the two cannot drift.
//
// The factory does NOT announce the child: the subagent_started broadcast
// belongs to the caller, AFTER admission. Spawn and Fork have already
// cleared their caps when the child is created, so they announce
// immediately; SpawnBackground checks its caps after registration and must
// discard a refused child WITHOUT announcing it — an announced child that
// is then refused would leave a phantom "running" row in the parent's
// sidebar that no event ever resolves.
func (sp *subagentSpawner) newChildRuntime(ctx context.Context, parent *agent.Agent, job, model string, depth int) (*sessionRuntime, string, string, error) {
	s := sp.s
	if parent == nil {
		return nil, "", "", fmt.Errorf("subagent: parent agent is nil")
	}
	// Liveness guard only (the D6 approver re-resolves the parent by id at
	// approval time): the spawn must fail when the parent's runtime is gone.
	if _, ok := s.registry.get(parent.SessionID); !ok {
		return nil, "", "", fmt.Errorf("subagent: parent session is not live")
	}

	childID := sesspkg.NewID()
	child := s.ws.NewSessionAgent(nil, childID)
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	runtimeCfg := s.ws.GetRuntimeConfig()
	// Model cascade (fully shared with the TUI spawner — see
	// ResolveSubagentModel/ApplySubagentModel): explicit tool argument >
	// configured subagent model > the parent's live model (unless it is
	// the workspace default the child is already seeded with). The
	// explicit argument is a hard requirement: an unselectable model
	// fails the spawn. The other tiers fall back to the default rather
	// than failing — it is already on the provider (fail open). The
	// parent tier passes the qualified selector form ("profile/id" for a
	// non-default owner) so the child selects the SAME (profile, ID) the
	// parent runs on, not a same-ID model on the default profile.
	m, src := agent.ResolveSubagentModel(model, runtimeCfg.SubagentModel, parent.CurrentModelRef().String(), s.ws.DefaultModel())
	if err := agent.ApplySubagentModel(ctx, child, m, src); err != nil {
		return nil, "", "", err
	}
	// The reasoning effort follows the same cascade as the model: the
	// configured subagent level (settings) wins; empty = inherit the
	// parent's live level. Applied AFTER the model cascade so validity is
	// resolved against the child's final model (a level it does not accept
	// is omitted — see ApplySubagentThinkingLevel).
	agent.ApplySubagentThinkingLevel(child, parent, runtimeCfg.SubagentThinkingLevel)
	label := agent.SubagentLabel(job)
	child.RenameSession(label)
	// The job wrapper applies AFTER label derivation: labels and the
	// subagent_started event keep the original job the parent wrote; only
	// the child's first message carries the wrapped job.
	rawJob := job

	childRt := newSessionRuntimeWithHold(child, sp.approvalHold())
	childRt.parentID = parent.SessionID
	childRt.nested = true
	// D6: delete approvals go to the child's own attached clients (the
	// child pane shows the modal); with none attached they route to the
	// parent's clients so a headless child can never hang an approval. The
	// parent is resolved by id at approval time (routeApprovalsTo) — the
	// runtime pointer captured here goes stale when the parent is
	// orphan-evicted and reopened while the child keeps running.
	childRt.routeApprovalsTo(s.registry, parent.SessionID)

	// Register BEFORE the turn so attach/cancel/approvals resolve. Nested
	// runtimes are cap-exempt, so register never evicts.
	s.registry.register(childID, childRt)

	// Children never push progress into the parent conversation. A run's
	// result is the foreground Spawn tool result or a compact background
	// terminal event plus get_agent_result; send_message adds one exact
	// reply. Progress remains pull-side through list_agents and the child pane.

	return childRt, label, rawJob, nil
}

func (sp *subagentSpawner) Spawn(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	s := sp.s
	// The foreground child holds a slot of the concurrent limit for the
	// whole turn, exactly like a background child: AcquireForeground
	// reserves the slot atomically with the cap check (active children
	// against the user-facing limit, non-finished against the internal
	// guard), and the deferred release frees it on every exit path.
	ok, guardHit := sp.AcquireForeground(parent.SessionID)
	if !ok {
		return "", sp.SpawnCapError(parent.SessionID, guardHit)
	}
	defer sp.ReleaseForeground(parent.SessionID)
	childRt, label, rawJob, err := sp.newChildRuntime(ctx, parent, job, model, depth)
	if err != nil {
		return "", err
	}
	childID := childRt.agent.SessionID
	parentRt, _ := s.registry.get(parent.SessionID) // still live: registered above

	// Announce the admitted child (the sidebar row's attach target). The
	// caps were checked before the child existed, so every Spawn that gets
	// here is admitted — announce unconditionally, like Fork does below.
	parentRt.broadcast(WSMessage{
		Type:           "subagent_started",
		SessionID:      parent.SessionID,
		SubagentID:     childID,
		SubagentLabel:  label,
		SubagentJob:    truncateJob(rawJob),
		SubagentParent: parent.SessionID,
	})

	report, err := sp.runChildTurn(ctx, childRt, agent.FormatSubagentJob(rawJob))
	// Every foreground return is bounded by the documented subagent-report
	// cap. The generic tool-result cap may be lower, but must never be the
	// only guard for a subagent report.
	report = agent.TruncateSubagentReport(report)
	sp.finalizeForegroundChild(childRt, parentRt, label, report, err)
	if err != nil {
		return report, fmt.Errorf("subagent %s: %w", childID, err)
	}
	return report, nil
}

// sweepOrphanedChild removes the child's saved file when a concurrent
// DELETE of the parent won the race against a child write that landed
// after it: the runtime was evicted as part of the delete cascade and the
// parent's file is gone, so the write resurrected the child as an
// invisible orphan (excluded from the flat list/prune/latest). Best
// effort — the delete/eviction paths' own sweeps cover the window before
// this check runs.
func (sp *subagentSpawner) sweepOrphanedChild(rt *sessionRuntime, parentID, childID string) {
	if sp.s.ws.Store == nil {
		return // no persistence: no file to resurrect
	}
	if rt.evicted.Load() && sp.s.ws.Store.Info(sp.s.ws.GetWorkingDir(), parentID) == nil {
		_ = sp.s.ws.Store.Delete(sp.s.ws.GetWorkingDir(), childID)
	}
}

// finalizeForegroundChild runs the shared post-turn epilogue for a
// foreground child (Spawn and Fork), in a fixed order that encodes the
// persistence/orphan contract: the outcome flush may resurrect the
// child's file after a concurrent parent DELETE, so the orphan sweep runs
// AFTER the flush; the registry removal, the detach notification and the
// finished broadcast then run exactly once, here, so the contract has a
// single definition.
func (sp *subagentSpawner) finalizeForegroundChild(rt, parentRt *sessionRuntime, label string, report string, err error) {
	rt.agent.FinishSubagentOutcome(report, err)
	sp.sweepOrphanedChild(rt, parentRt.agent.SessionID, rt.agent.SessionID)

	// The child is persisted by its turn (doPersist); unregistering keeps
	// the registry clean — the saved session stays attachable via the
	// sidebar row (ensureSessionRuntime reloads it).
	sp.s.registry.remove(rt.agent.SessionID)
	// Attached child panes must close: the runtime is gone, and a message
	// sent to it would be silently dropped (the evicted guard). Same
	// notification as the eviction path — the session stays in the saved
	// list and reopens from the store.
	rt.broadcast(WSMessage{Type: "session_detached", SessionID: rt.agent.SessionID})
	parentRt.broadcast(WSMessage{
		Type:            "subagent_finished",
		SessionID:       parentRt.agent.SessionID,
		SubagentID:      rt.agent.SessionID,
		SubagentLabel:   label,
		SubagentParent:  parentRt.agent.SessionID,
		SubagentSuccess: err == nil,
		SubagentSummary: truncateReport(report, err),
	})
}

// routeApprovalsTo wires the D6 delete-approval routing for a nested
// child: delete approvals go to the child's own attached clients (the
// child pane shows the modal); with none attached they route to the
// parent's clients so a headless child can never hang an approval. The
// parent is resolved by id AT APPROVAL TIME (nestedDeleteApprover), never
// via the runtime pointer captured at spawn: the parent can be
// orphan-evicted while its background child keeps running and later
// reopened as a fresh runtime under the same id, and an approval routed to
// the dead pointer would wait forever. An approval nobody can answer is
// denied fail-closed instead of being parked.
func (rt *sessionRuntime) routeApprovalsTo(reg *sessionRegistry, parentID string) {
	rt.approverOverride = reg.nestedDeleteApprover(rt, parentID)
}

// approvalHold returns the configured delete-approval hold window. Reads
// the live runtime overlay (like newSessionRuntimeFor), so a settings-modal
// web_approval_hold_secs change applies to children spawned afterwards.
func (sp *subagentSpawner) approvalHold() time.Duration {
	if sp.s != nil {
		return sp.s.ws.ApprovalHold()
	}
	return 0
}

// runChildTurn runs one full agent turn on the child runtime: the child's
// stream handles are registered so the Cancel button on an attached child
// pane works, events broadcast to the child's clients (live transcript when
// attached), and the parent context propagates cancellation. The turn
// skeleton itself (evicted check, turn-active lifecycle, stream handlers,
// error tail) is shared with startTurn via runTurnBody. The child's own
// turnMu serializes against attach/approval handlers.
func (sp *subagentSpawner) runChildTurn(parentCtx context.Context, rt *sessionRuntime, job string) (string, error) {
	// Derive the run directly from the supplied owner context. Foreground
	// callers pass the parent tool context, so cancellation propagates without
	// a relay goroutine; background callers pass their child-owned context,
	// which intentionally survives the spawning tool call. Installed before
	// stream registration, so an already-cancelled context is observed even
	// if the provider has not started.
	runCtx, runCancel := context.WithCancel(parentCtx)
	defer runCancel()
	errCh := rt.stream.begin(runCancel)

	type turnResult struct {
		out string
		err error
	}
	resCh := make(chan turnResult, 1)
	go func() {
		defer rt.releaseTurn()
		defer rt.stream.end()
		defer func() { errCh <- nil }()
		rt.turnMu.Lock()
		// Child turn options: no owner connection, no positioned token
		// frames, no error log/hook (the parent sees the error as the tool
		// result), no persist warning/usage frame (the spawner flushes the
		// outcome).
		out, err := rt.runTurnBody(runCtx, job, nil, turnOpts{})
		if errors.Is(err, errTurnEvicted) {
			// The runtime was released while we waited for the lock
			// (close/delete/release evict without holding turnMu): nothing
			// to stream to. The defers above still run, so the stream's
			// cancel handle is cleared (begin already ran) and
			// cancelInFlight's errCh wait is released instead of blocking
			// until the drain timeout. The caller's evicted check handles
			// the rest.
			err = fmt.Errorf("child runtime was released while starting")
		}
		resCh <- turnResult{out: out, err: err}
	}()

	select {
	case res := <-resCh:
		// Cancel the child context so the watcher used by an earlier
		// implementation (or a provider retaining the context) observes the
		// terminal transition. Background ownership is supplied by runCtx.
		runCancel()
		// A cancelled parent turn surfaces as a child error; translate it
		// so the parent agent sees a clear reason.
		if parentCtx.Err() != nil && res.err == nil {
			return res.out, parentCtx.Err()
		}
		return res.out, res.err
	case <-parentCtx.Done():
		runCancel()
		<-resCh // let the child unwind (bounded by its own turn)
		return "", parentCtx.Err()
	}
}

func truncateJob(job string) string {
	return contextmgr.Truncate(job, 200, contextmgr.TruncateOptions{Marker: "…"})
}
