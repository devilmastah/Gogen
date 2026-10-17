package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// openCodeKwargsRejection is the body the OpenCode Go gateway returns when an
// upstream refuses the chat_template_kwargs field (observed live: the whole
// request fails with a 400, the turn never starts).
const openCodeKwargsRejection = `{"param":"chat_template_kwargs","type":"invalid_request_error",` +
	`"message":"Upstream request failed: [unsupported_parameter] Unrecognized request argument supplied: chat_template_kwargs."}`

// newExtrasTestProvider builds a provider whose default profile points at
// baseURL (so defaultBaseURL() is non-empty and forced mode "on" applies the
// llama.cpp extras) and whose stream client is the SSE client used by the
// other provider tests.
func newExtrasTestProvider(baseURL string) *OpenAIProvider {
	c := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey("test"),
		option.WithHTTPClient(newSSEHTTPClient()),
		option.WithMaxRetries(0),
	)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{name: "default", baseURL: baseURL, stream: &c}},
		model:    "test-model",
	}
	p.SetPreserveReasoningMode("on")
	return p
}

// rejectingExtrasServer refuses any request carrying chat_template_kwargs the
// way a strict gateway does, and serves a valid completion (SSE or JSON)
// otherwise. It counts both outcomes.
func rejectingExtrasServer(t *testing.T, rejects, accepted *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("chat_template_kwargs")) {
			rejects.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, openCodeKwargsRejection)
			return
		}
		accepted.Add(1)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"test-model",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGenerateResponseRetriesWithoutRejectedExtras pins the strict-gateway
// recovery: a 400 naming chat_template_kwargs as an unrecognized request
// argument must not fail the turn on an optional prompt-cache parameter. The
// round is re-issued without the extras, and the endpoint's refusal is
// remembered so the NEXT round never sends it again.
func TestGenerateResponseRetriesWithoutRejectedExtras(t *testing.T) {
	t.Parallel()

	var rejects, accepted atomic.Int32
	srv := rejectingExtrasServer(t, &rejects, &accepted)
	p := newExtrasTestProvider(srv.URL + "/v1")

	resp, err := p.GenerateResponse(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("GenerateResponse: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
	if rejects.Load() != 1 {
		t.Fatalf("rejected requests = %d, want 1", rejects.Load())
	}
	if accepted.Load() != 1 {
		t.Fatalf("accepted requests = %d, want 1", accepted.Load())
	}

	// The refusal is a completed capability answer: the next round omits the
	// parameter outright (forced mode "on" included).
	if _, err := p.GenerateResponse(context.Background(), []Message{{Role: "user", Content: "again"}}, nil, nil); err != nil {
		t.Fatalf("second GenerateResponse: %v", err)
	}
	if rejects.Load() != 1 {
		t.Fatalf("second round re-sent chat_template_kwargs (rejections = %d)", rejects.Load())
	}
	if accepted.Load() != 2 {
		t.Fatalf("accepted requests = %d, want 2", accepted.Load())
	}
	params := openai.ChatCompletionNewParams{Model: "test-model"}
	if p.applyChatCompletionExtras(context.Background(), &params) {
		t.Fatal("forced mode still applied the rejected extras")
	}
}

// TestGenerateResponseStreamRetriesWithoutRejectedExtras covers the streaming
// path: the refusal arrives on the streaming POST before any output, so the
// turn is re-issued once without the extras and delivers live.
func TestGenerateResponseStreamRetriesWithoutRejectedExtras(t *testing.T) {
	t.Parallel()

	var rejects, accepted atomic.Int32
	srv := rejectingExtrasServer(t, &rejects, &accepted)
	p := newExtrasTestProvider(srv.URL + "/v1")

	var got strings.Builder
	h := &StreamHandlers{OnToken: func(s string) { got.WriteString(s) }}
	res, err := p.GenerateResponseStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, h)
	if err != nil {
		t.Fatalf("GenerateResponseStream: %v", err)
	}
	if res.Content != "ok" || got.String() != "ok" {
		t.Fatalf("content = %q (streamed %q), want ok", res.Content, got.String())
	}
	if rejects.Load() != 1 || accepted.Load() != 1 {
		t.Fatalf("rejected = %d, accepted = %d; want 1/1", rejects.Load(), accepted.Load())
	}
}

// TestChatTemplateKwargsRejectedClassification pins the detector: only a
// 400/422 whose body names the parameter is a refusal — a 401 or a 400 about
// something else must keep the existing behaviour (no reparsing of the turn).
func TestChatTemplateKwargsRejectedClassification(t *testing.T) {
	t.Parallel()

	oerr := func(status int, body string) error {
		var e openai.Error
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatalf("decode synthetic error body: %v", err)
		}
		e.StatusCode = status
		return &e
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "opencode 400", err: oerr(http.StatusBadRequest, openCodeKwargsRejection), want: true},
		{
			name: "422 unsupported",
			err:  oerr(http.StatusUnprocessableEntity, `{"message":"unknown parameter chat_template_kwargs"}`),
			want: true,
		},
		{
			name: "param attribute",
			err:  oerr(http.StatusBadRequest, `{"param":"chat_template_kwargs","message":"invalid request"}`),
			want: true,
		},
		{name: "401 auth", err: oerr(http.StatusUnauthorized, openCodeKwargsRejection), want: false},
		{
			name: "400 other param",
			err:  oerr(http.StatusBadRequest, `{"message":"unknown parameter reasoning_effort"}`),
			want: false,
		},
		{name: "nil", err: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatTemplateKwargsRejected(tc.err); got != tc.want {
				t.Fatalf("chatTemplateKwargsRejected = %v, want %v", got, tc.want)
			}
		})
	}
}
