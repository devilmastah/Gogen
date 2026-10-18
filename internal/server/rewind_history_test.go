package server

// The rewind payload: a mid-turn attach/resume must carry the in-flight
// turn's partial output (content/thinking/tool-args and their character
// positions) so a client switching to a running session sees the current
// reply instead of "Resuming…" with no context until the turn ends. Idle
// attaches must carry no rewind, and history payloads must stamp the
// conversation epoch so clients can detect reshaped (compacted/rolled back)
// histories.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// rewindStreamStub streams a deterministic partial reply (content, thinking,
// and optionally a streaming tool call), then blocks until released, then
// completes the turn. The partial is therefore well-defined for an attach
// that lands while the turn is in flight.
type rewindStreamStub struct {
	mu          sync.Mutex
	calls       int
	entered     int
	releaseCh   chan struct{}
	streamTools []llm.ToolCall // when set, call 1 streams this tool call's args
	rawArgs     string         // raw args deltas for the streamed tool call
}

func newRewindStreamStub() *rewindStreamStub {
	return &rewindStreamStub{releaseCh: make(chan struct{})}
}

func (s *rewindStreamStub) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.releaseCh:
	default:
		close(s.releaseCh)
	}
}

// waitStreaming waits until call 1 has streamed its partial and entered the
// blocking section (the buffer then holds exactly the partial).
func (s *rewindStreamStub) waitStreaming() {
	for {
		s.mu.Lock()
		e := s.entered
		s.mu.Unlock()
		if e >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *rewindStreamStub) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *rewindStreamStub) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	if h.OnStart != nil {
		h.OnStart()
	}
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	s.mu.Lock()
	s.calls++
	call := s.calls
	tools := append([]llm.ToolCall(nil), s.streamTools...)
	rawArgs := s.rawArgs
	s.mu.Unlock()

	if call == 1 {
		// Stream the partial, then block so the test can attach mid-turn.
		if h.OnThinkingToken != nil {
			h.OnThinkingToken("reasoning...")
		}
		for _, w := range []string{"the ", "quick ", "brown "} {
			if h.OnToken != nil {
				h.OnToken(w)
			}
		}
		if len(tools) > 0 {
			tc := tools[0]
			if h.OnToolCallStart != nil {
				h.OnToolCallStart(0, tc.ID, tc.Name)
			}
			for i := 0; i < len(rawArgs); i += 4 {
				end := i + 4
				if end > len(rawArgs) {
					end = len(rawArgs)
				}
				if h.OnToolCallArgsDelta != nil {
					h.OnToolCallArgsDelta(0, tc.ID, tc.Name, rawArgs[i:end])
				}
			}
		}
		s.mu.Lock()
		s.entered++
		s.mu.Unlock()
		select {
		case <-s.releaseCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if h.OnStreamEnd != nil {
			h.OnStreamEnd()
		}
		if len(tools) > 0 {
			return &llm.StreamResult{ToolCalls: tools}, nil
		}
		return &llm.StreamResult{Content: "the quick brown fox"}, nil
	}
	if h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
	return &llm.StreamResult{Content: "done"}, nil
}

func (s *rewindStreamStub) ModelContextLimit(_ context.Context) (int, error) { return 1000, nil }
func (s *rewindStreamStub) SetThinkingLevel(string)                          {}
func (s *rewindStreamStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *rewindStreamStub) SetModel(string) error { return nil }
func (s *rewindStreamStub) ModelName() string     { return "rewind-model" }

// drainHandshakePayloads reads the connect handshake's asynchronous payload
// frames (the basic and the full config, written after the history decision)
// so the handshake goroutine is provably done before the test drives the
// session. The payload goroutine snapshots the history WITHOUT the turn lock:
// a goroutine starved until after the turn below started would deliver a
// stale history frame whose rewind is empty (it snapshotted between the user
// message append and the first streamed token), and the mid-turn attach read
// would match that stale frame first — the send queue is FIFO (Windows CI
// flake: "mid-turn history must carry a rewind").
func drainHandshakePayloads(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	for i := 0; i < 2; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	}
}

func newRewindServer(t *testing.T, stub *rewindStreamStub, dir string) (*Server, *agent.Agent, *session.Store) {
	t.Helper()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	s := newTestServer(t, a, &config.Config{})
	return s, a, store
}

// TestMidTurnAttachCarriesRewind is the core guarantee: attach while a turn
// is streaming — the history payload must carry the partial content and
// thinking with exact character positions (and the conversation epoch).
func TestMidTurnAttachCarriesRewind(t *testing.T) {
	dir := t.TempDir()
	stub := newRewindStreamStub()
	s, a, _ := newRewindServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	drainHandshakePayloads(t, conn)
	sid := a.SessionID

	// Start a turn; the stub streams "the quick brown " + thinking and blocks.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitStreaming()

	// Attach mid-turn (the pane re-attach flow).
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sid}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	hist := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "history" && m.SessionID == sid
	})
	if hist.Rewind == nil {
		t.Fatal("mid-turn history must carry a rewind (the in-flight reply)")
	}
	if hist.Rewind.Content != "the quick brown " {
		t.Fatalf("rewind content = %q, want %q", hist.Rewind.Content, "the quick brown ")
	}
	if hist.Rewind.ContentPos != len("the quick brown ") {
		t.Fatalf("rewind contentPos = %d, want %d", hist.Rewind.ContentPos, len("the quick brown "))
	}
	if hist.Rewind.Thinking != "reasoning..." {
		t.Fatalf("rewind thinking = %q, want %q", hist.Rewind.Thinking, "reasoning...")
	}
	if hist.Rewind.ThinkingPos != len("reasoning...") {
		t.Fatalf("rewind thinkingPos = %d, want %d", hist.Rewind.ThinkingPos, len("reasoning..."))
	}
	if hist.HistoryEpoch != a.HistoryEpoch() {
		t.Fatalf("history epoch = %d, want agent epoch %d", hist.HistoryEpoch, a.HistoryEpoch())
	}

	// Release: the turn completes normally.
	stub.release()
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == sid
	})
}

// TestMidTurnAttachCarriesToolCall verifies a streaming tool call (raw args
// deltas + position) is captured in the rewind so the client can continue
// the card without losing or duplicating its arguments.
func TestMidTurnAttachCarriesToolCall(t *testing.T) {
	dir := t.TempDir()
	stub := newRewindStreamStub()
	stub.streamTools = []llm.ToolCall{{ID: "call_1", Name: "read_file", Args: map[string]any{"path": "/nonexistent"}}}
	stub.rawArgs = `{"path":"/nonexistent"}`
	s, a, _ := newRewindServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	drainHandshakePayloads(t, conn)
	sid := a.SessionID

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitStreaming()

	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sid}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	hist := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "history" && m.SessionID == sid
	})
	if hist.Rewind == nil || len(hist.Rewind.ToolCalls) != 1 {
		t.Fatalf("rewind tool calls = %+v, want exactly one", hist.Rewind)
	}
	tc := hist.Rewind.ToolCalls[0]
	if tc.Index != 0 || tc.ID != "call_1" || tc.Name != "read_file" {
		t.Fatalf("rewind tool call = %+v, want index 0 call_1 read_file", tc)
	}
	if tc.Args != `{"path":"/nonexistent"}` || tc.ArgsPos != len(`{"path":"/nonexistent"}`) {
		t.Fatalf("rewind args = %q (pos %d), want full raw args", tc.Args, tc.ArgsPos)
	}

	// Release: the tool executes (fails on the missing path), the loop
	// completes with call 2, and the turn ends normally.
	stub.release()
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == sid
	})
}

// TestIdleAttachHasNoRewind: a completed/idle session's attach must not
// carry a rewind (nothing is in flight).
func TestIdleAttachHasNoRewind(t *testing.T) {
	dir := t.TempDir()
	stub := newRewindStreamStub()
	s, a, _ := newRewindServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	drainHandshakePayloads(t, conn)
	sid := a.SessionID

	// One completed turn.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitStreaming()
	stub.release()
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })

	// Attach after completion: no rewind, and the completed reply is present.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sid}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	hist := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "history" && m.SessionID == sid
	})
	if hist.Rewind != nil {
		t.Fatalf("idle attach must carry no rewind, got %+v", hist.Rewind)
	}
	got := historyContent(hist)
	if len(got) != 2 || got[0] != "user:q1" || got[1] != "assistant:the quick brown fox" {
		t.Fatalf("completed history = %v, want [user:q1 assistant:the quick brown fox]", got)
	}
}

// TestHistoryEpochBumpsOnWholesaleChange pins the epoch semantics the client
// staleness guard relies on: append-only turns do not bump it, but a
// wholesale replacement (reset/compact/rollback) does.
func TestHistoryEpochBumpsOnWholesaleChange(t *testing.T) {
	dir := t.TempDir()
	exec := agent.NewExecutor(dir)
	stub := newBlockingStub()
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	if a.HistoryEpoch() != 0 {
		t.Fatalf("epoch = %d, want 0", a.HistoryEpoch())
	}
	a.SessionID = "s1"
	a.ResetSessionState() // replaceMessages(nil): wholesale change
	if a.HistoryEpoch() != 1 {
		t.Fatalf("epoch after reset = %d, want 1", a.HistoryEpoch())
	}
}
