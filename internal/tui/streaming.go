package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
	"gogen/internal/streambuf"
	"gogen/internal/streamutil"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// --- Bubble Tea messages for streaming ---

// Every streaming message carries its owning session id (sid) and the
// turn generation (seq) it was produced by; see streamEventAttribution
// and liveSession.turnSeq for the drop rules.

// streamStartMsg is sent when the agent first begins processing.
type streamStartMsg struct {
	sid string
	seq uint64
}

// streamRoundStartMsg is sent at the start of each LLM round after the first.
type streamRoundStartMsg struct {
	sid string
	seq uint64
}

type streamTokenMsg struct {
	token string
	sid   string
	seq   uint64
}

type streamThinkingMsg struct {
	token string
	sid   string
	seq   uint64
}

// streamStatsMsg carries the shared streamutil.SpeedMeter's smoothed
// token rate (tokens/sec over content, thinking, and tool-args deltas)
// for the progress line. It is timer-driven (at most a few per second),
// not per-token, and always precedes the round's terminal messages.
type streamStatsMsg struct {
	toksPerSec float64
	sid        string
	seq        uint64
}

// streamCompactingMsg is sent when the agent compacts history mid-turn
// (auto/forced compaction inside prepareMessages): the summarization is a
// full non-streaming LLM call, so without this the strip shows a bare
// "thinking" spinner for the whole silent summarization.
type streamCompactingMsg struct {
	sid string
	seq uint64
}

// streamStallMsg is sent when no SSE chunk has arrived for several seconds
// while the round streams (long prefill, stalled upstream): swaps the bare
// "thinking" spinner for a "still waiting" label. Cleared by the next
// progress event (any token/stats/round boundary).
type streamStallMsg struct {
	sid string
	seq uint64
}

// streamRetryMsg is sent when a failed stream attempt is retried with live
// delivery muted (streaming retry, or the non-streaming fallback): the
// silent stretch that follows is a whole regeneration, not a hang.
type streamRetryMsg struct {
	reason string
	sid    string
	seq    uint64
}

type streamToolCallMsg struct {
	index int
	id    string
	name  string
	sid   string
	seq   uint64
}

type streamToolCallArgsMsg struct {
	index int
	id    string
	delta string
	sid   string
	seq   uint64
}

type streamToolResultMsg struct {
	id      string
	name    string
	result  string
	success bool
	sid     string
	seq     uint64
}

// streamToolCallFinalMsg is sent when tool call args are fully parsed.
type streamToolCallFinalMsg struct {
	index int
	tc    llm.ToolCall
	sid   string
	seq   uint64
}

// streamToolExecuteMsg is sent immediately before a tool runs.
type streamToolExecuteMsg struct {
	name string
	sid  string
	seq  uint64
}

// streamRoundEndMsg is sent at the end of each streaming round
// (including intermediate tool-call rounds). It resets buffers
// but does NOT set streaming=false.
type streamRoundEndMsg struct {
	sid string
	seq uint64
}

// streamEndMsg is sent when all streaming is complete (final message from
// goroutine). sid attributes the turn to its owning live session so a
// background session's finish never mutates the focused transcript; seq
// attributes it to the turn generation so a superseded turn's late
// terminal (cancel + resubmit) is dropped instead of clobbering the new
// turn's state.
type streamEndMsg struct {
	sid string
	seq uint64
}

type streamErrorMsg struct {
	err error
	sid string
	seq uint64
}

// condensedNoteMsg carries the last-resort condensation announcement
// (Phase 0e) for rendering as a system line.
type condensedNoteMsg struct {
	note string
	sid  string
	seq  uint64
}

type contextStatsMsg struct {
	stats agent.TurnContext
	// sid is the session the probe was taken for, captured on the Update
	// thread at request time: the probe runs off-thread, so a slow probe
	// for a session that lost focus before it landed must not overwrite
	// the focused session's indicator (handler drops it).
	sid string
}

// compactResultMsg carries the outcome of an async /compact (cmdCompact)
// back to the Update thread. agent is the compacted session — the focused
// one may have changed while the summarization ran. seq identifies the
// compaction generation: a result from a SUPERSEDED run (cancelled, then
// restarted before the old goroutine finished unwinding) is dropped — it
// must neither clear the new run's flags nor report the old cancellation.
type compactResultMsg struct {
	agent *agent.Agent
	err   error
	seq   uint64
}

// modelListMsg carries the async /models list result back to the Update
// thread. agent is the listing session — the focused one may have changed
// while the provider round trip ran (the list would then describe the
// wrong endpoint).
type modelListMsg struct {
	agent *agent.Agent
	list  []llm.ModelInfo
	err   error
}

// modelSwitchMsg carries the async model-switch result (inline
// /models <sel> or the models modal selection) back to the Update thread.
type modelSwitchMsg struct {
	agent *agent.Agent
	out   string
	err   error
	// thinking is the models modal's staged thinking level, applied AFTER
	// a successful switch ("" = none, e.g. the inline /models <sel>
	// path). The modal validated it against the new model's accepted set
	// before the switch ran; applying it post-switch makes
	// HandleThinkingCommand's validation run against that same model.
	thinking agent.ThinkingLevel
}

// savedSessionsMsg carries an async persisted-session index snapshot
// (requestSavedSessions) back to the Update thread.
type savedSessionsMsg struct {
	seq      uint64
	sessions []agent.SessionInfo
	ok       bool
}

// programSender is the minimal program surface the adapter needs;
// *tea.Program satisfies it, and tests may record sends instead.
type programSender interface {
	Send(msg tea.Msg)
}

// StreamAdapter adapts llm.StreamHandlers to emit Bubble Tea messages
// that can be processed by the Model's Update method.
type StreamAdapter struct {
	program programSender
	// owner is the live-session id whose turn this adapter renders.
	owner string
	// seq is the turn generation this adapter was created for; every
	// emitted message carries it so the Update thread can drop stragglers
	// from a superseded turn (see liveSession.turnSeq).
	seq uint64
	// sess is the owning live session. Focused → events go to the
	// program; background → they are buffered for replay when focus
	// arrives (the turn keeps running either way; completion stays
	// attributed via sid).
	sess *liveSession
	// rounds feeds the owning session's round buffer from the stream
	// callbacks (shared reset/append timing with the web server — see
	// streambuf.RoundSink); a zero sink (adapters built without a
	// session) is a no-op.
	rounds streambuf.RoundSink
}

// NewStreamAdapter creates a new StreamAdapter for one live session's turn.
// seq must be the session's turnSeq at submit time (the submit paths
// increment it before constructing the adapter).
func NewStreamAdapter(owner string, seq uint64, p programSender, sess *liveSession) *StreamAdapter {
	var rounds streambuf.RoundSink
	if sess != nil {
		rounds = streambuf.RoundSink{Buf: &sess.round}
	}
	return &StreamAdapter{program: p, owner: owner, seq: seq, sess: sess, rounds: rounds}
}

// send emits a rendering message, or buffers it while the session streams
// in the background.
func (s *StreamAdapter) send(msg tea.Msg) {
	if s.sess != nil && !s.sess.focused.Load() {
		s.sess.enqueue(msg)
		return
	}
	s.program.Send(msg)
}

// Handlers returns a full set of stream handlers that emit tea.Msg values.
// The shared streamutil.BuildStreamHandlers owns the batcher
// flush/reset/close ordering (identical to the WebSocket frontend); the
// StreamAdapter's Sink methods below are the transport-specific half.
func (s *StreamAdapter) Handlers() *llm.StreamHandlers {
	tuiSend := func(think bool, text string) {
		if think {
			s.send(streamThinkingMsg{token: text, sid: s.owner, seq: s.seq})
		} else {
			s.send(streamTokenMsg{token: text, sid: s.owner, seq: s.seq})
		}
	}
	batch := streamutil.NewTokenBatcher(tuiSend, 32*time.Millisecond)
	// Tool-call args arrive as a high-rate delta stream (one message per
	// provider chunk); without coalescing the Update thread would do a
	// transcript update per chunk. The batcher concatenates per-index
	// deltas and flushes on a timer — the accumulated-args bookkeeping in
	// handleStreamToolArgs is unaffected (it sums the same bytes).
	// Shared with the WebSocket frontend (streamutil.ArgsBatcher) so both
	// coalesce identically.
	argsBatch := streamutil.NewArgsBatcher(func(index int, id, _ string, delta string) {
		s.send(streamToolCallArgsMsg{index: index, id: id, delta: delta, sid: s.owner, seq: s.seq})
	}, 32*time.Millisecond)
	// Shared rate meter (same type, interval, and estimator as the
	// WebSocket frontend): the builder feeds it every delta and emits
	// OnStreamStats a few times per second while the round streams.
	speed := streamutil.NewSpeedMeter(streamutil.StatsInterval)

	return streamutil.BuildStreamHandlers(s, streamutil.HandlersConfig{Tokens: batch, Args: argsBatch, Speed: speed})
}

// The StreamAdapter is the TUI's streamutil.Sink: each method emits the
// tea.Msg for one stream event and feeds the session's round buffer. The
// round-buffer feeds (s.rounds, streambuf.RoundSink) run on EVERY event
// regardless of focus — unlike send, which only enqueues while unfocused.
// The buffer must accumulate from round start even while the session is
// the focused one: that is the gap it closes (watch A stream, switch
// away, come back mid-round). The reset/append timing is the shared
// streambuf.RoundSink rule — the same one the web server's wsStreamSink
// calls — so the two hosts cannot drift. A zero sink (adapters built
// without a session, unit tests) is a no-op.
//
// The intentionally-empty methods are the callbacks the TUI does not
// consume: OnStreamOpened/OnStreamActivity are connection-level signals
// with no TUI surface (stall/retry/compacting DO surface — see their
// messages), OnReplyModel has no TUI indicator yet, and
// OnToolOutput/OnToolOutputEnd are unused because the TUI renders only
// the final tool result (no live terminal tabs). They are explicit
// no-ops, not omissions.
var _ streamutil.Sink = (*StreamAdapter)(nil)

func (s *StreamAdapter) OnStart() {
	s.rounds.TurnBegin() // new turn: fresh buffer
	s.send(streamStartMsg{sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnRoundStart() {
	s.rounds.RoundBegin() // new round: completed content is in Messages
	s.send(streamRoundStartMsg{sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnStreamOpened() {}

func (s *StreamAdapter) OnStreamActivity() {}

// OnCompacting flips the strip to a compacting indicator while the
// mid-turn summarization runs (a full silent LLM call).
func (s *StreamAdapter) OnCompacting() {
	s.send(streamCompactingMsg{sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnCondensed(note string) {
	s.send(condensedNoteMsg{note: note, sid: s.owner, seq: s.seq})
}

// OnStreamStall swaps the bare spinner for a "still waiting" label after a
// silent stretch (long prefill, stalled upstream). Informational only.
func (s *StreamAdapter) OnStreamStall() {
	s.send(streamStallMsg{sid: s.owner, seq: s.seq})
}

// OnStreamRetry reports a muted regeneration (streaming retry or the
// non-streaming fallback): the progress label must explain the silent
// stretch instead of leaving the bare spinner.
func (s *StreamAdapter) OnStreamRetry(reason string) {
	s.send(streamRetryMsg{reason: reason, sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnThinkingToken(token string) {
	s.rounds.Thinking(token)
}

// OnStreamStats emits the smoothed token rate for the progress line.
// Timer-driven (see streamutil.SpeedMeter): a few messages per second
// while the round streams, none during tool execution or between turns.
func (s *StreamAdapter) OnStreamStats(toksPerSec float64) {
	s.send(streamStatsMsg{toksPerSec: toksPerSec, sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnToken(token string) {
	s.rounds.Token(token)
}

func (s *StreamAdapter) OnStreamEnd() {
	// Round complete: the assistant message is appended to
	// Messages immediately after, so the buffer only ever
	// carries content a history snapshot would otherwise miss.
	s.rounds.RoundEnd()
	s.send(streamRoundEndMsg{sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnReplyModel(model string) {}

func (s *StreamAdapter) OnToolCallStart(index int, id, name string) {
	s.rounds.ToolStart(index, id, name)
	s.send(streamToolCallMsg{index: index, id: id, name: name, sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnToolCallArgsDelta(index int, id, name, argsDelta string) {
	s.rounds.ToolArgs(index, argsDelta)
}

func (s *StreamAdapter) OnToolCall(tc llm.ToolCall) {
	// The builder flushed the token/args batchers before this call, so
	// the Update thread sees the complete argument accumulation first.
	s.send(streamToolCallFinalMsg{index: tc.Index, tc: tc, sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnRecoverPartialStream() {}

func (s *StreamAdapter) OnToolExecute(name string) {
	s.send(streamToolExecuteMsg{name: name, sid: s.owner, seq: s.seq})
}

func (s *StreamAdapter) OnToolOutput(id, name, command, chunk string) {}

func (s *StreamAdapter) OnToolOutputEnd(id string, success bool) {}

// OnMessageAppended is unused: the TUI owns the whole transcript in-process
// (no attach/fingerprint round-trips), so there is no rendered-index mark to
// keep for a conditional history skip. Explicit no-op, like the live-output
// pair above.
func (s *StreamAdapter) OnMessageAppended(index int, role string) {}

func (s *StreamAdapter) OnToolResult(id, name, result string, success bool) {
	s.send(streamToolResultMsg{id: id, name: name, result: result, success: success, sid: s.owner, seq: s.seq})
}

// appendChatLine adds a line to the chat buffer and updates the viewport.
func (m *Model) appendChatLine(line string) {
	// Move the current last line's wrapping into the prefix so that only
	// the *new* last line needs re-wrapping on subsequent updates.
	if len(m.chatLines) > 0 {
		parts := m.wrapLine(m.chatLines[len(m.chatLines)-1])
		wrapped := strings.Join(parts, "\n")
		// wrappedPrefix already ends with "\n" (or is empty on the first
		// line); appending the wrapped line keeps a single separator.
		m.wrappedPrefix += wrapped + "\n"
		// Keep the incremental viewport prefix in sync (see buildFromPrefix).
		m.prefixLines = append(m.prefixLines, strings.Split(wrapped, "\n")...)
	}
	m.chatLines = append(m.chatLines, line)
	if isUserPromptLine(line) {
		m.tocAppendAnchor(line) // prompt rail: the web's appendTocDot
	}
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

// appendToLastLine appends text to the last line in the chat buffer.
func (m *Model) appendToLastLine(text string) {
	if len(m.chatLines) == 0 {
		m.appendChatLine(text)
		return
	}
	// Only the last line changes — prefix stays unchanged.
	m.chatLines[len(m.chatLines)-1] += text
	m.tocRefreshLastPrompt()
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

// replaceLastLine replaces the last line in the chat buffer.
func (m *Model) replaceLastLine(text string) {
	if len(m.chatLines) == 0 {
		m.chatLines = append(m.chatLines, text)
	} else {
		m.chatLines[len(m.chatLines)-1] = text
	}
	m.tocRefreshLastPrompt()
	// Prefix is unchanged; only the last line may have been replaced.
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamToken(token string) {
	m.closeThinkingBlock()
	if m.streamAssistantBuf.Len() == 0 {
		label := AssistantStyle.Render(assistantLabel)
		m.streamAssistantLine = len(m.chatLines)
		m.appendChatLine(label + " ")
	}
	m.streamAssistantBuf.WriteString(token)
	m.appendToStreamLine(m.streamAssistantLine, token)
	m.bumpContextEstimate(token)
}

// renderStyledBlock renders multi-line text with style applied per line.
// lipgloss pads every line (except the widest) with trailing spaces when a
// multi-line string is rendered in a single call; those padding runs make
// wrapLine (reflow wrap) emit spurious blank visual lines, because reflow
// drops every consecutive space after a forced wrap and the content's own
// newline then fires a second line break. Rendering each line separately
// never pads, so the artifact cannot occur.
func renderStyledBlock(style lipgloss.Style, text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = style.Render(l)
	}
	return strings.Join(lines, "\n")
}

// streamThinkingCache is the incremental render state of the open thinking
// block. styledPrefix holds the styled+wrapped output of every FINALIZED raw
// line — each a pure function of (line, style, width): ansi.Wrap resets its
// wrap state at every newline, so per-line style+wrap is byte-identical to
// the old whole-block rebuild. current is the raw in-progress last line; it
// is re-styled and re-wrapped FROM RAW TEXT on every batch (never extended
// in wrapped form — appending to a wrapped line would break the greedy wrap
// at the seam), so no stale SGR state can ever leak into the wrap (the
// failure mode the old rebuild-from-buffer comment describes).
//
// The tag lives inside the first raw line ("<thinking>..."), exactly as the
// old renderStyledBlock(ThinkingStyle, "<thinking>"+displayBuf) produced it.
// When the buffer ends with a newline, current is "" (the display trims
// trailing newlines, so the last visible line is the one before it).
//
// The chat line holds the PRE-WRAPPED display (see streamThinkingDisplay);
// re-wrapping it at the SAME width is idempotent, but at a CHANGED width it
// would break at the old wrap seams — setViewportContent rebuilds the cache
// and rewrites the line before its full re-wrap for exactly that reason.
type streamThinkingCache struct {
	styledPrefix string
	current      string
}

func (m *Model) handleStreamThinking(token string) {
	// Guard: if tool calls are already in progress, thinking tokens belong
	// before them (OpenAI protocol ensures this ordering).  Silently ignore
	// post-tool-call thinking to avoid placing it below tool call lines.
	if len(m.streamToolCallNames) > 0 {
		return
	}
	m.streamThinkingBuf.WriteString(token)
	m.bumpContextEstimate(token)

	if !m.streamThinkingOpen {
		m.streamThinkingOpen = true
		m.streamThinkingLine = len(m.chatLines)
		m.appendChatLine(ThinkingTagStyle.Render("<thinking>"))
		// Seed the cache: the tag opens the first raw line, which the
		// advance below extends with this token.
		m.streamThinkingCache = streamThinkingCache{current: "<thinking>"}
	}
	// Advance the cache incrementally (O(token) + O(in-progress line))
	// instead of re-copying, re-styling, and re-wrapping the whole block
	// every batch — the old rebuild was O(total thinking so far) per
	// 32ms flush, i.e. O(n²) over a long reasoning stream. The output is
	// byte-identical to that rebuild (see streamThinkingCache).
	m.advanceStreamThinkingCache(token)
	m.renderStreamThinkingLine()
}

// rebuildStreamThinkingCache rebuilds the thinking render cache from the
// full buffer. Called when the block is seeded from a round-buffer snapshot
// (mid-turn join) or after a width change (resize / sidebar drag), where
// the cached per-line output no longer matches the current wrap width.
func (m *Model) rebuildStreamThinkingCache() {
	m.streamThinkingCache = streamThinkingCache{}
	// Mirror the display rule: trailing newlines are trimmed so a buffer
	// ending in "\n" shows no blank trailing line.
	buf := strings.TrimRight(m.streamThinkingBuf.String(), "\n")
	if buf == "" {
		return
	}
	lines := strings.Split(buf, "\n")
	lines[0] = "<thinking>" + lines[0]
	c := &m.streamThinkingCache
	c.current = lines[len(lines)-1]
	if len(lines) > 1 {
		parts := make([]string, 0, len(lines)-1)
		for _, l := range lines[:len(lines)-1] {
			parts = append(parts, m.wrapLine(ThinkingStyle.Render(l))...)
		}
		c.styledPrefix = strings.Join(parts, "\n")
	}
}

// styleWrapLine styles one raw line and wraps it for the current width.
func (m *Model) styleWrapLine(raw string) string {
	return strings.Join(m.wrapLine(ThinkingStyle.Render(raw)), "\n")
}

// advanceStreamThinkingCache folds a fresh token batch into the thinking
// render cache: every newline finalizes the in-progress line into
// styledPrefix (styled+wrapped once), leaving only the new tail to re-render.
func (m *Model) advanceStreamThinkingCache(token string) {
	c := &m.streamThinkingCache
	for len(token) > 0 {
		i := strings.IndexByte(token, '\n')
		if i < 0 {
			c.current += token
			return
		}
		c.current += token[:i]
		wrapped := m.styleWrapLine(c.current)
		if c.styledPrefix == "" {
			c.styledPrefix = wrapped
		} else {
			c.styledPrefix += "\n" + wrapped
		}
		c.current = ""
		token = token[i+1:]
	}
}

// streamThinkingDisplay assembles the pre-wrapped thinking block from the
// cache. The in-progress line is the only part styled+wrapped, so each
// batch costs O(in-progress line) instead of O(total thinking so far).
func (m *Model) streamThinkingDisplay() string {
	c := &m.streamThinkingCache
	switch {
	case c.current != "":
		last := m.styleWrapLine(c.current)
		if c.styledPrefix == "" {
			return last
		}
		return c.styledPrefix + "\n" + last
	case c.styledPrefix != "":
		return c.styledPrefix
	default:
		// Buffer holds only newlines so far: show the bare tag, exactly
		// as the old renderStyledBlock(ThinkingStyle, "<thinking>") did.
		return ThinkingStyle.Render("<thinking>")
	}
}

// renderStreamThinkingLine writes the cached thinking block into its chat
// line.
func (m *Model) renderStreamThinkingLine() {
	m.replaceStreamLine(m.streamThinkingLine, m.streamThinkingDisplay())
}

// closeThinkingBlock finalizes an open thinking line in place (not necessarily
// the last chat line — assistant text or tool calls may have been appended).
func (m *Model) closeThinkingBlock() {
	if !m.streamThinkingOpen {
		return
	}
	m.streamThinkingOpen = false
	var line string
	if m.streamThinkingBuf.Len() > 0 {
		line = renderStyledBlock(ThinkingTagStyle, "<thinking>"+m.streamThinkingBuf.String()+"</thinking>")
	} else {
		line = renderStyledBlock(ThinkingTagStyle, "<thinking></thinking>")
	}
	m.replaceStreamLine(m.streamThinkingLine, line)
	m.streamThinkingBuf.Reset()
	m.streamThinkingCache = streamThinkingCache{}
	m.streamThinkingLine = -1
	// Reset assistant state so content tokens arriving after this thinking
	// block create a new line below it, preserving temporal order.
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
}

// commitChatLineWrite applies one write to chat line idx — appending text
// when append is true, replacing the line otherwise — and settles the
// viewport. This is the streaming prefix-cache invalidation rule in one
// place: wrappedPrefix caches the wrap of everything before the last line,
// so only a write to the LAST chat line keeps it valid (cheap suffix
// rebuild via buildFromPrefix); a write to any earlier line stales the
// prefix and needs the full re-wrap (setViewportContent). The viewport is
// always re-pinned to the bottom, matching streaming behavior.
func (m *Model) commitChatLineWrite(idx int, append bool, text string) {
	if append {
		m.chatLines[idx] += text
	} else {
		m.chatLines[idx] = text
	}
	if idx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()
}

// appendToStreamLine appends text to a tracked streaming line (refresh rule
// in commitChatLineWrite). If the recorded lineIdx is no longer valid (e.g.
// the chat was modified concurrently), fall back to appending to the last
// line so the token is not silently dropped. The mismatch is logged at
// debug level.
func (m *Model) appendToStreamLine(lineIdx int, text string) {
	if lineIdx < 0 || lineIdx >= len(m.chatLines) {
		debuglog.Write("tui/stream", "appendToStreamLine: slot invalid", "stream-slot-lost", map[string]any{
			"lineIdx":  lineIdx,
			"chatLen":  len(m.chatLines),
			"textSize": len(text),
		})
		m.appendToLastLine(text)
		return
	}
	m.commitChatLineWrite(lineIdx, true, text)
}

// replaceStreamLine replaces text in a tracked streaming line (refresh rule
// in commitChatLineWrite). If the recorded lineIdx is no longer valid, fall
// back to the last line and log.
func (m *Model) replaceStreamLine(lineIdx int, text string) {
	if lineIdx < 0 || lineIdx >= len(m.chatLines) {
		debuglog.Write("tui/stream", "replaceStreamLine: slot invalid", "stream-slot-lost", map[string]any{
			"lineIdx":  lineIdx,
			"chatLen":  len(m.chatLines),
			"textSize": len(text),
		})
		m.replaceLastLine(text)
		return
	}
	m.commitChatLineWrite(lineIdx, false, text)
}

func (m *Model) handleStreamToolCall(index int, id string, name string) {
	// Close thinking if open — finalize the block in chat
	m.closeThinkingBlock()
	// Close assistant buffer (text tokens before tool call are shown as-is in chat)
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	m.streamToolCallNames[index] = name
	m.setActiveTool(name)
	m.streamToolCallArgs[index] = &streamToolArgs{}
	m.streamToolCallIDs[index] = id
	m.streamToolCallLines[index] = len(m.chatLines) // appendChatLine will add at this index
	prefix := ToolCallStyle.Render("  →")
	m.appendChatLine(prefix + " " + name)
}

// streamToolDiffRender is the incremental render state of one patch_file
// call's "diff" argument while it streams. Raw args are append-only, so
// every batch extends the value at its tail: valueStart/consumed track how
// much of the value is already unescaped into diff (a trailing backslash is
// held back until its escape pair completes in a later batch), and
// renderedLines/lastRaw hold the rendered finalized lines plus the raw
// in-progress last line — the same finalize-on-newline pattern as
// streamThinkingCache. Per-batch work is O(delta) instead of O(diff).
type streamToolDiffRender struct {
	valueStart    int      // raw offset of the first value byte; -1 = key not found yet
	valueEnd      int      // raw offset just past the closing quote; -1 = value still open
	consumed      int      // raw bytes of the value already unescaped into diff
	diff          []byte   // accumulated unescaped diff text (append-grown: a string accumulator would copy O(diff) per batch)
	rawRendered   int      // bytes of diff already folded into renderedLines/lastRaw
	renderedLines []string // rendered finalized lines
	lastRaw       string   // in-progress last raw line (the caller renders it for display)
}

// diffKeyRaw is the needle advanceToolDiffRender uses to locate the "diff"
// member; a package-level slice avoids converting per batch.
var diffKeyRaw = []byte(`"diff"`)

// advanceToolDiffRender folds the current raw args buffer into the
// per-index incremental diff state, processing only bytes not seen before:
// it locates the "diff" value once, scans for the closing quote from where
// the previous batch stopped, unescapes only the new tail, and finalizes
// newly completed lines into renderedLines. The state is always returned
// (created on first use); while no diff content has arrived, diff is empty.
//
// The locator keeps the legacy whole-buffer extractor's semantics (first
// `"diff"` occurrence; wait for more bytes while the colon/opening quote
// has not arrived) so display behaviour is unchanged. raw is the caller's
// append-grown args buffer, passed by reference so batches never re-copy it.
func (m *Model) advanceToolDiffRender(index int, raw []byte) *streamToolDiffRender {
	st := m.streamToolDiffRender[index]
	if st == nil {
		st = &streamToolDiffRender{valueStart: -1, valueEnd: -1}
		m.streamToolDiffRender[index] = st
	}
	// Locate the start of the "diff" value once.
	if st.valueStart < 0 {
		keyIdx := bytes.Index(raw, diffKeyRaw)
		if keyIdx < 0 {
			return st
		}
		i := keyIdx + len(`"diff"`)
		for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t') {
			i++
		}
		if i >= len(raw) || raw[i] != ':' {
			return st
		}
		i++
		for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t') {
			i++
		}
		if i >= len(raw) || raw[i] != '"' {
			return st
		}
		st.valueStart = i + 1
	}
	// Scan for the closing quote from where the unescape left off (only new
	// bytes are scanned; escaped quotes are skipped as pairs).
	if st.valueEnd < 0 {
		i := st.valueStart + st.consumed
		for i < len(raw) {
			switch raw[i] {
			case '\\':
				i += 2
			case '"':
				st.valueEnd = i + 1
				i = len(raw)
			default:
				i++
			}
		}
	}
	// Unescape the new tail of the value.
	pos := st.valueStart + st.consumed
	chunkEnd := pos
	if st.valueEnd < 0 {
		chunkEnd = len(raw)
	} else if st.valueEnd-1 > chunkEnd {
		chunkEnd = st.valueEnd - 1
	}
	if chunkEnd > pos {
		chunk := raw[pos:chunkEnd]
		i := 0
		for i < len(chunk) {
			if chunk[i] == '\\' {
				if i+1 >= len(chunk) {
					// Trailing backslash: its escape may complete in the
					// next batch — hold it back rather than freezing a
					// literal backslash into the diff.
					break
				}
				switch chunk[i+1] {
				case 'n':
					st.diff = append(st.diff, '\n')
				case 't':
					st.diff = append(st.diff, '\t')
				case '"':
					st.diff = append(st.diff, '"')
				case '\\':
					st.diff = append(st.diff, '\\')
				case 'r':
					// \r is a no-op in the diff; skip
				default:
					st.diff = append(st.diff, chunk[i], chunk[i+1])
				}
				i += 2
				continue
			}
			st.diff = append(st.diff, chunk[i])
			i++
		}
		st.consumed += i
	}
	// Render the new tail: each newline finalizes the in-progress line into
	// renderedLines; the remainder stays raw in lastRaw. tail is a window
	// into st.diff used only within this call, after the appends above — it
	// never observes a later reallocation — and the string conversions copy,
	// so nothing retained aliases the buffer.
	for tail := st.diff[st.rawRendered:]; len(tail) > 0; {
		i := bytes.IndexByte(tail, '\n')
		if i < 0 {
			st.lastRaw += string(tail)
			break
		}
		st.renderedLines = append(st.renderedLines, renderDiffLine(st.lastRaw+string(tail[:i])))
		st.lastRaw = ""
		tail = tail[i+1:]
	}
	st.rawRendered = len(st.diff)
	return st
}

// streamJSONScan incrementally tracks whether the top-level JSON value in
// an append-only args buffer has closed, scanning only bytes not seen
// before. It understands just enough of the grammar — string state,
// backslash escapes, bracket depth — to find the closing brace of the root
// object, so callers can skip whole-buffer json.Unmarshal attempts that
// could not succeed yet (encoding/json pre-scans the entire input for
// validity before failing, O(N) per batch). The scan is monotonic: raw only
// grows, so once closed the buffer stays closed — and bytes after the
// closing brace cannot repair an unparseable prefix — which lets callers
// cache the once-computed parse result for good.
type streamJSONScan struct {
	consumed int  // bytes already scanned
	depth    int  // bracket nesting inside the root value
	inString bool // inside a JSON string
	escaped  bool // previous byte in the current string was an unescaped '\'
	closed   bool // root object's closing brace seen
}

// advance scans the newly arrived tail of buf, updating string and bracket
// state; per batch it is O(new bytes). The first '{' opens the root object
// and the matching '}' closes it — after that the scan is a no-op. Malformed
// input (a stray closing brace at root, a trailing escape) cannot make a
// later batch parseable either, so the worst case is the legacy one: the
// line simply never gains args.
func (s *streamJSONScan) advance(buf []byte) {
	if s.closed {
		return
	}
	for i := s.consumed; i < len(buf); i++ {
		c := buf[i]
		if s.inString {
			switch {
			case s.escaped:
				s.escaped = false
			case c == '\\':
				s.escaped = true
			case c == '"':
				s.inString = false
			}
			continue
		}
		switch c {
		case '"':
			s.inString = true
		case '{', '[':
			s.depth++
		case '}', ']':
			s.depth--
			if s.depth == 0 {
				// End of the root object: later bytes cannot change
				// parseability, so stop scanning for good.
				s.closed = true
				s.consumed = i + 1
				return
			}
			if s.depth < 0 {
				s.depth = 0
			}
		}
	}
	s.consumed = len(buf)
}

// streamToolArgs accumulates one tool call's streamed args plus the derived
// streaming display state, so no batch re-parses the whole buffer. buf is
// append-grown (a string accumulator would copy O(total) per batch); scan
// advances over only the new tail; and the compact (patch_file) and
// formatted (generic) args lines are computed exactly once, when the scan
// sees the top-level object close — while it is open, json.Unmarshal could
// not succeed anyway, so the per-batch re-parse was pure overhead.
type streamToolArgs struct {
	buf         []byte         // accumulated raw args, append-only
	scan        streamJSONScan // incremental completeness scan over buf
	compact     string         // patch_file: formatArgsCompact output ("" = none)
	compactDone bool           // patch_file: compact line finalized (object closed)
	argStr      string         // generic: formatToolArgs output ("" = none)
	genDone     bool           // generic: args line finalized (object closed)
}

func (m *Model) handleStreamToolArgs(index int, id string, delta string) {
	lineIdx, ok := m.streamToolCallLines[index]
	if !ok || lineIdx < 0 || lineIdx >= len(m.chatLines) {
		return
	}
	ab := m.streamToolCallArgs[index]
	if ab == nil {
		ab = &streamToolArgs{}
		m.streamToolCallArgs[index] = ab
	}
	// O(delta) append: the accumulated args are never re-copied per batch
	// (a string += would copy the whole buffer every ~32 ms).
	ab.buf = append(ab.buf, delta...)
	ab.scan.advance(ab.buf)
	raw := ab.buf
	m.bumpContextEstimate(delta)

	// For patch_file: progressively render diff content as it streams in.
	// This avoids a jarring "pop-up" of the entire diff block in the result.
	//
	// The raw args buffer is append-only, so the extracted diff (and its
	// rendered form) only grows at its tail: per batch, at most the previous
	// LAST diff line changes in place and everything after it is new. The
	// extract+render is incremental (advanceToolDiffRender, O(delta)) and
	// the chatLines updates go through the append funnels (appendChatLine(s)
	// + buildFromPrefix, O(new lines)) — the old path re-scanned the whole
	// raw buffer, re-rendered the entire diff, and fell back to
	// setViewportContent (a full re-wrap of the whole conversation) on
	// every 32 ms args batch.
	if m.streamToolCallNames[index] == "patch_file" {
		if st := m.advanceToolDiffRender(index, raw); len(st.renderedLines) > 0 || st.lastRaw != "" {
			// Displayed lines: the finalized rendered lines plus the
			// in-progress last line, if any.
			renderedLines := make([]string, 0, len(st.renderedLines)+1)
			renderedLines = append(renderedLines, st.renderedLines...)
			if st.lastRaw != "" {
				renderedLines = append(renderedLines, renderDiffLine(st.lastRaw))
			}
			prevCount := m.streamToolDiffCount[index]
			newCount := len(renderedLines)
			switch {
			case newCount > prevCount:
				// First time we're showing diff lines: add the top border
				// through the funnel so the prefix cache stays valid.
				if prevCount == 0 {
					m.appendChatLine(DiffMetaStyle.Render("  ╭─ diff ─"))
					m.streamToolDiffStart[index] = len(m.chatLines)
				}
				// At most the previous last diff line grew within the batch;
				// earlier lines are byte-identical and stay untouched.
				if prevCount > 0 {
					lastIdx := m.streamToolDiffStart[index] + prevCount - 1
					if updated := "  " + renderedLines[prevCount-1]; m.chatLines[lastIdx] != updated {
						m.chatLines[lastIdx] = updated
						if lastIdx != len(m.chatLines)-1 {
							// Another stream appended after our last diff
							// line, so this is a middle-line write and the
							// prefix cache is stale: full rebuild.
							m.setViewportContent()
						}
					}
				}
				newLines := make([]string, 0, newCount-prevCount)
				for i := prevCount; i < newCount; i++ {
					newLines = append(newLines, "  "+renderedLines[i])
				}
				m.appendChatLines(newLines)
				m.streamToolDiffCount[index] = newCount
			case newCount == prevCount && prevCount > 0:
				// No new lines: only the last diff line may have grown.
				lastIdx := m.streamToolDiffStart[index] + prevCount - 1
				if updated := "  " + renderedLines[prevCount-1]; m.chatLines[lastIdx] != updated {
					m.commitChatLineWrite(lastIdx, false, updated)
				}
			}
			// newCount < prevCount is unreachable: raw only grows, so the
			// extracted diff and its rendered line count are monotonic.
		}
		// Don't try to parse JSON for the args line; diff values are huge and
		// formatToolArgs would just truncate them. Show a clean compact line.
		//
		// While the top-level object is open the JSON is incomplete, so
		// formatArgsCompact could only return "" anyway (the legacy path
		// proved that by re-unmarshaling the whole diff every batch); the
		// unmarshal therefore runs exactly once, when the scan first sees
		// the object close.
		if ab.scan.closed && !ab.compactDone {
			ab.compact = formatArgsCompact(raw, 120)
			ab.compactDone = true
		}
		prefix := ToolCallStyle.Render("  →")
		toolName := m.streamToolCallNames[index]
		var lineText string
		if ab.compact == "" {
			lineText = prefix + " " + toolName
		} else {
			lineText = prefix + " " + toolName + " " + ToolCallArgsStyle.Render(ab.compact)
		}
		if m.chatLines[lineIdx] != lineText {
			// Rare — the compact line only changes when a non-diff key
			// appears in the JSON. Diff lines sit below the tool-call line,
			// so this is a middle-line write and commitChatLineWrite takes
			// the full-rebuild path.
			m.commitChatLineWrite(lineIdx, false, lineText)
		} else {
			m.viewport.GotoBottom()
		}
		return
	}

	// Only show args once JSON is fully parseable.  Raw / truncated JSON
	// varies in length enough to cause the line to re-wrap and make
	// content below jump when handleStreamToolCallFinal normalises it.
	//
	// Parseability is decided incrementally by the per-index scan (O(delta)
	// per batch): the whole-buffer unmarshal — whose validity pre-scan walks
	// every byte — runs exactly once, when the object first closes, and the
	// formatted args are cached from then on.
	if !ab.scan.closed {
		return
	}
	if !ab.genDone {
		ab.genDone = true
		args, parseErr := parseInlineJSONArgs(raw)
		if parseErr != nil || len(args) == 0 {
			return
		}
		// Format the same way handleStreamToolCallFinal does, so the line is
		// already in its final form when that fires.  This eliminates the
		// jump entirely for multi-key args and only leaves the minimal
		// name→name+args transition for tools whose last arg key completes
		// the JSON.
		ab.argStr = formatToolArgs(args)
	} else if ab.argStr == "" {
		// Finalized as unparseable (or empty): a closed buffer can never
		// become parseable again, so no later batch can change the line.
		return
	}
	argStr := ab.argStr

	// Rebuild the line cleanly from the accumulated buffer so there is a
	// single contiguous SGR wrapper.  Per-delta styling produces interleaved
	// \x1b[0m sequences that destabilise word-wrap, causing the line height
	// to jump when handleStreamToolCallFinal normalises the styling.
	name := m.streamToolCallNames[index]
	prefix := ToolCallStyle.Render("  →")
	line := prefix + " " + name + " " + ToolCallArgsStyle.Render(argStr)
	if m.chatLines[lineIdx] != line {
		m.commitChatLineWrite(lineIdx, false, line)
	} else {
		// argStr is cached, so the line is already final; just keep the
		// viewport pinned like the unchanged-line branch above.
		m.viewport.GotoBottom()
	}
}

// handleStreamToolCallFinal replaces the streaming tool call line with the final
// cleanly-formatted args (from the fully-parsed ToolCall).
func (m *Model) handleStreamToolCallFinal(index int, tc llm.ToolCall) {
	name, ok := m.streamToolCallNames[index]
	if !ok {
		return
	}
	lineIdx, ok := m.streamToolCallLines[index]
	if !ok || lineIdx < 0 || lineIdx >= len(m.chatLines) {
		return
	}
	prefix := ToolCallStyle.Render("  →")
	argStr := formatToolArgs(tc.Args)
	line := prefix + " " + name
	if argStr != "" {
		line += " " + ToolCallArgsStyle.Render(argStr)
	}
	m.commitChatLineWrite(lineIdx, false, line)

	// Capture diff content for patch_file calls so we can render it in the result
	// (fallback if progressive rendering didn't cover the full diff).
	if tc.Name == "patch_file" {
		if diff, ok := tc.Args["diff"].(string); ok && diff != "" {
			m.toolCallDiffs[tc.ID] = diff
			// If we already rendered diff lines progressively, close the border
			// and mark so the result handler skips the block render.
			if m.streamToolDiffCount[index] > 0 {
				m.appendChatLine(DiffMetaStyle.Render("  ╰───────"))
				m.toolDiffShown[tc.ID] = true
			}
		}
	}
}

// parseInlineJSONArgs attempts to parse incomplete streaming JSON args.
// Returns the parsed map on success; nil+error when JSON is not yet complete.
// Takes a byte slice so streaming callers can pass the append-grown args
// buffer without an O(N) string materialization per batch.
func parseInlineJSONArgs(raw []byte) (map[string]any, error) {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 || s[0] != '{' {
		return nil, fmt.Errorf("incomplete")
	}
	var args map[string]any
	err := json.Unmarshal(s, &args)
	return args, err
}

func (m *Model) handleStreamToolResult(id string, name string, result string, success bool) {
	// Collect all new lines first, then append in a batch so the viewport
	// rebuilds once instead of on every appendChatLine call.
	var newLines []string
	// The tool finished; the next indicator phase is "thinking" for the next
	// model round (which re-announces any new tool via handleStreamToolCall).
	m.setActiveTool("")

	newLines = append(newLines, toolResultStatusLine(name, success))

	// For patch_file: render the original diff with colors
	showDiffResult := name == "show_diff" && isDiffContent(result)
	if name == "patch_file" {
		// If diff was already shown progressively during arg streaming,
		// skip the full block render.
		if m.toolDiffShown[id] {
			// Summary only — border was already closed in handleStreamToolCallFinal
			if m.verbose {
				for _, line := range strings.Split(result, "\n") {
					newLines = append(newLines, ToolResultBodyStyle.Render("  │ "+line))
				}
			} else {
				summary := summarizeResult(result, success)
				newLines = append(newLines, DimStyle.Render(fmt.Sprintf("  %s", summary)))
			}
		} else if diff, ok := m.toolCallDiffs[id]; ok && diff != "" {
			newLines = append(newLines, diffBlock(diff)...)
		}
	} else if showDiffResult {
		newLines = append(newLines, diffBlock(result)...)
	} else if m.verbose {
		for _, line := range strings.Split(result, "\n") {
			newLines = append(newLines, ToolResultBodyStyle.Render("  │ "+line))
		}
	} else {
		summary := summarizeResult(result, success)
		newLines = append(newLines, DimStyle.Render(fmt.Sprintf("  %s", summary)))
	}

	// If this already came from a diff path, stop — don't double-append.
	if showDiffResult {
		m.appendChatLines(newLines)
		// Account for tool result tokens in the live context estimate even
		// when the visual block was already rendered during arg streaming.
		m.bumpContextEstimate(result)
		return
	}

	m.bumpContextEstimate(result)
	m.appendChatLines(newLines)
}

// appendChatLines adds multiple lines to chat and rebuilds the viewport once.
func (m *Model) appendChatLines(lines []string) {
	if len(lines) == 0 {
		return
	}
	for _, line := range lines {
		if len(m.chatLines) > 0 {
			parts := m.wrapLine(m.chatLines[len(m.chatLines)-1])
			wrapped := strings.Join(parts, "\n")
			m.wrappedPrefix += wrapped + "\n"
			m.prefixLines = append(m.prefixLines, strings.Split(wrapped, "\n")...)
		}
		m.chatLines = append(m.chatLines, line)
		if isUserPromptLine(line) {
			m.tocAppendAnchor(line)
		}
	}
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

// resetStreamState clears all per-turn streaming buffers and indices. When
// keepToolDiffShown is true the toolDiffShown map is preserved so a patch
// diff rendered in an earlier round is not re-shown in the next round. Pass
// false on new-turn / cancel / error paths to wipe everything.
func (m *Model) resetStreamState(keepToolDiffShown bool) {
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	m.streamThinkingBuf.Reset()
	m.streamThinkingCache = streamThinkingCache{}
	m.streamThinkingOpen = false
	m.streamThinkingLine = -1
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]*streamToolArgs)
	m.streamToolCallIDs = make(map[int]string)
	m.streamToolCallLines = make(map[int]int)
	m.setActiveTool("")
	m.toolCallDiffs = make(map[string]string)
	m.streamToolDiffCount = make(map[int]int)
	m.streamToolDiffStart = make(map[int]int)
	m.streamToolDiffRender = make(map[int]*streamToolDiffRender)
	if !keepToolDiffShown {
		m.toolDiffShown = make(map[string]bool)
	}
}

func (m *Model) handleStreamStart() {
	m.resetStreamState(false)
	m.clearStreamSpeed()
	// Snapshot the last authoritative context usage as the baseline for the
	// live streaming estimate (see bumpContextEstimate).
	m.contextEst.Rebase(m.contextStats.Snapshot.Used)
}

func (m *Model) handleStreamRoundStart() {
	m.resetStreamState(true)
	// Refresh the authoritative context stats before re-basing the live
	// estimate. By the time round N+1 starts, recordTurnUsage has stored
	// round N's exact API prompt_tokens and the tool results are already
	// appended to a.Messages, so ContextStats now reports the true
	// pre-round usage (API baseline + local estimates for the appended
	// messages). Re-basing from the stale end-of-previous-turn mirror
	// instead made the (est.) indicator visibly drop back to the
	// pre-reply level at every round boundary of a multi-round
	// (tool-calling) reply. Re-basing (which discards the accumulated
	// estimate) keeps the (est.) indicator incrementally accurate across
	// multi-round turns.
	m.refreshContextStatsMidTurn()
	m.contextEst.Rebase(m.contextStats.Snapshot.Used)
	// The server re-arms its rate meter per round: drop the previous
	// round's last rate so the thinking indicator never shows a stale one.
	m.clearStreamSpeed()
}

// clearStreamSpeed drops the displayed token rate (turn/round start). The
// focused session's mirror is cleared too so a focus switch cannot restore
// the previous round's rate.
func (m *Model) clearStreamSpeed() {
	m.streamSpeedLine = ""
	if s := m.focusedSession(); s != nil {
		s.streamSpeedLine = ""
	}
}

func (m *Model) handleStreamRoundEnd() {
	// Trim trailing newlines from the assistant content line before
	// finalizing the round, so intermediate display doesn't have trailing
	// blank lines.
	if m.streamAssistantLine >= 0 && m.streamAssistantLine < len(m.chatLines) {
		m.chatLines[m.streamAssistantLine] = strings.TrimRight(m.chatLines[m.streamAssistantLine], "\n")
	}
	m.closeThinkingBlock()
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	// Keep streamToolCallNames / toolCallDiffs until OnRoundStart or turn end so
	// OnToolCall finals and patch diffs still resolve after OnStreamEnd.

	m.setViewportContent()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamEnd() {
	// Trim trailing newlines from the assistant content line before
	// finalizing, so the display doesn't end with a blank line.
	if m.streamAssistantLine >= 0 && m.streamAssistantLine < len(m.chatLines) {
		m.chatLines[m.streamAssistantLine] = strings.TrimRight(m.chatLines[m.streamAssistantLine], "\n")
	}
	m.closeThinkingBlock()
	m.dismissApproval(false)
	m.streaming = false
	m.clearProgress()
	// ContextStats is read-only and local (no provider I/O) — safe on the
	// Update thread once StreamProcessInput has returned.
	m.refreshContextStats()
	if m.agent != nil {
		if err := m.agent.ConsumePersistError(); err != nil {
			m.statusMsg = fmt.Sprintf("Warning: failed to save session: %v", err)
		}
	}
	m.setViewportContent()
	m.viewport.GotoBottom()
}
