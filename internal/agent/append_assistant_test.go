package agent

import (
	"testing"

	"gogen/internal/llm"
)

// TestAppendAssistantCarriesEveryField pins the contract of the turn loop's
// single append site: the smart-compact exits used to build their own
// message and dropped Refusal along with the ToolCalls/ArgsStabilized
// wiring, so a refusing round that also tripped the compact budget persisted
// an empty assistant message.
func TestAppendAssistantCarriesEveryField(t *testing.T) {
	t.Run("refusal survives with empty content", func(t *testing.T) {
		a := &Agent{}
		res := &llm.StreamResult{Refusal: "I can't help with that", Model: "m1"}
		a.appendAssistant(nil, res, "", nil)

		if len(a.Messages) != 1 {
			t.Fatalf("appended %d messages, want 1", len(a.Messages))
		}
		got := a.Messages[0]
		if got.Role != "assistant" {
			t.Fatalf("role = %q, want assistant", got.Role)
		}
		if got.Refusal != res.Refusal {
			t.Fatalf("refusal = %q, want %q", got.Refusal, res.Refusal)
		}
		if got.Content != "" {
			t.Fatalf("content = %q, want empty (the refusal carries the text)", got.Content)
		}
		if got.Model != "m1" {
			t.Fatalf("model = %q, want m1", got.Model)
		}
		if got.ArgsStabilized {
			t.Fatal("ArgsStabilized set on a content-only round: the next stabilizeToolArgs is expected to visit it")
		}
	})

	t.Run("content override wins", func(t *testing.T) {
		a := &Agent{}
		res := &llm.StreamResult{Content: "model content"}
		a.appendAssistant(nil, res, "halt message", nil)

		if got := a.Messages[0].Content; got != "halt message" {
			t.Fatalf("content = %q, want the override", got)
		}
	})

	t.Run("tool round is stabilized", func(t *testing.T) {
		a := &Agent{}
		calls := []llm.ToolCall{{ID: "1", Name: "read_file"}}
		a.appendAssistant(nil, &llm.StreamResult{Content: "thinking", ToolCalls: calls}, "", calls)

		got := a.Messages[0]
		if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "1" {
			t.Fatalf("tool calls = %#v, want the round's calls", got.ToolCalls)
		}
		if !got.ArgsStabilized {
			t.Fatal("ArgsStabilized not set on the tool-call round, so every later round re-validates its args")
		}
	})
}
