package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// TestTurnYieldedReportsTheYield pins the flag a subagent runner needs so it
// cannot mistake a steered turn for a completed job: a turn stopped by
// RequestYieldFor reports TurnYielded, a turn that ran to its own end does not,
// and the next turn resets the flag.
func TestTurnYieldedReportsTheYield(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := llmtest.NewMockProvider()
	var calls atomic.Int32
	var a *Agent
	stub.OnStream = func(ctx context.Context, _ []llm.Message, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
		if calls.Add(1) == 1 {
			// Round 1: a tool call. The host asks for the yield from inside the
			// round (a real host asks from its delivery goroutine); the turn
			// must finish this round and its tools, then stop.
			a.RequestYieldFor(a.TurnGeneration())
			return &llm.StreamResult{
				ToolCalls: []llm.ToolCall{{ID: "t1", Name: "read_file", Args: map[string]any{"path": "f.txt"}}},
			}, nil
		}
		return &llm.StreamResult{Content: "second round"}, nil
	}
	a = NewAgent(stub, NewExecutor(dir), nil)

	if _, err := a.StreamProcessInput(context.Background(), "do it", nil); err != nil {
		t.Fatalf("yielded turn = %v, want no error (a yield is not a cancel)", err)
	}
	if !a.TurnYielded() {
		t.Fatal("a turn stopped at a round boundary must report TurnYielded")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("model calls = %d, want 1 (the round after the yield must not start)", got)
	}
	// The round's work is kept: the tool result stays in the transcript.
	sawTool := false
	for _, m := range a.SnapshotMessages() {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatal("the yielded turn must keep the round's tool result")
	}

	// The next turn resets the flag and runs to its own end.
	if _, err := a.StreamProcessInput(context.Background(), "again", nil); err != nil {
		t.Fatal(err)
	}
	if a.TurnYielded() {
		t.Fatal("a turn that finished on its own must not report TurnYielded")
	}
}
