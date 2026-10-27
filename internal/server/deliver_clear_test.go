package server

import (
	"strings"
	"testing"
)

// TestPoppedQueueItemsAreCleared pins the server side of M3: dropping a
// delivered item must remove it from the queue (the slot-zeroing that keeps a
// popped item's multi-MB data-URL images out of the backing array now lives in
// internal/steerq, covered by TestTakeZeroesPoppedSlots there).
func TestPoppedQueueItemsAreCleared(t *testing.T) {
	rt := newTestRuntime(t)
	big := strings.Repeat("x", 1<<20)
	seedDeliveries(rt,
		deliverItem{ID: "q1", Kind: deliverUser, Text: big},
		deliverItem{ID: "q2", Kind: deliverUser, Text: "second"},
	)
	rt.deliverMu.Lock()
	rt.dropDeliveredLocked(1)
	rt.deliverMu.Unlock()

	q := queuedItems(rt)
	if len(q) != 1 || q[0].ID != "q2" {
		t.Fatalf("queue after pop = %+v", q)
	}
}
