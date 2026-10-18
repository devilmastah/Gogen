package tui

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestContextIndicatorUpdatesDuringStreaming is a regression test: the
// status-bar context indicator previously only refreshed at streamEndMsg /
// streamErrorMsg (refreshContextStats reads a.Messages, which races with
// the streaming goroutine mutating it mid-turn), so it stayed frozen at the
// pre-turn value for the entire duration of a streamed response. It should
// now move as tokens/thinking/tool-args arrive, using a safe local estimate,
// and land on the exact value once refreshContextStats runs at stream end.
func TestContextIndicatorUpdatesDuringStreaming(t *testing.T) {
	m := Model{
		chatLines:           nil,
		streamAssistantLine: -1,
		streamThinkingLine:  -1,
		contextStats: agent.TurnContext{
			Snapshot: contextmgr.ContextSnapshot{
				Used:  1000,
				Limit: 100000,
			},
		},
	}
	m.contextLine = agent.FormatContextBrief(m.contextStats)
	before := m.contextLine

	// Start of a new turn: baseline is captured from the last authoritative
	// measurement above.
	m.handleStreamStart()

	m.handleStreamToken(strings.Repeat("a", 400)) // ~100 estimated tokens
	afterOneToken := m.contextLine

	if afterOneToken == before {
		t.Fatalf("context indicator did not update after a streamed token: still %q", afterOneToken)
	}
	if !strings.Contains(afterOneToken, "(est.)") {
		t.Fatalf("expected live estimate to be marked as approximate, got %q", afterOneToken)
	}

	m.handleStreamToken(strings.Repeat("b", 400)) // more tokens -> estimate should grow further
	afterTwoTokens := m.contextLine
	if afterTwoTokens == afterOneToken {
		t.Fatalf("context indicator did not move on a second streamed token: still %q", afterTwoTokens)
	}

	// Simulate the authoritative refresh done at stream end landing on an
	// exact figure that differs from the rough estimate.
	m.contextStats = agent.TurnContext{
		Snapshot: contextmgr.ContextSnapshot{Used: 1180, Limit: 100000},
	}
	m.contextLine = agent.FormatContextBrief(m.contextStats)
	if strings.Contains(m.contextLine, "(est.)") {
		t.Fatalf("final refreshed indicator should not be marked as an estimate: %q", m.contextLine)
	}
}

// TestContextIndicatorNoResetAtRoundStart is a regression test for the
// (est.) context indicator dropping back to the pre-reply value at each
// round boundary of a multi-round (tool-calling) reply. handleStreamRoundStart
// must re-base the live estimate from a refreshed authoritative value —
// which includes the previous round's growth — rather than from the stale
// end-of-previous-turn mirror.
func TestContextIndicatorNoResetAtRoundStart(t *testing.T) {
	mgr := contextmgr.NewManager(nil, contextmgr.Settings{ContextLimit: 100000})
	a := &agent.Agent{
		Context:  mgr,
		Messages: []llm.Message{{Role: "user", Content: "hello"}},
	}
	m := Model{
		agent:               a,
		chatLines:           nil,
		streamAssistantLine: -1,
		streamThinkingLine:  -1,
	}
	m.contextStats = a.ContextStats(context.Background())
	if m.contextStats.Snapshot.Used <= 0 {
		t.Fatalf("expected non-zero baseline usage, got %+v", m.contextStats.Snapshot)
	}
	m.contextLine = agent.FormatContextBrief(m.contextStats)
	usedBeforeTurn := m.contextStats.Snapshot.Used

	// Round 1 streams in: the estimate climbs above the pre-turn value.
	m.handleStreamStart()
	m.handleStreamToken(strings.Repeat("a", 400)) // ~100 estimated tokens
	if !strings.Contains(m.contextLine, "(est.)") {
		t.Fatalf("expected live estimate during round 1, got %q", m.contextLine)
	}
	estAfterRound1 := m.contextEst.Used()

	// Between rounds the agent appends the assistant message and the tool
	// result to a.Messages (and records the round's API usage).
	a.Messages = append(a.Messages,
		llm.Message{Role: "assistant", Content: "reading file"},
		llm.Message{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x", 4000)},
	)

	m.handleStreamRoundStart()

	// The re-base must reflect the refreshed authoritative value, which
	// includes round 1's growth — not the stale pre-turn mirror.
	if m.contextEst.Base() <= usedBeforeTurn {
		t.Fatalf("round-start baseline did not include round 1 growth: baseline=%d before-turn=%d",
			m.contextEst.Base(), usedBeforeTurn)
	}
	if m.contextEst.Base() < estAfterRound1 {
		t.Fatalf("indicator reset at round start: baseline=%d is below the pre-round estimate %d",
			m.contextEst.Base(), estAfterRound1)
	}
	if strings.Contains(m.contextLine, "(est.)") {
		t.Fatalf("refreshed round-start value should be exact, got %q", m.contextLine)
	}

	// The live estimate resumes from the new baseline.
	m.handleStreamToken(strings.Repeat("b", 400))
	if !strings.Contains(m.contextLine, "(est.)") {
		t.Fatalf("expected live estimate to resume after round start, got %q", m.contextLine)
	}
}

// TestModelChangedRefreshesContextIndicator pins the restore/resume path: a
// background model validation that resolves the restored session's context
// window sends modelChangedMsg. The handler must re-probe the indicator — a
// plain re-render reads the cached context line, which still holds the
// snapshot's pre-warmed fallback (the 128k the session was saved with).
func TestModelChangedRefreshesContextIndicator(t *testing.T) {
	a := newSwitchTestAgent(t)
	m := &Model{agent: a, ctx: context.Background()}

	model, cmd := m.Update(modelChangedMsg{})
	if model == nil {
		t.Fatal("Update returned a nil model")
	}
	if cmd == nil {
		t.Fatal("modelChangedMsg must request a context-stats refresh")
	}
	msg := cmd()
	if _, ok := msg.(contextStatsMsg); !ok {
		t.Fatalf("modelChangedMsg command delivered %T, want contextStatsMsg", msg)
	}
}
