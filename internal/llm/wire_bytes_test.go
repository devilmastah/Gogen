package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/debuglog"
)

// TestMeasureWireBytesAttributesContributors pins the byte attribution itself:
// each contributor is measured from its own source, so a session's traffic can
// be blamed on the right thing.
func TestMeasureWireBytesAttributesContributors(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("s", 100)},
		{Role: "user", Content: strings.Repeat("u", 50), Images: []ImageInput{{DataURL: strings.Repeat("i", 700), Detail: "low"}}},
		{
			Role:      "assistant",
			Content:   strings.Repeat("a", 30),
			Reasoning: strings.Repeat("r", 400),
			ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", ArgsStr: strings.Repeat("g", 60)}},
		},
		{Role: "tool", ToolCallID: "call_1", Content: strings.Repeat("o", 20)},
	}
	tools := []Tool{{Type: "function", Name: "read_file", Description: strings.Repeat("d", 25), Parameters: map[string]any{"type": "object"}}}

	got := measureWireBytes(msgs, tools)
	if got.System != 100 {
		t.Fatalf("System = %d, want 100", got.System)
	}
	if got.Reasoning != 400 {
		t.Fatalf("Reasoning = %d, want 400", got.Reasoning)
	}
	if got.Images != 700+len("low") {
		t.Fatalf("Images = %d, want %d", got.Images, 700+len("low"))
	}
	// Content + tool-call name/id/args + the tool result's id and content.
	wantHistory := 50 + 30 + 20 + len("call_1")*2 + len("read_file") + 60
	if got.History != wantHistory {
		t.Fatalf("History = %d, want %d", got.History, wantHistory)
	}
	if got.Tools == 0 {
		t.Fatal("Tools = 0, want the serialized tool definitions")
	}
}

// TestWireBytesLoggedPerRequest drives a real streaming round with debug logging
// on and asserts the accounting lands in the log, reconciled: the parts never
// exceed the exact encoded body size, and the categories of this fixture are
// non-zero (system, history, reasoning, images, tools).
func TestWireBytesLoggedPerRequest(t *testing.T) {
	// Global debug-log configuration: not parallel.
	logPath := filepath.Join(t.TempDir(), "debug.log")
	debuglog.Configure(logPath, "wire-bytes-test")
	t.Cleanup(func() { debuglog.Configure("", "") })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	msgs := []Message{
		{Role: "system", Content: "you are a test agent"},
		{Role: "user", Content: "look at this", Images: []ImageInput{{DataURL: "data:image/png;base64," + strings.Repeat("A", 2048)}}},
		{Role: "assistant", Content: "checking", Reasoning: strings.Repeat("think ", 40)},
	}
	if _, err := p.GenerateResponseStream(
		t.Context(), msgs, nil, roundRequestTestTools(), &StreamHandlers{OnToken: func(string) {}},
	); err != nil {
		t.Fatalf("GenerateResponseStream: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec struct {
			Location string         `json:"location"`
			Data     map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Location == "llm/wire_bytes" {
			entry = rec.Data
		}
	}
	if entry == nil {
		t.Fatalf("no llm/wire_bytes entry in the debug log:\n%s", raw)
	}

	num := func(key string) int64 {
		t.Helper()
		v, ok := entry[key].(float64)
		if !ok {
			t.Fatalf("entry[%q] missing or not a number: %v", key, entry[key])
		}
		return int64(v)
	}
	total := num("total")
	if total == 0 {
		t.Fatalf("total = 0: %v", entry)
	}
	sum := num("system") + num("history") + num("reasoning") + num("images") + num("tools") + num("framing")
	if sum != total {
		t.Fatalf("parts (%d) do not reconcile with total (%d): %v", sum, total, entry)
	}
	for _, key := range []string{"system", "history", "reasoning", "images", "tools"} {
		if num(key) <= 0 {
			t.Fatalf("%s = 0, want a measured contribution: %v", key, entry)
		}
	}
	if got := num("sessionImages"); got < 2048 {
		t.Fatalf("sessionImages = %d, want the 2 KB image counted cumulatively", got)
	}
	if num("sessionTotal") < total {
		t.Fatalf("sessionTotal = %d, want >= this request's %d", num("sessionTotal"), total)
	}
}
