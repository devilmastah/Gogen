package server

import (
	"runtime"
	"testing"
	"unsafe"

	"github.com/gorilla/websocket"
)

// TestWSSendQueueMemoryBounded pins the send-queue's memory shape: the queue
// must hold POINTERS, not WSMessage values.
//
// A buffered channel allocates its whole ring eagerly at make, and WSMessage
// is ~2.1 KB (model list, session list, image attachments, history rewind,
// queue payloads). A value channel at wsSendQueueSize therefore reserves
// ~8.3 MB of empty buffer per connection before a single frame is sent —
// measured as the dominant consumer of a live web server's heap (two sockets
// per browser tab, and the connection limiter allows
// defaultMaxWSConns=32, i.e. ~267 MB of empty buffers at the cap). Every
// reconnecting socket allocates another one, which is what makes a
// long-running server's RSS ratchet upward.
//
// The assertion is deliberately loose (8 MB for 32 connections ≈ 256 KB
// each): it is an order-of-magnitude guard, not a measurement of the exact
// allocation, so it stays stable across Go versions and GC timing while still
// failing loudly if the queue ever goes back to holding values.
func TestWSSendQueueMemoryBounded(t *testing.T) {
	const conns = 32
	msgSize := int(unsafe.Sizeof(WSMessage{}))

	// What the value-channel form would reserve for these connections. The
	// test is only meaningful while that number is the alarmingly large one
	// this guard exists for.
	valueBytes := int64(msgSize) * int64(wsSendQueueSize) * conns
	if valueBytes < 64<<20 {
		t.Fatalf("WSMessage (%d B) × wsSendQueueSize (%d) × %d conns = %d MB, too small for this guard to be meaningful — revisit it",
			msgSize, wsSendQueueSize, conns, valueBytes>>20)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	queues := make([]*wsConn, 0, conns)
	for i := 0; i < conns; i++ {
		w := &wsConn{
			conn:  &websocket.Conn{},
			sendQ: make(chan *WSMessage, wsSendQueueSize),
			quit:  make(chan struct{}),
			done:  make(chan struct{}),
		}
		queues = append(queues, w)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	used := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("WSMessage=%d B, %d conns: idle send queues hold %d KB total (value channels would hold %d MB)",
		msgSize, conns, used>>10, valueBytes>>20)

	const budget = 8 << 20 // 8 MB for 32 idle queues
	if used > budget {
		t.Fatalf("idle send queues hold %d KB for %d connections (budget %d KB) — the queue must carry *WSMessage, not WSMessage",
			used>>10, conns, budget>>10)
	}
	// Sanity: the queues are real and usable.
	for _, w := range queues {
		if err := w.enqueueJSON(WSMessage{Type: "noop"}); err != nil {
			t.Fatalf("enqueue on idle queue: %v", err)
		}
		if got := <-w.sendQ; got == nil || got.Type != "noop" {
			t.Fatalf("dequeued frame = %+v, want Type=noop", got)
		}
	}
}
