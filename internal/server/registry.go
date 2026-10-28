package server

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/steerq"
	"gogen/internal/streambuf"
)

// sessionRuntime is one live chat session in the web server: the agent, the
// per-session turn lock, and the turn machinery that used
// to live per connection: the in-flight stream handles, the pending delete
// approvals, and the set of attached client sockets (fan-out). A turn is
// owned by the runtime, not by any connection: disconnecting the last client
// detaches it but the turn keeps running server-side and persists normally
// (§4 connection-loss continuation).
type sessionRuntime struct {
	agent  *agent.Agent
	turnMu sync.RWMutex // turns take Lock; config/history reads take RLock

	// turnRelease is the turn-lock release broadcast: every write unlock
	// goes through releaseTurn, which closes the current channel and
	// installs a fresh one (notifyTurnReleased), so tryAcquireTurn and the
	// delivery worker's idle wait wake exactly when a holder frees the lock
	// instead of polling. turnReleaseMu guards the swap; it is a leaf lock —
	// never held while acquiring turnMu, r.mu, or deliverMu.
	turnReleaseMu sync.Mutex
	turnRelease   chan struct{}

	// stream owns the in-flight stream cancel handles for this session
	// (moved from per-connection).
	stream *wsConnStream

	// live is the current round's in-flight LLM output buffer (see
	// liveTurnState): a self-synchronizing round core plus wire-side sent
	// markers, each under its own leaf lock — never held while acquiring
	// turnMu.
	live liveTurnState

	// approvals are pending delete-approval requests, keyed by approvalID
	// (per-session, so two sessions' approvals cannot collide, E5). Each
	// entry keeps the original broadcast payload (paths/reason) so a
	// reconnecting client can be re-notified of the pending request (F2).
	approvalMu sync.Mutex
	approvals  map[string]*pendingApproval

	// approvalHold is how long pending delete approvals survive the last
	// attached client detaching before they are auto-denied. Zero (the
	// default) denies immediately on detach (D10); a positive value gives a
	// reconnecting client a window to answer before the hold expires.
	approvalHold time.Duration

	// clients are the attached sockets; broadcast() fans events out to all
	// of them. A write failure detaches that socket, never cancels
	// the turn.
	clientsMu sync.Mutex
	clients   map[*wsConn]struct{}

	// passive is the subset of clients attached for approval delivery only,
	// not as viewers (the board start's background attach: the initiating
	// tab receives the headless turn's delete approvals via the global
	// modal without "viewing" the session). Passive sockets count for
	// broadcast and approval auto-deny (clientCount) but NOT for the
	// live-session signal: viewerCount backs the sessions payload's active
	// flag and the orphan eviction, so a completed board session reads as a
	// plain saved session — not a stale "resume to continue" row — while
	// the initiating tab is still open. Invariant: passive ⊆ clients; a
	// viewer attach upgrades a passive socket (delete from passive).
	passive map[*wsConn]struct{}

	// closing latches an explicit session_close that has claimed this
	// runtime (claimClose): no client was attached at the claim and the
	// runtime is about to be cancelled and evicted, so attach must refuse —
	// a viewer racing the close would otherwise attach to a runtime the
	// eviction removes from under it. Guarded by clientsMu (both the claim
	// and the attach check hold it); sticky: never cleared, the claimed
	// runtime is leaving memory.
	closing bool

	// turnActive backs the session_state reply on attach so a reconnecting
	// client can distinguish "turn running headless" from "idle"; startedAt
	// records when the turn began (retained bookkeeping, not sent to the
	// client). turnOwner is the connection that started the current turn;
	// only it may interrupt via the cancel-then-lock path — a second
	// connection attached to the same session must not kill the turn
	// it does not own.
	stateMu    sync.Mutex
	turnActive bool
	startedAt  time.Time
	turnOwner  *wsConn

	// evicted marks a runtime that has been removed from the registry
	// (registry-cap eviction or session delete). It is set while the eviction
	// holds the runtime's turnMu and checked by message handlers AFTER they
	// acquire turnMu, so a handler that resolved the runtime just before the
	// eviction cannot start a turn on a runtime that is no longer registered
	// (invisible to cancel/prune/shutdown). Monotonic: an evicted runtime is
	// never re-registered — resume/attach build a fresh one.
	evicted atomic.Bool

	// registry is the owning sessionRegistry. Set by register(); used by
	// detach and the turn-end hook to evict the runtime when it becomes an
	// orphan — no attached clients and no running turn (a warm runtime
	// nobody is viewing should read as a plain saved session, not a stale
	// "resume to continue" row). Nil for runtimes built outside a registry
	// (unit tests): the orphan checks then no-op.
	registry *sessionRegistry

	// parentID is non-empty for nested (subagent) runtimes: the sidebar
	// renders them under their parent, cap eviction skips them, and delete
	// approvals route to the parent's clients when no child client is
	// attached (D6).
	parentID string
	// nested marks subagent child runtimes (parentID non-empty). Nested
	// runtimes are exempt from the active-session cap eviction: evicting a
	// running child mid-task would strand the parent's waiting tool call.
	nested bool
	// held marks runtimes whose lifecycle must survive orphan eviction
	// (background continuable subagents): with no clients and no running
	// turn they would otherwise be evicted right after their turn ends.
	// Set by the spawner, cleared when the runtime is released.
	held atomic.Bool

	// deliverMu guards pendingDeliver; deliverWorker gates the single
	// queue worker; deliverNotify wakes it at turn end (see deliver.go).
	// pendingDeliver is the per-session FIFO queue shared by system
	// deliveries (job notices, compact subagent terminal events, exact
	// replies) and user steering messages (typed while a turn runs) — one
	// queue, one drain path. The queue itself (item kinds, caps, eviction and
	// batching policy, slot zeroing) is internal/steerq, shared with the TUI;
	// it is NOT internally synchronized, so every access below is made under
	// deliverMu. A delivered item's deliverSource travels in Item.Tag.
	deliverMu      sync.Mutex
	pendingDeliver *steerq.Queue
	deliverWorker  atomic.Bool
	deliverNotify  chan struct{}
	// joinQueued reports the live join_queued flag at each pop. Never nil:
	// newSessionRuntime installs "off", and newWorkspaceRuntime replaces it
	// with the workspace's flag before the runtime is used. Never
	// reassigned after that.
	joinQueued func() bool

	// deliverStartHook is invoked by the delivery worker immediately before
	// it starts a delivered turn (see deliver.go) — for a send_message
	// delivery ONLY (deliverFromChildMessage). It arms the child's per-run
	// reply capture exactly when that delivery starts, not when it was queued.
	// Set once at child creation; never reassigned.
	deliverStartHook func()
	// deliverResultFunc receives the exact output/error from a delivered turn
	// whose source armed deliverStartHook. It replaces transcript scanning:
	// runTurnBody already knows which response belongs to this invocation.
	// Set once at child creation; never reassigned.
	deliverResultFunc func(string, error)
	// replyPending reports that the child's send_message reply is armed and
	// not yet delivered (subagents.Child.PendingReply), so Send now leaves
	// that turn alone. nil for a session that is not a continuable child.
	// Set once at child creation; never reassigned.
	replyPending func() bool
	// approverOverride replaces the default deleteApprover when non-nil
	// (the subagent spawner installs the D6 forwarding approver; the board
	// start installs a deny-when-unattended approver).
	approverOverride agent.DeleteApprover
	// commandApproverOverride replaces the default commandApprover when
	// non-nil, for the same two callers and for the same reason: a nested
	// child or a headless board session has no clients of its own, so its
	// dangerous-command prompt must reach whoever can answer it (the parent
	// session's tabs, or the board start's initiating tab) instead of being
	// denied silently. Set alongside approverOverride.
	commandApproverOverride agent.CommandApprover
	// turnErrorHook is invoked by startTurn's error path with the turn's
	// error (nil when no hook is installed — the normal case). The board
	// start installs it so a failed first turn is commented on the ticket
	// instead of failing silently in an unattended session.
	turnErrorHook func(err error)
}

// pendingApproval is one in-flight approval request (a file delete or a
// blocked shell command): the channel the turn's approver waits on, plus the
// original broadcast payload so a re-attaching client can be re-notified of
// the pending request. kind is "delete" or "command" and selects the
// re-notification message type; command is set for command approvals.
type pendingApproval struct {
	ch      chan bool
	kind    string
	paths   []string
	command string
	reason  string
}

// approvalKindOrDelete normalizes an empty kind (older callers/tests) to the
// delete approval, the historical default.
func approvalKindOrDelete(kind string) string {
	if kind == "" {
		return "delete"
	}
	return kind
}

// newSessionRuntime builds a runtime with the per-session turn machinery
// initialized. The agent must be non-nil.
func newSessionRuntime(a *agent.Agent) *sessionRuntime {
	rt := &sessionRuntime{
		agent:          a,
		stream:         &wsConnStream{},
		approvals:      make(map[string]*pendingApproval),
		clients:        make(map[*wsConn]struct{}),
		passive:        make(map[*wsConn]struct{}),
		deliverNotify:  make(chan struct{}, 1),
		pendingDeliver: steerq.New(maxUserQueueCap, maxSystemDeliveryQueue),
		turnRelease:    make(chan struct{}),
		joinQueued:     func() bool { return false },
	}
	// When this session's background work drains, re-check whether it has
	// become an evictable orphan. Both eviction paths refuse to evict a
	// session that still owns background work (keepsBackgroundWork), so
	// without this callback a session kept alive only by a long-running job
	// would stay resident forever. A no-op until register sets rt.registry,
	// and for nested/held runtimes, whose own guards apply.
	a.SetBackgroundIdleHook(rt.evictOrphanedIfPossible)
	return rt
}

// newWorkspaceRuntime builds a runtime that follows ws's live settings: its
// pending delete approvals are auto-denied after ws's approval hold once the
// last attached client detaches (instead of immediately), and its drain
// reads ws's join_queued flag. ws may be nil (both then read as off). Tests
// and non-server embeddings keep using newSessionRuntime.
func newWorkspaceRuntime(a *agent.Agent, ws *Workspace) *sessionRuntime {
	rt := newSessionRuntime(a)
	rt.approvalHold = ws.ApprovalHold()
	if ws != nil {
		rt.joinQueued = ws.JoinQueued
	}
	return rt
}

// liveTurnState accumulates the current round's in-flight LLM output so an
// attach/resume mid-turn can include it in the history snapshot: the
// assistant reply is only appended to a.Messages when a round completes, so
// without this buffer a switch to a running session would show "Resuming…"
// with no context until the turn ends.
//
// The round core (builders, tool-call maps, UTF-16 unit counters,
// reset/append/snapshot) is the shared streambuf.RoundBuffer — the TUI's
// mid-turn join renders the same structure, and the rewind payload emitted
// on attach is the shared streambuf.Snapshot itself (WSMessage.Rewind).
// This type layers the wire-side *Sent markers on top: they track how much
// of each stream has been flushed to the wire (the batcher lags the buffer
// by up to one flush interval), so stream frames can carry the exact end
// offset of their chunk. Combined with the buffer lengths in the rewind
// payload, a client can merge an attach rewind with live content exactly —
// never duplicating or dropping a character. The sent markers live under
// their own sentMu (leaf lock, never held together with the buffer's lock):
// the rewind snapshot never reads them, and the SegmentEnd methods never
// touch buffer state.
type liveTurnState struct {
	streambuf.RoundBuffer

	// sentMu guards the sent markers below.
	sentMu       sync.Mutex
	contentSent  int
	thinkingSent int
	// toolArgsSent tracks, per tool-call index, the end offset of the
	// last args segment flushed to the wire. Unlike contentSent (which
	// the rewind payload reads), the rewind payload keeps reporting the
	// FULL buffer length in toolArgsUnits: the client's rewind merge
	// (appendStreamingToolArgs + trimToEnd) recovers any delta it already
	// received beyond the snapshot, so a partial position here would
	// drop the tail. toolArgsSent exists solely to stamp the exact end
	// offset on each outgoing tool_call_delta frame.
	toolArgsSent map[int]int
}

// resetAll clears the round buffer AND the sent markers. The two leaf locks
// are taken sequentially, never held together: a rewind snapshot (buffer
// lock only) and a SegmentEnd (sentMu only) can interleave with the reset
// exactly as they could with the old single-lock reset.
func (st *liveTurnState) resetAll() {
	st.RoundBuffer.Reset()
	st.sentMu.Lock()
	st.contentSent = 0
	st.thinkingSent = 0
	st.toolArgsSent = nil
	st.sentMu.Unlock()
}

// liveContentSegmentEnd advances the sent-content marker by one flushed
// segment and returns the segment's end offset in the round's content
// stream. Called from the token batcher's send callback per content segment.
func (rt *sessionRuntime) liveContentSegmentEnd(text string) int {
	rt.live.sentMu.Lock()
	rt.live.contentSent += streambuf.UTF16Len(text)
	pos := rt.live.contentSent
	rt.live.sentMu.Unlock()
	return pos
}

// liveThinkingSegmentEnd is liveContentSegmentEnd for thinking segments.
func (rt *sessionRuntime) liveThinkingSegmentEnd(text string) int {
	rt.live.sentMu.Lock()
	rt.live.thinkingSent += streambuf.UTF16Len(text)
	pos := rt.live.thinkingSent
	rt.live.sentMu.Unlock()
	return pos
}

// liveToolArgsSegmentEnd advances the sent-args marker for one flushed
// coalesced segment and returns the segment's end offset in that tool
// call's args stream. Called from the args batcher's send callback (the
// args analogue of liveContentSegmentEnd): the batcher lags the buffer by up
// to one flush interval, so the frame must carry the offset of the text it
// actually carries, not the buffer length.
func (rt *sessionRuntime) liveToolArgsSegmentEnd(index int, text string) int {
	rt.live.sentMu.Lock()
	if rt.live.toolArgsSent == nil {
		rt.live.toolArgsSent = make(map[int]int)
	}
	rt.live.toolArgsSent[index] += streambuf.UTF16Len(text)
	pos := rt.live.toolArgsSent[index]
	rt.live.sentMu.Unlock()
	return pos
}

// attach registers a socket as a viewer of this session. Broadcasts reach all
// attached sockets; the first client to send a message starts the turn. A
// socket previously attached passively (approval delivery only) is upgraded
// to a viewer: the session is now genuinely open in this tab, so it counts
// for the live-session signal again.
//
// It returns false when the attach is refused: the runtime is closing (a
// claimed session_close, claimClose) or already evicted, so no socket may
// attach to a runtime that is leaving memory. The caller reports
// session_detached instead — the session stays saved and reopens from the
// store, the same self-heal an eviction's own broadcast produces, minus the
// eviction-under-attach.
func (rt *sessionRuntime) attach(ws *wsConn) bool {
	return rt.attachWithRole(ws, false)
}

// attachPassive registers a socket for approval delivery WITHOUT making it a
// viewer: broadcasts (delete approvals) reach it, and its presence keeps the
// approval auto-deny machinery armed (clientCount), but it does not count as
// a live viewer — viewerCount backs the sessions payload's active flag and
// the orphan eviction, so the session can read as a plain saved session (no
// stale "resume to continue" row) while only passively attached sockets
// remain. The board start uses it: the initiating tab must receive the
// headless turn's delete approvals without "viewing" the session. Refusal
// semantics mirror attach (closing or evicted runtime — attach returns
// false, the caller reports session_detached).
func (rt *sessionRuntime) attachPassive(ws *wsConn) bool {
	return rt.attachWithRole(ws, true)
}

// attachWithRole adds ws to the runtime's clients — atomically with
// claimClose's clientless check (both hold clientsMu), so an attach either
// lands before a close claim, keeping the session open, or is refused by
// the closing latch. It returns false when the attach is refused: the
// runtime is closing (a claimed session_close) or already evicted, i.e.
// leaving memory; nothing is mutated, not even the pending-approval
// re-notify below.
func (rt *sessionRuntime) attachWithRole(ws *wsConn, passive bool) bool {
	if ws == nil {
		return true
	}
	rt.clientsMu.Lock()
	if rt.closing || rt.evicted.Load() {
		rt.clientsMu.Unlock()
		return false
	}
	rt.clients[ws] = struct{}{}
	if passive {
		rt.passive[ws] = struct{}{}
	} else {
		delete(rt.passive, ws) // viewer attach upgrades a passive socket
	}
	rt.clientsMu.Unlock()
	// A reconnecting client missed the original delete_approval broadcast.
	// Re-notify it of every pending approval (F2: the hold window is only
	// actionable if the reconnected client knows the approval ids). The
	// response routes by approvalId, so duplicate notifications are harmless.
	type pendingNotify struct {
		id      string
		kind    string
		paths   []string
		command string
		reason  string
	}
	rt.approvalMu.Lock()
	pending := make([]pendingNotify, 0, len(rt.approvals))
	for id, pa := range rt.approvals {
		pending = append(pending, pendingNotify{
			id:      id,
			kind:    approvalKindOrDelete(pa.kind),
			paths:   pa.paths,
			command: pa.command,
			reason:  pa.reason,
		})
	}
	rt.approvalMu.Unlock()
	for _, p := range pending {
		msg := WSMessage{ApprovalID: p.id, Reason: p.reason, SessionID: rt.agent.SessionID}
		if p.kind == "command" {
			msg.Type = "command_approval"
			msg.Command = p.command
		} else {
			msg.Type = "delete_approval"
			msg.Paths = p.paths
		}
		_ = ws.writeJSON(msg)
	}
	return true
}

// detach removes a socket from the session. It never cancels the turn: the
// turn belongs to the runtime, not the connection. When the last
// attached client leaves, pending delete approvals are auto-denied so
// an unattended turn cannot hang forever on a destructive prompt.
func (rt *sessionRuntime) detach(ws *wsConn) {
	if ws == nil {
		return
	}
	rt.clientsMu.Lock()
	_, ok := rt.clients[ws]
	if ok {
		delete(rt.clients, ws)
	}
	delete(rt.passive, ws)
	empty := len(rt.clients) == 0
	rt.clientsMu.Unlock()
	if ok && empty {
		if rt.approvalHold > 0 {
			// F2: give a reconnecting client a window to answer pending
			// approvals. The timer is idempotent: if a client re-attaches and
			// answers first, completeApproval consumes the channel and the
			// eventual auto-deny finds nothing pending; if they re-attach but
			// do not answer, the hold still expires into auto-deny so an
			// unattended turn cannot hang forever.
			rt.scheduleAutoDenyAfter(rt.approvalHold)
		} else {
			rt.autoDenyPendingApprovals()
		}
		// Last viewer left: an idle runtime is now an orphan — flush and
		// unregister it so it reads as a plain saved session instead of a
		// stale "resume to continue" row. A running turn keeps it
		// registered; the turn-end hook evicts it when the turn completes.
		rt.evictOrphanedIfPossible()
	}
}

// scheduleAutoDenyAfter denies every pending approval after `after` unless a
// client answered them first. See detach for the idempotency argument.
func (rt *sessionRuntime) scheduleAutoDenyAfter(after time.Duration) {
	rt.approvalMu.Lock()
	if len(rt.approvals) == 0 {
		rt.approvalMu.Unlock()
		return
	}
	ids := make([]string, 0, len(rt.approvals))
	for id := range rt.approvals {
		ids = append(ids, id)
	}
	rt.approvalMu.Unlock()
	time.AfterFunc(after, func() {
		// A client that re-attached before the hold expired takes over the
		// pending prompts: denying them now would answer a prompt somebody is
		// looking at, and blanket-denying would also kill an approval raised
		// AFTER this timer was armed (the detach that armed it is stale).
		if rt.clientCount() > 0 {
			return
		}
		for _, id := range ids {
			rt.completeApproval(id, false)
		}
	})
}

// clientCount returns the number of attached sockets (for tests/shutdown).
// Includes passive (approval-only) attachments: they can still answer
// delete approvals, so the auto-deny and deny-when-unattended machinery
// must see them.
func (rt *sessionRuntime) clientCount() int {
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	return len(rt.clients)
}

// viewerCount returns the number of attached sockets that are genuine
// viewers (not passive approval-only attachments). It backs the sessions
// payload's active flag and the orphan eviction: a runtime whose only
// attachments are passive, with no running turn, is a warm session nobody
// is viewing and must read as a plain saved session — not a stale
// "resume to continue" row.
func (rt *sessionRuntime) viewerCount() int {
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	return len(rt.clients) - len(rt.passive)
}

// viewedBy reports whether ws is attached to this runtime as a genuine
// viewer (a pane open on that connection), as opposed to a passive
// approval-only attachment. sessionResume uses it to distinguish "resume
// a session this tab does not show" (switch) from "resume a session this
// tab already shows as a background pane" (do not duplicate it across
// panes — tell the client to focus the pane it already has).
func (rt *sessionRuntime) viewedBy(ws *wsConn) bool {
	if ws == nil {
		return false
	}
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	if _, ok := rt.clients[ws]; !ok {
		return false
	}
	_, passive := rt.passive[ws]
	return !passive
}

// claimClose atomically decides whether an explicit session_close may
// proceed: it returns true only when the runtime has no attached clients,
// and latches closing so a viewer attach that races the close is refused
// (its handler reports session_detached — the session stays saved) instead
// of attaching to a runtime the imminent eviction removes from under it.
// The claim and a racing attach are serialized by clientsMu, so exactly one
// of the two wins: either the attach lands first and the claim fails (the
// session stays open for the new viewer), or the claim wins and the attach
// is refused.
//
// This closes the check-then-close window the session_close handler used to
// have: a plain clientCount()==0 followed — after the parent-notification
// detour — by closeRuntime let a concurrent session_attach resolve the
// still-registered runtime in between and attach, and the close then evicted
// the racing viewer's runtime from under it (cancelling a turn started in
// that window), violating the handler's own multi-tab guarantee.
//
// Once claimed, the close is unconditional: the caller must run
// closeRuntime, and the latch is never cleared — the runtime is leaving
// memory. A claim on an already-evicted runtime still returns true
// (clientless); closeRuntime then no-ops on its evicted flag, matching the
// handler's pre-claim behavior for that case.
func (rt *sessionRuntime) claimClose() bool {
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	if rt.closing || len(rt.clients) > 0 {
		return false
	}
	rt.closing = true
	return true
}

// broadcast writes a message to every attached socket. A socket whose write
// fails is detached (write failure detaches, never cancels);
// the stream goroutine's enqueue is bounded by the send-queue timeout, so a
// dead socket cannot stall the turn.
func (rt *sessionRuntime) broadcast(msg WSMessage) {
	// Marshal and prepare ONCE for all clients: the old per-client writeJSON
	// re-marshaled (and re-deflated) the same payload for every socket, which
	// for a multi-MB session history dominated the fan-out.
	out, err := prepareOut(&msg)
	if err != nil {
		log.Printf("websocket marshal (%s): %v", msg.Type, err)
		return
	}
	rt.clientsMu.Lock()
	clients := make([]*wsConn, 0, len(rt.clients))
	for c := range rt.clients {
		clients = append(clients, c)
	}
	rt.clientsMu.Unlock()
	for _, c := range clients {
		if err := c.writePrepared(&msg, out); err != nil {
			rt.detach(c)
		}
	}
}

// broadcastExcept delivers msg to every attached socket except skip. It is the
// shape for handlers that answer the ACTING connection directly (a per-socket
// echo) but must keep every other client attached to the same session
// converged on session-scoped state that all of them render — a second tab/
// window, or a second pane on the same session, would otherwise keep showing
// the previous value.
//
// A skip socket that is not attached is simply absent from the client set, so
// the caller's own echo stays its only copy.
func (rt *sessionRuntime) broadcastExcept(skip *wsConn, msg WSMessage) {
	out, err := prepareOut(&msg)
	if err != nil {
		log.Printf("websocket marshal (%s): %v", msg.Type, err)
		return
	}
	rt.clientsMu.Lock()
	clients := make([]*wsConn, 0, len(rt.clients))
	for c := range rt.clients {
		if c != skip {
			clients = append(clients, c)
		}
	}
	rt.clientsMu.Unlock()
	for _, c := range clients {
		if err := c.writePrepared(&msg, out); err != nil {
			rt.detach(c)
		}
	}
}

// detachAllClients removes every attached socket from the session. Used when
// a runtime is leaving the registry (session_delete, eviction): every
// attachment must be released — not just one connection's — or the removed
// runtime's clients set would leak the other sockets until their connection
// teardown, and teardown's detachAll only sweeps REGISTERED sessions, so it
// would never reach this runtime. detach is idempotent and never cancels a
// turn; callers broadcast the session_detached/session_removed notification
// BEFORE sweeping so the message reaches every attached socket first.
func (rt *sessionRuntime) detachAllClients() {
	rt.clientsMu.Lock()
	clients := make([]*wsConn, 0, len(rt.clients))
	for c := range rt.clients {
		clients = append(clients, c)
	}
	rt.clientsMu.Unlock()
	for _, c := range clients {
		rt.detach(c)
	}
}

// evictOrphanedIfPossible asks the owning registry to evict this runtime if
// it became an orphan (no attached clients, no running turn). No-op for
// runtimes built outside a registry (registry field nil) and for runtimes
// that are already evicted.
func (rt *sessionRuntime) evictOrphanedIfPossible() {
	if rt.registry == nil {
		return
	}
	rt.registry.evictOrphaned(rt)
}

// setTurnActive records whether a turn is running (session_state payload) and
// which connection owns it. The active→false transition also wakes the
// delivery worker (signalDeliveries): every turn runner calls this on exit,
// so pending system messages are flushed exactly when a turn ends.
func (rt *sessionRuntime) setTurnActive(active bool, startedAt time.Time, owner *wsConn) {
	rt.stateMu.Lock()
	rt.turnActive = active
	rt.startedAt = startedAt
	rt.turnOwner = owner
	rt.stateMu.Unlock()
	if !active {
		rt.signalDeliveries()
	}
}

// turnState returns the current turn-active flag and start time.
func (rt *sessionRuntime) turnState() (bool, time.Time) {
	rt.stateMu.Lock()
	defer rt.stateMu.Unlock()
	return rt.turnActive, rt.startedAt
}

// ownsTurn reports whether ws started the currently running turn. Only the
// owner may interrupt it via the cancel-then-lock path (E3/E29); everyone
// else waits and gets the busy rejection.
func (rt *sessionRuntime) ownsTurn(ws *wsConn) bool {
	if ws == nil {
		return false
	}
	rt.stateMu.Lock()
	defer rt.stateMu.Unlock()
	return rt.turnOwner == ws
}

// completeApproval resolves a pending delete approval. Later responses for
// the same approvalId are no-ops (the channel is already consumed, §3.2).
func (rt *sessionRuntime) completeApproval(id string, approved bool) {
	rt.approvalMu.Lock()
	ch := rt.approvals[id]
	delete(rt.approvals, id)
	rt.approvalMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch.ch <- approved:
	default:
		// Waiter already left (cancel) or buffer full — don't block the reader.
	}
}

// autoDenyPendingApprovals denies every pending approval on this session.
// Used when the last attached client detaches: the turn continues with
// the "not approved" tool result instead of hanging.
func (rt *sessionRuntime) autoDenyPendingApprovals() {
	rt.approvalMu.Lock()
	for id, ch := range rt.approvals {
		delete(rt.approvals, id)
		select {
		case ch.ch <- false:
		default:
		}
	}
	rt.approvalMu.Unlock()
}

// deleteApprover returns the session-scoped delete approver. Requests are
// broadcast to all attached clients; the first response wins.
//
// With no attached client the request is DENIED fail-closed (false, nil),
// exactly like commandApprover: a queued message that keeps running after the
// last tab closes must not block the turn on a prompt nobody can answer (it
// would hold turnMu, pin the runtime, and stall the rest of the queue).
func (rt *sessionRuntime) deleteApprover() agent.DeleteApprover {
	return func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		if rt.clientCount() == 0 {
			return false, nil
		}
		id := newApprovalID()
		ch := make(chan bool, 1)

		rt.approvalMu.Lock()
		rt.approvals[id] = &pendingApproval{ch: ch, kind: "delete", paths: req.Paths, reason: req.Reason}
		rt.approvalMu.Unlock()

		defer func() {
			rt.approvalMu.Lock()
			delete(rt.approvals, id)
			rt.approvalMu.Unlock()
		}()

		rt.broadcast(WSMessage{
			Type:       "delete_approval",
			ApprovalID: id,
			Paths:      req.Paths,
			Reason:     req.Reason,
			SessionID:  rt.agent.SessionID,
		})
		// The last client may have detached between the guard above and the
		// registration: detach auto-denies what is already pending, so an
		// approval registered after that sweep would wait forever. Deny it
		// now instead.
		if rt.clientCount() == 0 {
			rt.completeApproval(id, false)
		}

		select {
		case approved := <-ch:
			return approved, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// commandApprover returns the session-scoped command approver. A blocked
// shell command (a dangerous-pattern match the guard escalated) is
// broadcast to all attached clients; the first response wins.
//
// With no attached client the request is DENIED fail-closed (false, nil):
// an unattended turn must not block on a prompt nobody can answer, and by
// default a dangerous command never runs unless a human allows it.
func (rt *sessionRuntime) commandApprover() agent.CommandApprover {
	return func(ctx context.Context, req agent.CommandRequest) (bool, error) {
		if rt.clientCount() == 0 {
			return false, nil
		}
		id := newApprovalID()
		ch := make(chan bool, 1)

		rt.approvalMu.Lock()
		rt.approvals[id] = &pendingApproval{ch: ch, kind: "command", command: req.Command, reason: req.Reason}
		rt.approvalMu.Unlock()

		defer func() {
			rt.approvalMu.Lock()
			delete(rt.approvals, id)
			rt.approvalMu.Unlock()
		}()

		rt.broadcast(WSMessage{
			Type:       "command_approval",
			ApprovalID: id,
			Command:    req.Command,
			Reason:     req.Reason,
			SessionID:  rt.agent.SessionID,
		})

		select {
		case approved := <-ch:
			return approved, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// nestedDeleteApprover returns the D6 delete-approval override for a nested
// (subagent) child runtime: the request takes the route approvalTarget
// resolves (the child's own clients, else the live parent's), and is denied
// fail-closed when nobody can answer.
func (r *sessionRegistry) nestedDeleteApprover(child *sessionRuntime, parentID string) agent.DeleteApprover {
	return func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		target := r.approvalTarget(child, parentID)
		if target == nil {
			return false, nil
		}
		return target.deleteApprover()(ctx, req)
	}
}

// nestedCommandApprover is the command-approval counterpart: a blocked shell
// command raised by a nested (subagent) child takes the same route as its
// delete approvals (approvalTarget), so a headless child asks the parent's
// user instead of denying every dangerous command with "denied by user" for a
// prompt nobody saw.
func (r *sessionRegistry) nestedCommandApprover(child *sessionRuntime, parentID string) agent.CommandApprover {
	return func(ctx context.Context, req agent.CommandRequest) (bool, error) {
		target := r.approvalTarget(child, parentID)
		if target == nil {
			return false, nil
		}
		return target.commandApprover()(ctx, req)
	}
}

// approvalTarget resolves which runtime can answer an approval raised by
// child: the child itself when it has attached clients (its pane shows the
// modal), else the live parent when that one has clients. nil means nobody can
// answer, and the caller denies fail-closed.
//
// The parent is resolved by id AT APPROVAL TIME, never via a runtime pointer
// captured at spawn/reopen: the parent can be orphan-evicted while its
// background child keeps running and later reopened as a FRESH runtime under
// the same id. An approver closing over the spawn-time pointer would broadcast
// the approval to the dead runtime's swept clients set and wait forever —
// reports and completion notices survive that eviction via deliverToParent
// queueing, but approvals have no queue: the waiter holds the child's turn
// hostage, with no auto-deny armed (that machinery fires on client detach, and
// a headless child has no client). Recovery would only be interrupt_agent.
//
// Parking an approval with no possible answer is why the nil case denies: it
// matches the board start's deny-when-unattended override and the last-detach
// auto-deny — an unattended turn must not block on a destructive prompt.
func (r *sessionRegistry) approvalTarget(child *sessionRuntime, parentID string) *sessionRuntime {
	if child.clientCount() > 0 {
		return child
	}
	if parentRt, ok := r.get(parentID); ok && parentRt != child && parentRt.clientCount() > 0 {
		return parentRt
	}
	return nil
}

// resolvedApprover prefers an installed override (subagent forwarding, board
// start deny-when-unattended) over the runtime's own approver. Shared by both
// approval kinds so the override precedence cannot diverge.
func resolvedApprover[Req any](def, override func(context.Context, Req) (bool, error)) func(context.Context, Req) (bool, error) {
	if override != nil {
		return override
	}
	return def
}

// sessionRegistry owns the live sessions of the web server. The map itself
// is guarded by mu (short critical sections only); per-session state is
// serialized by the session's own turnMu.
type sessionRegistry struct {
	mu        sync.RWMutex
	agents    map[string]*sessionRuntime
	order     []string // session ids in registration order (LRU eviction, §4)
	maxActive int

	// evictHook is invoked with a session id after its runtime is evicted
	// (cap eviction, session close, delete, shutdown, child release) —
	// deliberately NOT orphan eviction, see fireEvictHook. The web server
	// wires it to cancel the session's background subagent children. Set
	// once at construction; never reassigned.
	evictHook func(sessionID string)

	// parentDeliverMu guards parentDeliveries, the per-parent queue of
	// system messages (background-subagent completion notices and captured
	// send_message replies) for sessions whose runtime is not live. The spawner queues
	// when the parent is evicted or closed while its children keep
	// running; register() flushes the queue into the fresh runtime when
	// the session is next loaded — "deliver once it is live again"
	// (deliverToParent).
	parentDeliverMu  sync.Mutex
	parentDeliveries map[string][]string

	// bgWorkHook reports whether a session id still owns background work
	// that evicting its runtime would destroy. Agent.HasBackgroundWork
	// covers the shell jobs and terminals and is consulted directly; the web
	// server wires this to the subagent spawner's live continuable children
	// (the cap-eviction tail cancels those through evictHook). Set once at
	// construction; nil when the host has no such work.
	bgWorkHook func(sessionID string) bool
}

// keepsBackgroundWork reports whether evicting rt would destroy background
// work the user started in that session: a running execute_command
// background=true job, a persistent terminal, or a live background subagent
// child. Such a session is NOT idle however few clients are attached to it:
// the eviction tail (evictRuntime → Agent.Close) kills exactly this work,
// while the session itself stays saved on disk — so a client that merely
// stopped watching (a pane switch, a page refresh, registry-cap pressure)
// must not destroy it. Only an explicit end-of-session (session_close,
// delete, shutdown) does.
//
// Locking: the cap-eviction loop (register) calls this while holding r.mu;
// the orphan-eviction path calls it without it. Both are safe because
// everything consulted here is a LEAF lock — the agent's bgMu/termMu and the
// continuable-child registry's mu — and every holder of that lock releases it
// before calling back into the registry, so no reverse edge to r.mu exists
// (see register's LOCK ORDER note).
func (r *sessionRegistry) keepsBackgroundWork(rt *sessionRuntime) bool {
	if rt == nil || rt.agent == nil {
		return false
	}
	if rt.agent.HasBackgroundWork() {
		return true
	}
	if r.bgWorkHook == nil {
		return false
	}
	return r.bgWorkHook(rt.agent.SessionID)
}

func newSessionRegistry(maxActive int) *sessionRegistry {
	if maxActive <= 0 {
		maxActive = config.DefaultWebMaxActiveSessions
	}
	return &sessionRegistry{
		agents:           make(map[string]*sessionRuntime),
		maxActive:        maxActive,
		parentDeliveries: make(map[string][]string),
	}
}

// register adds a runtime under id and returns the ids of any sessions it
// evicted to make room (empty when none). Registering an already-active id is
// a no-op (dedupe, E9) returning nil. Evicted sessions were flushed before
// eviction; their ids are returned so callers that prune the store right
// after (session create/fork) can protect the fresh files (E2/E11). A
// victim with live background work or an attached viewer is never chosen
// (see keepsBackgroundWork) — the cap is exceeded rather than destroy it.
func (r *sessionRegistry) register(id string, rt *sessionRuntime) []string {
	var victims []*sessionRuntime
	var victimIDs []string
	r.mu.Lock()
	if id == "" || rt == nil {
		r.mu.Unlock()
		return nil
	}
	// Back-pointer for the orphan eviction (detach / turn-end hook). Set
	// before the dedupe no-op below so any runtime that passes through
	// register can evict itself later.
	rt.registry = r
	if _, ok := r.agents[id]; ok {
		r.mu.Unlock()
		return nil
	}
	// Cap the registry (D6/E11): evict the least-recently-active IDLE
	// session (scan from the back of order) until the new registration
	// fits. The default session (front of order) is never evicted —
	// messages without a sessionId and new connections' initial pane
	// depend on it. "Idle" means more than "no turn running": a session
	// with an attached VIEWER is never evicted (the client is looking at
	// it — an eviction would send it session_detached and close the pane
	// out from under the user), and neither is one that still owns
	// background work (keepsBackgroundWork: a running
	// execute_command background=true job, a persistent terminal, or a
	// live background subagent child — the tail below kills all three).
	// As with a running (including headless) turn, if every candidate is
	// busy the cap is allowed to be exceeded rather than destroy live work.
	// Victims are only COLLECTED under the lock (cheap bookkeeping); the
	// flush + notify happen after the lock is released, because
	// broadcast's enqueue can block up to 5s on a stuck socket and must
	// not stall registry lookups. Evicted sessions are flushed (nothing in
	// memory is lost — the client's pane re-attaches from the store, E11)
	// and their attached sockets get session_detached so the client closes
	// the pane instead of sending messages to a stale runtime.
	//
	// The turnState check races a turn that is ABOUT to start: a message
	// handler may have resolved this runtime before the eviction and not
	// yet set turnActive. TryLocking the victim's turnMu closes the window
	// while the eviction holds it (a turn can only run under turnMu, so the
	// flush below cannot race a turn's save), and the evicted flag — set
	// here, before the lock is released — makes the eviction sticky: a
	// handler that acquires turnMu AFTER the eviction sees the flag and
	// drops the message instead of starting a turn on an unregistered
	// runtime. TryLock never blocks: a busy (or starting) session is
	// skipped, exceeding the cap rather than killing the turn.
	//
	// LOCK ORDER (documented invariant): this loop takes r.mu → turnMu,
	// r.mu → clientsMu, r.mu → stateMu, r.mu → bgMu/termMu and r.mu →
	// continuable-child registry's mu — all of them LEAF locks (every holder
	// of that lock releases it before calling into the registry, so no
	// reverse edge exists) — while sessionDelete takes turnMu → r.mu
	// (registry.remove), the reverse order. That is NOT a deadlock only
	// because neither path ever blocks on the other's inner lock: the
	// victim lock here is a non-blocking TryLock and sessionDelete's turnMu
	// acquisition is also a TryLock. Do not convert either to a blocking
	// Lock()/RLock() without reworking both paths.
	for i := len(r.order) - 1; len(r.agents) >= r.maxActive && i > 0; i-- {
		victimID := r.order[i]
		victim := r.agents[victimID]
		// Nested (subagent) runtimes are exempt from the cap: evicting a
		// running child would strand the parent's waiting tool call, and a
		// finished child is unregistered by its spawner within the same
		// tool call.
		if victim.nested {
			continue
		}
		// A session the user is looking at is not a memory-management
		// candidate: evicting it would close the pane (session_detached)
		// mid-read. Attach recency (setDefault) keeps the pane just opened
		// at the front, but a pane opened earlier and still open must not
		// become the LRU victim either.
		if victim.viewerCount() > 0 {
			continue
		}
		if active, _ := victim.turnState(); active {
			continue
		}
		// Live background work outlives the runtime's memory: the tail
		// below kills it (agent.Close) and cancels the session's background
		// subagent children (fireEvictHook), so it must not pick this
		// victim. The session stays saved and reopens against the same
		// live runtime, work included.
		if r.keepsBackgroundWork(victim) {
			continue
		}
		if !victim.turnMu.TryLock() {
			continue
		}
		victim.evicted.Store(true)
		delete(r.agents, victimID)
		r.order = append(r.order[:i], r.order[i+1:]...)
		victims = append(victims, victim)
		victimIDs = append(victimIDs, victimID)
	}
	r.agents[id] = rt
	r.order = append(r.order, id)
	r.mu.Unlock()
	for i, victim := range victims {
		// Persist any UNSAVED state before eviction (FlushPending writes only
		// when the session is dirty). FlushSession would force a write on a
		// clean session too, re-stamping its Updated timestamp with ~now —
		// the evicted session was evicted for being the LEAST recently
		// active, and stamping it newest would push it to the top of the
		// saved-session list and distort the recency ordering that
		// Store.List/LatestID/Prune rely on.
		victim.agent.FlushPending()
		// The victim owns no live background work (the loop above skips
		// those), so any job still registered here has FINISHED: Close
		// clears the bookkeeping rather than leave a stray process — the
		// documented evictRuntime contract every other eviction path goes
		// through. Close is idempotent, non-blocking (context cancels +
		// retention-timer stops under bgMu), and nils the job registry: a
		// finishing job can neither rearm a reaper on the closing Agent nor
		// fire a completion notice the registry-backed deliverer would
		// silently drop (the victim is already unregistered here). Under
		// turnMu, mirroring evictRuntime; the sticky evicted flag set above
		// already blocks any new turn once the lock is released.
		victim.agent.Close()
		victim.releaseTurn()
		victim.broadcast(WSMessage{Type: "session_detached", SessionID: victimIDs[i]})
		// Detach every attached socket AFTER the notification (the client
		// closes its pane on session_detached): the victim left the
		// registry under the lock above, and teardown's detachAll only
		// sweeps REGISTERED sessions — an unattended attachment here would
		// leak the socket in the evicted runtime's clients set.
		victim.detachAllClients()
		// Cap eviction is an explicit teardown: the session's background
		// subagent children are cancelled and released.
		r.fireEvictHook(victimIDs[i])
	}
	// A session re-registered after eviction/close may have queued
	// deliveries (background-subagent completions and replies queued while
	// its runtime was not live); flush them into the fresh runtime now that
	// the session is live again. Outside r.mu: deliverToSession can
	// broadcast (bounded by the send-queue timeout).
	r.flushParentDeliveries(id, rt)
	return victimIDs
}

// evictRuntime is the shared eviction tail: flush the session, remove the
// runtime from the registry, then notify any client that raced in between
// (session_detached — it re-attaches from the store) and detach it. The
// session stays saved on disk; reopening it (session_attach / resume) loads
// a fresh runtime. Callers must hold (or win) the runtime's turnMu so no new
// turn can start on a runtime that is about to leave the registry; the lock
// order is turnMu → r.mu, matching sessionDelete and the cap eviction's
// TryLock. A caller that cannot take the lock (a released child's delivery
// worker may hold it mid-handoff) latches rt.evicted and drains the lock with
// drainTurnLock BEFORE calling here.
//
// Removal happens BEFORE the notification so the registry never contains a
// dying-but-registered runtime: a concurrent session_attach/resume of the
// same id can never resolve it — the lookup fails, the caller loads fresh
// from the store and registers cleanly — instead of attaching to a runtime
// that remove() is about to yank out from under it. This mirrors the cap
// eviction in register, which deletes the victim from the map before any
// session_detached is sent.
//
// The flush is FlushPending — it writes only when the session is dirty — for
// every eviction path, including the explicit session_close: the close drain
// (cancelInFlight) waits for the cancelled turn's own final persist, so by
// the time eviction runs a clean session's state is already on disk and the
// forced FlushSession would only re-stamp its Updated timestamp with ~now.
// That stamp pushed a just-closed session to the top of the saved-session
// list (recency ordering is interaction-based) and made "resume latest"
// target a session the user merely closed. A genuinely dirty runtime
// (unsaved turn state) still persists before eviction, so no state is at
// risk (a failed save whose error was consumed by the turn-end warning is
// retried by the next FlushSession/persistSession, exactly as before).
func (r *sessionRegistry) evictRuntime(rt *sessionRuntime) {
	// Claim the runtime FIRST: the evicted flag is the only guard a delivery
	// worker re-checks after it wins the turn lock, and everything below (the
	// flush, the Close, the registry removal) is slow enough for that worker
	// to be mid-handoff. Latching before the flush makes it drop its queue
	// instead of starting a delivered turn whose own start-flush would land
	// after the runtime left the registry. Callers that must also wait out a
	// turn that WON that handoff drain the lock themselves before calling
	// here (see drainTurnLock and subagentSpawner.Release).
	rt.evicted.Store(true)
	rt.agent.FlushPending()
	// The runtime is leaving memory: its background jobs (execute_command
	// background=true) have no owner left to poll them, so kill them rather
	// than leak orphan processes. Idempotent; turns are unaffected.
	rt.agent.Close()
	r.remove(rt.agent.SessionID)
	rt.broadcast(WSMessage{Type: "session_detached", SessionID: rt.agent.SessionID})
	// Sweep attachments after the notification: a socket that raced in (or
	// was still attached) must not linger in the removed runtime's clients
	// set — teardown's detachAll only sweeps REGISTERED sessions and would
	// never reach it. detach is idempotent and never cancels a turn.
	rt.detachAllClients()
	// Deny whatever approvals are still parked on this runtime: it is
	// leaving memory, so nothing can ever answer them, and the normal
	// auto-deny arming (the last client's detach) may not have fired when
	// the runtime was already clientless — e.g. an orphan-evicted parent
	// holding an approval a background child routed to it before the
	// eviction. Without this the child's waiter hangs forever on the dead
	// runtime (no auto-deny is armed for it — that machinery is armed by
	// client detach, and the runtime has none left). Normally a no-op:
	// detach's auto-deny already released everything parked while a client
	// was attached.
	rt.autoDenyPendingApprovals()
}

// drainTurnLock waits (bounded by wait) for the runtime's turn lock and
// releases it, cancelling whatever turn holds it on every round.
//
// It closes the pop→start handoff that the evicted flag alone cannot: the
// delivery worker holds turnMu from the moment it pops a queued item until
// the delivered turn's goroutine finishes, and it re-checks the flag only
// AFTER winning the lock. A worker that won the lock before the flag landed
// has therefore already started a turn whose own start-flush (the user
// message persisted before the provider call) would otherwise run after the
// runtime left the registry — recreating a released child's session file,
// and in the test suite racing t.TempDir()'s removal ("TempDir RemoveAll
// cleanup: ... directory not empty").
//
// Cancelling again inside the loop matters: such a delivery turn registers
// its stream handle AFTER the caller's own cancelInFlight, so waiting
// without cancelling would burn the whole budget and still leave it writing.
//
// Callers must latch rt.evicted BEFORE calling (so no new turn can start
// while this waits) and must NOT hold the turn lock. A turn that ignores
// cancellation costs the window once and the eviction proceeds anyway — the
// sticky flag still refuses new turns, the same trade closeRuntime documents
// for a stuck turn.
func (rt *sessionRuntime) drainTurnLock(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if rt.tryAcquireTurn(0) {
				rt.releaseTurn()
				return true
			}
			return false
		}
		slice := wsTurnAcquireWait
		if slice > remaining {
			slice = remaining
		}
		if rt.tryAcquireTurn(slice) {
			rt.releaseTurn()
			return true
		}
		rt.stream.cancelInFlight()
	}
}

// fireEvictHook invokes the registry's eviction hook for id (the web server
// wires it to cancel the session's background subagent children). Called by
// the EXPLICIT teardown paths (session_close, delete, cap eviction,
// shutdown, child release) — deliberately NOT by orphan eviction: a parent
// that merely went idle with no viewers is reopened by id, and its children
// keep running until then — their completion notices and reports are queued
// (deliverToParent) and delivered once the parent session is live again.
func (r *sessionRegistry) fireEvictHook(id string) {
	if r.evictHook != nil {
		r.evictHook(id)
	}
}

// closeRuntime cancels any in-flight turn (draining it ≤ wsStreamDrainWait),
// then evicts the runtime. Used by the explicit session_close message: the
// user pressed ✕ on the pane — stop the session and put it back in the saved
// list. The session stays saved on disk.
//
// closeRuntime itself does not look at the clients set (tests call it
// directly on known-idle runtimes). A caller that must honor the multi-tab
// guarantee — close only when no other socket is attached — claims the close
// first (sessionRuntime.claimClose), which atomically checks clientlessness
// against attach; see wsHandleClose.
func (r *sessionRegistry) closeRuntime(rt *sessionRuntime) {
	if rt == nil || rt.evicted.Load() {
		return
	}
	rt.stream.cancelInFlight()
	// TryLock: if the drain timed out (the cancelled turn's goroutine is
	// still alive, e.g. stuck in a tool), that turn itself holds the lock —
	// proceed without it; the evicted flag prevents new turns regardless.
	if held := rt.turnMu.TryLock(); held {
		defer rt.releaseTurn()
	}
	r.evictRuntime(rt)
	// Explicit close: the session is gone for good (until reopened) — its
	// background subagent children are cancelled and released.
	r.fireEvictHook(rt.agent.SessionID)
}

// evictOrphaned unregisters a runtime with no attached clients and no running
// turn — the "resume to continue" state nobody is viewing (a page refresh, a
// closed tab, a re-keyed-away session, or a headless turn that just
// finished). The session stays saved on disk. Mirrors the cap-eviction
// pipeline: flush before evict, remove from the registry, then notify
// (session_detached) and detach any client that raced in between — see
// evictRuntime for the ordering rationale.
//
// Background work is the other half of "idle". A session that still owns a
// running background job, a persistent terminal, or a live background
// subagent child is NOT an orphan however many clients dropped: the eviction
// tail (evictRuntime → Agent.Close) kills exactly that work, and the session
// stays saved, so a client that merely stopped watching — a pane switch, a
// page refresh, a closed tab — must not destroy it. Such a runtime keeps its
// registry slot until the work drains; the agent's background-idle hook
// (installed in newSessionRuntime) then re-runs this check and the normal
// idle eviction proceeds. The memory a still-running job pins is the same
// trade the running-turn exemption makes above.
//
// A turn can only run while holding turnMu (startTurn's caller acquires it),
// so winning the TryLock proves the runtime is idle — and a concurrent
// message handler that resolved this runtime before the eviction cannot
// start a turn on it: its tryAcquireTurn loses the race and the evicted flag
// makes the guard sticky. A running turn's goroutine holds the lock, so
// TryLock fails and we skip — the turn-end hook re-checks when the turn
// finishes.
func (r *sessionRegistry) evictOrphaned(rt *sessionRuntime) {
	if rt == nil || rt.evicted.Load() {
		return
	}
	// A passive (approval-only) attachment is not a viewer: a completed
	// board session whose initiating tab is still open must be evictable,
	// or it would pin a stale "resume to continue" row for the tab's life.
	if rt.viewerCount() > 0 {
		return
	}
	if active, _ := rt.turnState(); active {
		return
	}
	// Idle, but still doing work: evicting now would kill it (see above).
	// Same exemption shape as a running turn.
	if r.keepsBackgroundWork(rt) {
		return
	}
	// A runtime with queued system deliveries is not idle: evicting it now
	// would strand the delivery worker (the evicted flag makes it drop the
	// queue). The worker's final turn-end re-check evicts once drained.
	if rt.hasPendingDeliveries() {
		return
	}
	// Held runtimes (background continuable subagents) are exempt from
	// orphan eviction for their whole lifecycle; the spawner releases them
	// explicitly.
	if rt.held.Load() {
		return
	}
	if !rt.turnMu.TryLock() {
		return
	}
	defer rt.releaseTurn()
	r.evictRuntime(rt)
}

// get returns the runtime for id.
func (r *sessionRegistry) get(id string) (*sessionRuntime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.agents[id]
	return rt, ok
}

// remove evicts the runtime for id.
func (r *sessionRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[id]; !ok {
		return
	}
	rt := r.agents[id]
	rt.evicted.Store(true)
	delete(r.agents, id)
	for i, oid := range r.order {
		if oid == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	// Wake any waiter parked on the runtime's turn-release broadcast so an
	// eviction that could not win the turn lock (a stuck turn holds it) still
	// lets the delivery worker re-check evicted and drop the queue instead of
	// blocking until that turn finally unlocks. The completed-eviction paths
	// release the lock themselves (releaseTurn), so this is redundant there —
	// harmless, since notifyTurnReleased always installs a fresh channel.
	rt.notifyTurnReleased()
	// Drop the per-parent delivery backlog: it is keyed by session id and would
	// otherwise keep a deleted session's undrained messages reachable for the
	// life of the process (and re-target them if the id were ever reused).
	r.parentDeliverMu.Lock()
	delete(r.parentDeliveries, id)
	r.parentDeliverMu.Unlock()
}

// setDefault makes id the default session (first in registration order), so
// messages without a sessionId route to it (D3: typed /new replaces the
// pane's session).
func (r *sessionRegistry) setDefault(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[id]; !ok {
		return
	}
	order := r.order[:0]
	for _, oid := range r.order {
		if oid != id {
			order = append(order, oid)
		}
	}
	r.order = append([]string{id}, order...)
}

// first returns the first registered runtime (the default session).
func (r *sessionRegistry) first() *sessionRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return nil
	}
	return r.agents[r.order[0]]
}

// activeIDs returns the ids of all registered sessions in registration order
// (the full protected set for store.Prune, E2).
func (r *sessionRegistry) activeIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.order))
	out = append(out, r.order...)
	return out
}

// runtimes returns a snapshot of the live session runtimes in registration
// order. It replaces the "range activeIDs() then get(id)" fan-out that was
// copy-pasted across the server: that pattern took the registry lock once for
// the id list and again per id, allocating a []string of ids only to look each
// one back up.
//
// The slice is a snapshot: a runtime can be evicted before the caller acts on
// it. Every fan-out tolerates that — an evicted runtime has already detached its
// clients, so broadcasting into it is a no-op — and callers must not hold the
// slice across a blocking operation expecting it to stay current.
func (r *sessionRegistry) runtimes() []*sessionRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*sessionRuntime, 0, len(r.order))
	for _, id := range r.order {
		if rt, ok := r.agents[id]; ok {
			out = append(out, rt)
		}
	}
	return out
}

// broadcastAll fans one server-wide message out to every socket attached to
// ANY registered session, exactly ONCE per socket. Unlike broadcast it is not
// session-scoped: board, automations and status-display payloads are
// process-wide (every tab renders the same board/row/automations list), so
// they go to every attached client regardless of which session its pane
// shows.
//
// The dedupe is what the old "range runtimes(); rt.broadcast(msg)" loops got
// wrong: a socket can be attached to MORE THAN ONE runtime at a time (its
// pane plus background panes opened via session_attach), so delivering
// per-runtime sent a socket watching K sessions K copies of the same frame —
// K re-renders, and for a notice K toasts. Here each socket appears once; the
// message is marshalled/prepared once for all of them (as broadcast does).
//
// A write failure detaches that socket from EVERY runtime that held it, not
// just the first: detach is per-runtime, so detaching only the first would
// leave stale client entries in the others until their own broadcasts failed.
// detach never cancels a turn.
//
// Locking: the per-runtime clientsMu is taken one runtime at a time to build
// the holder map, and the registry lock is only held inside runtimes() (a
// snapshot) — no lock is held across a socket write, which can block up to
// the enqueue timeout.
func (r *sessionRegistry) broadcastAll(msg WSMessage) {
	if r == nil {
		return
	}
	holders := make(map[*wsConn][]*sessionRuntime)
	for _, rt := range r.runtimes() {
		if rt == nil {
			continue
		}
		rt.clientsMu.Lock()
		for c := range rt.clients {
			holders[c] = append(holders[c], rt)
		}
		rt.clientsMu.Unlock()
	}
	if len(holders) == 0 {
		return
	}
	out, err := prepareOut(&msg)
	if err != nil {
		log.Printf("websocket marshal (%s): %v", msg.Type, err)
		return
	}
	for c, rts := range holders {
		if err := c.writePrepared(&msg, out); err != nil {
			for _, rt := range rts {
				rt.detach(c)
			}
		}
	}
}

// nestedDescendants returns the registered runtimes whose ParentID chain
// reaches rootID — the nested children (and grandchildren, recursively)
// whose files the store's cascade delete removes when rootID is deleted.
// The root runtime itself is NOT included. Used by sessionDelete so live
// child runtimes are drained and evicted alongside the file cascade: a
// registered child that kept running would otherwise resurrect its deleted
// file on the next persist.
//
// parentIDOf resolves ANY session id (registered or not) to its parent id:
// sessionDelete supplies a resolver that checks the registry first and
// falls back to the store's metadata index, so an unregistered intermediate
// parent (evicted child whose grandchild was reopened) cannot hide a
// descendant. Chains are walked with a visited set so a corrupt ParentID
// cycle cannot loop.
func (r *sessionRegistry) nestedDescendants(rootID string, parentIDOf func(id string) string) []*sessionRuntime {
	ids := r.activeIDs()
	byID := make(map[string]*sessionRuntime, len(ids))
	for _, id := range ids {
		if rt, ok := r.get(id); ok {
			byID[id] = rt
		}
	}
	var out []*sessionRuntime
	for id, rt := range byID {
		if id == rootID {
			continue
		}
		if runtimeIsDescendantOf(rt.agent.ParentID(), rootID, parentIDOf, map[string]bool{}) {
			out = append(out, rt)
		}
	}
	return out
}

// runtimeIsDescendantOf reports whether a ParentID chain starting at
// parentID reaches rootID. visited guards against corrupt cycles.
func runtimeIsDescendantOf(parentID, rootID string, parentIDOf func(id string) string, visited map[string]bool) bool {
	if parentID == "" {
		return false
	}
	if parentID == rootID {
		return true
	}
	if visited[parentID] {
		return false // corrupt ParentID cycle — never loop
	}
	visited[parentID] = true
	return runtimeIsDescendantOf(parentIDOf(parentID), rootID, parentIDOf, visited)
}

// detachAll removes the connection from every registered session it may be
// attached to: the current pane plus any background panes opened via
// session_attach. A killed tab cannot send session_detach for each pane, so
// connection teardown sweeps the registry (detach is idempotent and never
// cancels a turn). Without the sweep, a background pane's dead
// socket would linger in its runtime's clients set until the next broadcast
// failed — and if the connection died while that session awaited a delete
// approval, the turn would hang forever instead of auto-denying (D10: the
// last attached client leaving must deny pending approvals).
func (r *sessionRegistry) detachAll(ws *wsConn) {
	if ws == nil || r == nil {
		return
	}
	for _, rt := range r.runtimes() {
		rt.detach(ws)
	}
}

// ShutdownSessions drains in-flight streams and flushes every registered
// session agent so a multi-session web server exits without losing in-memory
// state. It is safe to call with zero clients attached (turns detach first —
// Start's ForceClose closes sockets before this runs, and closing a socket
// must never cancel its session's turn, or the disconnect-kills-turn bug
// returns). Each cancelInFlight bounds its drain at wsStreamDrainWait; the
// whole sweep is at most N×that, one session at a time.
//
// The flush is FlushPending, not FlushSession: the sweep must persist any
// state a running turn left unsaved, but must NOT force a full write on
// sessions whose last turn already flushed. Forcing a write on every clean
// session re-stamped each one's Updated timestamp with ~now in registry
// order (the focused session first, so it received the OLDEST stamp), which
// destroyed the recency ordering Store.List/LatestID rely on after a
// restart — the saved-session list reshuffled and the session that was
// active at shutdown was demoted instead of restored as current.
func (s *Server) ShutdownSessions() {
	if s == nil || s.registry == nil {
		return
	}
	// Drain + flush LEAST-recently-active first. Registry order has the
	// focused session at the front (setDefault moves it there on attach/
	// new/resume/fork), so a forward sweep stamps the Updated timestamps of
	// sessions that are still DIRTY at quit with ~now oldest-first — the
	// focused session, flushed first, received the OLDEST stamp and was
	// demoted on restart (the same reshuffle FlushPending fixed for clean
	// sessions). Reversing the sweep gives the most recently active session
	// the NEWEST stamp, so Store.List/LatestID keep the pre-shutdown recency
	// order for dirty sessions too.
	ids := s.registry.activeIDs()
	for i := len(ids) - 1; i >= 0; i-- {
		rt, ok := s.registry.get(ids[i])
		if !ok {
			continue
		}
		rt.stream.cancelInFlight()
		rt.agent.FlushPending()
		// Kill background jobs so no command outlives the process.
		rt.agent.Close()
		// Shutdown is an explicit teardown: the session's background
		// subagent children are cancelled and released.
		s.registry.fireEvictHook(ids[i])
	}
}

// acquireTurnForHandler is the single cancel-then-lock acquisition used by
// every WS handler that takes the session turn lock (chat messages, /compact,
// board start, and the UI-channel config handlers). It cancels the in-flight
// turn only when THIS connection owns it (interrupt semantics — typing a new
// message replaces your own turn). A second connection attached to the same
// session never cancels a turn it does not own; it waits and gets the
// standard busy rejection. Returns false when the runtime is busy or was
// evicted; the caller drops the message and notifies the client on its own
// channel (see errAgentBusy — UI-channel handlers must not emit the busy
// rejection as a "response"). On success the caller owns rt.turnMu.
//
// The cancel happens TWICE on purpose, and the up-front one is not
// redundant: when the caller owns the turn, its own turn's goroutine holds
// turnMu, so the first tryAcquireTurn would be guaranteed to time out —
// cancelling up front makes that 150 ms window cover the drain instead of
// delaying the interrupt. The retry-cancel after the first timeout covers
// the case where the drain outlived the window (a tool still exiting).
// Both are scoped to the owner via ownsTurn, and cancelInFlight is
// idempotent, so a caller that already cancelled (e.g.
// preprocessWSUserMessage before routing /compact) pays nothing.
func (rt *sessionRuntime) acquireTurnForHandler(ws *wsConn) bool {
	if rt.ownsTurn(ws) {
		rt.stream.cancelInFlight()
	}
	if !rt.tryAcquireTurn(wsTurnAcquireWait) {
		if rt.ownsTurn(ws) {
			rt.stream.cancelInFlight()
		}
		if !rt.tryAcquireTurn(wsStreamDrainWait) {
			return false
		}
	}
	// The session may have been evicted from the registry (cap eviction or
	// delete) after the caller resolved it but before the lock was acquired.
	// Starting a turn (or mutating config) on an evicted runtime would be
	// invisible to cancel/prune/shutdown. The flag is set while the eviction
	// holds turnMu, so checking it now — under the lock — is race-free:
	// either this handler acquired first (the eviction skipped the session)
	// or the eviction completed first (the flag is set). Drop silently: the
	// client already received session_detached and closed the pane.
	if rt.evicted.Load() {
		rt.releaseTurn()
		return false
	}
	return true
}

// releaseTurn releases the session turn (write) lock and wakes every waiter
// parked in tryAcquireTurn or the delivery worker's idle wait. Use it in
// place of rt.turnMu.Unlock() so a waiter learns of the release the instant
// it happens instead of re-probing on a timer.
//
// The unlock MUST happen before the signal: a waiter woken by the broadcast
// re-probes with TryLock, so signalling first would let it observe the lock
// still held, re-arm on the freshly installed channel, and miss the (later)
// release. Unlock-then-notify closes that window.
func (rt *sessionRuntime) releaseTurn() {
	rt.turnMu.Unlock()
	rt.notifyTurnReleased()
}

// notifyTurnReleased broadcasts a turn-lock release by closing the current
// release channel and installing a fresh one, so every current waiter wakes
// while a waiter that arrives later parks on the new channel. A plain
// buffered-channel send would wake only one of several waiters.
func (rt *sessionRuntime) notifyTurnReleased() {
	rt.turnReleaseMu.Lock()
	// nil-safe: runtimes built as bare struct literals (tests) skip
	// newSessionRuntime, so install the channel on first use.
	if rt.turnRelease != nil {
		close(rt.turnRelease)
	}
	rt.turnRelease = make(chan struct{})
	rt.turnReleaseMu.Unlock()
}

// turnReleaseChan returns the current release-broadcast channel. A waiter
// captures it BEFORE re-probing the lock: a release that lands between the
// failed TryLock and the wait closes the captured channel, so the wake-up is
// never missed.
func (rt *sessionRuntime) turnReleaseChan() chan struct{} {
	rt.turnReleaseMu.Lock()
	defer rt.turnReleaseMu.Unlock()
	if rt.turnRelease == nil {
		rt.turnRelease = make(chan struct{})
	}
	return rt.turnRelease
}

// tryAcquireTurn waits up to wait for the session's turnMu (e.g. after
// cancelling our own stream), waking on the turn-release broadcast rather
// than polling. Returns false if another client still holds the session's
// turn lock when the deadline expires. A non-positive wait is a single
// non-blocking probe.
func (rt *sessionRuntime) tryAcquireTurn(wait time.Duration) bool {
	if rt.turnMu.TryLock() {
		return true
	}
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		// Capture the release signal before re-probing so a release landing
		// now is not missed (capture-then-check).
		release := rt.turnReleaseChan()
		if rt.turnMu.TryLock() {
			return true
		}
		select {
		case <-release:
			// A holder released: retry the lock.
		case <-timer.C:
			// Last chance: a release may have landed in the same instant.
			return rt.turnMu.TryLock()
		}
	}
}
