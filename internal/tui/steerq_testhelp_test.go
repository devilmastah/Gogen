package tui

import "gogen/internal/steerq"

// testQueue builds a steering queue already holding items, so tests can
// install queue contents directly instead of driving the enqueue paths.
func testQueue(items ...steerq.Item) *steerq.Queue {
	q := steerq.New(maxSteerUserQueue, maxPendingDeliveries)
	for _, it := range items {
		// Seed directly: the cap policy must not refuse fixtures.
		q.ForceAppend(it)
	}
	return q
}

// notice is a one-way machine delivery (the batchable kind).
func notice(id, text string) steerq.Item {
	return steerq.Item{ID: id, Text: text, Kind: steerq.Notice}
}

// userItem is text the user typed into a busy session.
func userItem(id, text string) steerq.Item {
	return steerq.Item{ID: id, Text: text, Kind: steerq.User}
}
