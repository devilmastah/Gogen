package subagents

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
)

// MaxProgressLine bounds a child's progress snippet in list_agents. It is a
// listing line, not content: the full transcript stays in the child's pane.
const MaxProgressLine = 160

// Child is one live continuable subagent.
//
// The identity fields (ID, ParentID, Label, Depth, Agent, CreatedAt, Handle)
// are immutable after construction. The lifecycle fields are guarded by mu
// and may be read from any goroutine — that is what makes the control tools
// (called from the parent's tool goroutine) safe while the child's turn runs
// elsewhere.
//
// A child has exactly ONE logical return path per run: its final message —
// the foreground Spawn result, or for a background child a compact terminal
// event plus the authoritative GetAgentResult/WaitAgent pull — plus one
// exact reply per send_message. Children never push interim progress into
// the parent conversation as turns; the parent PULLS progress with
// list_agents and the user watches the child's pane.
type Child[H any] struct {
	ID        string
	ParentID  string
	Label     string
	Depth     int
	CreatedAt time.Time
	Agent     *agent.Agent
	// Handle is the host's own runtime/pane for this child; the lifecycle
	// never inspects it.
	Handle H

	// runCancel cancels the main run's context. Installed before the child is
	// published in the registry and valid for the main run's whole lifetime,
	// so an interrupt can never miss the pre-start window in which the turn
	// goroutine has not registered a stream handle yet.
	runCancel context.CancelFunc
	// done is closed exactly once when the main run reaches a terminal or an
	// interrupted state. WaitAgent waits on this one-shot signal and then
	// reads the authoritative result from the guarded fields.
	done     chan struct{}
	doneOnce sync.Once

	mu           sync.Mutex
	status       string // "running" | "idle" | "finished"
	result       string // final report (status finished)
	err          error  // final error (status finished)
	finishedAt   time.Time
	released     bool
	pendingReply bool // a targeted delivery is in flight; capture its exact result
}

// EventKind distinguishes the two child-to-parent events.
type EventKind uint8

const (
	// EventTerminal is a background child's compact completion event: the
	// authoritative (possibly large) report is pulled with get_agent_result,
	// so completion bursts cannot flood the parent's context.
	EventTerminal EventKind = iota
	// EventReply is the exact result of one targeted send_message delivery.
	EventReply
)

// Event is the typed, host-mediated child-to-parent event. Text is produced
// only at the delivery boundary; identity, kind, and result stay structured
// for routing and tests. A raw Go channel is deliberately NOT exposed to the
// model or the parent Agent: transcript writes belong to the parent's
// serialized delivery worker (web) / idle-boundary queue (TUI).
type Event struct {
	Kind     EventKind
	ChildID  string
	ParentID string
	Label    string
	Text     string
	Err      error
}

// ParentMessage renders the event as the one message the parent conversation
// receives. Shared by both hosts so the notice text cannot drift.
func (e Event) ParentMessage() string {
	switch e.Kind {
	case EventTerminal:
		return e.Text
	case EventReply:
		if e.Text == "" {
			if e.Err != nil {
				return Notice(e.Label, fmt.Sprintf("no reply (the turn failed: %v)", e.Err))
			}
			return Notice(e.Label, "no reply (the turn failed or was interrupted before producing one)")
		}
		return Notice(e.Label, agent.TruncateSubagentReport(e.Text))
	default:
		return e.Text
	}
}

// Notice is a subagent event as its parent reads it: "[subagent <name>]
// <text>". name is the child's label, or its id for the terminal notice
// (the id resolves duplicate or truncated labels; the text names the label).
// Every subagent notice is built here, so compaction can tell it from a user
// request by contextmgr.SubagentNoticePrefix.
func Notice(name, text string) string {
	return contextmgr.SubagentNoticePrefix + name + "] " + text
}

// DeliverEvent routes one typed child event to its parent through the host's
// delivery service, stamping the identity the host needs for routing and
// carrying the Kind: the host decides both how the text renders
// (Event.ParentMessage) and how a delivery that must wait for the parent is
// ordered (EventReply is never the eviction victim).
func (c *Child[H]) DeliverEvent(host Host[H], event Event) {
	event.ChildID = c.ID
	event.ParentID = c.ParentID
	event.Label = c.Label
	host.Deliver(event)
}

// Finish records the main-run outcome and closes Done.
func (c *Child[H]) Finish(report string, err error) {
	c.mu.Lock()
	c.status = "finished"
	c.result = report
	c.err = err
	c.finishedAt = time.Now()
	c.mu.Unlock()
	c.doneOnce.Do(func() { close(c.done) })
}

// MarkInterrupted records the main run's intentional cancellation and wakes
// waiters without manufacturing a terminal result. The child remains
// continuable through send_message.
func (c *Child[H]) MarkInterrupted() {
	c.mu.Lock()
	if c.status == "running" {
		c.status = "idle"
	}
	c.mu.Unlock()
	c.doneOnce.Do(func() { close(c.done) })
}

// IsFinished reports whether the main job reached a terminal state.
func (c *Child[H]) IsFinished() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status == "finished"
}

// IsRunning reports whether the main job has not completed or been
// interrupted yet.
func (c *Child[H]) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status == "running"
}

// Status returns the recorded main-run status: "running", "idle"
// (interrupted, still continuable), or "finished".
func (c *Child[H]) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// StatusOf returns the status a listing shows, upgrading an idle child to
// "running" while a follow-up delivery turn is in flight.
func (c *Child[H]) StatusOf(host Host[H]) string {
	status := c.Status()
	if host.TurnActive(c) {
		return "running"
	}
	return status
}

// Result returns the recorded terminal outcome.
func (c *Child[H]) Result() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.err
}

// FinishedAt returns when the main run finished (zero until it does).
func (c *Child[H]) FinishedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finishedAt
}

// Done returns the channel closed once the main run reaches a terminal or
// interrupted state. It is a one-shot internal signal, not a payload
// transport: all authoritative data stays in the guarded fields.
func (c *Child[H]) Done() <-chan struct{} { return c.done }

// Released reports whether the lifecycle has torn this child down. A released
// child is out of the registry; only its persisted session remains.
func (c *Child[H]) Released() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.released
}

// markReleased latches the teardown. Called by the lifecycle only.
func (c *Child[H]) markReleased() {
	c.mu.Lock()
	c.released = true
	c.mu.Unlock()
}

// SetRunCancel installs the cancel handle for the child's main run.
func (c *Child[H]) SetRunCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	c.runCancel = cancel
	c.mu.Unlock()
}

// RunCancel returns the main run's cancel handle (nil before it is
// installed).
func (c *Child[H]) RunCancel() context.CancelFunc {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runCancel
}

// BeginReply arms the exact-reply capture for the targeted delivery that is
// about to start. Wired by the host's delivery-start hook.
func (c *Child[H]) BeginReply() {
	c.SetPendingReply(true)
}

// SetPendingReply arms or clears the exact-reply capture explicitly. The
// normal path is BeginReply (the host's delivery-start hook) followed by
// OnTurnResult; hosts and tests that synthesise a delivery use this.
func (c *Child[H]) SetPendingReply(armed bool) {
	c.mu.Lock()
	c.pendingReply = armed
	c.mu.Unlock()
}

// PendingReply reports whether a targeted delivery's exact reply is armed but
// not yet delivered to the parent.
func (c *Child[H]) PendingReply() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingReply
}

// OnTurnResult completes an armed reply capture with that invocation's exact
// output and error. Using the turn's return value avoids inferring a reply by
// scanning (and potentially misidentifying) transcript messages after tool
// rounds or auto-compaction.
func (c *Child[H]) OnTurnResult(host Host[H], reply string, replyErr error) {
	c.mu.Lock()
	if !c.pendingReply {
		c.mu.Unlock()
		return
	}
	c.pendingReply = false
	c.mu.Unlock()
	if c.Released() {
		// The teardown was silent: a released child has no parent
		// conversation left to deliver to.
		return
	}
	c.DeliverEvent(host, Event{Kind: EventReply, Text: reply, Err: replyErr})
}

// Busy reports whether the child has lifecycle work a release would cut
// short: an active turn, a delivery the host's worker has not started, or an
// exact reply armed but not yet delivered.
//
// The predicate is the HOST's: it owns the turn machinery and the eviction
// state, so only it can tell that a runtime can no longer run the work (an
// evicted child with an armed capture must read quiet, or the record would
// pin its retention timer forever). Host.Busy must therefore cover
// PendingReply.
func (c *Child[H]) Busy(host Host[H]) bool {
	return host.Busy(c)
}

// LatestProgress returns a one-line, bounded snippet of the child's most
// recent assistant output: the PULL-side view of a background child's
// progress (list_agents). Empty when the child has produced no assistant
// content yet.
func (c *Child[H]) LatestProgress() string {
	if c.Agent == nil {
		return ""
	}
	msgs := c.Agent.SnapshotMessages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" || msgs[i].Content == "" {
			continue
		}
		line := strings.Join(strings.Fields(msgs[i].Content), " ")
		return contextmgr.Truncate(line, MaxProgressLine, contextmgr.TruncateOptions{Marker: "…"})
	}
	return ""
}
