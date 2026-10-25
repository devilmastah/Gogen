package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
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

func TestSmartCompactThresholdZeroSkipsSummary(t *testing.T) {
	tests := []struct {
		name     string
		res      *llm.StreamResult
		want     smartCompactAction
		wantTool bool
	}{
		{
			name: "finished call still runs",
			res: &llm.StreamResult{
				BudgetStop: true,
				ToolCalls: []llm.ToolCall{{
					ID: "call_2", Name: "read_file", ArgsStr: `{"path":"b.go"}`,
				}},
			},
			want:     smartCompactExecute,
			wantTool: true,
		},
		{
			name: "unfinished call is dropped",
			res: &llm.StreamResult{
				BudgetStop: true,
				Reasoning:  "still thinking",
				ToolCalls: []llm.ToolCall{{
					Name: "read_file", ArgsStr: `{"path":`, ArgsError: "unexpected end of JSON",
				}},
			},
			want: smartCompactHalt,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, provider := newEmergencyTestAgent(t, false)
			seedToolLoopOverBudget(t, a)
			a.Context.UpdateSettingsFunc(func(s contextmgr.Settings) contextmgr.Settings {
				s.CompactThreshold = 0
				s.SmartCompact = true
				return s
			})
			before, _ := compactState(a)
			action := a.smartCompactBeforeTools(context.Background(), nil, tt.res)
			if action != tt.want {
				t.Fatalf("action = %d, want %d", action, tt.want)
			}
			if provider.calls != 0 {
				t.Fatalf("summarizer calls = %d, want 0", provider.calls)
			}
			after, _ := compactState(a)
			if !after.Equal(before) {
				t.Fatal("closed gate armed the shared compact backoff")
			}
			if tt.wantTool && (len(tt.res.ToolCalls) != 1 || tt.res.ToolCalls[0].Name != "read_file") {
				t.Fatalf("tool call changed: %+v", tt.res.ToolCalls)
			}
			if !tt.wantTool && len(tt.res.ToolCalls) != 0 {
				t.Fatalf("unfinished tool call kept: %+v", tt.res.ToolCalls)
			}
		})
	}
}

// scriptedStreamProvider serves stream results in order. GenerateResponse
// stays the counting summarizer, so a test can see whether a turn called it.
type scriptedStreamProvider struct {
	countingProvider
	streams []*llm.StreamResult
	at      int
}

func (p *scriptedStreamProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	if p.at >= len(p.streams) {
		return &llm.StreamResult{Content: "done"}, nil
	}
	r := p.streams[p.at]
	p.at++
	return r, nil
}

func newScriptedSmartCompactAgent(t *testing.T, streams []*llm.StreamResult) (*Agent, *scriptedStreamProvider) {
	t.Helper()
	p := &scriptedStreamProvider{countingProvider: countingProvider{limit: 20000}, streams: streams}
	ctxMgr := contextmgr.NewManager(p, contextmgr.Settings{
		ContextLimit:              20000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	a := NewAgent(p, &Executor{WorkingDir: t.TempDir()}, ctxMgr)
	return a, p
}

func TestSmartCompactTurnClosesTheStream(t *testing.T) {
	incomplete := func(usage int) *llm.StreamResult {
		res := &llm.StreamResult{
			BudgetStop: true,
			Reasoning:  "still thinking about the next edit",
			ToolCalls: []llm.ToolCall{{
				Name: "read_file", ArgsStr: `{"path":`, ArgsError: "unexpected end of JSON",
			}},
		}
		if usage > 0 {
			res.Usage = &llm.Usage{PromptTokens: usage, CompletionTokens: 10}
		}
		return res
	}

	t.Run("continue", func(t *testing.T) {
		a, p := newScriptedSmartCompactAgent(t, []*llm.StreamResult{
			{},
			incomplete(15000),
			{Content: "done"},
		})
		seedToolLoopOverBudget(t, a)
		a.recordTurnUsage(&llm.Usage{PromptTokens: 1000, CompletionTokens: 10})
		enableSmartCompact(a)
		var ended int
		out, err := a.StreamProcessInput(context.Background(), "go on", &llm.StreamHandlers{
			OnStreamEnd: func() { ended++ },
		})
		if err != nil {
			t.Fatal(err)
		}
		if out != "done" {
			t.Fatalf("out = %q", out)
		}
		if ended != 3 {
			t.Fatalf("stream end calls = %d, want 3", ended)
		}
		if a.ghostRetries.Load() != 0 {
			t.Fatalf("ghost retries = %d, want 0", a.ghostRetries.Load())
		}
		if p.calls == 0 {
			t.Fatal("continue path did not summarize")
		}
		blob := messageBlob(a.Messages)
		if !strings.Contains(blob, "cut off") || !strings.Contains(blob, "done") {
			t.Fatalf("history missing the pause note or the final answer: %s", blob)
		}
	})

	t.Run("halt", func(t *testing.T) {
		a, p := newScriptedSmartCompactAgent(t, []*llm.StreamResult{
			{},
			incomplete(0),
		})
		seedToolLoopOverBudget(t, a)
		enableSmartCompact(a)
		a.noteCompactFailure(context.Canceled)
		var ended int
		out, err := a.StreamProcessInput(context.Background(), "go on", &llm.StreamHandlers{
			OnStreamEnd: func() { ended++ },
		})
		if err != nil {
			t.Fatal(err)
		}
		if out != smartCompactStuckMessage {
			t.Fatalf("out = %q", out)
		}
		if ended != 2 {
			t.Fatalf("stream end calls = %d, want 2", ended)
		}
		if a.ghostRetries.Load() != 0 {
			t.Fatalf("ghost retries = %d, want 0", a.ghostRetries.Load())
		}
		if p.calls != 0 {
			t.Fatalf("backed-off turn summarized (%d calls)", p.calls)
		}
		if !strings.Contains(messageBlob(a.Messages), smartCompactStuckMessage) {
			t.Fatal("halt did not append the assistant message")
		}
	})

	t.Run("execute", func(t *testing.T) {
		a, p := newScriptedSmartCompactAgent(t, []*llm.StreamResult{
			{
				BudgetStop: true,
				ToolCalls: []llm.ToolCall{{
					ID: "call_2", Name: "read_file", ArgsStr: `{"path":"b.go"}`,
				}},
			},
			{Content: "done"},
		})
		seedToolLoopOverBudget(t, a)
		a.Context.UpdateSettingsFunc(func(s contextmgr.Settings) contextmgr.Settings {
			s.CompactThreshold = 0
			s.SmartCompact = true
			return s
		})
		var ended int
		out, err := a.StreamProcessInput(context.Background(), "go on", &llm.StreamHandlers{
			OnStreamEnd: func() { ended++ },
		})
		if err != nil {
			t.Fatal(err)
		}
		if out != "done" {
			t.Fatalf("out = %q", out)
		}
		if ended < 1 {
			t.Fatal("execute path did not close the stream")
		}
		if p.calls != 0 {
			t.Fatalf("threshold 0 summarized (%d calls)", p.calls)
		}
		blob := messageBlob(a.Messages)
		if !strings.Contains(blob, "read_file") || !strings.Contains(blob, "done") {
			t.Fatalf("finished tool call did not run: %s", blob)
		}
	})
}

func messageBlob(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		for _, tc := range m.ToolCalls {
			b.WriteString(" ")
			b.WriteString(tc.Name)
		}
		b.WriteString("\n")
	}
	return b.String()
}
