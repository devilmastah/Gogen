package llm

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression tests for the openai-go "unexpected end of JSON input" stream
// failures: its decoder dispatches an event on every SSE blank line and
// json.Unmarshals the data unconditionally, so spec-legal keep-alives
// (comment lines, blank or empty-data frames) killed the stream before any
// output and burned the whole recovery ladder. v3 skipped the comment and
// field-less styles, but empty-data frames still break every released
// version — the transport filter (sse_filter.go) must make all of them
// harmless. These tests pin that contract end-to-end through the provider.

// TestGenerateResponseStreamSkipsCommentKeepalives: ": comment" frames around
// real chunks must not break the stream.
func TestGenerateResponseStreamSkipsCommentKeepalives(t *testing.T) {
	t.Parallel()
	const sse = ": keep-alive 1\n" +
		"\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"an\"}}]}\n" +
		"\n" +
		": keep-alive 2\n" +
		"\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"swer\"}}]}\n" +
		"\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
		"\n"
	assertStreamDelivers(t, sse, "answer")
}

// TestGenerateResponseStreamSkipsEmptyDataKeepalives: ping events with an
// empty (or whitespace-only) data field must not break the stream either.
func TestGenerateResponseStreamSkipsEmptyDataKeepalives(t *testing.T) {
	t.Parallel()
	const sse = "event: ping\n" +
		"data: \n" +
		"\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n" +
		"\n" +
		"data:\n" +
		"data:   \n" +
		"\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
		"\n"
	assertStreamDelivers(t, sse, "answer")
}

// assertStreamDelivers runs one streaming round against a canned SSE body and
// pins the delivered content.
func assertStreamDelivers(t *testing.T, sse, wantContent string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var content []string
	res, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil, nil,
		&StreamHandlers{OnToken: func(tok string) { content = append(content, tok) }},
	)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if got := strings.Join(content, ""); got != wantContent {
		t.Fatalf("content = %q, want %q", got, wantContent)
	}
	if res.Content != wantContent {
		t.Fatalf("result.Content = %q, want %q", res.Content, wantContent)
	}
}

// TestFilterSSEEvents pins the filter's line semantics directly: keep-alive
// frames (comments, data-less events) are suppressed, everything else is
// forwarded byte-for-byte, and a malformed-but-non-empty data frame still
// reaches the SDK so genuine payload errors keep failing loudly.
func TestFilterSSEEvents(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "comment-only keepalive dropped",
			in:   ": ping\n\ndata: {\"a\":1}\n\n",
			want: "data: {\"a\":1}\n\n",
		},
		{
			name: "event field without data dropped whole",
			in:   "event: ping\n\ndata: {\"a\":1}\n\n",
			want: "data: {\"a\":1}\n\n",
		},
		{
			name: "empty and whitespace-only data dropped",
			in:   "data:\n\ndata:   \n\ndata: {\"a\":1}\n\n",
			want: "data: {\"a\":1}\n\n",
		},
		{
			name: "leading keepalives before first event dropped",
			in:   ": OPENROUTER PROCESSING\n\nevent: ping\ndata: \n\ndata: {\"a\":1}\n\n",
			want: "data: {\"a\":1}\n\n",
		},
		{
			name: "comment inside event dropped, event kept",
			in:   "data: {\"a\":1}\n: mid-event note\n\ndata: {\"a\":2}\n\n",
			want: "data: {\"a\":1}\n\ndata: {\"a\":2}\n\n",
		},
		{
			name: "DONE sentinel kept",
			in:   "data: {\"a\":1}\n\ndata: [DONE]\n\n",
			want: "data: {\"a\":1}\n\ndata: [DONE]\n\n",
		},
		{
			name: "multi-line data kept with empty first line",
			in:   "data:\ndata: {\"a\":1}\n\n",
			want: "data:\ndata: {\"a\":1}\n\n",
		},
		{
			name: "malformed but non-empty data forwarded",
			in:   "data: {truncated\n\n",
			want: "data: {truncated\n\n",
		},
		{
			name: "CRLF normalized to LF",
			in:   ": ping\r\n\r\ndata: {\"a\":1}\r\n\r\n",
			want: "data: {\"a\":1}\n\n",
		},
		{
			name: "partial event at EOF stays dropped",
			in:   "data: {\"a\":1}\n\ndata: {\"a\":",
			want: "data: {\"a\":1}\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got bytes.Buffer
			if err := filterSSEEvents(strings.NewReader(tt.in), &got); err != nil {
				t.Fatalf("filterSSEEvents: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("filtered = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

// TestSSEFilterTransportNonSSEPassthrough: non-event-stream bodies — including
// the JSON error payloads the SDK parses for apierror — must pass this filter
// untouched, even when they contain comment-looking or data-looking lines.
// (Error payloads are normalized, if needed, by the separate transport this
// one is wrapped in; see api_error_body_test.go.)
func TestSSEFilterTransportNonSSEPassthrough(t *testing.T) {
	t.Parallel()
	const body = ": looks like a comment\n and data: {not sse}"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	res, err := (&sseFilterTransport{base: http.DefaultTransport}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if string(got) != body {
		t.Fatalf("non-SSE body = %q, want unchanged %q", got, body)
	}
}

// TestSSEFilterTransportFiltersSSEBody: at the RoundTrip level, SSE bodies
// come out with keep-alive frames removed.
func TestSSEFilterTransportFiltersSSEBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": ping\n\ndata: {\"a\":1}\n\n"))
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	res, err := (&sseFilterTransport{base: http.DefaultTransport}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if want := "data: {\"a\":1}\n\n"; string(got) != want {
		t.Fatalf("filtered body = %q, want %q", got, want)
	}
}
