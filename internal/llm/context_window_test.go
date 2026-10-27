package llm

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"
)

// openAIContextError builds an openai.Error carrying the OpenAI
// context-window refusal code, with the request/response fields set so
// its Error() method (which formats the request) does not panic.
func openAIContextError(code, message string) *openai.Error {
	return &openai.Error{
		Code:       code,
		Message:    message,
		StatusCode: 400,
		Request:    &http.Request{Method: "POST"},
		Response:   &http.Response{StatusCode: 400},
	}
}

func TestIsContextWindowError(t *testing.T) {
	overflowCode := openAIContextError("context_length_exceeded", "prompt too long")
	otherCode := openAIContextError("invalid_api_key", "bad key")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain error", err: errors.New("boom"), want: false},
		{name: "rate limit", err: errors.New("429 rate limit exceeded"), want: false},
		{name: "openai error other code", err: otherCode, want: false},
		{name: "sentinel", err: ErrContextWindowExceeded, want: true},
		{name: "wrapped sentinel", err: fmt.Errorf("stream error: %w (fallback also failed: %v)", ErrContextWindowExceeded, errors.New("x")), want: true},
		{name: "openai context_length_exceeded code", err: overflowCode, want: true},
		{name: "wrapped openai context_length_exceeded code", err: fmt.Errorf("openai api error: %w", overflowCode), want: true},
		{name: "marker: maximum context length", err: errors.New("This model's maximum context length is 128000 tokens"), want: true},
		{name: "marker: prompt is too long", err: errors.New("Prompt is too long: 200000 tokens > 128000 maximum"), want: true},
		{name: "marker: too many tokens", err: errors.New("request rejected: too many tokens in the prompt"), want: true},
		{name: "marker case-insensitive", err: errors.New("Maximum Context Length exceeded"), want: true},
		{name: "marker inside wrap", err: fmt.Errorf("openai api error: %w", errors.New("prompt is too long")), want: true},
		{name: "marker: exceeds the available context size", err: errors.New("request (221142 tokens) exceeds the available context size (220160 tokens), try increasing it"), want: true},
		{name: "marker: exceed_context_size error type", err: errors.New("400 Bad Request {\"code\":400,\"message\":\"request too big\",\"type\":\"exceed_context_size_error\"}"), want: true},
		// The exact refusal shape emitted by the local proxy (the openai-go
		// SDK error text includes the raw JSON body):
		{name: "proxy 400 body", err: fmt.Errorf("openai api error: %w", errors.New("POST http://10.0.0.174:8080/v1/chat/completions: 400 Bad Request {\"code\":400,\"message\":\"request (221142 tokens) exceeds the available context size (220160 tokens), try increasing it\",\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":221142,\"n_ctx\":220160}")), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsContextWindowError(tt.err); got != tt.want {
				t.Fatalf("IsContextWindowError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestWrapContextWindowError(t *testing.T) {
	// A classified error gets the sentinel in its chain...
	orig := errors.New("prompt is too long")
	wrapped := wrapContextWindowError(orig)
	if !errors.Is(wrapped, ErrContextWindowExceeded) {
		t.Fatal("wrapped error: errors.Is(err, ErrContextWindowExceeded) = false, want true")
	}
	// ...and never masks the original error.
	if !errors.Is(wrapped, orig) {
		t.Fatal("wrapped error: the original error is no longer in the chain")
	}
	if wrapped.Error() != orig.Error() {
		t.Fatalf("Error() = %q, want the original message %q", wrapped.Error(), orig.Error())
	}

	// A non-classified error is returned unchanged.
	other := errors.New("internal server error")
	if got := wrapContextWindowError(other); got != other {
		t.Fatalf("non-overflow error was wrapped: %v", got)
	}
	if got := wrapContextWindowError(nil); got != nil {
		t.Fatalf("nil was wrapped: %v", got)
	}
}

// TestIsContextWindowErrorIgnoresThrottles pins the 429 carve-out: Bedrock-style
// throttles say "Too many tokens, please wait…", and classifying that as an
// overflow runs a lossy forced compaction instead of the backoff the provider
// asked for.
func TestIsContextWindowErrorIgnoresThrottles(t *testing.T) {
	throttle := &openai.Error{
		StatusCode: http.StatusTooManyRequests,
		Message:    "Too many tokens, please wait before trying again.",
		Request:    &http.Request{Method: "POST"},
		Response:   &http.Response{StatusCode: http.StatusTooManyRequests},
	}
	if IsContextWindowError(throttle) {
		t.Fatal("a 429 mentioning tokens must not be classified as a context overflow")
	}
	// The same wording on a 400 is a genuine overflow (the marker backstop).
	badRequest := &openai.Error{
		StatusCode: http.StatusBadRequest,
		Message:    "Too many tokens, please wait before trying again.",
		Request:    &http.Request{Method: "POST"},
		Response:   &http.Response{StatusCode: http.StatusBadRequest},
	}
	if !IsContextWindowError(badRequest) {
		t.Fatal("the marker backstop must still classify a 400 with the same wording")
	}
}
