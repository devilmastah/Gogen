package server

import (
	"testing"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm/llmtest"
)

// drainQueued pops every frame the fake connection has queued.
func drainQueued(w *wsConn) []WSMessage {
	var out []WSMessage
	for {
		m, ok := w.nextQueued()
		if !ok {
			return out
		}
		out = append(out, m)
	}
}

// countType counts the queued frames of type typ (popping them all).
func countType(w *wsConn, typ string) int {
	n := 0
	for _, m := range drainQueued(w) {
		if m.Type == typ {
			n++
		}
	}
	return n
}

// broadcastAllFixture builds a server with two registered session runtimes and
// two fake sockets: shared is attached to BOTH runtimes (its pane plus a
// background pane, the multi-pane shape a browser tab produces) and single is
// attached to rt1 only.
type broadcastAllFixture struct {
	s      *Server
	rt1    *sessionRuntime
	rt2    *sessionRuntime
	shared *wsConn
	single *wsConn
}

func newBroadcastAllFixture(t *testing.T) *broadcastAllFixture {
	t.Helper()
	a := agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(t.TempDir()), nil)
	s := newTestServer(t, a, &config.Config{})
	rt1 := newSessionRuntime(agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(t.TempDir()), nil))
	rt2 := newSessionRuntime(agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(t.TempDir()), nil))
	s.registry.register("s1", rt1)
	s.registry.register("s2", rt2)

	shared, single := newFakeWSConn(), newFakeWSConn()
	for _, rt := range []*sessionRuntime{rt1, rt2} {
		if !rt.attach(shared) {
			t.Fatal("attach shared socket refused")
		}
	}
	if !rt1.attach(single) {
		t.Fatal("attach single socket refused")
	}
	return &broadcastAllFixture{s: s, rt1: rt1, rt2: rt2, shared: shared, single: single}
}

// TestServerWideBroadcastsDedupePerSocket pins the P8a fix: a socket attached
// to MORE THAN ONE session runtime must receive a server-wide frame (board
// state, board notice, automations state, status-display row) exactly ONCE,
// while a socket attached to one runtime still gets its own copy. The payloads
// are process-wide — a socket watching K panes used to get K copies, i.e. K
// board re-renders and K toasts.
func TestServerWideBroadcastsDedupePerSocket(t *testing.T) {
	tests := []struct {
		name string
		// send runs the deduped helper under test.
		send func(f *broadcastAllFixture)
		// typ is the frame type it must deliver.
		typ string
	}{
		{
			name: "board state",
			send: func(f *broadcastAllFixture) {
				f.s.ws.ensureBoardManager()
				f.s.broadcastBoardState()
			},
			typ: "board_state",
		},
		{
			name: "board notice",
			send: func(f *broadcastAllFixture) { f.s.broadcastBoardNotice("Moved board item #1") },
			typ:  "notice",
		},
		{
			name: "automations state",
			send: func(f *broadcastAllFixture) { f.s.broadcastAutomationsState() },
			typ:  "automations_state",
		},
		{
			name: "status display",
			send: func(f *broadcastAllFixture) {
				f.s.broadcastStatusDisplay([]byte(`{"state":"running"}`), false, "src")
			},
			typ: "status_display",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBroadcastAllFixture(t)
			tc.send(f)
			if got := countType(f.shared, tc.typ); got != 1 {
				t.Fatalf("frames of type %q on the two-runtime socket = %d, want 1", tc.typ, got)
			}
			if got := countType(f.single, tc.typ); got != 1 {
				t.Fatalf("frames of type %q on the single-runtime socket = %d, want 1", tc.typ, got)
			}
		})
	}
}

// TestServerWideBroadcastDetachesFailedSocketFromEveryRuntime pins the other
// half of the fix: a write failure still detaches the socket, and it must be
// detached from EVERY runtime that held it — detach is per-runtime, so
// detaching only the first would leave a stale client entry in the second
// until that runtime's own broadcast failed.
func TestServerWideBroadcastDetachesFailedSocketFromEveryRuntime(t *testing.T) {
	f := newBroadcastAllFixture(t)
	// A socket whose connection is gone: every write fails with errWSClosed.
	failing := newFakeWSConn()
	failing.conn = nil
	for _, rt := range []*sessionRuntime{f.rt1, f.rt2} {
		rt.clientsMu.Lock()
		rt.clients[failing] = struct{}{}
		rt.clientsMu.Unlock()
	}

	f.s.broadcastBoardNotice("Moved board item #1")

	for i, rt := range []*sessionRuntime{f.rt1, f.rt2} {
		rt.clientsMu.Lock()
		_, held := rt.clients[failing]
		rt.clientsMu.Unlock()
		if held {
			t.Fatalf("runtime %d still holds the failed socket", i+1)
		}
	}
	// The healthy sockets are untouched.
	if n := len(drainQueued(f.shared)); n != 1 {
		t.Fatalf("shared socket frames = %d, want 1", n)
	}
	if n := len(drainQueued(f.single)); n != 1 {
		t.Fatalf("single socket frames = %d, want 1", n)
	}
}
