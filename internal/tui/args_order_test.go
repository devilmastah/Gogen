package tui

import (
	"strings"
	"testing"
)

// TestFormatArgsMapIsStable pins the deterministic rendering: map iteration
// order is random, so the same tool call used to render differently on every
// rebuild — reshuffling arguments in the transcript (prompt-cache churn) and
// making the final line disagree with the streamed one.
func TestFormatArgsMapIsStable(t *testing.T) {
	args := map[string]any{"path": "a.go", "content": "x", "offset": 3}
	first := formatArgsMap(args, 80, nil)
	for i := 0; i < 20; i++ {
		if got := formatArgsMap(args, 80, nil); got != first {
			t.Fatalf("render %d differs:\n%q\n%q", i, got, first)
		}
	}
	if first != `content="x" offset="3" path="a.go"` {
		t.Fatalf("render = %q, want key-sorted output", first)
	}
}

// TestFormatArgsMapRuneBudget: maxLen counts runes, so a CJK value is actually
// shortened (comparing bytes appended "..." to a value that was never cut).
func TestFormatArgsMapRuneBudget(t *testing.T) {
	val := strings.Repeat("漢", 60) // 60 runes, 180 bytes
	got := formatArgsMap(map[string]any{"text": val}, 20, nil)
	if !strings.HasSuffix(got, `..."`) {
		t.Fatalf("render = %q, want an ellipsis", got)
	}
	if body := strings.TrimSuffix(strings.TrimPrefix(got, `text="`), `..."`); len([]rune(body)) > 17 {
		t.Fatalf("value kept %d runes, want <= 17", len([]rune(body)))
	}
}
