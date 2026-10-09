package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Regression tests for the reported stream failure:
//
//	stream error: json: cannot unmarshal string into Go value of type apierror.Error;
//	streaming retry also failed: ... (non-streaming fallback also failed: ...)
//
// openai-go parses a >=400 body by unmarshalling its top-level "error" member
// into apierror.Error, so an error member that is a JSON string (Ollama-style
// {"error":"model not found"}) fails the decode, replacing the provider's
// message with a Go type error — and, because that error is not an
// *openai.Error, the recovery ladder treats it as a transient transport drop
// and burns every rung on a definitive 4xx. apiErrorBodyTransport
// (api_error_body.go) normalizes those payloads before the SDK sees them.

// TestNormalizeAPIErrorBody pins the per-shape rewrite rules. An empty want
// means "the body must pass through byte-for-byte".
func TestNormalizeAPIErrorBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "OpenAI error object untouched",
			body: `{"error":{"code":"invalid_api_key","message":"Incorrect API key","param":null,"type":"invalid_request_error"}}`,
			want: "",
		},
		{
			name: "OpenAI error object with empty message untouched",
			body: `{"error":{"code":"x"}}`,
			want: "",
		},
		{
			name: "string error member rewritten",
			body: `{"error":"model not found"}`,
			want: `{"error":{"message":"model not found"}}`,
		},
		{
			name: "empty string error member kept as text",
			body: `{"error":""}`,
			want: `{"error":{"message":"{\"error\":\"\"}"}}`,
		},
		{
			name: "number error member rewritten",
			body: `{"error":404}`,
			want: `{"error":{"message":"404"}}`,
		},
		{
			name: "bool error member rewritten",
			body: `{"error":true}`,
			want: `{"error":{"message":"true"}}`,
		},
		{
			name: "array error member rewritten",
			body: `{"error":["a","b"]}`,
			want: `{"error":{"message":"[\"a\",\"b\"]"}}`,
		},
		{
			name: "null error member falls back to the raw body",
			body: `{"error":null}`,
			want: `{"error":{"message":"{\"error\":null}"}}`,
		},
		{
			name: "unexpected sibling member type does not defeat the error member",
			body: `{"error":"boom","message":123}`,
			want: `{"error":{"message":"boom"}}`,
		},
		{
			name: "vLLM message envelope",
			body: `{"object":"error","message":"The model does not exist.","type":"NotFoundError","code":404}`,
			want: `{"error":{"message":"The model does not exist."}}`,
		},
		{
			name: "FastAPI detail envelope",
			body: `{"detail":"Not authenticated"}`,
			want: `{"error":{"message":"Not authenticated"}}`,
		},
		{
			name: "bare JSON string body",
			body: `"model not found"`,
			want: `{"error":{"message":"model not found"}}`,
		},
		{
			name: "plain text body",
			body: `upstream unavailable`,
			want: `{"error":{"message":"upstream unavailable"}}`,
		},
		{
			name: "html escaping disabled",
			body: `<html><body>502 Bad Gateway</body></html>`,
			want: `{"error":{"message":"<html><body>502 Bad Gateway</body></html>"}}`,
		},
		{
			name: "unrecognized JSON envelope kept whole",
			body: `{"code":500}`,
			want: `{"error":{"message":"{\"code\":500}"}}`,
		},
		{
			name: "surrounding whitespace trimmed",
			body: "  \n {\"error\":\"boom\"} \n",
			want: `{"error":{"message":"boom"}}`,
		},
		{
			name: "empty body untouched",
			body: ``,
			want: "",
		},
		{
			name: "whitespace-only body untouched",
			body: " \n\t ",
			want: "",
		},
		{
			name: "non-UTF-8 body untouched",
			body: "\xff\xfe\x00binary",
			want: "",
		},
		{
			name: "NUL-containing body untouched",
			body: "text\x00with NUL",
			want: "",
		},
		{
			name: "long message clipped",
			body: `"` + strings.Repeat("x", 2000) + `"`,
			want: `{"error":{"message":"` + strings.Repeat("x", apiErrorMaxMessageBytes) + `… (truncated)"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeAPIErrorBody([]byte(tt.body))
			if tt.want == "" {
				if ok {
					t.Fatalf("normalizeAPIErrorBody(%q) = %q, want the body untouched", tt.body, got)
				}
				return
			}
			if !ok {
				t.Fatalf("normalizeAPIErrorBody(%q) = untouched, want %q", tt.body, tt.want)
			}
			if string(got) != tt.want {
				t.Fatalf("normalizeAPIErrorBody(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

// TestClipAPIErrorMessageKeepsRunesWhole pins the rune-boundary clip: a
// multi-byte message must never be cut mid-rune (the truncated message is
// re-serialized as JSON and rendered in the UI).
func TestClipAPIErrorMessageKeepsRunesWhole(t *testing.T) {
	// 400 three-byte runes = 1200 bytes; the 1024-byte cut lands mid-rune.
	got := clipAPIErrorMessage(strings.Repeat("世", 400))
	if !strings.HasSuffix(got, "… (truncated)") {
		t.Fatalf("clipAPIErrorMessage = %q, want the (truncated) marker", got)
	}
	body := strings.TrimSuffix(got, "… (truncated)")
	if !utf8.ValidString(body) {
		t.Fatalf("clipped message is not valid UTF-8: %q", body)
	}
	if !strings.HasSuffix(body, "世") {
		t.Fatalf("clipped message ends mid-rune: %q", body)
	}
}

// TestAPIErrorBodyTransportRoundTrip pins the transport-level contract: a
// >=400 JSON body is rewritten, sub-400 responses and compressed bodies are
// not.
func TestAPIErrorBodyTransportRoundTrip(t *testing.T) {
	newServer := func(t *testing.T, status int, headers map[string]string, body string) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	roundTrip := func(t *testing.T, url string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		// DisableCompression: a server that compressed an error body would
		// otherwise be decoded by the stdlib (removing Content-Encoding)
		// before this transport sees it — the SSE client's setup.
		res, err := (&apiErrorBodyTransport{base: &http.Transport{DisableCompression: true}}).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}

	t.Run("error body rewritten and content length updated", func(t *testing.T) {
		srv := newServer(t, http.StatusBadRequest, map[string]string{"Content-Type": "application/json"},
			`{"error":"model not found"}`)
		res := roundTrip(t, srv.URL)
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"error":{"message":"model not found"}}`
		if string(body) != want {
			t.Fatalf("body = %q, want %q", body, want)
		}
		if res.ContentLength != int64(len(want)) {
			t.Fatalf("ContentLength = %d, want %d", res.ContentLength, len(want))
		}
		if got := res.Header.Get("Content-Length"); got != "" && got != strconv.Itoa(len(want)) {
			t.Fatalf("Content-Length header = %q, want %d (kept consistent with the rewritten body)", got, len(want))
		}
	})

	t.Run("sub-400 body untouched", func(t *testing.T) {
		srv := newServer(t, http.StatusOK, map[string]string{"Content-Type": "application/json"},
			`{"error":"model not found"}`)
		res := roundTrip(t, srv.URL)
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"error":"model not found"}` {
			t.Fatalf("body = %q, want the 200 body untouched", body)
		}
	})

	t.Run("compressed error body left encoded", func(t *testing.T) {
		// A Content-Encoding this layer cannot decode: reading the body here
		// would corrupt it, so the transport must hand the raw stream on.
		srv := newServer(t, http.StatusBadGateway,
			map[string]string{"Content-Type": "text/plain", "Content-Encoding": "br"},
			`not-brotli-but-must-survive`)
		res := roundTrip(t, srv.URL)
		if got := res.Header.Get("Content-Encoding"); got != "br" {
			t.Fatalf("Content-Encoding = %q, want br preserved", got)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "not-brotli-but-must-survive" {
			t.Fatalf("body = %q, want the encoded bytes untouched", body)
		}
	})

	t.Run("oversize error body streams through intact", func(t *testing.T) {
		// Past the cap the body is not buffered for inspection: the head
		// already read must be spliced back in front of the remainder so
		// the SDK sees the body byte-for-byte (never a rewrite, never a
		// truncation at the cap).
		big := `{"error":"` + strings.Repeat("x", apiErrorMaxBodyBytes) + `"}`
		srv := newServer(t, http.StatusBadGateway, map[string]string{"Content-Type": "application/json"}, big)
		res := roundTrip(t, srv.URL)
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != big {
			t.Fatalf("oversize body = %d bytes, want the %d-byte original untouched", len(body), len(big))
		}
	})
}

// TestGenerateResponseSurfacesStringErrorPayload pins the reported failure
// end-to-end on the non-streaming path: the provider's message must survive
// the SDK's apierror parse, with the HTTP status still classified.
func TestGenerateResponseSurfacesStringErrorPayload(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	_, err := p.GenerateResponse(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("err = %v, want the provider's message", err)
	}
	if strings.Contains(err.Error(), "cannot unmarshal string") {
		t.Fatalf("err = %v, want no apierror decode failure", err)
	}
	var oerr *openai.Error
	if !errors.As(err, &oerr) || oerr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want an *openai.Error with status 400", err)
	}
}

// TestStreamStringErrorPayloadSkipsDecodeLadder pins the streaming-path
// consequence: with the payload normalized, a definitive 400 is no longer
// mistaken for a transient stream drop, so the streaming retry is skipped
// (previously the decode error made it retryable and the ladder ran in full).
// The non-streaming fallback still runs, exactly as for any other 4xx.
func TestStreamStringErrorPayloadSkipsDecodeLadder(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"stream":true`) {
			streamRequests.Add(1)
		} else {
			nonStreamRequests.Add(1)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	_, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil, nil, nil,
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("err = %v, want the provider's message", err)
	}
	if got := streamRequests.Load(); got != 1 {
		t.Fatalf("stream requests = %d, want 1 (a definitive 400 must not get the streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 1 {
		t.Fatalf("non-stream requests = %d, want 1 (the fallback still runs)", got)
	}
}

// TestStringErrorPayloadContextWindowClassification pins the interaction with
// error-text classification: the canonical context-window phrasings reach
// IsContextWindowError only once the payload is normalized, so a provider
// that reports the refusal as a bare string now takes the agent loop's
// compaction recovery instead of an opaque decode error.
func TestStringErrorPayloadContextWindowClassification(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"This model's maximum context length is 8192 tokens"}`)
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	_, err := p.GenerateResponse(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrContextWindowExceeded) {
		t.Fatalf("err = %v, want it classified as ErrContextWindowExceeded", err)
	}
}

// TestListModelsSurfacesStringErrorPayload covers the catalog client's wiring
// of the same transport (newClientPair builds it through
// newCatalogHTTPClient): a 401 body with a string error member must surface
// the provider's message instead of a decode error.
func TestListModelsSurfacesStringErrorPayload(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid api key"})
	}))
	defer srv.Close()

	_, err := listModelsFromServer(t, srv.URL)
	if err == nil {
		t.Fatal("expected a catalog fetch error")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("err = %v, want the provider's message", err)
	}
}
