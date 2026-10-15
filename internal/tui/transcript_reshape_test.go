package tui

import (
	"strconv"
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

// TestTranscriptRebuiltAfterReshape pins the fix for unbounded TUI memory
// growth on long-lived sessions. The transcript is append-only between full
// rebuilds, so a wholesale history reshape that does NOT go through an
// explicit rebuild path — a mid-turn AUTO-compaction being the common case —
// used to leave every line ever rendered in memory (chatLines plus its
// wrapped and styled copies and the prompt-rail anchors) for the life of the
// process, even though the agent's own history had been compacted in place.
func TestTranscriptRebuiltAfterReshape(t *testing.T) {
	m := dragModel(t)
	m.width, m.height = 100, 30
	m.agent.SessionID = "session-1"

	// A transcript that has rendered a long history (much of which the last
	// auto-compaction has already summarized away), recorded as current.
	renderedLines := 500
	for i := 0; i < renderedLines; i++ {
		m.chatLines = append(m.chatLines, SystemStyle.Render("rendered history line"))
	}
	m.setViewportContent()
	m.markTranscriptRendered()
	if len(m.chatLines) != renderedLines {
		t.Fatalf("setup: chatLines = %d, want %d", len(m.chatLines), renderedLines)
	}
	if m.rebuildTranscriptIfReshaped() {
		t.Fatal("no reshape has happened yet; the check must be a no-op")
	}

	// The agent auto-compacts mid-turn: Messages is replaced wholesale (a
	// summary plus the recent tail), which bumps the history epoch. No
	// explicit rebuild path runs for the TUI here.
	m.agent.RestoreSessionLocal(agent.SessionSnapshot{Messages: []llm.Message{
		{Role: "user", Content: "summary of the compacted middle"},
		{Role: "assistant", Content: "recent tail"},
	}}, "")
	if m.agent.HistoryEpoch() == m.historyEpoch {
		t.Fatal("setup: the reshape must move the history epoch")
	}

	// Turn end (the stream goroutine has returned): the transcript must be
	// rebuilt from the compacted history.
	finished, _ := m.handleTurnFinishedMsg("s1", 0, nil)
	if finished == nil {
		t.Fatal("turn end returned a nil model")
	}
	if got := len(m.chatLines); got >= renderedLines {
		t.Fatalf("transcript not rebuilt after the reshape: %d chatLines (was %d) for a 2-message history", got, renderedLines)
	}
	// The epoch is recorded, so further turn boundaries are no-ops (the
	// rebuild is one-shot per reshape, not a per-turn cost).
	if m.rebuildTranscriptIfReshaped() {
		t.Fatal("rebuild must be one-shot per reshape")
	}
}

// TestInputHistoryBounded pins that the ↑ history stops growing past
// maxInputHistory (entries are whole prompts, so the list is otherwise
// unbounded for the life of the process).
func TestInputHistoryBounded(t *testing.T) {
	m := dragModel(t)

	m.appendInputHistory("first")
	m.appendInputHistory("first") // immediate duplicate: skipped
	if len(m.inputHistory) != 1 {
		t.Fatalf("duplicate not skipped: %v", m.inputHistory)
	}

	for i := 0; i < maxInputHistory+50; i++ {
		m.appendInputHistory(strings.Repeat("x", 8) + strconv.Itoa(i))
	}
	if got := len(m.inputHistory); got != maxInputHistory {
		t.Fatalf("input history = %d entries, want %d", got, maxInputHistory)
	}
	// The newest entry survives; the oldest ("first") was dropped.
	if want := strings.Repeat("x", 8) + strconv.Itoa(maxInputHistory+49); m.inputHistory[len(m.inputHistory)-1] != want {
		t.Fatalf("newest entry = %q, want %q", m.inputHistory[len(m.inputHistory)-1], want)
	}
	if m.historyIdx != len(m.inputHistory) {
		t.Fatalf("historyIdx = %d, want %d", m.historyIdx, len(m.inputHistory))
	}
}

// TestRecordedSubagentsBounded pins that the /subagents list stops growing
// past maxRecordedSubagents (each record holds a report of up to
// agent.MaxSubagentReportBytes).
func TestRecordedSubagentsBounded(t *testing.T) {
	m := dragModel(t)
	for i := 0; i < maxRecordedSubagents+10; i++ {
		m.recordSubagent(subagentRecord{label: "job-" + strconv.Itoa(i), report: "r"})
	}
	if got := len(m.subagents); got != maxRecordedSubagents {
		t.Fatalf("subagent records = %d, want %d", got, maxRecordedSubagents)
	}
	// Newest kept, oldest dropped.
	if got := m.subagents[len(m.subagents)-1].label; got != "job-"+strconv.Itoa(maxRecordedSubagents+9) {
		t.Fatalf("newest record = %q", got)
	}
	if got := m.subagents[0].label; got != "job-"+strconv.Itoa(10) {
		t.Fatalf("oldest record = %q, want the first dropped one", got)
	}
}

// TestTranscriptRebuildPreservesDetachedReader pins that the reshape rebuild
// is a content rebuild, not a navigation event: a reader who scrolled away
// from the live edge must not be yanked to the bottom (turn-end parity, see
// handleStreamEnd).
func TestTranscriptRebuildPreservesDetachedReader(t *testing.T) {
	m := dragModel(t)
	m.width, m.height = 100, 30
	m.agent.SessionID = "session-1"
	for i := 0; i < 200; i++ {
		m.chatLines = append(m.chatLines, SystemStyle.Render("line"))
	}
	m.setViewportContent()
	m.markTranscriptRendered()
	m.viewport.GotoTop()
	m.viewDetached = true

	m.agent.RestoreSessionLocal(agent.SessionSnapshot{Messages: []llm.Message{
		{Role: "user", Content: "summary"},
	}}, "")

	if !m.rebuildTranscriptIfReshaped() {
		t.Fatal("reshape not detected")
	}
	if !m.viewDetached {
		t.Fatal("a reshape rebuild must not resume auto-follow under a detached reader")
	}
}
