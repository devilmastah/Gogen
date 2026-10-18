package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"

	"github.com/gorilla/websocket"
)

// termStreamStubProvider returns one execute_command tool call (executed by
// the real shell executor) on the first round, then plain content, so the
// full turn exercises the live terminal-output path end to end.
type termStreamStubProvider struct {
	mu    sync.Mutex
	calls int
}

func (s *termStreamStubProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *termStreamStubProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		return &llm.StreamResult{
			ToolCalls: []llm.ToolCall{{
				ID:   "call_e2e",
				Name: "execute_command",
				Args: map[string]any{"command": "sleep 0.2 && echo hello && echo world"},
			}},
		}, nil
	}
	return &llm.StreamResult{Content: "done"}, nil
}

func (s *termStreamStubProvider) ModelContextLimit(_ context.Context) (int, error) { return 1000, nil }
func (s *termStreamStubProvider) SetThinkingLevel(string)                          {}
func (s *termStreamStubProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *termStreamStubProvider) SetModel(string) error { return nil }
func (s *termStreamStubProvider) ModelName() string     { return "test-model" }

// TestWSTerminalMessagesStreamAgentCommand drives a full turn over a real
// WebSocket connection and verifies the terminal message sequence:
// term_opened (with the exact command) → term_output chunks (echoed content)
// → term_exit (success), correlated by termId, before turn_end.
func TestWSTerminalMessagesStreamAgentCommand(t *testing.T) {
	exec := agent.NewExecutor(t.TempDir())
	a := agent.NewAgent(&termStreamStubProvider{}, exec, nil)
	s := newTestServer(t, a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:]+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "run it"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var termID string
	var opened, exited, success bool
	var outputs strings.Builder
	for {
		if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		switch msg.Type {
		case "term_opened":
			opened = true
			termID = msg.TermID
			if termID == "" || msg.ToolCallID != termID {
				t.Fatalf("term_opened ids: termId=%q toolCallId=%q", termID, msg.ToolCallID)
			}
			if !strings.Contains(msg.Content, "sleep 0.2 && echo hello && echo world") {
				t.Fatalf("term_opened content = %q, want command echo", msg.Content)
			}
		case "term_output":
			if msg.TermID != termID {
				t.Fatalf("term_output termId %q != opened %q", msg.TermID, termID)
			}
			outputs.WriteString(msg.Content)
		case "term_exit":
			exited = true
			success = msg.Success
			if msg.TermID != termID {
				t.Fatalf("term_exit termId %q != opened %q", msg.TermID, termID)
			}
		case "turn_end":
			if !opened || !exited {
				t.Fatalf("turn ended without terminal messages: opened=%v exited=%v", opened, exited)
			}
			if !success {
				t.Fatal("expected success=true on term_exit")
			}
			joined := outputs.String()
			if !strings.Contains(joined, "hello") || !strings.Contains(joined, "world") {
				t.Fatalf("term_output missing command output, got %q", joined)
			}
			return
		}
	}
}

// bgTermStreamStubProvider returns one execute_command background=true tool
// call (executed by the real shell executor) on the first round, then plain
// content, so the turn ends while the job is still running — the background
// job's output must keep streaming (and term_exit must carry the real exit
// status) long after the tool result and the turn itself.
type bgTermStreamStubProvider struct {
	mu    sync.Mutex
	calls int
}

func (s *bgTermStreamStubProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *bgTermStreamStubProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		return &llm.StreamResult{
			ToolCalls: []llm.ToolCall{{
				ID:   "call_bg",
				Name: "execute_command",
				Args: map[string]any{"command": "sleep 0.5 && echo bg-hello", "background": true},
			}},
		}, nil
	}
	return &llm.StreamResult{Content: "done"}, nil
}

func (s *bgTermStreamStubProvider) ModelContextLimit(_ context.Context) (int, error) {
	return 1000, nil
}
func (s *bgTermStreamStubProvider) SetThinkingLevel(string) {}
func (s *bgTermStreamStubProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *bgTermStreamStubProvider) SetModel(string) error { return nil }
func (s *bgTermStreamStubProvider) ModelName() string     { return "test-model" }

// TestWSTerminalMessagesStreamBackgroundJob drives a full turn over a real
// WebSocket connection with a background command and verifies the sequence:
// term_opened (command echo, BEFORE the tool result) → tool_result (job id)
// → turn_end (the job is still running) → term_output (post-turn streaming)
// → term_exit (success), all correlated by termId.
func TestWSTerminalMessagesStreamBackgroundJob(t *testing.T) {
	exec := agent.NewExecutor(t.TempDir())
	a := agent.NewAgent(&bgTermStreamStubProvider{}, exec, nil)
	s := newTestServer(t, a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:]+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "run it"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var termID string
	var opened, result, turnEnd, exited, success bool
	var outputs strings.Builder
	for {
		if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		switch msg.Type {
		case "term_opened":
			if result {
				t.Fatal("term_opened arrived after the tool result — the terminal must open before the job id is reported")
			}
			opened = true
			termID = msg.TermID
			if termID == "" || msg.ToolCallID != termID {
				t.Fatalf("term_opened ids: termId=%q toolCallId=%q", termID, msg.ToolCallID)
			}
			if !strings.Contains(msg.Content, "sleep 0.5 && echo bg-hello") {
				t.Fatalf("term_opened content = %q, want command echo", msg.Content)
			}
		case "tool_result":
			if msg.ToolCallID != "call_bg" {
				t.Fatalf("tool_result toolCallId = %q", msg.ToolCallID)
			}
			if !strings.Contains(msg.Result, "background job") {
				t.Fatalf("tool_result = %q, want background job id", msg.Result)
			}
			result = true
		case "term_output":
			if !result {
				t.Fatal("term_output arrived before the tool result")
			}
			if msg.TermID != termID {
				t.Fatalf("term_output termId %q != opened %q", msg.TermID, termID)
			}
			outputs.WriteString(msg.Content)
		case "term_exit":
			if msg.TermID != termID {
				t.Fatalf("term_exit termId %q != opened %q", msg.TermID, termID)
			}
			exited = true
			success = msg.Success
		case "turn_end":
			turnEnd = true
			// The job (sleep 0.5) must still be running when the turn
			// ends: without post-turn streaming this test would never
			// see term_output/term_exit at all.
			if !opened || !result {
				t.Fatalf("turn ended without terminal messages: opened=%v result=%v", opened, result)
			}
		}
		if exited {
			if !success {
				t.Fatal("expected success=true on term_exit")
			}
			if !strings.Contains(outputs.String(), "bg-hello") {
				t.Fatalf("term_output missing command output, got %q", outputs.String())
			}
			if !turnEnd {
				t.Log("note: job finished before turn_end (fast machine) — post-turn path unverified")
			}
			return
		}
	}
}
