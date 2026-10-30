package server

import (
	"testing"

	"gogen/internal/steerq"
)

// TestRemoveDropsParentDeliveries pins the cleanup: the per-parent delivery
// backlog is keyed by session id, and a removed session's undrained messages
// stayed reachable for the life of the process (and would be re-targeted if the
// id were ever reused).
func TestRemoveDropsParentDeliveries(t *testing.T) {
	r := newSessionRegistry(4)
	rt := newTestRuntime(t)
	rt.agent.SessionID = "parent-1"
	r.register("parent-1", rt)

	stash := steerq.New(0, steerq.SystemCap)
	stash.Append(steerq.Item{ID: steerq.NewID(), Text: "undrained notice", Kind: steerq.Notice})
	r.parentDeliverMu.Lock()
	r.parentDeliveries["parent-1"] = stash
	r.parentDeliverMu.Unlock()

	r.remove("parent-1")

	r.parentDeliverMu.Lock()
	defer r.parentDeliverMu.Unlock()
	if got := r.parentDeliveries["parent-1"]; got != nil {
		t.Fatalf("parent deliveries survived removal: %#v", got)
	}
}
