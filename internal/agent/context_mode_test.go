package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/llm/llmtest"
)

// TestSystemPromptContextAdvice pins which context advice each session gets:
// the deep-dive text only in deep_dive while auto-compaction is on, the
// default text otherwise.
func TestSystemPromptContextAdvice(t *testing.T) {
	ConfigureSystemPrompt("")
	t.Cleanup(func() { ConfigureSystemPrompt("") })

	if n := strings.Count(DefaultSystemPromptTemplate(), contextAdviceDefault); n != 1 {
		t.Fatalf("built-in template holds the default advice %d times, want 1", n)
	}
	tests := []struct {
		name      string
		deepDive  bool
		threshold float64
		want      string
	}{
		{"default mode", false, 0.85, contextAdviceDefault},
		{"deep dive with auto-compaction", true, 0.85, contextAdviceDeepDive},
		{"deep dive with auto-compaction off falls back", true, 0, contextAdviceDefault},
		{"default mode with auto-compaction off", false, 0, contextAdviceDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{Context: contextmgr.NewManager(nil, contextmgr.Settings{
				ContextLimit: 100000, CompactThreshold: tt.threshold, DeepDive: tt.deepDive,
			})}
			got := a.systemPromptBase("/w")
			if !strings.Contains(got, tt.want) {
				t.Fatalf("prompt does not carry the expected advice:\n%s", got)
			}
			other := contextAdviceDeepDive
			if tt.want == contextAdviceDeepDive {
				other = contextAdviceDefault
			}
			if strings.Contains(got, other) {
				t.Fatal("prompt carries both advice texts")
			}
		})
	}
}

// TestSessionAgentContextMode pins that context_mode reaches a session built
// by the factory: its manager reports DeepDive and its prompt carries the
// deep-dive advice.
func TestSessionAgentContextMode(t *testing.T) {
	ConfigureSystemPrompt("")
	t.Cleanup(func() { ConfigureSystemPrompt("") })
	tests := []struct {
		name string
		mode string
		want bool
	}{
		{"deep dive", config.ContextModeDeepDive, true},
		{"default", config.DefaultContextMode, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.ContextLimit = 128000
			cfg.ContextMode = tt.mode
			exec := NewExecutor(t.TempDir())
			a := NewSessionAgent(SessionAgentOptions{
				Provider:   llmtest.NewMockProvider(),
				Executor:   exec,
				Config:     &cfg,
				WorkingDir: exec.GetWorkingDir(),
			}, nil, "context-mode-"+tt.mode)
			if got := a.Context.DeepDive(); got != tt.want {
				t.Fatalf("DeepDive = %v, want %v", got, tt.want)
			}
			if got := strings.Contains(a.systemPromptBase("/w"), contextAdviceDeepDive); got != tt.want {
				t.Fatalf("prompt carries the deep-dive advice = %v, want %v", got, tt.want)
			}
			// The wire view and the summarization prefix must carry the same
			// advice, or the summarization request misses the prompt cache.
			a.Messages = []llm.Message{{Role: "user", Content: "hi"}}
			view := a.rebuildView()
			prefix := a.systemPromptPrefix()
			if len(view) == 0 || len(prefix) != 1 || view[0].Content != prefix[0].Content {
				t.Fatal("the view's system message and the summarization prefix differ")
			}
			if got := strings.Contains(view[0].Content, contextAdviceDeepDive); got != tt.want {
				t.Fatalf("view carries the deep-dive advice = %v, want %v", got, tt.want)
			}
			wire, err := a.prepareMessages(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) == 0 || wire[0].Content != prefix[0].Content {
				t.Fatal("the request sent to the model has a different system message")
			}
			want := a.Context.SnapshotWithCounts(a.Messages, view, contextmgr.CountTokens(a.Messages)).Used
			if got := a.ContextStats(context.Background()).Snapshot.Used; got != want {
				t.Fatalf("ContextStats counts %d tokens, the sent view is %d", got, want)
			}
		})
	}
}

// TestAnnotateCompactionSummaryDeepDive pins where the smart-compact pause
// note goes: never onto the session summary before the current request in
// deep dive, which stays unchanged for the rest of the turn.
func TestAnnotateCompactionSummaryDeepDive(t *testing.T) {
	session := llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"}
	progress := llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"}
	req := llm.Message{Role: "user", Content: "the request"}
	tests := []struct {
		name     string
		deepDive bool
		msgs     []llm.Message
		wantAt   int
	}{
		{"default notes the summary", false, []llm.Message{{Role: "user", Content: "first"}, session, req}, 1},
		{"deep dive leaves the session summary", true, []llm.Message{{Role: "user", Content: "first"}, session, req}, -1},
		{"deep dive notes the progress summary", true, []llm.Message{{Role: "user", Content: "first"}, session, req, progress}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{Context: contextmgr.NewManager(nil, contextmgr.Settings{ContextLimit: 100000, CompactThreshold: 0.85, DeepDive: tt.deepDive})}
			a.Messages = append([]llm.Message(nil), tt.msgs...)
			ok := a.annotateCompactionSummary("[Paused before execution]")
			if ok != (tt.wantAt >= 0) {
				t.Fatalf("annotated = %v, want %v", ok, tt.wantAt >= 0)
			}
			for i, msg := range a.Messages {
				if got := strings.Contains(msg.Content, "[Paused before execution]"); got != (i == tt.wantAt) {
					t.Fatalf("message %d carries the note = %v", i, got)
				}
			}
		})
	}
}

// TestSetDeepDive pins that a live switch reaches both the manager and the
// next prompt, and that it leaves the other context settings alone.
func TestSetDeepDive(t *testing.T) {
	ConfigureSystemPrompt("")
	t.Cleanup(func() { ConfigureSystemPrompt("") })
	a := &Agent{Context: contextmgr.NewManager(nil, contextmgr.Settings{ContextLimit: 100000, CompactThreshold: 0.85, SmartCompact: true})}
	a.SetDeepDive(true)
	if !a.Context.DeepDive() || !strings.Contains(a.systemPromptBase("/w"), contextAdviceDeepDive) {
		t.Fatal("SetDeepDive(true) did not reach the manager and the prompt")
	}
	if !a.Context.SmartCompact() || a.Context.Settings.ContextLimit != 100000 {
		t.Fatal("SetDeepDive changed other settings")
	}
	a.SetDeepDive(false)
	if a.Context.DeepDive() || strings.Contains(a.systemPromptBase("/w"), contextAdviceDeepDive) {
		t.Fatal("SetDeepDive(false) left deep dive on")
	}
	(&Agent{}).SetDeepDive(true)
}

// TestSystemPromptContextAdviceCustomTemplate pins that a custom
// system_prompt without the default sentence is sent as it is.
func TestSystemPromptContextAdviceCustomTemplate(t *testing.T) {
	ConfigureSystemPrompt("Custom agent in {working_dir}")
	t.Cleanup(func() { ConfigureSystemPrompt("") })
	a := &Agent{Context: contextmgr.NewManager(nil, contextmgr.Settings{ContextLimit: 100000, CompactThreshold: 0.85, DeepDive: true})}
	if got := a.systemPromptBase("/x"); got != "Custom agent in /x" {
		t.Fatalf("custom prompt = %q", got)
	}
}
