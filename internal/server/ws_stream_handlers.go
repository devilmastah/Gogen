package server

import (
	"context"
	"fmt"
	"sync"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/streambuf"
	"gogen/internal/streamutil"
)

// buildStreamHandlers wires the runtime's live-turn state and the token
// batcher to the agent's stream callbacks via the shared
// streamutil.BuildStreamHandlers: the builder owns the batcher
// flush/reset/close ordering (see that function), and wsStreamSink turns
// the events into WebSocket frames. write fans out to every attached
// socket with session tagging; the terminal-tab maps are mutex-guarded
// (accessed from the exec pipe and the stream goroutines).
func (rt *sessionRuntime) buildStreamHandlers(ctx context.Context, write func(WSMessage), tokens *streamutil.TokenBatcher, argsBatch *streamutil.ArgsBatcher, speed *streamutil.SpeedMeter, termMu *sync.Mutex, termBatches map[string]*streamutil.TokenBatcher, termOpened map[string]struct{}) *llm.StreamHandlers {
	sk := &wsStreamSink{
		rt:          rt,
		ctx:         ctx,
		write:       write,
		termMu:      termMu,
		termBatches: termBatches,
		termOpened:  termOpened,
	}
	// The round-buffer half of the sink is the shared streambuf.RoundSink
	// (the same reset/append timing the TUI's StreamAdapter calls). Its
	// hooks carry the server-only wire-side sent markers, which the buffer
	// does not know about: Reset clears them with the buffer, and
	// OnToolStart restarts the marker for a recycled tool-call index (the
	// client's card was created by tool_call_start, which always precedes
	// the first delta, so both counters start at 0).
	sk.rounds = streambuf.RoundSink{
		Buf:   &rt.live.RoundBuffer,
		Reset: rt.live.resetAll,
		OnToolStart: func(index int, id, name string) {
			rt.live.sentMu.Lock()
			if rt.live.toolArgsSent != nil {
				delete(rt.live.toolArgsSent, index)
			}
			rt.live.sentMu.Unlock()
		},
	}
	return streamutil.BuildStreamHandlers(sk, streamutil.HandlersConfig{Tokens: tokens, Args: argsBatch, Speed: speed})
}

// wsStreamSink is the server's streamutil.Sink: it turns the shared
// builder's events into WebSocket frames and live-turn state updates.
// The builder flushes the token/args batchers at the boundaries the
// client depends on (before tool_call_start/tool_call/tool_execute and
// model_used, and at round end), so the methods here only emit frames
// and update the live-turn buffer. The args batcher's send callback
// (created in runTurnBody) emits the tool_call_delta frames; OnToolCallArgsDelta
// only feeds the live buffer.
type wsStreamSink struct {
	rt          *sessionRuntime
	ctx         context.Context
	write       func(WSMessage)
	termMu      *sync.Mutex
	termBatches map[string]*streamutil.TokenBatcher
	termOpened  map[string]struct{}
	// rounds feeds the runtime's live-turn buffer from the stream
	// callbacks (shared timing with the TUI — see streambuf.RoundSink);
	// its Reset/OnToolStart hooks carry the wire-side sent markers.
	rounds streambuf.RoundSink
}

var _ streamutil.Sink = (*wsStreamSink)(nil)

func (sk *wsStreamSink) OnStart() {
	// Reset the live-turn buffer (and sent markers) for the new turn.
	sk.rounds.TurnBegin()
	// Tell the client the server-side index of the user message that
	// StreamProcessInput just appended (for edit/resend).
	// Index goes in Content because WSMessage.Index has omitempty
	// and the first message is index 0.
	userIdx := sk.rt.agent.MessageCount() - 1
	if userIdx >= 0 {
		sk.write(WSMessage{Type: "user_acked", Content: fmt.Sprintf("%d", userIdx)})
	}
	sk.write(WSMessage{Type: "thinking"})
	if sk.ctx.Err() != nil {
		return
	}
	sk.write(contextMsg(sk.ctx, sk.rt.agent))
}

func (sk *wsStreamSink) OnRoundStart() {
	sk.rounds.RoundBegin()
	sk.write(WSMessage{Type: "thinking"})
	if sk.ctx.Err() != nil {
		return
	}
	sk.write(contextMsg(sk.ctx, sk.rt.agent))
}

func (sk *wsStreamSink) OnStreamOpened() {
	sk.write(WSMessage{Type: "waiting"})
}

func (sk *wsStreamSink) OnStreamActivity() {}

func (sk *wsStreamSink) OnCompacting() {
	sk.write(WSMessage{Type: "compacting"})
}

func (sk *wsStreamSink) OnCondensed(note string) {
	// Last-resort condensation announcement (Phase 0e): the
	// client renders it as a banner above the composer.
	sk.write(WSMessage{Type: "condensed", Content: note, SessionID: sk.rt.agent.SessionID})
}

func (sk *wsStreamSink) OnStreamStall() {}

// OnStreamRetry tells the client a failed stream attempt is being retried
// (muted streaming retry, or the non-streaming fallback): the retry
// regenerates the whole response with live delivery muted, so the client's
// progress label should explain the silent window instead of a bare
// "Thinking…". Clients without a stream_retry handler ignore the frame
// (unknown types fall through both dispatcher switches).
func (sk *wsStreamSink) OnStreamRetry(reason string) {
	sk.write(WSMessage{Type: "stream_retry", Reason: reason})
}

func (sk *wsStreamSink) OnThinkingToken(token string) {
	sk.rounds.Thinking(token)
}

// OnStreamStats emits the shared SpeedMeter's smoothed token rate (same
// meter, interval, and estimator as the TUI) for the client's progress
// label. Timer-driven (at most once per StatsInterval) while the round
// streams; the builder stops the meter at round end, so the frame always
// precedes the round's stream_end (the send queue is FIFO).
func (sk *wsStreamSink) OnStreamStats(toksPerSec float64) {
	sk.write(WSMessage{Type: "stream_stats", TokensPerSec: toksPerSec})
}

func (sk *wsStreamSink) OnToken(token string) {
	sk.rounds.Token(token)
}

func (sk *wsStreamSink) OnStreamEnd() {
	sk.rounds.RoundEnd()
	sk.write(WSMessage{Type: "stream_end"})
}

func (sk *wsStreamSink) OnReplyModel(model string) {
	// Fired by the agent before each round's OnStreamEnd, so
	// this frame must arrive before stream_end for the client
	// to stamp the still-live assistant bubble (intermediate
	// content+tool rounds included). The builder flushed pending
	// content tokens first, so the client has created the bubble
	// by the time model_used is processed.
	if model == "" {
		return
	}
	sk.write(WSMessage{Type: "model_used", Model: model})
}

func (sk *wsStreamSink) OnToolCallStart(index int, id, name string) {
	sk.rounds.ToolStart(index, id, name)
	sk.write(WSMessage{
		Type:       "tool_call_start",
		Tool:       name,
		ToolCallID: id,
		Index:      index,
	})
}

func (sk *wsStreamSink) OnToolCallArgsDelta(index int, id, name, argsDelta string) {
	// Buffered synchronously (the rewind snapshot must see the complete
	// args); the frame is emitted by the args batcher's send
	// callback: one tool_call_delta per flush interval instead of
	// one per provider chunk. No token flush here — it fired on
	// every chunk before, forcing an extra stream frame per delta
	// in a content+tool interleaving. The builder flushes both
	// batchers at the boundaries exactly once.
	sk.rounds.ToolArgs(index, argsDelta)
}
func (sk *wsStreamSink) OnToolCall(tc llm.ToolCall) {
	sk.write(WSMessage{
		Type:       "tool_call",
		Tool:       tc.Name,
		ToolCallID: tc.ID,
		Index:      tc.Index,
		Args:       tc.Args,
	})
}

func (sk *wsStreamSink) OnToolExecute(name string) {
	sk.write(WSMessage{Type: "tool_execute", Tool: name})
}

func (sk *wsStreamSink) OnToolOutput(id, name, command, chunk string) {
	if sk.ctx.Err() != nil {
		return
	}
	sk.termMu.Lock()
	first := false
	if _, ok := sk.termOpened[id]; !ok {
		sk.termOpened[id] = struct{}{}
		first = true
	}
	b := sk.termBatches[id]
	if b == nil {
		b = streamutil.NewTokenBatcher(func(_ bool, text string) {
			sk.write(WSMessage{Type: "term_output", TermID: id, Content: text})
		}, wsTokenFlushInterval)
		sk.termBatches[id] = b
	}
	sk.termMu.Unlock()
	if first {
		sk.write(WSMessage{Type: "term_opened", TermID: id, ToolCallID: id, Tool: name, Content: "$ " + command})
	}
	b.StreamToken(chunk)
}

func (sk *wsStreamSink) OnToolOutputEnd(id string, success bool) {
	// Close this tool call's live terminal tab, if one was
	// opened. Flush first so buffered chunks land before
	// term_exit (the send queue is FIFO). Fired by the executor
	// when a foreground command finishes (right before
	// OnToolResult) and by the background job's wait goroutine
	// when the job exits — possibly long after the turn ended;
	// the send queue is FIFO, so the flush still lands first.
	// A no-op when the command produced no output (no tab).
	sk.termMu.Lock()
	b := sk.termBatches[id]
	delete(sk.termBatches, id)
	_, opened := sk.termOpened[id]
	delete(sk.termOpened, id)
	sk.termMu.Unlock()
	if b != nil {
		b.Flush()
		b.Close()
	}
	if opened {
		sk.write(WSMessage{Type: "term_exit", TermID: id, ToolCallID: id, Success: success})
	}
}

// StopStream is unused here. The agent installs the smart-compact budget
// stop on top of this callback. The web host has no separate reason to cut
// a stream.
func (sk *wsStreamSink) StopStream() bool { return false }

// OnMessageAppended reports the index of a message the turn just appended.
// The agent fires it for ASSISTANT rounds only (see Agent.notifyMessageAppended):
// reporting tool results (or the synthetic image messages) could advance the
// client's rendered-index mark past the transcript's last shippable message
// on a turn that ends on a tool message, so those deliberately stay silent
// and the mark trails — a conditional attach then falls back to the full
// snapshot, correct but unoptimized. The web client stamps the live bubble
// and advances its per-pane rendered-index mark, so a later conditional
// attach can prove the transcript is current and skip the history snapshot —
// the turn-end convergence refetch used to rebuild the whole transcript
// because live messages carried no index at all. The index goes in Content
// because WSMessage.Index has omitempty and message index 0 is valid (the
// same convention as user_acked); role is informational.
func (sk *wsStreamSink) OnMessageAppended(index int, role string) {
	if index < 0 {
		return
	}
	sk.write(WSMessage{
		Type:      "hist_index",
		SessionID: sk.rt.agent.SessionID,
		Content:   fmt.Sprintf("%d", index),
	})
}

func (sk *wsStreamSink) OnToolResult(id, name, result string, success bool) {
	// The terminal is closed by OnToolOutputEnd, not here: for
	// background jobs (execute_command background=true) the
	// output stream outlives this result, and closing it here
	// would kill the tab while the job is still running.
	sk.write(WSMessage{
		Type:            "tool_result",
		Tool:            name,
		ToolCallID:      id,
		Result:          truncateToolResult(result),
		Success:         success,
		ResultTruncated: len(result) > 128*1024,
	})
}

func (sk *wsStreamSink) OnRecoverPartialStream() {}

// truncateToolResult cuts oversized tool results at a rune boundary so the
// client never renders a broken UTF-8 character, marking the cut explicitly.
// Delegates to contextmgr.Truncate rather than hand-rolling the cut — the
// previous local copy had drifted from the context manager's truncation
// discipline. The marker is appended OUTSIDE the byte budget (no
// MarkerInBudget, unlike the context manager's tool-result cap): that is
// fine here because the 128 KiB limit is a display cap and the result is
// never re-truncated by a later pass.
func truncateToolResult(result string) string {
	const maxResult = 128 * 1024
	return contextmgr.Truncate(result, maxResult, contextmgr.TruncateOptions{
		Marker: fmt.Sprintf("\n… truncated (%d bytes total)", len(result)),
	})
}
