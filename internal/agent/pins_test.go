package agent

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestPinLastUserSkipsCompactionSummaries pins the pin semantics for
// conversations whose last user-role message is a compaction summary
// (legacy sessions stored summaries with a user role): the pin must land on
// the last real user message, never on a summary.
func TestPinLastUserSkipsCompactionSummaries(t *testing.T) {
	p := NewPinManager()
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "work"},
		{Role: "user", Content: "real last user message"},
		{Role: "assistant", Content: "more work"},
		{Role: "user", Content: contextmgr.SummaryPrefix + "legacy summary"},
	}
	p.PinLastUser(msgs)
	if !p.IsPinned(2) {
		t.Fatalf("expected the last real user message (index 2) to be pinned, got %v", p.PinnedIndices())
	}
	if p.IsPinned(4) {
		t.Fatalf("summary message (index 4) must not be pinned, got %v", p.PinnedIndices())
	}
}

// TestPinLastUserSkipsMachineMessages pins that context_pin_last pins the
// request, not a message gogen wrote after it: a job notice (as the job's
// producer formats it), a subagent notice, or a read_image caption, which
// would otherwise keep its image through every compaction.
func TestPinLastUserSkipsMachineMessages(t *testing.T) {
	tests := []struct {
		name  string
		after llm.Message
	}{
		{"job notice", llm.Message{Role: "user", Content: jobExitSummary(&BackgroundJob{ID: "bg-1", Command: "go test ./..."})}},
		{"subagent notice", llm.Message{Role: "user", Content: contextmgr.SubagentNoticePrefix + "scout] found it"}},
		{"read_image caption", llm.Message{Role: "user", Content: contextmgr.ReadImagePrefix + "shot.png", Images: []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPinManager()
			msgs := []llm.Message{
				{Role: "user", Content: "first"},
				{Role: "user", Content: "the request"},
				{Role: "assistant", Content: "work"},
				tt.after,
			}
			p.PinLastUser(msgs)
			if !p.IsPinned(1) || len(p.PinnedIndices()) != 1 {
				t.Fatalf("pins = %v, want only the request (index 1)", p.PinnedIndices())
			}
		})
	}
}

// TestPinLastUserNoRealUserMessage pins the no-op case: when every user
// message is a compaction summary, nothing is pinned.
func TestPinLastUserNoRealUserMessage(t *testing.T) {
	p := NewPinManager()
	msgs := []llm.Message{
		{Role: "user", Content: contextmgr.SummaryPrefix + "legacy summary"},
	}
	p.PinLastUser(msgs)
	if len(p.PinnedIndices()) != 0 {
		t.Fatalf("expected no pin, got %v", p.PinnedIndices())
	}
}

// TestListPinsTruncatesRuneSafe pins the display truncation: a long pinned
// message is cut at 80 RUNES (never mid-character), so the tool result stays
// valid UTF-8.
func TestListPinsTruncatesRuneSafe(t *testing.T) {
	p := NewPinManager()
	// 85 multi-byte runes: a byte-slice cut at 80 would split a rune.
	content := ""
	for i := 0; i < 85; i++ {
		content += "é"
	}
	msgs := []llm.Message{{Role: "user", Content: content}}
	p.PinLastUser(msgs)

	out := p.ListPins(msgs)
	want := string([]rune(content)[:80]) + "…"
	if !strings.Contains(out, want) {
		t.Fatalf("ListPins output = %q, want it to contain the rune-safe 80-rune cut %q", out, want)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("ListPins output is not valid UTF-8: %q", out)
	}
}

// IsPinned, PinnedIndices, and ListPins are the PinManager's test-only read
// surface, moved out of pins.go: production uses only PinLastUser /
// ClearPins / ReplacePins / PinnedSet.

// IsPinned reports whether the message at the given index is pinned.
func (p *PinManager) IsPinned(index int) bool {
	_, ok := p.pinned[index]
	return ok
}

// PinnedIndices returns all pinned indices.
func (p *PinManager) PinnedIndices() []int {
	indices := make([]int, 0, len(p.pinned))
	for idx := range p.pinned {
		indices = append(indices, idx)
	}
	return indices
}

// ListPins returns a formatted list of pinned messages.
func (p *PinManager) ListPins(messages []llm.Message) string {
	if len(p.pinned) == 0 {
		return "No pinned messages"
	}
	var b strings.Builder
	b.WriteString("Pinned messages (survive compaction):\n")
	for idx := range p.pinned {
		if idx >= 0 && idx < len(messages) {
			content := messages[idx].Content
			// Rune-safe cut: slicing on bytes could split a multi-byte
			// character and inject invalid UTF-8 into the tool result.
			if r := []rune(content); len(r) > 80 {
				content = string(r[:80]) + "…"
			}
			fmt.Fprintf(&b, "  #%d: %s\n", idx, content)
		}
	}
	return b.String()
}
