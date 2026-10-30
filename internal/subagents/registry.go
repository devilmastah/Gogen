package subagents

import (
	"slices"
	"sync"
)

// registry owns the live children of one host. A nil registry means the
// host's foreground-only path (tests, or a host that never enables
// continuable children): the continuable operations report unsupported
// rather than panicking.
//
// Lock order: registry.mu → Child.mu. Child.mu is a leaf lock and is never
// held while acquiring registry.mu, so the cap accounting may read a child's
// status under the registry lock. Every critical section drops registry.mu
// before calling into the host, so the host may consult the registry (for
// example an eviction hook asking HasLiveChildren) without deadlocking.
type registry[H any] struct {
	mu       sync.Mutex
	children map[string]*Child[H]
	// reviving holds the ids whose host runtime is being reopened from the
	// store right now. It closes the check-then-act in Spawner.reviveChild:
	// two concurrent revivals of one saved child would build two Child
	// records (and two host runtimes) for the same session id — the web host
	// dedupes the runtime by id, so the two records would share one, and the
	// loser's release would then tear down the runtime the winner is using.
	// Guarded by mu.
	reviving map[string]struct{}
	// foreground counts in-flight foreground (Spawn/Fork) children per parent
	// session id. Foreground children are not Child records, but they hold a
	// slot of the concurrent limit for the whole turn; guarded by mu.
	foreground map[string]int
}

// newRegistry returns an empty child registry.
func newRegistry[H any]() *registry[H] {
	return &registry[H]{
		children:   make(map[string]*Child[H]),
		reviving:   make(map[string]struct{}),
		foreground: make(map[string]int),
	}
}

// register publishes an admitted child, reporting false when the id is
// already taken. A duplicate is refused rather than overwriting: the
// registered record owns the host runtime, its armed reply capture and its
// retention timer, so replacing it would strand all three (and the displaced
// record's release would evict the runtime the newcomer is using). A caller
// that gets false tears its own record down.
func (r *registry[H]) register(c *Child[H]) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.children[c.ID]; ok {
		return false
	}
	r.children[c.ID] = c
	return true
}

// beginRevive claims id for a revival in flight, so the lookup that decided
// "not live, reopen it" and the register that publishes the reopened child
// are one critical section. It refuses when the id is already registered
// (somebody revived it, or the child was never released) or another revival
// of it is in flight.
func (r *registry[H]) beginRevive(id string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.children[id]; ok {
		return false
	}
	if _, ok := r.reviving[id]; ok {
		return false
	}
	r.reviving[id] = struct{}{}
	return true
}

// endRevive releases a claim taken by beginRevive, on every path: by then the
// id is registered or nothing was published.
func (r *registry[H]) endRevive(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reviving, id)
}

// get resolves a live child by id (nil when unknown or already released).
func (r *registry[H]) get(id string) *Child[H] {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.children[id]
}

// childrenOf returns the live children of parentID, newest last
// (registration order).
func (r *registry[H]) childrenOf(parentID string) []*Child[H] {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Child[H]
	for _, c := range r.children {
		if c.ParentID == parentID {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *Child[H]) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// hasLiveChildren reports whether parentID still has a child registered —
// background subagent work an eviction would destroy, because the host's
// eviction hook cancels every live child of an evicted parent. Hosts wire
// this into their "does this session still own background work?" check so an
// eviction never collects a parent whose children keep running.
func (r *registry[H]) hasLiveChildren(parentID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.children {
		if c.ParentID == parentID {
			return true
		}
	}
	return false
}

// claimRelease atomically latches the child's teardown and unpublishes it,
// reporting whether THIS call won. Concurrent release requests (retention
// timer, finished cap, parent cascade) therefore reach the host exactly once.
// Lock order: registry.mu → Child.mu (the child's lock is a leaf).
func (r *registry[H]) claimRelease(c *Child[H]) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.Released() {
		return false
	}
	c.markReleased()
	delete(r.children, c.ID)
	return true
}

// activeCountLocked counts the ACTIVE children of parentID: background
// children whose StatusOf reports "running" (main job in flight, or mid-turn
// replying to a send_message) plus in-flight foreground children. An
// interrupted (idle) child holds no slot — interrupt frees it, as the
// spawn-refusal error promises. Caller holds mu.
func (r *registry[H]) activeCountLocked(parentID string, host Host[H]) int {
	n := r.foreground[parentID]
	for _, c := range r.children {
		if c.ParentID == parentID && c.StatusOf(host) == "running" {
			n++
		}
	}
	return n
}

// liveCountLocked counts the NON-finished children of parentID (running +
// idle background + in-flight foreground) — the memory guard's metric: an
// idle child holds no slot of the concurrent limit, but its runtime still
// lives until the retention window, so it is bounded separately. Caller holds
// mu.
func (r *registry[H]) liveCountLocked(parentID string) int {
	n := r.foreground[parentID]
	for _, c := range r.children {
		if c.ParentID == parentID && !c.IsFinished() {
			n++
		}
	}
	return n
}

// guardExceeded reports whether parentID already holds guard NON-finished
// children. It is the admission check for a REVIVED child (loaded from the
// store for a send_message): unlike a spawn there is no in-flight child to
// count itself, so a plain ">=" is the whole rule. guard <= 0 means unbounded.
func (r *registry[H]) guardExceeded(parentID string, guard int) bool {
	if r == nil || guard <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.liveCountLocked(parentID) >= guard
}

// acquireForeground atomically checks both spawn caps and, when allowed,
// reserves a foreground slot for parentID: the user-facing limit against
// active children (the new child counts itself via the increment, so >=
// refuses at a full cap) and the internal guard against non-finished
// children. hitGuard reports a refusal by the guard rather than the
// user-facing limit. Check and increment are one critical section, so
// concurrent spawns cannot slip past either cap. A nil registry (hosts that
// only exercise the foreground path in tests) is unbounded.
func (r *registry[H]) acquireForeground(parentID string, host Host[H], limit, guard int) (ok bool, hitGuard bool) {
	if r == nil {
		return true, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit > 0 && r.activeCountLocked(parentID, host) >= limit {
		return false, false
	}
	if r.liveCountLocked(parentID) >= guard {
		return false, true
	}
	r.foreground[parentID]++
	return true, false
}

// releaseForeground returns a slot reserved by acquireForeground. The key is
// deleted at zero so a long-lived host does not accumulate one entry per
// session.
func (r *registry[H]) releaseForeground(parentID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.foreground[parentID] - 1; n > 0 {
		r.foreground[parentID] = n
	} else {
		delete(r.foreground, parentID)
	}
}

// spawnCapExceeded reports, in one snapshot, whether the spawn the caller has
// ALREADY committed (the child is registered) exceeds either cap: the
// user-facing active limit or the internal non-finished guard. Strictly
// greater because the committed child counts itself in both.
func (r *registry[H]) spawnCapExceeded(parentID string, host Host[H], limit, guard int) (activeOver, guardOver bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return limit > 0 && r.activeCountLocked(parentID, host) > limit,
		r.liveCountLocked(parentID) > guard
}

// finishedChildren returns the finished children of parentID, oldest finish
// first — the release order for the per-parent finished cap.
func (r *registry[H]) finishedChildren(parentID string) []*Child[H] {
	r.mu.Lock()
	var out []*Child[H]
	for _, c := range r.children {
		if c.ParentID == parentID && c.IsFinished() {
			out = append(out, c)
		}
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b *Child[H]) int { return a.FinishedAt().Compare(b.FinishedAt()) })
	return out
}

// allChildren returns every live child (used by the parent cascade).
func (r *registry[H]) allChildren() []*Child[H] {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Child[H], 0, len(r.children))
	for _, c := range r.children {
		out = append(out, c)
	}
	return out
}
