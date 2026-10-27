package server

import (
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestBroadcastSharesOnePreparedFrame pins the fan-out contract: a broadcast
// marshals and prepares its payload ONCE and every client's queued frame points
// at that same prepared message. Previously each client re-marshaled (and,
// since the upgrader negotiates permessage-deflate, re-deflated) the identical
// payload in its own writer goroutine — the dominant cost of broadcasting a
// multi-MB session history to several tabs.
func TestBroadcastSharesOnePreparedFrame(t *testing.T) {
	rt := newTestRuntime(t)
	clients := []*wsConn{newFakeWSConn(), newFakeWSConn(), newFakeWSConn()}
	for _, c := range clients {
		rt.attach(c)
	}

	msg := WSMessage{
		Type:      "history",
		SessionID: "s1",
		Content:   strings.Repeat("payload ", 4096), // a payload worth sharing
	}
	rt.broadcast(msg)

	var shared *websocket.PreparedMessage
	var accounted int
	for i, c := range clients {
		out, ok := nextFrame(c)
		if !ok {
			t.Fatalf("client %d got no frame", i)
		}
		if out.frame == nil {
			t.Fatalf("client %d queued a frame with no prepared message", i)
		}
		if shared == nil {
			shared, accounted = out.frame, out.size
			continue
		}
		if out.frame != shared {
			t.Fatalf("client %d prepared its own frame: the payload was marshaled/deflated per client", i)
		}
		if out.size != accounted {
			t.Fatalf("client %d accounted %d bytes, client 0 accounted %d", i, out.size, accounted)
		}
	}
	// The budget is accounted in the ENCODED payload size, not in frames.
	if accounted < 1000 {
		t.Fatalf("accounted size = %d, want the encoded payload length", accounted)
	}
}

// nextFrame pops one queued frame without needing the captured message.
func nextFrame(w *wsConn) (*wsOut, bool) {
	select {
	case out := <-w.sendQ:
		return out, out != nil
	default:
		return nil, false
	}
}

// BenchmarkBroadcastFanout measures the fan-out with the payload prepared once
// (what broadcast now does) against the per-client marshal+prepare it replaced.
// The gap is the cost a broadcast to K tabs used to pay K times.
func BenchmarkBroadcastFanout(b *testing.B) {
	const payload = 256 << 10
	msg := WSMessage{Type: "history", SessionID: "s1", Content: strings.Repeat("x", payload)}

	b.Run("prepared-once", func(b *testing.B) {
		conns := make([]*wsConn, 8)
		for i := range conns {
			conns[i] = &wsConn{conn: nil} // enqueue only: measure prepare+accounting
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out, err := prepareOut(&msg)
			if err != nil {
				b.Fatal(err)
			}
			_ = out
		}
	})

	b.Run("per-client-marshal", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for c := 0; c < 8; c++ {
				out, err := prepareOut(&msg)
				if err != nil {
					b.Fatal(err)
				}
				_ = out
			}
		}
	})
}
