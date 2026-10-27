package server

import (
	"runtime"
	"testing"
	"time"
)

// TestWSSendQueueIdleMemoryBounded pins the send-queue's memory shape: the
// queue must hold POINTERS to prepared frames, not WSMessage values.
//
// A buffered channel allocates its whole ring eagerly at make, and WSMessage is
// ~2.1 KB (model list, session list, image attachments, history rewind, queue
// payloads). A value channel at wsSendQueueSize would reserve ~8.3 MB of empty
// buffer per connection before a single frame is sent — two sockets per browser
// tab, and the connection limiter allows 32, i.e. ~267 MB of empty buffers.
//
// The assertion is deliberately loose (8 MB for 32 connections): an
// order-of-magnitude guard, not an exact measurement, so it stays stable across
// Go versions and GC timing while still failing loudly if the queue goes back to
// holding values.
func TestWSSendQueueIdleMemoryBounded(t *testing.T) {
	const conns = 32

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	queues := make([]*wsConn, 0, conns)
	for i := 0; i < conns; i++ {
		queues = append(queues, &wsConn{
			conn:  nil, // idle queues only: no writer, nothing to write
			sendQ: make(chan *wsOut, wsSendQueueSize),
			quit:  make(chan struct{}),
			done:  make(chan struct{}),
		})
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	used := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d conns: idle send queues hold %d KB total", conns, used>>10)

	const budget = 8 << 20 // 8 MB for 32 idle queues
	if used > budget {
		t.Fatalf("idle send queues hold %d KB for %d connections (budget %d KB) — the queue must carry pointers, not WSMessage values",
			used>>10, conns, budget>>10)
	}
	if len(queues) != conns {
		t.Fatalf("built %d queues", conns)
	}
}

// TestSendQueueBoundedByBytes pins the backpressure contract that replaced the
// frame count: the budget is the ENCODED SIZE of the backlog, so few large
// frames and many small ones are bounded the same way, and exceeding it is
// refused (the caller detaches) instead of blocking the producer forever.
func TestSendQueueBoundedByBytes(t *testing.T) {
	conn := newFakeWSConn() // no writer goroutine: nothing drains the queue
	half := int(wsMaxQueuedBytes/2) + 1
	out, err := prepareOut(&WSMessage{Type: "history"})
	if err != nil {
		t.Fatal(err)
	}
	// Fabricate sizes (the payload itself need not be huge): the accounting is
	// what is under test.
	first := *out
	first.size = half
	if err := conn.enqueueOut(&first); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if got := conn.queuedBytes.Load(); got != int64(half) {
		t.Fatalf("queuedBytes = %d, want %d", got, half)
	}
	second := *out
	second.size = half
	if err := conn.enqueueOut(&second); err != errWSQueueFull {
		t.Fatalf("second enqueue error = %v, want errWSQueueFull", err)
	}
	// The refused frame must not have leaked into the accounting.
	if got := conn.queuedBytes.Load(); got != int64(half) {
		t.Fatalf("queuedBytes after refusal = %d, want %d", got, half)
	}
}

// TestSendQueueEnqueueWaitBounds pins the second half of the bound: when the
// byte budget allows a frame but the channel depth is exhausted (many tiny
// frames), the producer waits at most wsEnqueueWait and then fails rather than
// stalling the turn.
func TestSendQueueEnqueueWaitBounds(t *testing.T) {
	conn := newFakeWSConn()
	for i := 0; i < cap(conn.sendQ); i++ {
		out, err := prepareOut(&WSMessage{Type: "token"})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.enqueueOut(out); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	start := time.Now()
	out, err := prepareOut(&WSMessage{Type: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.enqueueOut(out); err != errWSQueueFull {
		t.Fatalf("enqueue on a full depth = %v, want errWSQueueFull", err)
	}
	if waited := time.Since(start); waited < wsEnqueueWait/2 {
		t.Fatalf("returned after %s, want a bounded wait of ~%s", waited, wsEnqueueWait)
	}
}
