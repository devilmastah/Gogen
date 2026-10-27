package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func reasoningFixture() []Message {
	return []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "answer one", Reasoning: strings.Repeat("think ", 30)},
		{Role: "user", Content: "more"},
		{Role: "assistant", Content: "answer two", Reasoning: strings.Repeat("ponder ", 30)},
	}
}

// TestReasoningEchoDefaultsOn pins the default: historical reasoning is echoed
// exactly as before (providers that expect their earlier thinking keep getting
// it, and the bytes stay stable for prompt caching).
func TestReasoningEchoDefaultsOn(t *testing.T) {
	t.Parallel()
	p := &OpenAIProvider{}
	chat := p.messagesToChat(reasoningFixture())
	for _, i := range []int{1, 3} {
		raw, err := json.Marshal(chat[i])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"reasoning_content"`) {
			t.Fatalf("assistant message %d lost its reasoning echo: %s", i, raw)
		}
	}
}

// TestReasoningEchoOffDropsHistoricalReasoning pins the traffic switch: with the
// echo off, no reasoning_content rides on the request at all — the payload is
// smaller and stays byte-stable (nothing is sent, so nothing shifts).
func TestReasoningEchoOffDropsHistoricalReasoning(t *testing.T) {
	t.Setenv("GOGEN_REASONING_ECHO", "off")
	p := &OpenAIProvider{}
	chat := p.messagesToChat(reasoningFixture())
	for _, i := range []int{1, 3} {
		raw, err := json.Marshal(chat[i])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "reasoning_content") {
			t.Fatalf("assistant message %d still echoes reasoning: %s", i, raw)
		}
		if !strings.Contains(string(raw), "answer") {
			t.Fatalf("assistant message %d lost its content: %s", i, raw)
		}
	}
	// Content and tool protocol are untouched, so a session that switches the
	// setting keeps working — only the reasoning field disappears.
	if !strings.Contains(string(mustJSON(t, chat[1])), `"role":"assistant"`) {
		t.Fatalf("assistant role missing: %s", mustJSON(t, chat[1]))
	}
}

// TestReasoningEchoAcceptsOnSpellings pins the knob parsing, so "1"/"on"/"yes"
// do not silently disable the echo.
func TestReasoningEchoAcceptsOnSpellings(t *testing.T) {
	for _, spelling := range []string{"on", "1", "true", "yes", "ON"} {
		t.Run(spelling, func(t *testing.T) {
			t.Setenv("GOGEN_REASONING_ECHO", spelling)
			if !ReasoningEchoEnabled() {
				t.Fatalf("%q must enable the reasoning echo", spelling)
			}
		})
	}
	for _, spelling := range []string{"off", "0", "false", "no", "OFF"} {
		t.Run(spelling, func(t *testing.T) {
			t.Setenv("GOGEN_REASONING_ECHO", spelling)
			if ReasoningEchoEnabled() {
				t.Fatalf("%q must disable the reasoning echo", spelling)
			}
		})
	}
	t.Run("unrecognized falls back to on", func(t *testing.T) {
		t.Setenv("GOGEN_REASONING_ECHO", "sometimes")
		if !ReasoningEchoEnabled() {
			t.Fatal("an unrecognized value must not change the default")
		}
	})
}

// TestMeasureWireBytesAccountsDroppedReasoning pins the instrumentation: with
// the echo off the Reasoning category is zero and the dropped bytes are
// reported separately, so the logged parts still reconcile with the exact body
// size (a negative Framing would be a measurement bug).
func TestMeasureWireBytesAccountsDroppedReasoning(t *testing.T) {
	t.Setenv("GOGEN_REASONING_ECHO", "off")
	msgs := reasoningFixture()
	got := measureWireBytes(msgs, nil)
	if got.Reasoning != 0 {
		t.Fatalf("Reasoning = %d, want 0 when the echo is off", got.Reasoning)
	}
	wantDropped := len(msgs[1].Reasoning) + len(msgs[3].Reasoning)
	if got.ReasoningDropped != wantDropped {
		t.Fatalf("ReasoningDropped = %d, want %d (the bytes the setting kept off the wire)", got.ReasoningDropped, wantDropped)
	}
	if got.History == 0 || got.ReasoningDropped == 0 {
		t.Fatalf("measurement lost the request's content: %+v", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
