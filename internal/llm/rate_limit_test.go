package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func openAIErrorWithHeader(status int, header, value string) error {
	h := http.Header{}
	if header != "" {
		h.Set(header, value)
	}
	return &openai.Error{StatusCode: status, Response: &http.Response{Header: h}}
}

func TestRetryAfterDelay(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{name: "nil", err: nil, want: 0},
		{name: "not an api error", err: io.EOF, want: 0},
		{name: "no response", err: &openai.Error{StatusCode: 429}, want: 0},
		{name: "no header", err: openAIErrorWithHeader(429, "", ""), want: 0},
		{name: "delta seconds", err: openAIErrorWithHeader(429, "Retry-After", "5"), want: 5 * time.Second},
		{name: "zero seconds", err: openAIErrorWithHeader(429, "Retry-After", "0"), want: 0},
		{name: "negative seconds", err: openAIErrorWithHeader(429, "Retry-After", "-3"), want: 0},
		{name: "unparseable", err: openAIErrorWithHeader(429, "Retry-After", "soon"), want: 0},
		{name: "http date in the past", err: openAIErrorWithHeader(429, "Retry-After", past), want: 0},
		{name: "capped", err: openAIErrorWithHeader(429, "Retry-After", "3600"), want: retryAfterMax},
		{name: "own header only", err: openAIErrorWithHeader(503, "Retry-After", "2"), want: 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := retryAfterDelay(tc.err); got != tc.want {
				t.Fatalf("retryAfterDelay = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("http date in the future", func(t *testing.T) {
		t.Parallel()
		got := retryAfterDelay(openAIErrorWithHeader(429, "Retry-After", future))
		if got <= 0 || got > 10*time.Second {
			t.Fatalf("retryAfterDelay = %v, want a positive delay within the advertised 10s", got)
		}
	})
}

// TestRecoveryDelayPrefersRetryAfter pins the pacing rule: the provider's own
// Retry-After wins over the configured ladder backoff when it asks for longer,
// and the ladder backoff is the floor otherwise.
func TestRecoveryDelayPrefersRetryAfter(t *testing.T) {
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "1s")
	if got := recoveryDelay(openAIErrorWithHeader(429, "Retry-After", "7"), 0); got != 7*time.Second {
		t.Fatalf("recoveryDelay = %v, want the provider's 7s", got)
	}
	if got := recoveryDelay(openAIErrorWithHeader(429, "Retry-After", "0"), 0); got != time.Second {
		t.Fatalf("recoveryDelay = %v, want the 1s ladder floor", got)
	}
	if got := recoveryDelay(io.EOF, 1); got != 2*time.Second {
		t.Fatalf("recoveryDelay = %v, want the stage-1 ladder backoff (2s)", got)
	}
}

func TestIsRateLimited(t *testing.T) {
	t.Parallel()
	if !isRateLimited(openAIErrorWithHeader(429, "", "")) {
		t.Fatal("429 must be reported as rate limited")
	}
	if isRateLimited(openAIErrorWithHeader(503, "", "")) {
		t.Fatal("503 is a server failure, not a rate limit")
	}
	if isRateLimited(io.EOF) {
		t.Fatal("EOF is not a rate limit")
	}
}

// TestStreamRateLimitSkipsFallback pins the traffic rule: when the streaming
// attempt AND its retry are rate limited, the ladder stops instead of spending
// a third full-conversation upload on a non-streaming regeneration. A rate
// limit is not fixed by re-requesting — and the re-request deepens the very
// queue the caller is waiting on.
func TestStreamRateLimitSkipsFallback(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamRequests.Add(1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":"rate_limit_exceeded"}}`))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback answer"}}]}`))
	}))
	defer srv.Close()

	// MaxRetries(0): the SDK's own transport retry would add its requests and
	// blur the count this test is about (gogen's ladder).
	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	var reasons []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnStreamRetry: func(reason string) { reasons = append(reasons, reason) }},
	)
	if err == nil {
		t.Fatalf("rate-limited round returned no error (result %+v); the turn must surface the limit", result)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("error = %v, want the 429 surfaced", err)
	}
	if got := streamRequests.Load(); got != 2 {
		t.Fatalf("stream requests = %d, want 2 (attempt + one streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 0 {
		t.Fatalf("non-stream requests = %d, want 0: a rate limit must not spend a regeneration", got)
	}
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "stream interrupted") {
		t.Fatalf("OnStreamRetry fired %v, want only the streaming retry (no non-streaming recovery)", reasons)
	}
}

// TestStreamServerErrorStillFallsBack is the complement: a server-side failure
// (not a rate limit) keeps the full ladder, because a re-request genuinely can
// recover it.
func TestStreamServerErrorStillFallsBack(t *testing.T) {
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	result, err := p.GenerateResponseStream(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, nil, &StreamHandlers{})
	if err != nil {
		t.Fatalf("GenerateResponseStream: %v", err)
	}
	if got := nonStreamRequests.Load(); got != 1 {
		t.Fatalf("non-stream requests = %d, want 1 (5xx keeps the fallback)", got)
	}
	if result.Content != "fallback answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}
