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

func newTestOpenAIProvider(srv *httptest.Server, opts ...option.RequestOption) *OpenAIProvider {
	c := openai.NewClient(append([]option.RequestOption{
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithHTTPClient(newSSEHTTPClient()),
	}, opts...)...)
	// No profile baseURL: the direct-construction shape resolved
	// defaultBaseURL() to "", so /props probes and models.dev lookups stay
	// off; the stream client carries the endpoint itself.
	return &OpenAIProvider{
		profiles: []*providerProfile{{name: "default", stream: &c}},
		model:    "test-model",
	}
}

func TestGenerateResponseStreamThinkingKeepalive(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"step one"}}]}

data: {"choices":[{"index":0,"delta":{}}]}

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
	var content []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnThinkingToken: func(token string) { thinking = append(thinking, token) },
			OnToken:         func(token string) { content = append(content, token) },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(thinking, ""); got != "step one step two" {
		t.Fatalf("thinking = %q", got)
	}
	if got := strings.Join(content, ""); got != "answer" {
		t.Fatalf("content = %q", got)
	}
	if result.Content != "answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

func TestGenerateResponseStreamKeepsRefusalSeparate(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"refusal":"I cannot help with that."}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "" {
		t.Fatalf("Content should stay empty, got %q", result.Content)
	}
	if result.Refusal != "I cannot help with that." {
		t.Fatalf("Refusal = %q", result.Refusal)
	}
}

func TestGenerateResponseStreamKeepsReasoningOutOfContent(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"only thinking"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "" {
		t.Fatalf("Content should stay empty, got %q", result.Content)
	}
	if result.Reasoning != "only thinking" {
		t.Fatalf("Reasoning = %q", result.Reasoning)
	}
}

// TestGenerateResponseStreamReasoningStopGraceFires pins the bounded wait
// after a reasoning-only finish_reason="stop": a provider that sends the stop
// and then HOLDS the connection open (no [DONE], no more data, no close) must
// be treated as complete after reasoningStopGrace — returning the accumulated
// reasoning without a fallback re-request — instead of blocking for the full
// streamReadIdleTimeout. NOT parallel: it sets GOGEN_REASONING_STOP_GRACE.
func TestGenerateResponseStreamReasoningStopGraceFires(t *testing.T) {
	t.Setenv("GOGEN_REASONING_STOP_GRACE", "150ms")

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flushing")
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"reasoning_content":"only thinking"}}]}`+"\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fl.Flush()
		// Hold the connection open with no further data. The client's grace
		// timer closes the stream, which cancels this request context (the
		// handler returns so the test server can shut down). Bound the wait
		// in case the teardown does not propagate.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	start := time.Now()
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reasoning != "only thinking" {
		t.Fatalf("Reasoning = %q, want the accumulated reasoning", result.Reasoning)
	}
	if result.Content != "" {
		t.Fatalf("Content should stay empty, got %q", result.Content)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want the provider-reported stop", result.FinishReason)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("grace did not bound the wait: elapsed %v", elapsed)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want exactly 1 (the grace close must not trigger a fallback re-request)", got)
	}
}

// TestGenerateResponseStreamUsageDrainGraceFires pins the bounded post-finish
// usage wait: a provider that sends finish_reason, then [DONE], then HOLDS
// the connection open WITHOUT the usage chunk (llama-swap proxies and older
// llama.cpp builds — the connection is never closed, so the drain cannot end
// at EOF) must be treated as complete after streamDrainGrace — returning the
// accumulated content without a retry or fallback re-request — instead of
// blocking for the full streamReadIdleTimeout with the reply already
// rendered on screen. NOT parallel: it sets GOGEN_STREAM_DRAIN_GRACE.
func TestGenerateResponseStreamUsageDrainGraceFires(t *testing.T) {
	t.Setenv("GOGEN_STREAM_DRAIN_GRACE", "150ms")

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flushing")
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"all done"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
		// Hold the connection open: no usage chunk, no close. The drain
		// timer closes the stream, which cancels this request context (the
		// handler returns so the test server can shut down). Bound the wait
		// in case the teardown does not propagate.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	start := time.Now()
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnToken: func(string) {},
		},
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "all done" {
		t.Fatalf("Content = %q, want the accumulated reply", result.Content)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want the provider-reported stop", result.FinishReason)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("drain grace did not bound the wait: elapsed %v", elapsed)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want exactly 1 (the drain close must not trigger a retry or fallback re-request)", got)
	}
}

// TestGenerateResponseStreamUsageArrivingBeatsDrainGrace pins that a
// compliant endpoint (usage chunk delivered right after finish) is not
// delayed by the drain bound: the loop breaks on the usage chunk before the
// timer can matter. NOT parallel: it sets GOGEN_STREAM_DRAIN_GRACE.
func TestGenerateResponseStreamUsageArrivingBeatsDrainGrace(t *testing.T) {
	t.Setenv("GOGEN_STREAM_DRAIN_GRACE", "150ms")

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"answer"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	start := time.Now()
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnToken: func(string) {},
		},
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "answer" {
		t.Fatalf("Content = %q", result.Content)
	}
	if result.Usage == nil || result.Usage.TotalTokens != 2 {
		t.Fatalf("Usage = %+v, want the delivered usage", result.Usage)
	}
	if elapsed > time.Second {
		t.Fatalf("usage path must not wait out the drain grace: elapsed %v", elapsed)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want exactly 1", got)
	}
}

// TestGenerateResponseStreamStallSignal pins the "still waiting" signal: a
// stretch without SSE chunks (long prefill, stalled upstream) fires
// OnStreamStall while the round is live, and a stream with continuous
// chunks never fires it. The signal is informational — the stream completes
// either way and content is unaffected. NOT parallel: it sets
// GOGEN_STREAM_STALL.
func TestGenerateResponseStreamStallSignal(t *testing.T) {
	// A generous threshold keeps the "continuous chunks" phase deterministic
	// on a loaded CI runner (it shares cores across every package's -race
	// test binary): the chunk cadence sits 50x below the window and the
	// stream outlasts it, so a watchdog tick must land mid-stream and still
	// find the clock freshly reset. A tight window flakes when a 10ms sleep
	// stretches under CPU contention. The silent phase is held open until
	// the signal fires: a fixed sleep races the watchdog ticker, and a tick
	// delivered after the next chunk resets the clock counts as no stall.
	const (
		stallAfter = 500 * time.Millisecond
		chunkGap   = stallAfter / 50 // 10ms between chunks
		chunkCount = 60              // ~600ms of streaming, past the threshold
	)
	t.Setenv("GOGEN_STREAM_STALL", stallAfter.String())

	var phase atomic.Int32 // 0 = silent-gap phase, 1 = continuous phase
	// One counter per phase: the silent phase's watchdog goroutine may still
	// be winding down when the continuous phase starts (its select can pick a
	// pending ticker tick while racing the closed stop channel), and a
	// straggler must not be read as a continuous-phase fire.
	var silentStalls, continuousStalls atomic.Int32
	// held keeps the silent phase open until the test has seen the stall
	// (or given up). Closing it lets the rest of the stream through.
	held := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if phase.Load() == 0 {
			// One chunk, then silence until the test releases the gap.
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"one "}}]}`+"\n\n")
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-held:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"two"}}]}`+"\n\n"+
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
			return
		}
		// Continuous chunks: each gap is a fraction of the threshold and the
		// total outlasts it, so at least one watchdog tick lands mid-stream
		// and must find the clock reset.
		for i := 0; i < chunkCount; i++ {
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(chunkGap)
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)

	// Phase A: the silent gap must produce at least one stall signal and a
	// complete stream (the signal never interrupts). The call blocks in the
	// handler until held is closed, so the wait below observes the signal
	// while the round is still live.
	type streamOut struct {
		res *StreamResult
		err error
	}
	done := make(chan streamOut, 1)
	go func() {
		res, err := p.GenerateResponseStream(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, nil,
			&StreamHandlers{OnStreamStall: func() { silentStalls.Add(1) }})
		done <- streamOut{res, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for silentStalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	close(held)
	var out streamOut
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not finish after the stall gap")
	}
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.res.Content != "one two" {
		t.Fatalf("content = %q, want %q", out.res.Content, "one two")
	}
	if n := silentStalls.Load(); n < 1 {
		t.Fatalf("OnStreamStall fired %d times while the stream was held silent (threshold %v), want >= 1", n, stallAfter)
	}

	// Phase B: continuous chunks — no stall signal.
	phase.Store(1)
	if _, err := p.GenerateResponseStream(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, nil,
		&StreamHandlers{OnStreamStall: func() { continuousStalls.Add(1) }}); err != nil {
		t.Fatal(err)
	}
	if n := continuousStalls.Load(); n != 0 {
		t.Fatalf("OnStreamStall fired %d times over %v of continuous chunks (threshold %v), want 0", n, chunkCount*chunkGap, stallAfter)
	}
}

func TestGenerateResponseStreamIgnoresSpuriousStopDuringReasoning(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"step one"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

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
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(thinking, ""); got != "step one step two" {
		t.Fatalf("thinking = %q", got)
	}
	if result.Content != "answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

func TestGenerateResponseStreamTerminalToolSignal(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{}"}}]}}]}

data: {"choices":[{"index":0,"delta":{}}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "read_file" {
		t.Fatalf("toolCalls = %#v", result.ToolCalls)
	}
}

// TestGenerateResponseStreamNamelessToolCallInfersStop pins the finish-reason
// inference against the built tool-call list rather than the raw accumulator
// count. An arguments-only delta that never carries a tool name accumulates a
// tcAccum (so the old `len(a.tcAccums) > 0` check fired "tool_calls"), but
// buildResult drops nameless accumulators from ToolCalls — reporting a tool
// round with zero calls, which the caller cannot execute. The inferred reason
// must be "stop".
func TestGenerateResponseStreamNamelessToolCallInfersStop(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]}}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 0 {
		t.Fatalf("toolCalls = %#v, want none (nameless accumulator must be dropped)", result.ToolCalls)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want %q (no usable tool calls)", result.FinishReason, "stop")
	}
	if result.Content != "answer" {
		t.Fatalf("Content = %q, want %q", result.Content, "answer")
	}
}

// TestGenerateResponseStreamIgnoresSpuriousStopOnReasoningChunk is the
// regression test for the real-world failure: llama.cpp emits a spurious
// finish_reason:"stop" on a chunk that ALSO carries a reasoning_content
// token. The old guard's `!deltaIsEmptyDelta(delta)` clause treated that as
// real content and terminated mid-reasoning, discarding the rest of the
// stream (more reasoning + the actual answer).
func TestGenerateResponseStreamIgnoresSpuriousStopOnReasoningChunk(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"step one"}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":" step two"},"finish_reason":"stop"}]}

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
	var content []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnThinkingToken: func(token string) { thinking = append(thinking, token) },
			OnToken:         func(token string) { content = append(content, token) },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(thinking, ""); got != "step one step two" {
		t.Fatalf("thinking = %q", got)
	}
	if got := strings.Join(content, ""); got != "answer" {
		t.Fatalf("content = %q", got)
	}
	if result.Content != "answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

// TestGenerateResponseStreamTerminalToolSignalIgnoresKeepaliveBetweenArgs
// verifies that an empty {} keepalive chunk arriving between tool-argument
// fragments does NOT terminate the stream. The terminal-tool-signal branch
// must require the accumulated args to be complete JSON.
func TestGenerateResponseStreamTerminalToolSignalIgnoresKeepaliveBetweenArgs(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}

data: {"choices":[{"index":0,"delta":{}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{}}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("toolCalls = %#v", result.ToolCalls)
	}
	tc := result.ToolCalls[0]
	if tc.Name != "read_file" {
		t.Fatalf("name = %q", tc.Name)
	}
	if tc.Args["path"] != "a.go" {
		t.Fatalf("args = %#v", tc.Args)
	}
}

// TestGenerateResponseStreamCapturesReportedModel verifies the model ID the
// provider reports on the stream chunks (router endpoints such as OpenCode
// Zen resolve aliases server-side) is surfaced on the StreamResult.
func TestGenerateResponseStreamCapturesReportedModel(t *testing.T) {
	t.Parallel()
	const sse = `data: {"model":"glm-4.6","choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "glm-4.6" {
		t.Fatalf("result.Model = %q, want %q", result.Model, "glm-4.6")
	}
	if result.Content != "answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

// TestGenerateResponseStreamNoReportedModelIsEmpty verifies a stream whose
// chunks carry no model field yields an empty StreamResult.Model.
func TestGenerateResponseStreamNoReportedModelIsEmpty(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "" {
		t.Fatalf("result.Model = %q, want empty", result.Model)
	}
}

// TestStreamFallbackDefersRoundEndCallbacks pins the single-source-of-truth
// contract for round-end callbacks: when a failed stream is recovered via the
// non-streaming fallback, the LLM layer must NOT fire OnStreamEnd /
// OnRecoverPartialStream — the agent loop fires them exactly once when it
// finalizes the round. Firing them here too (the pre-fix behavior) delivered
// duplicate stream_end frames to the web client and duplicate
// streamRoundEndMsg events to the TUI on every stream failure.
func TestStreamFallbackDefersRoundEndCallbacks(t *testing.T) {
	t.Parallel()
	var streamed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamed = true
			w.Header().Set("Content-Type", "text/event-stream")
			// One valid chunk, then malformed SSE data: the stream errors
			// mid-way, forcing the non-streaming fallback.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"recovered answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var ends, recovers int
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnToken:                func(string) {},
			OnStreamEnd:            func() { ends++ },
			OnRecoverPartialStream: func() { recovers++ },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !streamed {
		t.Fatal("test setup: stream request never reached the server")
	}
	if ends != 0 || recovers != 0 {
		t.Fatalf("fallback fired round-end callbacks (OnStreamEnd=%d, OnRecoverPartialStream=%d); the agent loop owns these", ends, recovers)
	}
	if result.Content != "recovered answer" {
		t.Fatalf("fallback content = %q, want %q", result.Content, "recovered answer")
	}
	if !result.PartialStream {
		t.Fatal("PartialStream must be true: the failed stream produced partial content")
	}
}

// TestStreamFallbackTrimsAlreadyStreamedPrefix verifies the fallback does not
// re-render text the client already saw from the failed stream: when the
// recovered response starts with exactly the streamed prefix, only the suffix
// is emitted via OnToken, while the persisted StreamResult keeps the complete
// recovery.
func TestStreamFallbackTrimsAlreadyStreamedPrefix(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			// One valid chunk, then malformed SSE: the stream errors mid-way,
			// forcing the non-streaming fallback.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"partial recovered answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var tokens []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnToken: func(token string) { tokens = append(tokens, token) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	// The streamed prefix fires OnToken once, and the fallback re-renders
	// only the suffix beyond it — the recovered text must not be emitted in
	// full on top of what the client already rendered.
	if len(tokens) != 2 || tokens[0] != "partial " || tokens[1] != "recovered answer" {
		t.Fatalf("OnToken fired %v, want [partial  recovered answer] (streamed prefix + trimmed suffix only)", tokens)
	}
	if result.Content != "partial recovered answer" {
		t.Fatalf("result.Content = %q, want %q (persisted result stays complete)", result.Content, "partial recovered answer")
	}
}

// TestStreamFallbackNoDuplicateWhenRecoveredEqualsStreamed verifies that when
// the recovered response is byte-identical to what the stream already
// emitted, nothing is re-rendered (there is nothing new to show).
func TestStreamFallbackNoDuplicateWhenRecoveredEqualsStreamed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"exact answer"}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"exact answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var tokens []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnToken: func(token string) { tokens = append(tokens, token) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0] != "exact answer" {
		t.Fatalf("OnToken fired %v, want only the streamed [exact answer] (fallback must emit nothing new)", tokens)
	}
	if result.Content != "exact answer" {
		t.Fatalf("result.Content = %q", result.Content)
	}
}

// TestStreamRetryRecoversOnStreamingPath pins the recovery ladder: a
// transient mid-stream failure is retried ONCE on the streaming path before
// the non-streaming fallback is considered. When the retry succeeds the
// fallback never runs, the client receives only the suffix beyond the
// already-rendered prefix (same trim contract as the fallback), and the
// round-end callbacks stay with the agent loop.
func TestStreamRetryRecoversOnStreamingPath(t *testing.T) {
	t.Parallel()
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			if streamRequests.Add(1) == 1 {
				// Transient failure: one valid chunk, then malformed SSE.
				_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
					`data: this is not json` + "\n\n"))
				return
			}
			// Retry succeeds with the same opening.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial recovered answer"}}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var tokens []string
	var ends, recovers int
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnToken:                func(token string) { tokens = append(tokens, token) },
			OnStreamEnd:            func() { ends++ },
			OnRecoverPartialStream: func() { recovers++ },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := streamRequests.Load(); got != 2 {
		t.Fatalf("stream requests = %d, want 2 (original + one streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 0 {
		t.Fatalf("non-stream requests = %d, want 0 (a successful retry must skip the fallback)", got)
	}
	if len(tokens) != 2 || tokens[0] != "partial " || tokens[1] != "recovered answer" {
		t.Fatalf("OnToken fired %v, want [partial  recovered answer] (streamed prefix + trimmed suffix only)", tokens)
	}
	if ends != 0 || recovers != 0 {
		t.Fatalf("retry fired round-end callbacks (OnStreamEnd=%d, OnRecoverPartialStream=%d); the agent loop owns these", ends, recovers)
	}
	if result.Content != "partial recovered answer" {
		t.Fatalf("result.Content = %q, want %q", result.Content, "partial recovered answer")
	}
	if !result.PartialStream {
		t.Fatal("PartialStream must be true: the first attempt rendered partial output")
	}
}

// TestStreamRetryFailureFallsBackToNonStreaming verifies the full ladder:
// the stream fails, the muted streaming retry fails too, and the
// non-streaming fallback recovers. The fallback must trim against what the
// client actually rendered — the FIRST attempt's partial; the muted retry
// rendered nothing of its own.
func TestStreamRetryFailureFallsBackToNonStreaming(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamRequests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"partial recovered answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var tokens []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnToken: func(token string) { tokens = append(tokens, token) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := streamRequests.Load(); got != 2 {
		t.Fatalf("stream requests = %d, want 2 (original + one streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 1 {
		t.Fatalf("non-stream requests = %d, want 1 (fallback after the retry also failed)", got)
	}
	if len(tokens) != 2 || tokens[0] != "partial " || tokens[1] != "recovered answer" {
		t.Fatalf("OnToken fired %v, want [partial  recovered answer] (streamed prefix + trimmed fallback suffix)", tokens)
	}
	if result.Content != "partial recovered answer" {
		t.Fatalf("result.Content = %q, want %q", result.Content, "partial recovered answer")
	}
	if !result.PartialStream {
		t.Fatal("PartialStream must be true: the first attempt rendered partial output")
	}
}

// TestStreamRetrySignalsOnStreamRetry pins the host signal for the silent
// recovery windows: a retryable first-attempt failure fires OnStreamRetry
// ("stream interrupted: <cause>") before the muted streaming retry, and the
// non-streaming fallback fires it again ("non-streaming recovery: <cause>").
// The
// regeneration that follows delivers nothing live, so hosts must be able
// to label the silent stretch instead of showing a bare spinner.
func TestStreamRetrySignalsOnStreamRetry(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamRequests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"partial recovered answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var reasons []string
	_, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnStreamRetry: func(reason string) { reasons = append(reasons, reason) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := streamRequests.Load(); got != 2 {
		t.Fatalf("stream requests = %d, want 2 (original + one streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 1 {
		t.Fatalf("non-stream requests = %d, want 1 (fallback after the retry also failed)", got)
	}
	if len(reasons) != 2 || !strings.HasPrefix(reasons[0], "stream interrupted") || !strings.HasPrefix(reasons[1], "non-streaming recovery") {
		t.Fatalf("OnStreamRetry fired %v, want [stream interrupted… non-streaming recovery…] with the failure cause appended", reasons)
	}
}

// TestStreamRetrySignalFiresOnceOnSuccess pins that a successful muted
// retry fires OnStreamRetry exactly once — the "non-streaming recovery"
// signal belongs to the fallback path only.
func TestStreamRetrySignalFiresOnceOnSuccess(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			if streamRequests.Add(1) == 1 {
				// Transient failure: one valid chunk, then malformed SSE.
				_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
					`data: this is not json` + "\n\n"))
				return
			}
			// Retry succeeds with the same opening.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial recovered answer"}}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	var reasons []string
	_, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnStreamRetry: func(reason string) { reasons = append(reasons, reason) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := nonStreamRequests.Load(); got != 0 {
		t.Fatalf("non-stream requests = %d, want 0 (the retry succeeded)", got)
	}
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "stream interrupted") {
		t.Fatalf("OnStreamRetry fired %v, want exactly one [stream interrupted…] with the failure cause appended", reasons)
	}
}

// TestStreamContextWindowErrorSkipsStreamingRetry verifies the retry gate: a
// context-window refusal carried by the stream is deterministic — it must go
// straight to the non-streaming fallback (exactly one stream request) so the
// agent loop's compaction recovery sees it classified, without burning a
// doomed second stream request.
func TestStreamContextWindowErrorSkipsStreamingRetry(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamRequests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			// An error frame carrying the llama.cpp exceed_context_size
			// marker: IsContextWindowError classifies it.
			_, _ = w.Write([]byte(`data: {"error":{"message":"request exceeds the available context size","type":"exceed_context_size_error"}}` + "\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"recovered after compaction"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnToken: func(string) {}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := streamRequests.Load(); got != 1 {
		t.Fatalf("stream requests = %d, want 1 (context-window refusals must not be retried)", got)
	}
	if got := nonStreamRequests.Load(); got != 1 {
		t.Fatalf("non-stream requests = %d, want 1 (direct fallback)", got)
	}
	if result.Content != "recovered after compaction" {
		t.Fatalf("result.Content = %q, want %q", result.Content, "recovered after compaction")
	}
}

// TestStreamTransientHTTPStatusRetriesOnStreamingPath pins the retry gate
// for HTTP statuses: a transient failure of the streaming POST (429/408/5xx)
// gets ONE streaming retry before the non-streaming fallback is considered.
// Skipping the retry for these sent every router blip straight to the
// fallback, which is why the TUI hit the non-streaming recovery more often
// than it should. A successful retry must never reach the fallback. SDK
// retries are disabled so one HTTP request equals one logical attempt (the
// SDK's own transport retries would otherwise mask a single blip — and do
// in production, where isolated failures rarely reach this ladder).
func TestStreamTransientHTTPStatusRetriesOnStreamingPath(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			if streamRequests.Add(1) == 1 {
				// Transient upstream failure: the router answers 502.
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"recovered on the stream path"}}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	var reasons []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnStreamRetry: func(reason string) { reasons = append(reasons, reason) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := streamRequests.Load(); got != 2 {
		t.Fatalf("stream requests = %d, want 2 (transient 502 must get the streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 0 {
		t.Fatalf("non-stream requests = %d, want 0 (a successful retry must skip the fallback)", got)
	}
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "stream interrupted") {
		t.Fatalf("OnStreamRetry fired %v, want exactly one [stream interrupted…] with the failure cause appended", reasons)
	}
	if result.Content != "recovered on the stream path" {
		t.Fatalf("result.Content = %q, want %q", result.Content, "recovered on the stream path")
	}
}

// TestStreamTransientHTTPStatusStillFallsBackAfterFailedRetry pins the
// ladder's last rung: when the streaming retry for a transient status also
// fails, the non-streaming fallback runs exactly once and recovers. SDK
// retries are disabled so one HTTP request equals one logical attempt.
func TestStreamTransientHTTPStatusStillFallsBackAfterFailedRetry(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamRequests.Add(1)
			// Both streaming attempts fail transiently.
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	var reasons []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnStreamRetry: func(reason string) { reasons = append(reasons, reason) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := streamRequests.Load(); got != 2 {
		t.Fatalf("stream requests = %d, want 2 (original + one streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 1 {
		t.Fatalf("non-stream requests = %d, want 1 (fallback after the retry also failed)", got)
	}
	if len(reasons) != 2 || !strings.HasPrefix(reasons[0], "stream interrupted") || !strings.HasPrefix(reasons[1], "non-streaming recovery") {
		t.Fatalf("OnStreamRetry fired %v, want [stream interrupted… non-streaming recovery…] with the failure cause appended", reasons)
	}
	if result.Content != "fallback answer" {
		t.Fatalf("result.Content = %q, want %q", result.Content, "fallback answer")
	}
	if result.PartialStream {
		t.Fatal("PartialStream must be false: neither failed attempt rendered output")
	}
}

// TestLiveStreamingRetryWhenNothingRendered pins the no-output retry path:
// when the first attempt failed before rendering anything, the retry
// delivers LIVE (one OnToken per chunk) instead of muting into one
// end-of-retry batch — there is no rendered prefix to diverge from, so
// muting serves no purpose.
func TestLiveStreamingRetryWhenNothingRendered(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			if streamRequests.Add(1) == 1 {
				// Transient upstream failure before any output.
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			// Two content chunks: live delivery emits one OnToken per chunk,
			// the muted path would deliver one combined batch.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"live "}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"content":"retry answer"}}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	var tokens, reasons []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{
			OnToken:       func(s string) { tokens = append(tokens, s) },
			OnStreamRetry: func(reason string) { reasons = append(reasons, reason) },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := streamRequests.Load(); got != 2 {
		t.Fatalf("stream requests = %d, want 2 (original + one streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 0 {
		t.Fatalf("non-stream requests = %d, want 0 (a successful retry must skip the fallback)", got)
	}
	if len(tokens) != 2 || tokens[0] != "live " || tokens[1] != "retry answer" {
		t.Fatalf("OnToken delivered %q, want per-chunk live delivery [live  retry answer]", tokens)
	}
	if result.Content != "live retry answer" {
		t.Fatalf("result.Content = %q, want %q", result.Content, "live retry answer")
	}
	if result.PartialStream {
		t.Fatal("PartialStream must be false: nothing was rendered before the retry, so there is no stale partial UI")
	}
	if len(reasons) != 1 || !strings.HasPrefix(reasons[0], "stream interrupted before any output") {
		t.Fatalf("OnStreamRetry fired %v, want exactly one [stream interrupted before any output…]", reasons)
	}
}

// TestLiveRetryFailureTrimsFallbackAgainstRetryOutput pins the trim source
// when the live retry breaks after rendering partial output: the fallback
// trims against the RETRY's partial text (it rendered live before failing),
// not attempt 1's empty accumulator, so only the suffix beyond it is
// re-delivered.
func TestLiveRetryFailureTrimsFallbackAgainstRetryOutput(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	var streamRequests, nonStreamRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			streamRequests.Add(1)
			if streamRequests.Load() == 1 {
				// Transient upstream failure before any output.
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		nonStreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"partial recovered answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv, option.WithMaxRetries(0))
	var tokens []string
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		&StreamHandlers{OnToken: func(s string) { tokens = append(tokens, s) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := streamRequests.Load(); got != 2 {
		t.Fatalf("stream requests = %d, want 2 (original + one streaming retry)", got)
	}
	if got := nonStreamRequests.Load(); got != 1 {
		t.Fatalf("non-stream requests = %d, want 1 (fallback after the retry also failed)", got)
	}
	if len(tokens) != 2 || tokens[0] != "partial " || tokens[1] != "recovered answer" {
		t.Fatalf("OnToken delivered %q, want [partial  recovered answer]: the live retry's partial output must be trimmed from the fallback", tokens)
	}
	if result.Content != "partial recovered answer" {
		t.Fatalf("result.Content = %q, want %q", result.Content, "partial recovered answer")
	}
	if !result.PartialStream {
		t.Fatal("PartialStream must be true: the live retry rendered partial output before failing")
	}
}

// TestTrimRecoveredText pins the suffix-only trimming contract: only an exact
// byte prefix is dropped; divergent re-generations are emitted in full.
func TestTrimRecoveredText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		streamed  string
		recovered string
		want      string
	}{
		{"empty streamed", "", "answer", "answer"},
		{"empty recovered", "partial", "", ""},
		{"both empty", "", "", ""},
		{"exact prefix", "partial ", "partial recovered answer", "recovered answer"},
		{"identical", "exact", "exact", ""},
		{"divergent", "abc", "abx", "abx"},
		{"recovered shorter than streamed", "hello world", "hello", "hello"},
		{"utf8 prefix", "日", "日本語", "本語"},
		{"utf8 not a byte prefix", "本", "日本語", "日本語"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimRecoveredText(tc.streamed, tc.recovered); got != tc.want {
				t.Fatalf("trimRecoveredText(%q, %q) = %q, want %q", tc.streamed, tc.recovered, got, tc.want)
			}
		})
	}
}

// TestStreamFallbackKeepsStreamedModel verifies the fallback StreamResult
// keeps the model ID the failed stream reported when the non-streaming
// response omits the model field.
func TestStreamFallbackKeepsStreamedModel(t *testing.T) {
	// Recovery backoff disabled so the ladder runs instantly; t.Setenv
	// precludes t.Parallel.
	t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", "0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"model":"glm-4.6","choices":[{"delta":{"content":"partial "}}]}` + "\n\n" +
				`data: this is not json` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// No "model" field: the fallback must fall back to the streamed model.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"recovered answer"}}]}`))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "glm-4.6" {
		t.Fatalf("result.Model = %q, want %q", result.Model, "glm-4.6")
	}
}

// TestGenerateResponseStreamUsageAfterManyNoOps verifies the post-finish
// drain keeps consuming chunks until the usage chunk arrives, so a provider
// that sends many no-op chunks between finish_reason and the final usage
// chunk does not lose usage (the old 8-chunk drain bound dropped it).
func TestGenerateResponseStreamUsageAfterManyNoOps(t *testing.T) {
	t.Parallel()
	var sse strings.Builder
	sse.WriteString(`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}` + "\n\n")
	for i := 0; i < 10; i++ {
		sse.WriteString("data: {}\n\n")
	}
	sse.WriteString(`data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse.String()))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage == nil {
		t.Fatal("result.Usage is nil: the usage chunk arriving after 10 no-op chunks was lost in the drain")
	}
	if result.Usage.PromptTokens != 10 || result.Usage.CompletionTokens != 5 || result.Usage.TotalTokens != 15 {
		t.Fatalf("result.Usage = %#v, want prompt=10 completion=5 total=15", result.Usage)
	}
}

// TestGenerateResponseStreamReasoningBareJSONNotExtracted verifies that a
// bare JSON tool-call shape inside reasoning_content is NOT turned into a
// tool call: reasoning is scanned for explicit <tool_call>/<invoke> blocks
// only, and models routinely draft/quote JSON there (previously this
// produced phantom tool calls executed with prose-derived arguments).
func TestGenerateResponseStreamReasoningBareJSONNotExtracted(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"I'll use {\"name\": \"list_files\", \"arguments\": {\"path\": \"/tmp\"}}"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 0 {
		t.Fatalf("expected 0 tool calls from bare JSON in reasoning, got %+v", result.ToolCalls)
	}
}

// TestGenerateResponseStreamContentBareJSONExtracted verifies that the same
// bare JSON tool-call shape in CONTENT is still recovered as a tool call
// (the text-format recovery path for providers that never deliver structured
// tool_calls).
func TestGenerateResponseStreamContentBareJSONExtracted(t *testing.T) {
	t.Parallel()
	const sse = `data: {"choices":[{"delta":{"content":"I'll use {\"name\": \"list_files\", \"arguments\": {\"path\": \"/tmp\"}}"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(srv)
	result, err := p.GenerateResponseStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call from bare JSON in content, got %d: %+v", len(result.ToolCalls), result.ToolCalls)
	}
	if result.ToolCalls[0].Name != "list_files" {
		t.Fatalf("tool name = %q, want list_files", result.ToolCalls[0].Name)
	}
}
