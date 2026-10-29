package server

import (
	"gogen/internal/llm"
	"gogen/internal/steerq"
)

// The per-runtime delivery queue stores internal/steerq items, so the kinds
// here are steerq's, and each one names a producer on this host:
//
//   - steerq.User: text the user typed into a busy session (rendered as the
//     user's own message).
//   - steerq.Notice: every machine-generated one-way message — job notices,
//     scheduled reminders, compact subagent terminal events, and notices
//     flushed from the registry's per-parent queue.
//   - steerq.Reply: a TARGETED delivery — a parent's send_message into a
//     child's runtime, whose exact turn output is the answer the parent
//     asked for.
//
// Only a Reply arms the receiving session's delivery-start hook (the
// continuable-subagent reply capture, see deliverLoop). Every other machine
// message must NOT be mistaken for a message whose answer belongs to the
// parent: a child's reaction to an unrelated notice would otherwise be
// injected into ITS parent as an unsolicited "reply" (and another parent
// turn).

// deliveriesLocked returns the runtime's delivery queue, creating it on first
// use (a hand-built runtime — tests, a nested child without the full
// constructor — may have none). Caller holds deliverMu: the shared queue is not
// internally synchronized, and this assigns the field.
func (rt *sessionRuntime) deliveriesLocked() *steerq.Queue {
	if rt.pendingDeliver == nil {
		rt.pendingDeliver = steerq.NewDefault()
	}
	return rt.pendingDeliver
}

// The per-session bounds (user cap, system cap), the queue-item id generator
// and the offline-parent stash's bound are internal/steerq's: they are queue
// policy, and the TUI applies the same ones. What is left here is this host's
// own contract for client-supplied queue ids.

// maxQueueIDLen bounds a client-supplied queue id: it is a correlation
// token, never content, and the server-generated form is ~20 characters.
const maxQueueIDLen = 64

// validQueueID reports whether a client-supplied queue id is usable as a
// correlation token: non-empty, bounded, and made of URL-safe token
// characters (the client's own "q-<n>-<base36>" shape). Anything else is
// replaced by a generated id rather than stored and broadcast verbatim.
func validQueueID(id string) bool {
	if id == "" || len(id) > maxQueueIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// queueIDInUseLocked reports whether id is already queued for this runtime.
// Caller holds deliverMu.
func (rt *sessionRuntime) queueIDInUseLocked(id string) bool {
	return rt.pendingDeliver.Has(id, steerq.User) || rt.pendingDeliver.Has(id, steerq.Notice) || rt.pendingDeliver.Has(id, steerq.Reply)
}

// enqueueUserTurn queues a user steering message typed while a turn is
// running: it is delivered FIFO as the next user turn after the in-flight
// one ends. id is the client-provided correlation id (empty → generated).
//
// User text is never silently dropped: when the per-kind cap is reached the
// enqueue is REFUSED (ok=false) and the caller reports the rejection. The
// item keeps the runtime alive (hasPendingDeliveries counts it), so a
// queued message still runs after the user closes every tab.
func (rt *sessionRuntime) enqueueUserTurn(id, content string, images []llm.ImageInput) bool {
	if rt == nil || rt.evicted.Load() {
		return false
	}
	rt.deliverMu.Lock()
	if rt.countUser() >= steerq.UserCap {
		rt.deliverMu.Unlock()
		return false
	}
	// The client's id is the correlation key for its optimistic bubble, so a
	// usable one is kept verbatim; a missing, oversized, or non-token-shaped
	// id gets a server-generated one, and so does a DUPLICATE — two queued
	// items sharing an id would collapse into one entry in every client's
	// per-id bubble map, so a removal or drain would clear the wrong chip.
	// (The client's own ids carry a per-tab nonce, so a duplicate only ever
	// arrives from a hand-built frame.)
	if !validQueueID(id) || rt.queueIDInUseLocked(id) {
		id = steerq.NewID()
	}
	// The user cap was checked above under the same lock, so Accepted is the
	// only outcome here.
	rt.deliveriesLocked().Append(steerq.Item{ID: id, Text: content, Kind: steerq.User, Images: images})
	if !rt.deliverWorker.Swap(true) {
		go rt.deliverLoop()
	}
	rt.deliverMu.Unlock()
	rt.broadcastQueueState()
	select {
	case rt.deliverNotify <- struct{}{}:
	default:
	}
	return true
}

// countUser counts queued user items. Caller holds deliverMu.
func (rt *sessionRuntime) countUser() int {
	return rt.pendingDeliver.Count(steerq.User)
}

// broadcastQueueState sends a queue_update frame describing the runtime's
// queued items. Called on every queue mutation so every attached client
// (all tabs, background panes) renders the same queue state. Ids absent
// from the frame were REMOVED before running (per-item ✕, clear-all,
// interrupt) and render as cancelled-before-running. Deliberately OUTSIDE
// deliverMu (same self-deadlock chain as deliverToSession's overflow
// notice: broadcast → detach → evictOrphaned → deliverMu).
func (rt *sessionRuntime) broadcastQueueState() {
	rt.broadcastQueueStarted("", nil)
}

// broadcastQueueStarted is broadcastQueueState for the drain's pop. left
// lists the user item ids whose turn STARTED (the frame reaches every
// client before that turn's user_acked), and text is the message that turn
// runs, so a client shows the delivered text instead of rebuilding a
// joined run itself.
func (rt *sessionRuntime) broadcastQueueStarted(text string, left []string) {
	rt.broadcast(WSMessage{Type: "queue_update", SessionID: rt.agent.SessionID, Queue: rt.queueItems(), QueueLeft: left, Content: text})
}

// sendNow asks the running turn to stop at its next round boundary so the
// queue can deliver (steerq.CheckSendNow), and returns the outcome. id names
// the queued user item the client showed the control on ("" = any user
// item). The turn is read before the queue, so a queued item that starts
// its own turn in between is no longer queued, and that turn is not the
// one asked to stop.
func (rt *sessionRuntime) sendNow(id string) steerq.SendNowResult {
	busy, _ := rt.turnState()
	turn := rt.agent.TurnGeneration()
	rt.deliverMu.Lock()
	queued := rt.pendingDeliver.SendNowReady(id)
	rt.deliverMu.Unlock()
	replying := rt.replyPending != nil && rt.replyPending()
	res := steerq.CheckSendNow(busy, turn, replying, queued)
	if res == steerq.SendNowRecorded && !rt.agent.RequestYieldFor(turn) {
		res = steerq.SendNowIdle
	}
	return res
}

// queueItems snapshots the queue for the wire (user items only are sent:
// system deliveries are the agent's internal bookkeeping, and image data
// stays out of the frame — the sending tab already rendered its bubble).
func (rt *sessionRuntime) queueItems() []QueueItem {
	rt.deliverMu.Lock()
	defer rt.deliverMu.Unlock()
	out := make([]QueueItem, 0, rt.pendingDeliver.Len())
	for _, it := range rt.pendingDeliver.Items() {
		if it.Kind == steerq.User {
			out = append(out, QueueItem{ID: it.ID, Text: it.Text})
		}
	}
	return out
}

// removeQueuedItem drops one USER queue item by id. System deliveries are
// not individually removable (machine bookkeeping). Reports whether an item
// was removed; the caller broadcasts the new state.
func (rt *sessionRuntime) removeQueuedItem(id string) bool {
	if rt == nil || id == "" {
		return false
	}
	rt.deliverMu.Lock()
	_, removed := rt.pendingDeliver.Remove(id, steerq.User)
	rt.deliverMu.Unlock()
	return removed
}

// clearUserQueue drops every USER queue item (the interrupt semantic:
// "stop what you're doing" removes the user's own queued text) and returns
// how many were removed. SYSTEM deliveries are deliberately kept — they are
// machine bookkeeping (job notices, subagent completion notices, reply
// capture) whose
// loss strands the delivery machinery. Caller broadcasts the new state when
// the count is non-zero.
func (rt *sessionRuntime) clearUserQueue() int {
	if rt == nil {
		return 0
	}
	rt.deliverMu.Lock()
	// DropKind zeroes the dropped slots: a removed item can carry multi-MB
	// data-URL images, and leaving them in the backing array kept them
	// reachable for the lifetime of the queue.
	removed := len(rt.pendingDeliver.DropKind(steerq.User))
	rt.deliverMu.Unlock()
	return removed
}

// dropDeliveredLocked removes the first n queued items (caller holds
// deliverMu) and CLEARS their slots: reslicing alone left the item, which can
// carry a multi-MB data-URL image, reachable through the backing array.
func (rt *sessionRuntime) dropDeliveredLocked(n int) {
	// Take zeroes the popped slots, so a removed item's text and images are
	// not kept reachable through the backing array.
	rt.pendingDeliver.Take(n)
}

// deliverToSession injects a system-generated message into the session as a
// user message and runs a turn on it at the next idle boundary. It never
// blocks: the message is queued and a per-runtime worker delivers it.
//
// Producers (job completion notices, scheduled reminders, subagent
// completion notices) call this from arbitrary goroutines; the worker owns
// all interaction with
// the turn machinery, so the "Messages are owned by the turn goroutine"
// invariant holds by construction. The item's kind is steerq.Notice: it
// never arms the delivery-start hook (see deliverChildMessage).
//
// Delivery is at-most-once: the item is popped BEFORE the turn starts, so a
// crash between pop and start loses one message rather than duplicating one.
//
// Returns false when the message was NOT accepted (nil runtime, or the
// runtime was evicted between the caller's lookup and this call). Callers
// that must not lose the message can fall back to deliverToParent, which
// queues it for the session's next registration.
//
// true means the notice was CONSUMED, not necessarily queued: a notice
// arriving at a queue whose every slot holds a targeted (send_message)
// delivery is dropped and toasted instead of evicting a pending reply (see
// enqueueSystem). Only false may be re-routed to deliverToParent; a true
// result has already been resolved either way.
func (rt *sessionRuntime) deliverToSession(text string) bool {
	accepted, _ := rt.enqueueSystem(text, steerq.Notice)
	return accepted
}

// deliverChildMessage delivers a parent's send_message into a child's
// runtime. It is deliverToSession with the steerq.Reply kind: the delivered
// turn's final assistant message is the reply the parent asked for, so
// starting it arms the child's delivery-start hook (the reply capture). This
// is the ONLY producer that enqueues a Reply — see the kind notes at the top
// of this file.
//
// ok=false means nothing was queued: full=true reports a queue holding only
// targeted deliveries, so the caller can tell the model to wait and retry
// instead of dropping a message whose reply is pending.
func (rt *sessionRuntime) deliverChildMessage(text string) (ok, full bool) {
	return rt.enqueueSystem(text, steerq.Reply)
}

// enqueueSystem is the shared system-delivery enqueue used by
// deliverToSession (kind = steerq.Notice) and deliverChildMessage
// (kind = steerq.Reply); kind must be one of those two. A USER steering
// message never comes through here — enqueueUserTurn owns User items and
// their separate cap.
//
// Per-kind overflow policy (the per-kind caps never touch user items):
//   - a NOTICE first evicts the OLDEST queued notice (freshness wins — a
//     stale job notice is worse than none). If every slot holds a targeted
//     delivery, the notice itself is dropped instead: a message whose reply
//     the parent is waiting on must not be evicted by a notice.
//   - a TARGETED delivery (Reply) first evicts the oldest notice to make
//     room; when every slot holds a targeted delivery the enqueue is
//     REFUSED (ok=false, full=true) rather than silently discarding a
//     message — the caller turns that into a visible tool error the model
//     can retry.
func (rt *sessionRuntime) enqueueSystem(text string, kind steerq.Kind) (ok, full bool) {
	if rt == nil || rt.evicted.Load() {
		return false, false
	}
	rt.deliverMu.Lock()
	// The cap/eviction preference is the shared queue's policy (steerq): at
	// the system cap the OLDEST NOTICE is evicted for either kind; when every
	// slot holds a targeted reply, a new reply is REFUSED (so the caller
	// reports an actionable error instead of dropping a message whose answer
	// is pending) and a new notice is DROPPED (a full reply queue must not
	// lose its replies).
	outcome, evicted := rt.deliveriesLocked().Append(steerq.Item{ID: steerq.NewID(), Text: text, Kind: kind})
	var dropped bool
	switch {
	case outcome == steerq.Refused:
		rt.deliverMu.Unlock()
		return false, true
	case outcome == steerq.Dropped:
		rt.deliverMu.Unlock()
		rt.broadcast(WSMessage{Type: "notice", Kind: "delivery", Success: false,
			Content: "A background message was dropped (delivery queue full)."})
		return true, false
	case evicted != nil:
		dropped = true
	}
	if !rt.deliverWorker.Swap(true) {
		go rt.deliverLoop()
	}
	rt.deliverMu.Unlock()
	if dropped {
		// Deliberately OUTSIDE deliverMu: a failing write on the last
		// attached client funnels into detach → evictOrphaned →
		// hasPendingDeliveries, which takes deliverMu. Broadcasting under
		// the lock self-deadlocks that chain on the same goroutine.
		rt.broadcast(WSMessage{Type: "notice", Kind: "delivery", Success: false,
			Content: "A background message was dropped (delivery queue full)."})
	}
	select {
	case rt.deliverNotify <- struct{}{}:
	default:
	}
	return true, false
}

// --- parent-scoped delivery (survives runtime eviction) ---

// deliverToParent delivers a child-to-parent event's text to a live parent
// session, or — when the session's runtime is not live (orphan-evicted,
// closed) — stashes it in the registry so it is delivered when the session is
// next registered (reopened from the store). Never blocks; at-most-once per
// queue entry. Used by the continuable-subagent machinery, whose producers
// must not lose a completion notice or a reply to a parent that merely went
// idle with no viewers.
//
// kind orders the OFFLINE STASH only (see queueParentDelivery): a Reply is
// never the eviction victim while the parent is away. The LIVE path injects
// as a one-way Notice whatever the kind — the answer belongs to whoever asked
// for it, and reply capture is armed per targeted delivery, never by a child
// event arriving at its parent (which may itself be a child, and whose turn
// output would then be forwarded one level too far).
func (r *sessionRegistry) deliverToParent(parentID string, kind steerq.Kind, text string) {
	if rt, ok := r.get(parentID); ok {
		if rt.deliverToSession(text) {
			return
		}
		// The runtime was evicted between the lookup and the queue append:
		// fall through and queue for the next registration.
	}
	r.queueParentDelivery(parentID, kind, text)
}

// queueParentDelivery stashes a message for a session whose runtime is not
// live. The stash is a shared steerq queue, so it applies the same bound and
// the same eviction preference as a live one: at the cap the OLDEST NOTICE is
// evicted, a NOTICE arriving at an all-reply stash drops itself, and a REPLY
// arriving there is refused — a message whose answer somebody is waiting on
// must not evict another one. Overflow is silently unrecoverable either way
// (the parent is not live, so there is no client to toast), which is why the
// stash carries replies at all instead of a plain string list.
//
// The kind is carried for EVICTION PRIORITY ONLY: flushParentDeliveries
// injects every stashed message as a one-way notice, because by then no turn
// is waiting on an answer.
//
// Flushed by register() when the session is next loaded; cleared by
// sessionDelete (a deleted session can never be reopened).
func (r *sessionRegistry) queueParentDelivery(parentID string, kind steerq.Kind, text string) {
	if parentID == "" {
		return
	}
	r.parentDeliverMu.Lock()
	defer r.parentDeliverMu.Unlock()
	q := r.parentDeliveries[parentID]
	if q == nil {
		// No User items are ever stashed: the user cap does not apply.
		q = steerq.New(0, steerq.SystemCap)
		r.parentDeliveries[parentID] = q
	}
	q.Append(steerq.Item{ID: steerq.NewID(), Text: text, Kind: kind})
}

// flushParentDeliveries delivers every stashed message for id into rt (the
// freshly registered runtime of a session that was evicted/closed while its
// background subagents kept running). No-op when nothing is queued. Must
// NOT be called with r.mu held: deliverToSession can broadcast (bounded by
// the send-queue timeout).
func (r *sessionRegistry) flushParentDeliveries(id string, rt *sessionRuntime) {
	if rt == nil {
		return
	}
	r.parentDeliverMu.Lock()
	q := r.parentDeliveries[id]
	delete(r.parentDeliveries, id)
	r.parentDeliverMu.Unlock()
	if q == nil {
		return
	}
	// Take zeroes the popped slots, so the drained messages' text is not kept
	// reachable through the stash after it leaves the map.
	for _, it := range q.Take(q.Len()) {
		rt.deliverToSession(it.Text)
	}
}

// clearParentDeliveries drops the queued deliveries for id. Used when the
// session is DELETED: its file is gone, so it can never be reopened and the
// queue would be dead weight in memory.
func (r *sessionRegistry) clearParentDeliveries(id string) {
	r.parentDeliverMu.Lock()
	delete(r.parentDeliveries, id)
	r.parentDeliverMu.Unlock()
}

// deliverLoop is the per-runtime queue worker (at most one per runtime,
// guarded by deliverWorker). It pops the head run that drains as one turn
// (steerq.Batch: a run of one-way notices, a run of user messages when
// join_queued is on, otherwise the head item alone; a run stops at a
// different kind, so FIFO arrival order holds across kinds), waits for the
// session turn lock (the idle check), hands the lock to startTurn (whose
// goroutine defers the unlock), and repeats until the queue is empty. A
// notice run shares ONE turn so a burst of completions costs one provider
// request instead of one per notice. A targeted send_message delivery always
// keeps its own turn, because its reply capture is armed per turn.
//
// The worker wakes without polling when either a turn-lock holder releases
// the lock (releaseTurn's broadcast — covering both a turn end and a brief
// config handler) or a new item is enqueued (deliverNotify). A failed
// TryLock does not mean a turn is running (brief config handlers also take
// turnMu), so the worker keeps the item at the HEAD (FIFO, peek-not-pop) and
// parks on those signals; the item is popped only once the turn lock is
// acquired, so orphan eviction (hasPendingDeliveries) never races the
// handoff. An eviction that cannot win the turn lock (a stuck turn holds it)
// still wakes the worker via remove's notifyTurnReleased, so the queue is
// dropped rather than waiting for that turn to unlock.
func (rt *sessionRuntime) deliverLoop() {
	for {
		rt.deliverMu.Lock()
		if rt.pendingDeliver.Len() == 0 {
			rt.deliverWorker.Store(false)
			rt.deliverMu.Unlock()
			return
		}
		// Peek, do NOT pop yet: the item must stay visible to
		// evictOrphaned's hasPendingDeliveries until the delivered turn is
		// actually starting. Popping first opened a handoff window (queue
		// empty, turnMu still free) where the just-ended turn's orphan
		// re-check could evict the runtime and drop the item.
		item, _ := rt.pendingDeliver.Peek()
		rt.deliverMu.Unlock()

		if rt.evicted.Load() {
			rt.deliverMu.Lock()
			rt.dropDeliveredLocked(1)
			rt.deliverMu.Unlock()
			continue // dropped: the runtime left the registry mid-queue
		}
		// Capture the release signal BEFORE probing the lock
		// (capture-then-check): a holder that frees the lock between this
		// failed TryLock and the wait below closes the captured channel, so
		// the wake-up is never missed. No polling: the worker sleeps until
		// the holder (a running turn, or a brief config handler) releases
		// the lock, or a fresh enqueue signals deliverNotify, and keeps the
		// item at the head (FIFO) in the meantime.
		release := rt.turnReleaseChan()
		if !rt.turnMu.TryLock() {
			select {
			case <-release:
			case <-rt.deliverNotify:
			}
			continue
		}
		// Re-check under the lock: the runtime may have been evicted while
		// we waited (orphan eviction wins the TryLock between our pop and
		// acquire; closeRuntime/delete evict without the lock). Starting a
		// turn on an evicted runtime would stream into the void and
		// persist a message nobody sees — drop it (at-most-once).
		if rt.evicted.Load() {
			rt.deliverMu.Lock()
			rt.dropDeliveredLocked(1)
			rt.deliverMu.Unlock()
			rt.releaseTurn()
			continue
		}
		// The turn lock is ours and the runtime is live: pop and hand the
		// lock to startTurn (its goroutine defers the unlock). The
		// delivery-start hook (if any) fires immediately before, so
		// consumers can arm per-delivery state exactly when the delivered
		// turn begins — Reply items only: the hook arms send_message
		// reply capture, which must not treat a user steering message or a
		// one-way notice as a targeted delivery.
		//
		// Re-verify the head under the lock before popping: the queue can
		// shift between the peek above and this pop (an overflow drop or an
		// eviction prunes the head while we waited for the turn lock). If
		// the head is no longer the item we peeked, that item was dropped —
		// do NOT pop the (different) item now at the head and deliver the
		// stale one; release the turn lock and re-peek instead.
		rt.deliverMu.Lock()
		if head, ok := rt.pendingDeliver.Peek(); !ok || head.ID != item.ID {
			rt.deliverMu.Unlock()
			rt.releaseTurn()
			continue
		}
		// Pop the head run that drains as ONE turn and deliver it. steerq.Batch
		// is the one definition of that run: a contiguous run of one-way
		// notices (one provider request instead of one per notice), a run of
		// user messages when join_queued is on, and otherwise the head item
		// alone. A TARGETED send_message delivery is a Reply and never
		// batches, because its reply capture is armed per turn and must stay
		// 1:1 with the tool call. Only a contiguous head run is taken, so
		// FIFO across kinds is preserved.
		batch := rt.pendingDeliver.Batch(rt.joinQueued())
		// Capture everything the delivery needs BEFORE dropping the run:
		// dropDeliveredLocked zeroes the popped slots (so their text/images
		// are no longer retained by the backing array). batch is a COPY, and
		// steerq.Join builds a new string, so both stay valid after the clear.
		item = batch[0]
		text, images := steerq.Join(batch)
		rt.dropDeliveredLocked(len(batch))
		rt.deliverMu.Unlock()
		// USER items left the queue to run: every attached client must see
		// the queue state converge (the sending tab's optimistic queued
		// bubbles resolve against this frame; the paired user_acked —
		// emitted by the turn this pop starts, after this broadcast —
		// resolves the pending ack and carries the real history index).
		// Every id of the run is in queueLeft, so the client clears those
		// chips instead of rendering the bubbles cancelled-before-running.
		// A batch is a single kind, and SYSTEM deliveries are not on the
		// wire queue (queueItems sends user items only), so their pops
		// change nothing client-visible — no frame.
		if item.Kind == steerq.User {
			left := make([]string, 0, len(batch))
			for _, it := range batch {
				left = append(left, it.ID)
			}
			rt.broadcastQueueStarted(text, left)
		}
		// The delivery-start hook arms per-delivery state exactly when the
		// delivered turn begins — and ONLY for a targeted send_message
		// delivery (steerq.Reply). The callback receives that exact turn's
		// output/error, so a child's reaction to any other machine message
		// can never be injected into its parent as a reply.
		var onResult func(string, error)
		if item.Kind == steerq.Reply {
			if rt.deliverStartHook != nil {
				rt.deliverStartHook()
			}
			onResult = rt.deliverResultFunc
		}
		// The turn-end signal wakes the worker for the next pop, so queued
		// items drain in FIFO order across consecutive turns. `text` and
		// `images` were captured before the pop cleared the batch slots.
		rt.startTurnWithResult(nil, text, images, onResult)
	}
}

// signalDeliveries wakes the delivery worker after a turn ends. Called from
// setTurnActive's active→false transition, which every turn runner performs
// on exit — a single hook covering startTurn, runChildTurn, and compact
// without touching any defer chain.
func (rt *sessionRuntime) signalDeliveries() {
	select {
	case rt.deliverNotify <- struct{}{}:
	default:
	}
}

// hasPendingDeliveries reports whether the runtime has undelivered queued
// items (system deliveries AND user steering messages). Consulted by orphan
// eviction: a runtime with a non-empty queue is not idle — a queued message
// still runs after the user closes every tab.
func (rt *sessionRuntime) hasPendingDeliveries() bool {
	if rt == nil {
		return false
	}
	rt.deliverMu.Lock()
	defer rt.deliverMu.Unlock()
	return rt.pendingDeliver.Len() > 0
}
