package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestCompactHistoryKeepsExecutedCallRoster pins the agent-level end-to-end
// behavior behind the compaction roster: when a background execute call is
// summarized away, its wire call id and the job id from its result survive in
// the summary. Without the roster the model would see a condensed span and
// silently lose the handle it needs to address the running job (background_job
// wait/list, terminal send) on a later turn.
func TestCompactHistoryKeepsExecutedCallRoster(t *testing.T) {
	provider := &countingProvider{limit: 20000}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              20000,
		CompactKeepRecentMessages: 2,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)

	a.appendMessage(llm.Message{Role: "user", Content: "start a long build"})
	// Two large middle messages so the summarized span clears the
	// minimum-middle guard (defaultMinMiddleTokens).
	for i := 0; i < 2; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 2000)})
	}
	a.appendMessage(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID:   "call_bg1",
		Name: "execute_command",
		Args: map[string]any{"command": "make build", "background": true},
	}}})
	a.appendMessage(llm.Message{
		Role:       "tool",
		ToolCallID: "call_bg1",
		Content:    "Started background job job-bg1.\nCommand: make build",
	})
	a.appendMessage(llm.Message{Role: "assistant", Content: "build started in the background"})
	a.appendMessage(llm.Message{Role: "user", Content: "check it later"})

	if err := a.CompactHistory(context.Background()); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}

	var summary string
	for _, msg := range a.SnapshotMessages() {
		if contextmgr.IsCompactionSummary(msg.Content) {
			summary = msg.Content
			break
		}
	}
	if summary == "" {
		t.Fatal("no compaction summary in history")
	}
	for _, want := range []string{
		"[Executed tool calls in the summarized span — id, tool, result head] — 1 executed",
		"- call_bg1 execute_command: Started background job job-bg1.",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	// Only the result head is carried; the full result text stays gone.
	if strings.Contains(summary, "Command: make build") {
		t.Fatalf("summary should carry only the result head, got:\n%s", summary)
	}
}
