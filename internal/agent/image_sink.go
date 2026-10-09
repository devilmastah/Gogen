package agent

import (
	"context"
	"sync"
	"time"

	"gogen/internal/llm"
)

// imageEntry is one image produced by a read_image tool call: the image
// itself and the caption attached to the synthetic user message that carries
// it into context.
type imageEntry struct {
	img     llm.ImageInput
	caption string
}

// imageSink accumulates images during tool execution so a handler never
// touches the transcript itself. executeTool runs before appendToolResult;
// a handler that appended a user message directly would place the image
// between the assistant's tool_call and its tool result — a wire-protocol
// violation. Instead the handler only collects the image into the sink, and
// the tool-round coordinator appends the synthetic user message only after
// EVERY tool message of the round is on the transcript (see
// appendRoundImages): strict OpenAI-compatible providers require the tool
// messages answering one assistant tool_calls message to directly follow it,
// before any other role. Handlers run concurrently in parallel read-only
// batches, so every mutation is mutex-guarded.
type imageSink struct {
	mu    sync.Mutex
	items []imageEntry
}

// Add records one image (thread-safe).
func (s *imageSink) Add(img llm.ImageInput, caption string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, imageEntry{img: img, caption: caption})
}

// drain returns the collected entries and clears the sink. Called by the
// tool-round coordinator on the turn goroutine after the tool result was
// appended; a never-populated sink drains to nil.
func (s *imageSink) drain() []imageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.items
	s.items = nil
	return items
}

type imageSinkKey struct{}

// ContextWithImageSink returns a copy of ctx carrying sink. read_image
// handlers read it via ImageSinkFromContext; the tool-round coordinator
// attaches a fresh sink per tool call (mirroring ContextWithToolOutput).
func ContextWithImageSink(ctx context.Context, sink *imageSink) context.Context {
	return context.WithValue(ctx, imageSinkKey{}, sink)
}

// ImageSinkFromContext returns the image sink attached to ctx, or nil when
// none was set.
func ImageSinkFromContext(ctx context.Context) *imageSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(imageSinkKey{}).(*imageSink)
	return sink
}

// appendImageMessages appends every image collected by one read_image tool
// call as a synthetic user message, preserving collection order. Callers
// must drain the round's sinks only after EVERY tool message of the round
// (results and cancel placeholders) is on the transcript — see
// appendRoundImages. The transcript then reads assistant(tool_call) →
// tool(result) × N → user(image+caption) …, and the image never lands
// between two tool messages: strict OpenAI-compatible providers require the
// tool messages answering one assistant tool_calls message to directly
// follow it and reject an interleaved message with "insufficient tool
// messages following tool_calls message". Image parts are accepted on user
// messages only, so a user message is the only carrier for the image. The
// append goes through appendMessage, so token-count caching and the statsMu
// discipline apply exactly as they do for user input; the turn loop's
// persistSession (and the cancel paths' FlushSession) then persist it like
// any other message.
func (a *Agent) appendImageMessages(sink *imageSink) {
	if sink == nil {
		return
	}
	for _, e := range sink.drain() {
		a.appendMessage(llm.Message{
			Role:      "user",
			Content:   e.caption,
			Images:    []llm.ImageInput{e.img},
			CreatedAt: time.Now().Truncate(time.Millisecond),
		})
	}
}

// appendRoundImages drains every per-call image sink of one tool round, in
// model order, after all of the round's tool messages were appended. This is
// the only protocol-safe point for the synthetic user image messages: an
// assistant message with tool_calls must be directly followed by one tool
// message per tool_call_id before any other role, so an image inserted right
// after its own tool result splits the batch and strict providers reject the
// round's next request (or the next turn) with "insufficient tool messages
// following tool_calls message". Empty sinks — calls that produced no image,
// or never ran — are no-ops.
func (a *Agent) appendRoundImages(sinks []*imageSink) {
	for _, sink := range sinks {
		a.appendImageMessages(sink)
	}
}
