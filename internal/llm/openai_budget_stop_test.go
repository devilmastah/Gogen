package llm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateResponseStreamBudgetStop(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"step one"}}]}

data: {"choices":[{"delta":{"reasoning_content":" step two"}}]}

data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var thinking []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnThinkingToken: func(token string) { thinking = append(thinking, token) },
			StopStream:      func() bool { return true },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.BudgetStop {
		t.Fatalf("result = %+v, want BudgetStop", result)
	}
	if got := strings.Join(thinking, ""); got != "step one" {
		t.Fatalf("thinking = %q, want the first chunk only", got)
	}
	if result.Content != "" {
		t.Fatalf("content = %q, want the stream to stop before the answer", result.Content)
	}
	if result.Reasoning != "step one" {
		t.Fatalf("reasoning = %q", result.Reasoning)
	}
}
