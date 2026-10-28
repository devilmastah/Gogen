package agent

import (
	"context"
	"testing"

	"gogen/internal/llm"
	"gogen/internal/llm/llmtest"
)

// newYieldProvider returns two tool rounds, then a plain answer, so a turn
// has a second model round to stop before and a later turn still has a tool
// round of its own.
func newYieldProvider() *llmtest.MockProvider {
	toolRound := &llm.StreamResult{ToolCalls: []llm.ToolCall{{
		ID:   "call_yield",
		Name: "execute_command",
		Args: map[string]any{"command": "echo hello"},
	}}}
	p := llmtest.NewMockProvider()
	p.StreamResults = []*llm.StreamResult{toolRound, toolRound, {Content: "done"}}
	return p
}

// yieldNow asks the running turn to stop, the way a host does.
func yieldNow(a *Agent) bool {
	return a.RequestYieldFor(a.TurnGeneration())
}

// TestRequestYieldStopsBeforeTheNextRound pins the natural stop: the tool
// the current round already asked for runs, and the following model call
// does not start.
func TestRequestYieldStopsBeforeTheNextRound(t *testing.T) {
	provider := newYieldProvider()
	a := NewAgent(provider, NewExecutor(t.TempDir()), nil)
	var recorded bool
	h := &llm.StreamHandlers{
		OnToolExecute: func(string) { recorded = yieldNow(a) },
	}
	out, err := a.StreamProcessInput(context.Background(), "run something", h)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("RequestYieldFor refused the running turn")
	}
	if out != "" {
		t.Fatalf("yielded turn returned %q, want an empty finish", out)
	}
	if provider.CallCount != 1 {
		t.Fatalf("model calls = %d, want 1 (the next round must not start)", provider.CallCount)
	}
	var toolResults int
	for _, m := range a.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	if toolResults != 1 {
		t.Fatalf("tool results = %d, want the in-flight call's result kept", toolResults)
	}
	if a.TurnGeneration() != 0 {
		t.Fatalf("TurnGeneration after the turn = %d, want 0", a.TurnGeneration())
	}
}

// TestRequestYieldForNoTurn pins the gate the hosts rely on for a
// compaction or an idle session: with no model turn running there is no
// generation to name, and nothing is recorded.
func TestRequestYieldForNoTurn(t *testing.T) {
	a := NewAgent(newYieldProvider(), NewExecutor(t.TempDir()), nil)
	if a.TurnGeneration() != 0 {
		t.Fatalf("TurnGeneration before any turn = %d, want 0", a.TurnGeneration())
	}
	if a.RequestYieldFor(0) {
		t.Fatal("RequestYieldFor(0) recorded a request with no turn running")
	}
}

// TestRequestYieldDoesNotCutTheNextTurn pins the hand-off: a request that
// names a turn that has already finished is refused and does not stop the
// turn after it.
func TestRequestYieldDoesNotCutTheNextTurn(t *testing.T) {
	provider := newYieldProvider()
	a := NewAgent(provider, NewExecutor(t.TempDir()), nil)
	var first uint64
	if _, err := a.StreamProcessInput(context.Background(), "first", &llm.StreamHandlers{
		OnToolExecute: func(string) {
			first = a.TurnGeneration()
			a.RequestYieldFor(first)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if a.RequestYieldFor(first) {
		t.Fatal("RequestYieldFor accepted a turn that has ended")
	}
	// The second turn names the first turn's generation mid-round, as a
	// host would if that turn ended between its read and its request.
	out, err := a.StreamProcessInput(context.Background(), "second", &llm.StreamHandlers{
		OnToolExecute: func(string) {
			if a.RequestYieldFor(first) {
				t.Error("RequestYieldFor accepted the previous turn's generation")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "done" {
		t.Fatalf("second turn returned %q, want the model's answer", out)
	}
	if provider.CallCount != 3 {
		t.Fatalf("model calls = %d, want 3 (one yielded round, then a full tool round and its answer)", provider.CallCount)
	}
}
