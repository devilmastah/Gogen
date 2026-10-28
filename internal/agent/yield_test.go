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

func alwaysReady() bool { return true }

// TestRequestYieldStopsBeforeTheNextRound pins the natural stop: the tool
// the current round already asked for runs, and the following model call
// does not start.
func TestRequestYieldStopsBeforeTheNextRound(t *testing.T) {
	provider := newYieldProvider()
	a := NewAgent(provider, NewExecutor(t.TempDir()), nil)
	h := &llm.StreamHandlers{
		OnToolExecute: func(string) { a.RequestYield(alwaysReady) },
	}
	out, err := a.StreamProcessInput(context.Background(), "run something", h)
	if err != nil {
		t.Fatal(err)
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
}

// TestRequestYieldNotReady pins the host check: with nothing waiting to run,
// the request is not recorded and the turn runs to its answer.
func TestRequestYieldNotReady(t *testing.T) {
	provider := newYieldProvider()
	a := NewAgent(provider, NewExecutor(t.TempDir()), nil)
	var recorded bool
	h := &llm.StreamHandlers{
		OnToolExecute: func(string) {
			recorded = recorded || a.RequestYield(func() bool { return false })
		},
	}
	out, err := a.StreamProcessInput(context.Background(), "run something", h)
	if err != nil {
		t.Fatal(err)
	}
	if recorded {
		t.Fatal("RequestYield recorded a request whose check failed")
	}
	if out != "done" || provider.CallCount != 3 {
		t.Fatalf("out = %q after %d calls, want the full turn", out, provider.CallCount)
	}
}

// TestRequestYieldDoesNotCutTheNextTurn pins the hand-off: a yield recorded
// for a turn that has already finished does not stop the turn after it.
func TestRequestYieldDoesNotCutTheNextTurn(t *testing.T) {
	provider := newYieldProvider()
	a := NewAgent(provider, NewExecutor(t.TempDir()), nil)
	if _, err := a.StreamProcessInput(context.Background(), "first", &llm.StreamHandlers{
		OnToolExecute: func(string) { a.RequestYield(alwaysReady) },
	}); err != nil {
		t.Fatal(err)
	}
	// The first turn has ended. This request names that finished turn.
	a.RequestYield(alwaysReady)
	out, err := a.StreamProcessInput(context.Background(), "second", nil)
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
