package streamutil

import (
	"gogen/internal/llm"
)

// Sink receives the stream events dispatched by BuildStreamHandlers. Each
// method corresponds to exactly one llm.StreamHandlers callback, and the
// builder is the single place where those callbacks are wired: a new
// callback added to llm.StreamHandlers forces a new Sink method and a
// compile error in every host (TUI, WebSocket server) until it decides
// how to handle it — a forgotten callback can no longer be silently
// ignored.
//
// Sink methods are pure transport emission (WebSocket frames, tea
// messages, live-state bookkeeping). They must NOT touch the
// HandlersConfig batchers: the builder owns all batcher
// flush/reset/close ordering (see BuildStreamHandlers).
type Sink interface {
	OnStart()
	OnRoundStart()
	OnStreamOpened()
	OnStreamActivity()
	OnCompacting()
	OnCondensed(note string)
	OnStreamStall()
	// OnStreamRetry reports that a failed stream attempt is being retried
	// (muted streaming retry, or the non-streaming fallback): the retry
	// regenerates the whole response with live delivery muted, so hosts
	// should swap their "streaming" state for a "retrying" state until the
	// next round-start/token. reason is a short human-readable cause that
	// includes the underlying failure (e.g. "stream interrupted: read tcp
	// ...: connection reset by peer").
	OnStreamRetry(reason string)
	OnThinkingToken(token string)
	OnToken(token string)
	OnStreamEnd()
	OnReplyModel(model string)
	OnToolCallStart(index int, id, name string)
	OnToolCallArgsDelta(index int, id, name, argsDelta string)
	OnToolCall(tc llm.ToolCall)
	OnRecoverPartialStream()
	OnToolExecute(name string)
	OnToolOutput(id, name, command, chunk string)
	OnToolOutputEnd(id string, success bool)
	OnToolResult(id, name, result string, success bool)
	// OnMessageAppended reports the index in the agent's message list of the
	// assistant message each round just appended. Hosts that render live
	// turns track a rendered-index mark from it, so a later conditional
	// attach can prove the transcript is current without a history refetch.
	// Hosts without such a model (the TUI) treat it as a no-op.
	OnMessageAppended(index int, role string)
	// StopStream is polled after each streamed chunk. Hosts return false.
	// The agent chains its own budget check on top of this callback when
	// smart compact is on; a true return ends the stream with the text
	// accumulated so far.
	StopStream() bool
	// OnStreamStats reports the SpeedMeter's smoothed token rate
	// (tokens/sec over content, thinking, and tool-args deltas). Unlike
	// the other methods it is not a 1:1 mapping of an llm.StreamHandlers
	// callback: it is timer-driven (at most once per StatsInterval) and
	// fires only while a round is streaming — the builder stops the meter
	// at round end, so the last stats event always precedes the round's
	// OnStreamEnd on the consumer's FIFO queue.
	OnStreamStats(toksPerSec float64)
}

// HandlersConfig carries the per-turn batchers the builder drives. Hosts
// create the batchers themselves — their send callbacks are
// transport-specific — and keep their own references for the turn's
// error/exit flushes.
type HandlersConfig struct {
	Tokens *TokenBatcher
	Args   *ArgsBatcher
	// Speed optionally measures the stream's token rate; the builder
	// feeds it every content/thinking/tool-args delta and drives its
	// ticker across the round lifecycle (Start at turn/round start, Stop
	// at round end). Nil disables rate reporting.
	Speed *SpeedMeter
}

// BuildStreamHandlers wires s to a full llm.StreamHandlers with the
// ordering discipline both frontends must obey:
//
//   - turn/round start: pending args from the previous round would be
//     stamped against a reset position counter, so the args batcher is
//     closed (dropping them) and re-armed; the token batcher is re-armed
//     too (a no-op while it is still open).
//   - every event that assumes the consumer already has the in-flight
//     content (tool_call_start, tool_call, tool_execute, model_used) is
//     preceded by a flush of the token and/or args batcher, so the send
//     queue's FIFO order carries the content first.
//   - round end: both batchers are flushed before the sink's round-end
//     event (the consumer must have the content by the time the round
//     boundary is processed) and closed; the next round start re-arms
//     them.
func BuildStreamHandlers(s Sink, cfg HandlersConfig) *llm.StreamHandlers {
	tokens, args := cfg.Tokens, cfg.Args
	speed := cfg.Speed
	// startSpeed (re)arms the rate meter at a turn/round boundary; the
	// meter's Start resets its counters, so a new round's rate never
	// inherits the previous round's tail.
	startSpeed := func() {
		if speed != nil {
			speed.Start(s.OnStreamStats)
		}
	}
	return &llm.StreamHandlers{
		OnStart: func() {
			s.OnStart()
			args.Close()
			args.Reset()
			tokens.Reset()
			startSpeed()
		},
		OnRoundStart: func() {
			s.OnRoundStart()
			args.Close()
			args.Reset()
			tokens.Reset()
			startSpeed()
		},
		OnStreamOpened: func() {
			s.OnStreamOpened()
		},
		OnCompacting: func() {
			s.OnCompacting()
		},
		OnCondensed: func(note string) {
			s.OnCondensed(note)
		},
		OnStreamStall: func() {
			s.OnStreamStall()
		},
		OnStreamRetry: func(reason string) {
			s.OnStreamRetry(reason)
		},
		OnStreamActivity: func() {
			s.OnStreamActivity()
		},
		OnThinkingToken: func(token string) {
			s.OnThinkingToken(token)
			if speed != nil {
				speed.Feed(token)
			}
			tokens.ThinkToken(token)
		},
		OnToken: func(token string) {
			s.OnToken(token)
			if speed != nil {
				speed.Feed(token)
			}
			tokens.StreamToken(token)
		},
		OnStreamEnd: func() {
			// Stop BEFORE the flushes and the sink's round-end event:
			// Stop returns only once no in-flight emission can still run,
			// so a stats event can never overtake stream_end on the
			// consumer's FIFO queue.
			if speed != nil {
				speed.Stop()
			}
			tokens.Flush()
			args.Flush()
			s.OnStreamEnd()
			tokens.Close()
			args.Close()
		},
		OnReplyModel: func(model string) {
			// The consumer must have the round's content before the
			// model stamp (the frame queue is FIFO); the callback fires
			// right before OnStreamEnd, so this is the last flush that
			// can still precede the round boundary.
			tokens.Flush()
			s.OnReplyModel(model)
		},
		OnToolCallStart: func(index int, id, name string) {
			tokens.Flush()
			// Flush pending args from any PREVIOUS index first: the
			// consumer must see them before this call's start frame
			// (FIFO).
			args.Flush()
			s.OnToolCallStart(index, id, name)
		},
		OnToolCallArgsDelta: func(index int, id, name, argsDelta string) {
			s.OnToolCallArgsDelta(index, id, name, argsDelta)
			if speed != nil {
				speed.Feed(argsDelta)
			}
			args.Add(index, id, name, argsDelta)
		},
		OnToolCall: func(tc llm.ToolCall) {
			// The consumer must see the complete content and the
			// complete args accumulation before the final tool call.
			tokens.Flush()
			args.Flush()
			s.OnToolCall(tc)
		},
		OnRecoverPartialStream: func() {
			s.OnRecoverPartialStream()
		},
		OnToolExecute: func(name string) {
			// The tool is about to run: the consumer must show the
			// complete args first (the args stream is over at this
			// point).
			args.Flush()
			s.OnToolExecute(name)
		},
		OnToolOutput: func(id, name, command, chunk string) {
			s.OnToolOutput(id, name, command, chunk)
		},
		OnToolOutputEnd: func(id string, success bool) {
			s.OnToolOutputEnd(id, success)
		},
		OnToolResult: func(id, name, result string, success bool) {
			s.OnToolResult(id, name, result, success)
		},
		OnMessageAppended: func(index int, role string) {
			s.OnMessageAppended(index, role)
		},
		StopStream: func() bool {
			return s.StopStream()
		},
	}
}
