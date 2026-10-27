package server

import (
	"gogen/internal/steerq"
)

// seedDeliveries replaces the runtime's delivery queue with items, so tests can
// install queue contents directly instead of driving the enqueue paths (which
// validate client ids, notify the worker, and broadcast).
func seedDeliveries(rt *sessionRuntime, items ...deliverItem) {
	q := steerq.New(maxUserQueueCap, maxSystemDeliveryQueue)
	for _, it := range items {
		q.ForceAppend(deliverItemFrom(it.ID, it.Text, it.Kind, it.Source, it.Images))
	}
	rt.deliverMu.Lock()
	rt.pendingDeliver = q
	rt.deliverMu.Unlock()
}

// queuedItems snapshots the runtime's queue for assertions.
func queuedItems(rt *sessionRuntime) []steerq.Item {
	rt.deliverMu.Lock()
	defer rt.deliverMu.Unlock()
	return rt.pendingDeliver.Items()
}
