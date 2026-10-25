package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/llm"
)

func seedToolLoopOverBudget(t *testing.T, a *Agent) {
	t.Helper()
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	for i := 0; i < 3; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 2000)})
	}
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: "call_1", Name: "read_file", ArgsStr: `{"path":"a.go"}`,
	}}})
	a.appendMessage(llm.Message{Role: "tool", ToolCallID: "call_1", Content: "file contents"})
	_ = a.ContextStats(context.Background())
	a.recordTurnUsage(&llm.Usage{PromptTokens: 15000, CompletionTokens: 10})
}

func enableSmartCompact(a *Agent) {
	a.SetSmartCompact(true)
}

func TestSmartCompactOffDoesNotCompactToolLoop(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	before := len(a.Messages)
	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 {
		t.Fatalf("smart compact off compacted a tool loop (%d summarizer calls)", provider.calls)
	}
	if len(a.Messages) != before {
		t.Fatalf("messages = %d, want %d", len(a.Messages), before)
	}
}

func TestSmartCompactOnCompactsToolLoopBeforeNextThink(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	enableSmartCompact(a)
	before := len(a.Messages)
	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if provider.calls == 0 {
		t.Fatal("smart compact on did not summarize a tool loop already over budget")
	}
	if len(a.Messages) >= before {
		t.Fatalf("messages = %d, want fewer than %d", len(a.Messages), before)
	}
}

func TestSmartCompactOnUnderBudgetDoesNotCompact(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	a.recordTurnUsage(&llm.Usage{PromptTokens: 1000, CompletionTokens: 10})
	enableSmartCompact(a)
	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 {
		t.Fatalf("under budget compacted (%d calls)", provider.calls)
	}
}

func TestSmartCompactBeforeToolsRunsCallAfterSummary(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	enableSmartCompact(a)
	res := &llm.StreamResult{
		Reasoning: strings.Repeat("z", 4000),
		ToolCalls: []llm.ToolCall{{
			ID: "call_2", Name: "read_file", ArgsStr: `{"path":"b.go"}`,
		}},
	}
	action := a.smartCompactBeforeTools(context.Background(), nil, res)
	if action != smartCompactExecute {
		t.Fatalf("action = %d, want execute", action)
	}
	if provider.calls == 0 {
		t.Fatal("did not compact before the tool call")
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool call changed: %+v", res.ToolCalls)
	}
	if len([]rune(res.Reasoning)) > reasoningExcerptRunes+3 {
		t.Fatalf("reasoning kept %d runes", len([]rune(res.Reasoning)))
	}
	found := false
	for _, m := range a.Messages {
		if strings.Contains(m.Content, "[Paused before execution]") && strings.Contains(m.Content, "read_file") {
			found = true
		}
	}
	if !found {
		t.Fatal("summary is missing the paused tool call")
	}
}

func TestSmartCompactOffDoesNotDelayToolCall(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	res := &llm.StreamResult{
		Reasoning: strings.Repeat("z", 4000),
		ToolCalls: []llm.ToolCall{{
			ID: "call_2", Name: "read_file", ArgsStr: `{"path":"b.go"}`,
		}},
	}
	action := a.smartCompactBeforeTools(context.Background(), nil, res)
	if action != smartCompactNone {
		t.Fatalf("action = %d, want none", action)
	}
	if provider.calls != 0 {
		t.Fatalf("off path compacted (%d calls)", provider.calls)
	}
	if len(res.Reasoning) != 4000 {
		t.Fatalf("reasoning changed (%d)", len(res.Reasoning))
	}
}

func TestSmartCompactDropsIncompleteCut(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	enableSmartCompact(a)
	res := &llm.StreamResult{
		BudgetStop: true,
		Reasoning:  "still thinking about the next edit",
		ToolCalls: []llm.ToolCall{{
			Name: "read_file", ArgsStr: `{"path":`, ArgsError: "unexpected end of JSON",
		}},
	}
	action := a.smartCompactBeforeTools(context.Background(), nil, res)
	if action != smartCompactContinue {
		t.Fatalf("action = %d, want continue", action)
	}
	if provider.calls == 0 {
		t.Fatal("did not compact a cut thinking round")
	}
	if len(res.ToolCalls) != 0 {
		t.Fatalf("incomplete tool call kept: %+v", res.ToolCalls)
	}
}

func TestSmartCompactHaltWhenCompactCannotShrink(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	enableSmartCompact(a)
	a.noteCompactFailure(context.Canceled)
	res := &llm.StreamResult{
		BudgetStop: true,
		ToolCalls: []llm.ToolCall{{
			Name: "read_file", ArgsStr: `{"path":`, ArgsError: "unexpected end of JSON",
		}},
	}
	action := a.smartCompactBeforeTools(context.Background(), nil, res)
	if action != smartCompactHalt {
		t.Fatalf("action = %d, want halt", action)
	}
	if provider.calls != 0 {
		t.Fatalf("backed-off compact ran again (%d calls)", provider.calls)
	}
	if res.Content != smartCompactStuckMessage {
		t.Fatalf("content = %q", res.Content)
	}
}
