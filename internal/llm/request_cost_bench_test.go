package llm

import (
	"strconv"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

// Per-round request cost — the record behind the P7.3 decision.
//
// Measured on this box (load ~17, so absolute times are ~2x an idle machine;
// the allocation figures are exact):
//
//	messagesToChat (200 messages)        50 us   92 KB   786 allocs
//	MarshalJSON, 0.1 MB image          2.0 ms   3.5 MB  6.4k allocs
//	MarshalJSON, 1 MB image            4.5 ms  11.5 MB  6.4k allocs
//	MarshalJSON, 5 MB image           12.7 ms  46.0 MB  6.4k allocs
//	whole round (convert + marshal)    4.2 ms  11.1 MB  7.2k allocs
//
// The body for the 1 MB case is ~1.5 MB, so the SDK's param encoding
// allocates ~7x the payload — and that ratio is NOT image-specific: a 1 MB
// plain-text user message costs 6.8 MB, a 1 MB tool result 6.9 MB, and a
// 1 MB string in a plain struct only 1.1 MB. The amplification is the SDK's
// union/param marshaling, not encoding/json.
//
// What that means: a round already serializes the conversation exactly ONCE
// (see roundRequest: the stream body, the muted retry and the non-streaming
// fallback share one serialize), so the remaining cost is one O(history) pass
// per round with a ~7x allocation factor. Caching or replacing that encoder
// is a CPU/GC optimization, not a latency fix: the same bytes are uploaded
// anyway, and an LLM round is measured in seconds.

// benchMessages builds a history shaped like a long agent turn: a system
// prompt, alternating assistant tool-call rounds and tool results, one user
// message carrying a base64 image, and a tail of plain chat.
func benchMessages(imageBytes int) []Message {
	big := strings.Repeat("x", imageBytes)
	msgs := make([]Message, 0, 200)
	msgs = append(msgs, Message{Role: "system", Content: strings.Repeat("s", 8<<10)})
	msgs = append(msgs, Message{Role: "user", Content: "refactor the parser", Images: []ImageInput{
		{DataURL: "data:image/png;base64," + big},
	}})
	for i := 0; i < 60; i++ {
		// One assistant round with three tool calls...
		asst := Message{Role: "assistant", Content: "working on it " + strconv.Itoa(i)}
		for j := 0; j < 3; j++ {
			asst.ToolCalls = append(asst.ToolCalls, ToolCall{
				ID:      "call_" + strconv.Itoa(i) + "_" + strconv.Itoa(j),
				Name:    "read_file",
				Args:    map[string]any{"path": "internal/agent/patch.go"},
				ArgsStr: `{"path":"internal/agent/patch.go"}`,
			})
		}
		msgs = append(msgs, asst)
		// ...and three tool results (~2 KB each).
		for j := 0; j < 3; j++ {
			msgs = append(msgs, Message{
				Role:       "tool",
				ToolCallID: "call_" + strconv.Itoa(i) + "_" + strconv.Itoa(j),
				Content:    strings.Repeat("r", 2<<10),
			})
		}
	}
	return msgs
}

// BenchmarkMessagesToChat measures the per-round conversion of the whole
// history into SDK params (allocation-bound; no content is copied).
func BenchmarkMessagesToChat(b *testing.B) {
	p := &OpenAIProvider{}
	msgs := benchMessages(1 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := p.messagesToChat(msgs); len(got) != len(msgs) {
			b.Fatalf("converted %d of %d messages", len(got), len(msgs))
		}
	}
}

// BenchmarkRoundRequestMarshal measures the per-round serialization of the
// whole conversation — the one O(history bytes) pass a round still pays.
func BenchmarkRoundRequestMarshal(b *testing.B) {
	for _, tc := range []struct {
		name  string
		image int
	}{
		{"image-0.1MB", 100 << 10},
		{"image-1MB", 1 << 20},
		{"image-5MB", 5 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			p := &OpenAIProvider{}
			params := openai.ChatCompletionNewParams{
				Messages: p.messagesToChat(benchMessages(tc.image)),
				Model:    "bench-model",
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				body, err := params.MarshalJSON()
				if err != nil {
					b.Fatal(err)
				}
				if len(body) == 0 {
					b.Fatal("empty body")
				}
			}
		})
	}
}

// BenchmarkRoundRequestBuild is what a round actually does today: convert, then
// serialize — once, with the bytes shared by the stream attempt, the muted
// retry and the non-streaming fallback.
func BenchmarkRoundRequestBuild(b *testing.B) {
	p := &OpenAIProvider{}
	msgs := benchMessages(1 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		params := openai.ChatCompletionNewParams{
			Messages: p.messagesToChat(msgs),
			Model:    "bench-model",
		}
		if _, err := params.MarshalJSON(); err != nil {
			b.Fatal(err)
		}
	}
}
