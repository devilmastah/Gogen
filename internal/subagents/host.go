// Package subagents implements the host-neutral lifecycle of continuable
// (background) subagents: the live-child registry, the per-parent
// concurrency caps, the retention/release policy, the exit classification,
// and the model-visible text of the control tools (list_agents,
// get_agent_result, wait_agent, send_message, interrupt_agent).
//
// The two hosts — the web server (internal/server) and the terminal UI
// (internal/tui) — differ only in how a child turn is executed and observed,
// so everything host-specific sits behind Host: how a child runtime is
// built, how one turn runs and is cancelled, how a release tears the runtime
// down, how the parent conversation receives a delivered message, and how
// the UI is notified. The lifecycle RULES live here once — including the
// ordering constraints the web host's fixes depend on (a refused spawn
// leaves no session artifact, reply capture is armed per targeted delivery,
// retention never releases a child whose work is in flight, an interrupted
// main run stays continuable and notifies nobody) — so the two hosts cannot
// drift.
package subagents

import (
	"context"
	"errors"
	"time"

	"gogen/internal/agent"
)

// Zero values in a Policy mean the default below.
const (
	// DefaultRetain is how long a finished child stays registered after its
	// main job completes before its runtime is released (the saved session
	// remains attachable).
	DefaultRetain = 10 * time.Minute
	// DefaultMaxFinished caps finished children per parent; beyond it the
	// oldest finished children are released.
	DefaultMaxFinished = 32
	// DefaultMaxLive caps NON-finished children per parent (running + idle
	// background + in-flight foreground). The user-facing limit counts only
	// ACTIVE children, so without this guard a spawn+interrupt loop could
	// accumulate unbounded runtimes. Spawning beyond it is refused with an
	// error — never silently released mid-task.
	DefaultMaxLive = 64
	// DefaultSystemQueueCap bounds a child's pending targeted (send_message)
	// deliveries, quoted in the actionable refusal error SendMessage returns
	// when every queue slot holds one. Hosts that bound the queue themselves
	// report the real figure through Policy.SystemQueueCap.
	DefaultSystemQueueCap = 32
)

// ErrEvicted reports that a child's host runtime was released while its turn
// ran (parent teardown, delete cascade, shutdown): the child is marked idle
// and the parent is NOT notified, because a released child has no
// conversation left to deliver a terminal event to. Hosts return it from
// RunTurn when their own release hook already tore the runtime down.
var ErrEvicted = errors.New("subagent: child runtime was released")

// Policy is one host's live lifecycle policy. Every field is read on each
// spawn/finish/retention decision (never cached at construction), so a host
// may change the policy at runtime — a settings toggle, or a test override.
type Policy struct {
	// Retain bounds how long a finished child stays registered
	// (0 = DefaultRetain).
	Retain time.Duration
	// MaxFinished caps finished children per parent
	// (0 = DefaultMaxFinished).
	MaxFinished int
	// MaxLiveGuard caps non-finished children per parent
	// (0 = DefaultMaxLive).
	MaxLiveGuard int
	// Concurrent returns the user-facing per-parent cap on ACTIVE children
	// (running background + in-flight foreground). nil means no active cap;
	// the MaxLiveGuard bound still applies.
	Concurrent func(parentID string) int
	// SystemQueueCap is the host's per-child targeted-delivery queue bound,
	// quoted in SendMessage's "queue is full" error
	// (0 = DefaultSystemQueueCap).
	SystemQueueCap int
}

// retain resolves the retention window.
func (p Policy) retain() time.Duration {
	if p.Retain > 0 {
		return p.Retain
	}
	return DefaultRetain
}

// maxFinished resolves the finished-children cap.
func (p Policy) maxFinished() int {
	if p.MaxFinished > 0 {
		return p.MaxFinished
	}
	return DefaultMaxFinished
}

// guardLimit resolves the non-finished-children cap.
func (p Policy) guardLimit() int {
	if p.MaxLiveGuard > 0 {
		return p.MaxLiveGuard
	}
	return DefaultMaxLive
}

// activeLimit resolves the user-facing active-children cap for parentID.
// A nil Concurrent (or a non-positive result) means no active cap; the
// guard limit still bounds the parent.
func (p Policy) activeLimit(parentID string) int {
	if p.Concurrent == nil {
		return 0
	}
	return p.Concurrent(parentID)
}

// systemQueueCap resolves the targeted-delivery queue bound.
func (p Policy) systemQueueCap() int {
	if p.SystemQueueCap > 0 {
		return p.SystemQueueCap
	}
	return DefaultSystemQueueCap
}

// PersistedChild is one SAVED nested (subagent) session of a caller, as the
// host's store records it: the view of a child whose runtime is gone —
// released after finishing, evicted, or left behind by a previous process.
//
// Status is the recorded outcome: "success", "failed", or "running" for a
// child that was mid-run and never reached a terminal state (interrupted, or
// its process went away). Legacy records may carry "". The report itself is
// NOT here on purpose: it can be tens of KiB, and get_agent_result already
// pulls it one child at a time.
type PersistedChild struct {
	ID       string
	Label    string
	Status   string
	Messages int
	// Updated is the session's last-update time, which orders the listing
	// (newest first). Zero when the store could not supply one; such an
	// entry sinks to the end instead of being dropped.
	Updated time.Time
}

// NewChild is what a host returns from Host.NewChild: its own runtime handle,
// the child agent, and the label derived from the job.
type NewChild[H any] struct {
	// Handle is the host's runtime/pane for the child. The lifecycle never
	// inspects it — it is carried so the host can recover its own object
	// from a child handed back by the registry.
	Handle H
	// Agent is the child session agent (required).
	Agent *agent.Agent
	// Label is the child's session label (agent.SubagentLabel(job), shared).
	Label string
}

// Host is the per-host seam the lifecycle drives. Implementations must be
// safe for concurrent use: the lifecycle calls them from the spawning
// tool's goroutine, from retention timers, and from control-tool calls.
type Host[H any] interface {
	// NewChild builds and registers the child's host runtime: the child
	// agent (with the shared model/thinking cascade applied), its pane or
	// runtime, and any approval routing. ctx is the spawning tool's context
	// (used for model selection, which may dial the provider). It must NOT
	// announce the child — Started runs only after the child is admitted by
	// the caps.
	NewChild(ctx context.Context, parent *agent.Agent, job, model string, depth int) (NewChild[H], error)
	// RunTurn runs one turn on the child and returns its final output. ctx
	// survives the spawning tool call for background children (it is owned
	// by the lifecycle, not by the parent turn). A turn cancelled by the
	// host's own teardown must return ErrEvicted.
	RunTurn(ctx context.Context, c *Child[H], prompt string) (string, error)
	// CancelTurn cancels the child's in-flight follow-up turn (no-op when
	// idle). The child itself stays alive and continuable.
	CancelTurn(c *Child[H])
	// DeliverChildMessage queues a targeted (send_message) delivery into the
	// child, queued when the child is mid-turn. ok=false means nothing was
	// queued: full=true reports a queue holding only targeted deliveries, so
	// the caller reports an actionable error instead of dropping a message
	// whose reply the parent is waiting on.
	DeliverChildMessage(c *Child[H], text string) (ok, full bool)
	// ArmReplyCapture wires the host's delivery hooks so the next targeted
	// delivery arms Child.BeginReply and reports that exact turn's output and
	// error to Child.OnTurnResult. Called once per background child, at spawn.
	ArmReplyCapture(c *Child[H])
	// Release tears the child's host resources down. The lifecycle has
	// already removed the child from the registry and marked it released;
	// cancelledMidRun reports that the main job was still running (the
	// cancelled outcome must be recorded before the runtime is evicted, or
	// the sidebar renders it as done). The host also cascades to the child's
	// own children and re-checks the parent for orphan eviction.
	Release(c *Child[H], cancelledMidRun bool)
	// Deliver routes one typed child event into the parent conversation (the
	// host's delivery service; queued for the session's next registration
	// when the parent is not live). The EVENT crosses this seam, not a
	// pre-flattened string: its Kind is what tells the host whether somebody
	// is waiting on the text (EventReply), and that decides how a delivery
	// which has to wait for the parent is ordered against the others.
	Deliver(event Event)
	// Started announces an ADMITTED child (its pane/sidebar row's attach
	// target). Called after the caps pass, so a refused child is never
	// announced — an announced-but-refused child would leave a phantom
	// running row no event ever resolves. job is the RAW job (labels keep
	// the original text the parent wrote).
	Started(c *Child[H], job string)
	// Finished announces a child's terminal state.
	Finished(c *Child[H], report string, err error)
	// PersistedResult resolves a released child from the session store,
	// applying the caller-ownership check (a model must not retrieve a
	// nested session belonging to another parent). ok=false when the store
	// has no usable record.
	PersistedResult(caller *agent.Agent, childID string) (string, bool)
	// ReviveChild reopens one of the caller's SAVED nested sessions as a live
	// runtime so a send_message can reach it again (after its retention
	// window, an eviction, or a restart — the live registry is process-local,
	// the session is not). It must keep the session's id, restore its parent
	// link, and refuse (ok=false, no error) when the store has no such child
	// OR it belongs to another parent: the caller-ownership check is the same
	// boundary PersistedResult applies. Returning ok=false *and* an error is
	// redundant; an error means "the store had it but could not open it".
	ReviveChild(caller *agent.Agent, childID string) (nc NewChild[H], ok bool, err error)
	// PersistedChildren returns the caller's SAVED nested (subagent)
	// sessions, newest first — the children whose runtime is not live
	// (released, evicted, or from a previous process). The lifecycle drops
	// any id it still tracks live, so a child is never listed twice, and it
	// never sees another parent's children: the ownership boundary is the
	// same one PersistedResult applies. nil when the host has no store.
	PersistedChildren(caller *agent.Agent) []PersistedChild
	// TurnActive reports whether the child is mid-turn on a follow-up
	// delivery (the status reads "running" until it ends).
	TurnActive(c *Child[H]) bool
	// Busy reports whether a release would cut short lifecycle work: an
	// active turn, a delivery queued for the host's worker but not yet
	// started, or Child.PendingReply (an exact reply armed but not yet
	// delivered — omitting it silently drops send_message replies on
	// release). A runtime that can no longer run that work (evicted, torn
	// down) must report false, or the armed capture would re-arm the
	// retention timer forever.
	Busy(c *Child[H]) bool
	// Policy returns the host's current policy. Read on every decision.
	Policy() Policy
}
