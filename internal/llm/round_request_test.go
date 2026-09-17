package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3"
)

// roundRequestTestMessages returns a history exercising every message shape the
// wire conversion handles: plain user text, an assistant turn with reasoning and
// a tool call, a tool result, and a vision message.
func roundRequestTestMessages() []Message {
	return []Message{
		{Role: "system", Content: "you are a test agent"},
		{Role: "user", Content: "look at this", Images: []ImageInput{{DataURL: "data:image/png;base64,AAAA", Detail: "low"}}},
		{
			Role:      "assistant",
			Content:   "checking",
			Reasoning: "let me think",
			ToolCalls: []ToolCall{{
				ID:      "call_1",
				Name:    "read_file",
				Args:    map[string]any{"path": "a.go"},
				ArgsStr: `{"path":"a.go"}`,
			}},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "package main"},
	}
}

func roundRequestTestTools() []Tool {
	return []Tool{
		{
			Type:        "function",
			Name:        "read_file",
			Description: "read a file",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			},
		},
	}
}

// TestRoundRequestBodyMatchesSDKSerialization pins that the pre-serialized
// bodies are exactly what the SDK would have serialized from the equivalent
// params: the streaming variant adds StreamOptions.include_usage and nothing
// else, and the non-streaming variant is the same params without it. Comparing
// decoded JSON is deliberate — the property under test is "same document", not
// byte order.
func TestRoundRequestBodyMatchesSDKSerialization(t *testing.T) {
	t.Parallel()
	p := newTestOpenAIProvider(httptest.NewServer(http.NotFoundHandler()))
	msgs := roundRequestTestMessages()
	tools := roundRequestTestTools()

	req := p.newRoundRequest(t.Context(), msgs, nil, tools)

	streamBody, err := req.streaming()
	if err != nil {
		t.Fatalf("streaming() error: %v", err)
	}
	fallbackBody, err := req.nonStreaming()
	if err != nil {
		t.Fatalf("nonStreaming() error: %v", err)
	}

	// Reference: the params the attempt functions used to build, serialized by
	// the SDK's own MarshalJSON.
	ref := openai.ChatCompletionNewParams{
		Messages:      p.messagesToChat(msgs),
		Tools:         toolsToOpenAI(tools, nil),
		Model:         p.currentModel(),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	refStream, err := ref.MarshalJSON()
	if err != nil {
		t.Fatalf("reference stream MarshalJSON: %v", err)
	}
	ref.StreamOptions = openai.ChatCompletionStreamOptionsParam{}
	refNonStream, err := ref.MarshalJSON()
	if err != nil {
		t.Fatalf("reference non-stream MarshalJSON: %v", err)
	}

	if !reflect.DeepEqual(decodeJSONMap(t, streamBody), decodeJSONMap(t, refStream)) {
		t.Fatalf("streaming body differs from the SDK serialization of the equivalent params:\n got %s\nwant %s",
			streamBody, refStream)
	}
	if !reflect.DeepEqual(decodeJSONMap(t, fallbackBody), decodeJSONMap(t, refNonStream)) {
		t.Fatalf("fallback body differs from the SDK serialization of the equivalent params:\n got %s\nwant %s",
			fallbackBody, refNonStream)
	}

	// The stream flag is NOT part of the cached body: NewStreaming adds it to
	// the serialized body itself, and baking it in here would send a streaming
	// body on the non-streaming fallback.
	if strings.Contains(string(streamBody), `"stream"`) {
		t.Fatalf("streaming body must not carry its own stream flag (the SDK adds it): %s", streamBody)
	}
	if strings.Contains(string(fallbackBody), `"stream_options"`) {
		t.Fatalf("fallback body must not carry stream_options: %s", fallbackBody)
	}
}

// TestRoundRequestBodySerializedOnce pins the memoization: the retry and the
// fallback must reuse the bytes already built for the round instead of
// re-encoding the whole conversation per attempt.
func TestRoundRequestBodySerializedOnce(t *testing.T) {
	t.Parallel()
	p := newTestOpenAIProvider(httptest.NewServer(http.NotFoundHandler()))
	req := p.newRoundRequest(t.Context(), roundRequestTestMessages(), nil, roundRequestTestTools())

	first, err := req.streaming()
	if err != nil {
		t.Fatal(err)
	}
	second, err := req.streaming()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 || &first[0] != &second[0] {
		t.Fatal("streaming body was re-serialized: the second call returned different bytes")
	}

	firstFallback, err := req.nonStreaming()
	if err != nil {
		t.Fatal(err)
	}
	secondFallback, err := req.nonStreaming()
	if err != nil {
		t.Fatal(err)
	}
	if len(firstFallback) == 0 || &firstFallback[0] != &secondFallback[0] {
		t.Fatal("fallback body was re-serialized: the second call returned different bytes")
	}
	if &first[0] == &firstFallback[0] {
		t.Fatal("the streaming and non-streaming variants must not share bytes (stream_options differ)")
	}
}

// TestStreamAttemptsSendIdenticalBody drives the real recovery ladder through
// an HTTP server: the first streaming attempt is interrupted by a malformed SSE
// frame, the muted retry recovers. Both requests must carry byte-identical
// bodies — the retry is the same turn, and re-encoding it per attempt is the
// duplicated work this change removes.
func TestStreamAttemptsSendIdenticalBody(t *testing.T) {
	var streamRequests atomic.Int32
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("non-streaming request on the streaming path: %s", body)
			return
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		if streamRequests.Add(1) == 1 {
			// Fallback-eligible transient failure after partial output.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial recovered"}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		roundRequestTestMessages(),
		nil,
		roundRequestTestTools(),
		&StreamHandlers{OnToken: func(string) {}},
	)
	if err != nil {
		t.Fatalf("GenerateResponseStream: %v", err)
	}
	if result.Content != "partial recovered" {
		t.Fatalf("result.Content = %q", result.Content)
	}
	if len(bodies) != 2 {
		t.Fatalf("streaming requests = %d, want 2 (attempt + muted retry)", len(bodies))
	}
	if string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("retry body differs from the first attempt's:\nfirst %s\nsecond %s", bodies[0], bodies[1])
	}
	// The turn's content must actually be on the wire (a placeholder leaking
	// through would show up as an empty messages array).
	for _, want := range []string{`"model":"test-model"`, "package main", "read_file", "data:image/png"} {
		if !strings.Contains(string(bodies[0]), want) {
			t.Fatalf("request body missing %q: %s", want, bodies[0])
		}
	}
}

// TestStreamFallbackSendsNonStreamVariantOfRoundBody drives the full ladder
// (stream fails, muted retry fails, non-streaming fallback recovers) and pins
// that the fallback request carries the round's conversation in its
// non-streaming shape: same messages/model/tools, no stream flag, no
// stream_options.
func TestStreamFallbackSendsNonStreamVariantOfRoundBody(t *testing.T) {
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamBodies, nonStreamBodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamBodies = append(streamBodies, body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		nonStreamBodies = append(nonStreamBodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"partial fallback answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		roundRequestTestMessages(),
		nil,
		roundRequestTestTools(),
		&StreamHandlers{OnToken: func(string) {}},
	)
	if err != nil {
		t.Fatalf("GenerateResponseStream: %v", err)
	}
	if result.Content != "partial fallback answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
	if len(streamBodies) != 2 {
		t.Fatalf("streaming requests = %d, want 2", len(streamBodies))
	}
	if len(nonStreamBodies) != 1 {
		t.Fatalf("non-streaming requests = %d, want 1", len(nonStreamBodies))
	}

	streamDoc := decodeJSONMap(t, streamBodies[0])
	fallbackDoc := decodeJSONMap(t, nonStreamBodies[0])
	if _, ok := fallbackDoc["stream"]; ok {
		t.Fatalf("fallback body must not carry a stream flag: %s", nonStreamBodies[0])
	}
	if _, ok := fallbackDoc["stream_options"]; ok {
		t.Fatalf("fallback body must not carry stream_options: %s", nonStreamBodies[0])
	}
	for _, key := range []string{"model", "messages", "tools"} {
		if !reflect.DeepEqual(streamDoc[key], fallbackDoc[key]) {
			t.Fatalf("fallback %q differs from the streaming request's:\nstream   %v\nfallback %v",
				key, streamDoc[key], fallbackDoc[key])
		}
	}
}

// TestGenerateResponseBodyMatchesWirePins the direct (non-streaming) entry
// point too: GenerateResponse must send the same document it always did.
func TestGenerateResponseBodyMatchesWire(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	msgs := roundRequestTestMessages()
	tools := roundRequestTestTools()
	resp, err := p.GenerateResponse(t.Context(), msgs, nil, tools)
	if err != nil {
		t.Fatalf("GenerateResponse: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("resp.Content = %q", resp.Content)
	}

	ref := openai.ChatCompletionNewParams{
		Messages: p.messagesToChat(msgs),
		Tools:    toolsToOpenAI(tools, nil),
		Model:    p.currentModel(),
	}
	refBody, err := ref.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodeJSONMap(t, got), decodeJSONMap(t, refBody)) {
		t.Fatalf("wire body differs from the SDK serialization:\n got %s\nwant %s", got, refBody)
	}
}

func decodeJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}
