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

// TestEmergencyDeepDiveFallsBack pins the emergency tier in deep dive when
// the request itself is the bulk: the layout around it frees nothing, so the
// single summary runs instead of a refusal that arms the backoff.
func TestEmergencyDeepDiveFallsBack(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	big := strings.Repeat("x", 30000)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
	a.appendMessage(llm.Message{Role: "user", Content: big})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok2"})
	_ = a.ContextStats(context.Background())
	a.recordTurnUsage(&llm.Usage{PromptTokens: ctxMgr.HardLimit() + 100, CompletionTokens: 10})

	if !a.compactIfDue(context.Background(), nil) {
		t.Fatalf("emergency compaction did not shrink (%d summarizer calls)", provider.calls)
	}
	for _, m := range a.Messages {
		if m.Content == big {
			t.Fatal("the oversized request is still verbatim")
		}
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("emergency recorded a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestDeepDiveSmallProgressRegionNoBackoff pins that a normal compaction in
// deep dive whose regions around the request leave nothing to summarize at
// the first keep is not a failure: it moves on to a smaller keep, which
// puts the recent work in the progress region, and records no backoff.
func TestDeepDiveSmallProgressRegionNoBackoff(t *testing.T) {
	// The history really weighs over the budget (no provider baseline), so
	// the re-measure after an unchanged pass still sees it over.
	seed := func(last string) (*Agent, *contextmgr.Manager, *countingProvider) {
		a, ctxMgr, provider := newEmergencyTestAgent(t, false)
		a.SetDeepDive(true)
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
		a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
		a.appendMessage(llm.Message{Role: "user", Content: "the request"})
		a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"})
		a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("step ", 800)})
		a.appendMessage(llm.Message{Role: "assistant", Content: last})
		_ = a.ContextStats(context.Background())
		return a, ctxMgr, provider
	}
	probe, ctxMgr, _ := seed("ok")
	budget, hard := ctxMgr.CompactBudget(), ctxMgr.HardLimit()
	a, _, provider := seed(strings.Repeat(" pad", budget+300-probe.compactionTokenTotal()))
	if total := a.compactionTokenTotal(); total < budget || total >= hard {
		t.Fatalf("test setup: total %d not in [budget %d, hard limit %d)", total, budget, hard)
	}

	if !a.compactIfDue(context.Background(), nil) {
		t.Fatalf("compaction did not shrink (%d summarizer calls)", provider.calls)
	}
	if got := len(a.Messages); got != 5 || a.Messages[2].Content != "the request" ||
		!strings.HasPrefix(a.Messages[3].Content, contextmgr.ProgressSummaryPrefix) || !strings.HasPrefix(a.Messages[4].Content, " pad") {
		t.Fatalf("layout after compaction: %d messages", got)
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("recorded a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestSmartCompactDeepDiveKeepsPausedNote pins that the paused-call note is
// not lost in deep dive when no progress summary can take it: the tool-call
// message carries it, and the session summary before the request does not.
func TestSmartCompactDeepDiveKeepsPausedNote(t *testing.T) {
	a, _, _ := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	enableSmartCompact(a)
	a.SetDeepDive(true)
	res := &llm.StreamResult{ToolCalls: []llm.ToolCall{{
		ID: "call_2", Name: "read_file", ArgsStr: `{"path":"b.go"}`,
	}}}
	if action := a.smartCompactBeforeTools(context.Background(), nil, res); action != smartCompactExecute {
		t.Fatalf("action = %d, want execute", action)
	}
	for _, m := range a.Messages {
		if strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) && strings.Contains(m.Content, "[Paused before execution]") {
			t.Fatal("the note landed in the session summary")
		}
	}
	if !strings.Contains(res.Content, "[Paused before execution]") || !strings.Contains(res.Content, "b.go") {
		t.Fatalf("tool-call message does not carry the note: %q", res.Content)
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
		usage    int // 0 keeps the seeded baseline
		want     smartCompactAction
		wantTool bool
		wantMsg  string
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
			want:    smartCompactHalt,
			wantMsg: smartCompactAutoOffMessage,
		},
		{
			// The emergency tier used to summarize at the window reserve
			// even with compaction switched off. compact_threshold 0 closes
			// it too, and the note says so instead of blaming a compaction
			// that never ran.
			name: "over the window reserve still does not summarize",
			res: &llm.StreamResult{
				BudgetStop: true,
				ToolCalls: []llm.ToolCall{{
					Name: "read_file", ArgsStr: `{"path":`, ArgsError: "unexpected end of JSON",
				}},
			},
			usage:   19000, // past HardLimit (20000 - 4000)
			want:    smartCompactHalt,
			wantMsg: smartCompactAutoOffMessage,
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
			if tt.usage > 0 {
				a.recordTurnUsage(&llm.Usage{PromptTokens: tt.usage, CompletionTokens: 10})
			}
			if a.Context.AutoCompactEnabled() {
				t.Fatal("test needs auto-compaction off")
			}
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
			if tt.wantMsg != "" && tt.res.Content != tt.wantMsg {
				t.Fatalf("halt note = %q, want %q", tt.res.Content, tt.wantMsg)
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

// TestCompactIfDueThresholdZeroClosesBothTiers pins the setting's contract
// ("never compact automatically", README) at the shared gate: with
// compact_threshold 0 the budget tier AND the window-reserve tier stay
// closed, for a user-message boundary and for a smart-compact tool loop. The
// request-level rescue is a different path (preflightForcedCompact) and is
// not asserted here.
func TestCompactIfDueThresholdZeroClosesBothTiers(t *testing.T) {
	tests := []struct {
		name  string
		usage int
	}{
		{"over the budget", 15000},
		{"over the window reserve", 19000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, provider := newEmergencyTestAgent(t, false)
			seedToolLoopOverBudget(t, a)
			a.recordTurnUsage(&llm.Usage{PromptTokens: tt.usage, CompletionTokens: 10})
			a.Context.UpdateSettingsFunc(func(s contextmgr.Settings) contextmgr.Settings {
				s.CompactThreshold = 0
				s.SmartCompact = true
				return s
			})
			if a.emergencyCompactDue(a.compactionTokenTotal()) && tt.name == "over the budget" {
				t.Fatal("test needs the window reserve to stay clear")
			}
			before := len(a.Messages)
			if a.compactIfDue(context.Background(), nil) {
				t.Fatal("compactIfDue reported a shrink with automatic compaction off")
			}
			if provider.calls != 0 {
				t.Fatalf("summarizer calls = %d, want 0", provider.calls)
			}
			if len(a.Messages) != before {
				t.Fatalf("messages = %d, want %d", len(a.Messages), before)
			}
			backoff, _ := compactState(a)
			if !backoff.IsZero() {
				t.Fatal("a closed gate armed the shared compact backoff")
			}
		})
	}
}

// TestSmartCompactOffNoteWhenBackingOff keeps the halt note honest on the
// give-up path: compaction is off, so a cut that cannot run a complete tool
// call must say so rather than blame a compaction attempt.
func TestSmartCompactOffNoteWhenBackingOff(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedToolLoopOverBudget(t, a)
	a.Context.UpdateSettingsFunc(func(s contextmgr.Settings) contextmgr.Settings {
		s.CompactThreshold = 0
		s.SmartCompact = true
		return s
	})
	// A backoff left over from before the switch was flipped must take the
	// give-up path, not the gate.
	a.noteCompactFailure(context.Canceled)
	res := &llm.StreamResult{
		BudgetStop: true,
		ToolCalls: []llm.ToolCall{{
			Name: "read_file", ArgsStr: `{"path":`, ArgsError: "unexpected end of JSON",
		}},
	}
	if action := a.smartCompactBeforeTools(context.Background(), nil, res); action != smartCompactHalt {
		t.Fatalf("action = %d, want halt", action)
	}
	if provider.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", provider.calls)
	}
	if res.Content != smartCompactAutoOffMessage {
		t.Fatalf("halt note = %q, want the auto-compaction-off note", res.Content)
	}
}
