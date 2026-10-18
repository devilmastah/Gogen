package server

// The tool-args frame path: argument deltas are the highest-rate callback in
// a tool-heavy turn (a patch_file diff arrives as hundreds of ~1KB
// fragments). They must reach the client coalesced (one tool_call_delta per
// flush interval, like content tokens) rather than one frame per provider
// chunk, and each flushed frame must carry an ArgsPos that satisfies the
// client's trimToEnd merge invariant: endPos - len(delta) equals the previous
// endPos exactly, so concatenating every frame's delta reproduces the full
// args with no gap and no duplicate.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/streambuf"

	"github.com/gorilla/websocket"
)

// argsStreamStub streams one tool call's args in many small fragments and
// then completes the turn. call 1 returns the tool call; call 2 ends the
// turn so the stub never loops forever.
type argsStreamStub struct {
	mu    sync.Mutex
	calls int

	toolName string
	toolID   string
	rawArgs  string
	chunkLen int
}

func (s *argsStreamStub) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *argsStreamStub) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	name := s.toolName
	id := s.toolID
	rawArgs := s.rawArgs
	chunkLen := s.chunkLen
	s.mu.Unlock()

	if call != 1 {
		if h.OnStreamEnd != nil {
			h.OnStreamEnd()
		}
		return &llm.StreamResult{Content: "done"}, nil
	}
	if h.OnStart != nil {
		h.OnStart()
	}
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	if h.OnToolCallStart != nil {
		h.OnToolCallStart(0, id, name)
	}
	if chunkLen <= 0 {
		chunkLen = 64
	}
	for i := 0; i < len(rawArgs); i += chunkLen {
		end := i + chunkLen
		if end > len(rawArgs) {
			end = len(rawArgs)
		}
		if h.OnToolCallArgsDelta != nil {
			h.OnToolCallArgsDelta(0, id, name, rawArgs[i:end])
		}
	}
	if h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
	return &llm.StreamResult{
		ToolCalls: []llm.ToolCall{{Index: 0, ID: id, Name: name, Args: map[string]any{"diff": rawArgs}}},
	}, nil
}

func (s *argsStreamStub) ModelContextLimit(_ context.Context) (int, error) { return 100000, nil }
func (s *argsStreamStub) SetThinkingLevel(string)                          {}
func (s *argsStreamStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *argsStreamStub) SetModel(string) error { return nil }
func (s *argsStreamStub) ModelName() string     { return "args-model" }

func newArgsStreamServer(t *testing.T, stub *argsStreamStub) (*Server, *agent.Agent) {
	t.Helper()
	exec := agent.NewExecutor(t.TempDir())
	a := agent.NewAgent(stub, exec, nil)
	return newTestServer(t, a, &config.Config{}), a
}

// collectUntil reads messages until match returns true, accumulating every
// tool_call_delta frame seen along the way.
func collectUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, match func(WSMessage) bool) []WSMessage {
	t.Helper()
	var deltas []WSMessage
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for matching WS message")
		}
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		if msg.Type == "tool_call_delta" {
			deltas = append(deltas, msg)
		}
		if match(msg) {
			return deltas
		}
	}
}

// TestWSToolCallArgsDeltasAreCoalesced is the core frame-count guarantee: a
// large diff streamed in small chunks must produce far fewer
// tool_call_delta frames than chunks, while delivering the exact same bytes.
func TestWSToolCallArgsDeltasAreCoalesced(t *testing.T) {
	// ~40 KB of "diff" body in 512-byte fragments => ~80 chunks.
	body := strings.Repeat("+added line of the patch\n", 1600)
	stub := &argsStreamStub{
		toolName: "patch_file",
		toolID:   "call_patch",
		rawArgs:  `{"diff":"` + body + `"}`,
		chunkLen: 512,
	}
	s, a := newArgsStreamServer(t, stub)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "patch it", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}

	deltas := collectUntil(t, conn, 20*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == a.SessionID
	})

	chunks := (len(stub.rawArgs) + 511) / 512
	if len(deltas) == 0 {
		t.Fatal("no tool_call_delta frames received")
	}
	if len(deltas) >= chunks {
		t.Fatalf("got %d delta frames for %d chunks — args are not being coalesced", len(deltas), chunks)
	}
	t.Logf("coalesced %d chunks into %d frames (%d bytes)", chunks, len(deltas), len(stub.rawArgs))

	// Every byte must still arrive, in order.
	var got strings.Builder
	for _, d := range deltas {
		got.WriteString(d.ArgsDelta)
	}
	if got.String() != stub.rawArgs {
		t.Fatalf("reassembled args differ from streamed args: got %d bytes, want %d",
			got.Len(), len(stub.rawArgs))
	}
}

// TestWSToolCallDeltaArgsPosIsMonotonicAndExact pins the wire invariant the
// client's trimToEnd depends on: for each frame, ArgsPos - len(ArgsDelta)
// equals the previous frame's ArgsPos (starting at 0). Any batching bug that
// stamped buffer-length positions would break this and make the client drop
// or duplicate a tail.
func TestWSToolCallDeltaArgsPosIsMonotonicAndExact(t *testing.T) {
	// Multi-byte content: positions are UTF-16 code units (streambuf.UTF16Len),
	// so an emoji/CJK body catches a byte-vs-code-unit mixup.
	body := strings.Repeat("+ línea con acento y emoji 😀\n", 400)
	stub := &argsStreamStub{
		toolName: "patch_file",
		toolID:   "call_utf16",
		rawArgs:  `{"diff":"` + body + `"}`,
		chunkLen: 256,
	}
	s, a := newArgsStreamServer(t, stub)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "patch it", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}

	deltas := collectUntil(t, conn, 20*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == a.SessionID
	})
	if len(deltas) == 0 {
		t.Fatal("no tool_call_delta frames received")
	}

	// ArgsPos is stamped in UTF-16 code units because that is what the
	// browser's String.length / slice operate in — the client computes
	// start = endPos - text.length in the same unit. Compare in UTF-16
	// units, not bytes, or a multi-byte body falsely reports a mismatch.
	prev := 0
	for i, d := range deltas {
		if d.Index != 0 {
			t.Fatalf("frame[%d].Index = %d, want 0", i, d.Index)
		}
		if d.ArgsPos <= prev && i > 0 {
			t.Fatalf("frame[%d].ArgsPos = %d, want > previous %d (positions must be monotonic)", i, d.ArgsPos, prev)
		}
		if start := d.ArgsPos - streambuf.UTF16Len(d.ArgsDelta); start != prev {
			t.Fatalf("frame[%d] breaks the trimToEnd invariant: ArgsPos(%d) - utf16Len(delta)(%d) = %d, want %d",
				i, d.ArgsPos, streambuf.UTF16Len(d.ArgsDelta), start, prev)
		}
		prev = d.ArgsPos
	}
	// The final position must equal the full args length in UTF-16 units.
	if want := streambuf.UTF16Len(stub.rawArgs); prev != want {
		t.Fatalf("final ArgsPos = %d, want %d (full args in UTF-16 code units)", prev, want)
	}
}

// TestWSToolCallDeltaPrecedesToolCall: the coalesced args must be flushed
// before the tool_call frame that finalizes the card, or the client renders
// a card with truncated arguments.
func TestWSToolCallDeltaPrecedesToolCall(t *testing.T) {
	stub := &argsStreamStub{
		toolName: "patch_file",
		toolID:   "call_order",
		rawArgs:  `{"diff":"` + strings.Repeat("x", 4096) + `"}`,
		chunkLen: 256,
	}
	s, a := newArgsStreamServer(t, stub)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "patch it", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var sawStart, sawFinal bool
	var argsBeforeFinal strings.Builder
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for turn_end")
		}
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		switch msg.Type {
		case "tool_call_start":
			sawStart = true
		case "tool_call_delta":
			if sawFinal {
				t.Fatal("tool_call_delta arrived after tool_call")
			}
			argsBeforeFinal.WriteString(msg.ArgsDelta)
		case "tool_call":
			sawFinal = true
			if argsBeforeFinal.Len() != len(stub.rawArgs) {
				t.Fatalf("tool_call arrived with %d bytes of args, want %d (args not flushed first)",
					argsBeforeFinal.Len(), len(stub.rawArgs))
			}
		case "turn_end":
			if msg.SessionID != a.SessionID {
				continue
			}
			if !sawStart || !sawFinal {
				t.Fatalf("turn ended without the tool sequence: start=%v final=%v", sawStart, sawFinal)
			}
			return
		}
	}
}

// TestWSToolCallDeltaFieldsCarryIdentity: the coalesced frame must still
// carry the tool name and call id (the batcher merges many fragments that
// each repeated them), since the client uses them to pick/create the card.
func TestWSToolCallDeltaFieldsCarryIdentity(t *testing.T) {
	stub := &argsStreamStub{
		toolName: "patch_file",
		toolID:   "call_identity",
		rawArgs:  `{"diff":"` + strings.Repeat("y", 2048) + `"}`,
		chunkLen: 128,
	}
	s, a := newArgsStreamServer(t, stub)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "patch it", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}

	deltas := collectUntil(t, conn, 20*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == a.SessionID
	})
	if len(deltas) == 0 {
		t.Fatal("no tool_call_delta frames received")
	}
	for i, d := range deltas {
		if d.Tool != "patch_file" {
			t.Fatalf("frame[%d].Tool = %q, want %q", i, d.Tool, "patch_file")
		}
		if d.ToolCallID != "call_identity" {
			t.Fatalf("frame[%d].ToolCallID = %q, want %q", i, d.ToolCallID, "call_identity")
		}
	}
}
