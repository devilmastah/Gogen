package contextmgr

import (
	"testing"

	"gogen/internal/llm"
)

// TestChooseCompactKeep pins the keep-count selection for the emergency
// compaction tier: the configured keep is returned unchanged when the
// summarized middle at that keep already covers the deficit, and is lowered
// stepwise — floor 1 (the current user prompt is preserved verbatim) — only
// as far as the deficit requires. -1 is returned when no compactable middle
// exists.
func TestChooseCompactKeep(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:              20000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})

	// 6 messages: head (user), 3 large middle, 2 short tail. At keep=2 the
	// middle is the 3 large messages (1500 tokens); at keep=1 it gains the
	// first tail message (1510).
	msgs := []llm.Message{
		{Role: "user", Content: "start"},
		{Role: "user", Content: "big1"},
		{Role: "user", Content: "big2"},
		{Role: "user", Content: "big3"},
		{Role: "user", Content: "recent1"},
		{Role: "user", Content: "recent2"},
	}
	counts := []int{10, 500, 500, 500, 10, 10}

	tests := []struct {
		name   string
		needed int
		want   int
	}{
		{"no deficit keeps configured keep", 0, 2},
		{"negative deficit keeps configured keep", -100, 2},
		{"middle at configured keep covers deficit", 1400, 2},
		{"deficit beyond keep-2 middle lowers to 1", 1505, 1},
		{"deficit beyond any middle still floors at 1", 100000, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.ChooseCompactKeep(msgs, counts, tc.needed, nil); got != tc.want {
				t.Fatalf("ChooseCompactKeep(needed=%d) = %d, want %d", tc.needed, got, tc.want)
			}
		})
	}

	// 3 messages with keep=2: the keep is clamped to len-2=1, and the
	// single middle message is used.
	msgs3, counts3 := msgs[:3], counts[:3]
	if got := m.ChooseCompactKeep(msgs3, counts3, 100, nil); got != 1 {
		t.Fatalf("ChooseCompactKeep with 3 messages = %d, want 1", got)
	}

	// No compactable middle: too few messages.
	if got := m.ChooseCompactKeep(msgs[:2], counts[:2], 100, nil); got != -1 {
		t.Fatalf("ChooseCompactKeep with 2 messages = %d, want -1", got)
	}
	// No user message to anchor the head.
	assistant := []llm.Message{
		{Role: "assistant", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "assistant", Content: "c"},
	}
	if got := m.ChooseCompactKeep(assistant, []int{1, 1, 1}, 100, nil); got != -1 {
		t.Fatalf("ChooseCompactKeep without a user message = %d, want -1", got)
	}
	// Incomplete count cache.
	if got := m.ChooseCompactKeep(msgs, counts[:3], 100, nil); got != -1 {
		t.Fatalf("ChooseCompactKeep with incomplete counts = %d, want -1", got)
	}
}

// TestChooseCompactKeepHonoursPinsAndToolTail pins the sizing fix: the middle
// must be measured on the split compactPinned will actually use. A pinned
// message (or a trailing tool group) pulls the tail earlier and shrinks the
// middle, so sizing on the naive len-keep split overestimated the savings and
// an "emergency" keep could still overflow after compaction.
func TestChooseCompactKeepHonoursPinsAndToolTail(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:              20000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 3,
		CompactReserveTokens:      4000,
	})
	// head + 5 large middle + 3-message tail.
	msgs := []llm.Message{
		{Role: "user", Content: "start"},
		{Role: "user", Content: "big1"},
		{Role: "user", Content: "big2"},
		{Role: "user", Content: "big3"},
		{Role: "user", Content: "big4"},
		{Role: "user", Content: "big5"},
		{Role: "user", Content: "recent1"},
		{Role: "user", Content: "recent2"},
		{Role: "user", Content: "recent3"},
	}
	counts := []int{10, 500, 500, 500, 500, 500, 10, 10, 10}

	// Unpinned, keep=3 covers big1..big5 = 2500 tokens.
	if got := m.ChooseCompactKeep(msgs, counts, 2400, nil); got != 3 {
		t.Fatalf("no pin: keep = %d, want 3", got)
	}
	// Pinning index 3 pulls the tail to 3, so the middle is big1+big2 at EVERY
	// keep: 2400 can never be freed, and the function floors at 1 instead of
	// claiming keep=3 would do it.
	if got := m.ChooseCompactKeep(msgs, counts, 2400, map[int]struct{}{3: {}}); got != 1 {
		t.Fatalf("pinned middle: keep = %d, want the floor 1", got)
	}

	// Trailing tool group: with keep=2 the naive tail starts ON the tool result,
	// which adjustCompactTailStart walks back over the calling assistant
	// message. The result carries 4000 tokens: the naive split (1..7 = 7000)
	// would satisfy 5000 at keep=2, but the adjusted middle (1..6 = 3000) cannot
	// — the tool group must stay verbatim, so keep drops to 1.
	m2 := NewManager(&stubProvider{}, Settings{
		ContextLimit:              20000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	toolTail := append([]llm.Message{}, msgs...)
	toolTail[6] = llm.Message{Role: "assistant", Content: "calling",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}}
	toolTail[7] = llm.Message{Role: "tool", ToolCallID: "c1", Content: "huge result"}
	toolCounts := []int{10, 500, 500, 500, 500, 500, 500, 4000, 10}
	if got := m2.ChooseCompactKeep(toolTail, toolCounts, 5000, nil); got != 1 {
		t.Fatalf("tool tail: keep = %d, want 1 (the tool group must stay in the tail)", got)
	}
}
