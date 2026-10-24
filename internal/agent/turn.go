package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/debuglog"
	"gogen/internal/llm"
)

// logToolDefinitions records the exact tool set sent to the provider for this
// round (mode + names + allowlist size). Only runs when debug logging is
// enabled (GOGEN_DEBUG_LOG), so it costs one RLock-guarded path read per
// round otherwise.
//
// A model's own report of "my tool list has no <tool>" is not evidence: models
// (small or heavily quantized ones especially) mis-state and even invent tool
// names. This line settles the question from the client side — what the
// request actually carried — without needing the endpoint's logs.
func logToolDefinitions(mode Mode, tools []llm.Tool, allowed map[string]struct{}) {
	if !debuglog.Enabled() {
		return
	}
	// Mirror the provider's filter (toolsToOpenAI) so "tools" is what the
	// wire actually carries: a nil allowlist sends everything, otherwise a
	// definition not named in it is dropped.
	sent := make([]string, 0, len(tools))
	for _, t := range tools {
		if allowed != nil {
			if _, ok := allowed[t.Name]; !ok {
				continue
			}
		}
		sent = append(sent, t.Name)
	}
	debuglog.Write("agent/tools", "tool definitions sent to the provider", "", map[string]any{
		"mode":  mode.String(),
		"tools": sent,
		"count": len(sent),
		// definitions is the pre-filter list size (builtins + feature tools +
		// MCP); the difference is exactly what the mode removed.
		"definitions": len(tools),
	})
}

// toolRoundOutcome reports how the agent loop must continue after a tool
// round.
type toolRoundOutcome int

const (
	// toolRoundContinue: results were appended; the loop may call the model
	// again.
	toolRoundContinue toolRoundOutcome = iota
	// toolRoundCancelled: the context was cancelled mid-round; the caller
	// aborts with ctx.Err().
	toolRoundCancelled
	// toolRoundStopped: a patch_file failure ended the turn (marker-only
	// diff or exhausted per-turn mismatch budget); the caller returns
	// stopMsg to the host instead of calling the model again.
	toolRoundStopped
)

// toolCanceledMsg is the tool result surfaced for a call that was
// interrupted by user cancellation (or never started before it). Shared
// by the sequential and parallel execution paths.
const toolCanceledMsg = "The operation was cancelled by the user."

// toolCallContext attaches the per-call UI hooks to ctx so the tool can
// stream intermediate output to the UI as it runs, tagged with this call's
// identity (id/name), and collect images into sink. The sink is per-tool
// so each call gets its own identity in the handler; read_image gets a
// per-tool image sink the same way (the handler only collects the image
// into the sink; the synthetic user message is appended after every tool
// message of the round — see appendRoundImages — so the tool-call/result
// protocol is never violated). Shared by the sequential and parallel
// execution paths so both stay in sync.
func toolCallContext(ctx context.Context, h *llm.StreamHandlers, tc llm.ToolCall, sink *imageSink) context.Context {
	if h.OnToolOutput != nil {
		ctx = ContextWithToolOutput(ctx, func(command, chunk string) {
			h.OnToolOutput(tc.ID, tc.Name, command, chunk)
		})
	}
	if h.OnToolOutputEnd != nil {
		ctx = ContextWithToolOutputEnd(ctx, func(success bool) {
			h.OnToolOutputEnd(tc.ID, success)
		})
	}
	return ContextWithImageSink(ctx, sink)
}

// deliverToolResult fires the OnToolResult callback and appends the tool
// result to the transcript. res must already be the final result string
// (callers wrap errors with formatToolError first). Shared by the sequential
// and parallel execution paths so both stay in sync. Images collected during
// the call are NOT appended here: each path drains the round's sinks with
// appendRoundImages only after every tool message of the round is on the
// transcript (an image user message between two tool messages of one
// assistant tool_calls batch breaks the strict pairing providers enforce).
func (a *Agent) deliverToolResult(h *llm.StreamHandlers, tc llm.ToolCall, res string, success bool) {
	if h.OnToolResult != nil {
		h.OnToolResult(tc.ID, tc.Name, res, success)
	}
	a.appendToolResult(tc, res)
	// Tool messages are deliberately NOT reported: they render no bubble of
	// their own, and a completed turn always ends with an assistant message,
	// so the client's rendered-index mark still reaches the transcript's last
	// index without them. A turn that ends on a tool message (stop/error)
	// leaves the mark one behind — a conditional attach then falls back to
	// the snapshot, which is correct, just not optimized.
}

// notifyMessageAppended tells the host the index of a message the turn just
// appended (StreamHandlers.OnMessageAppended): hosts that render live turns
// track a rendered-index mark, so a later conditional attach can prove the
// transcript is current and skip the history snapshot. Reads the index here
// so every call site stays a one-liner; the turn goroutine is the only
// appender, so MessageCount()-1 is the message just appended.
func (a *Agent) notifyMessageAppended(h *llm.StreamHandlers, role string) {
	if h == nil || h.OnMessageAppended == nil {
		return
	}
	h.OnMessageAppended(a.MessageCount()-1, role)
}

// runTurn executes one model round: it checks the context, prepares the
// view, fires the round-start UI callbacks, streams the provider response,
// and records usage. It does NOT process the result — the caller appends
// the assistant message and runs the tool round. Returns the raw provider
// result; on error the round's UI was finished and the session flushed, and
// the caller aborts the turn.
func (a *Agent) runTurn(ctx context.Context, h *llm.StreamHandlers, first bool) (*llm.StreamResult, error) {
	if ctx.Err() != nil {
		finishStreamUI(h)
		a.RepairOrphanToolCalls()
		a.FlushSession()
		return nil, ctx.Err()
	}
	view, err := a.prepareMessages(ctx, h)
	if err != nil {
		// The pre-flight check could not make the request fit and the
		// last-resort mode is "error" (Phase 0e): the diagnostic is
		// returned instead of letting the provider refuse the request.
		finishStreamUI(h)
		a.RepairOrphanToolCalls()
		a.FlushSession()
		return nil, err
	}

	if first && h.OnStart != nil {
		h.OnStart()
	} else if !first && h.OnRoundStart != nil {
		h.OnRoundStart()
	}
	if ctx.Err() != nil {
		finishStreamUI(h)
		a.RepairOrphanToolCalls()
		a.FlushSession()
		return nil, ctx.Err()
	}
	if !first {
		// The model is about to be called again after a tool round: this
		// is where a live "thinking…" state resumes after the tool names.
		a.fireTurnEvent(a.newTurnEvent(TurnPhaseRoundStart))
	}

	a.Provider.SetThinkingLevel(string(a.ThinkingLevel))
	tools, allowed := a.llmTools(), a.AllowedToolNames()
	logToolDefinitions(a.Mode, tools, allowed)
	result, err := a.Provider.GenerateResponseStream(ctx, view, allowed, tools, h)
	if err != nil {
		finishStreamUI(h)
		if ctx.Err() != nil {
			a.RepairOrphanToolCalls()
		}
		a.FlushSession()
		return nil, err
	}
	a.recordTurnUsage(result.Usage)
	a.statsMu.Lock()
	a.UsageAccum.Add(result.Usage)
	a.statsMu.Unlock()
	// The round completed: refresh any context-usage display (the observer
	// samples ContextStats itself, off the turn's critical path).
	a.fireTurnEvent(a.newTurnEvent(TurnPhaseProgress))

	// Attribute the round to the provider-reported model BEFORE the
	// round is finalized: OnStreamEnd (inside finishStreamUI below)
	// writes stream_end, which tells the client the bubble is done. The
	// model must reach the client first so the live bubble can be
	// stamped while it is still current. Fired for every round (not
	// just the final one) so intermediate content+tool rounds get their
	// chip live instead of only via history replay.
	if h.OnReplyModel != nil {
		h.OnReplyModel(result.Model)
	}
	return result, nil
}

// runToolRound executes a round's tool calls: read-only eligible batches run
// concurrently (bounded by maxParallelTools), everything else strictly
// sequentially in model order so mutating tools stay serialized and results
// append in the model's call order. Returns the round outcome and, for
// toolRoundStopped, the final tool-result message to surface as the turn
// outcome. On toolRoundCancelled the tool result protocol was already
// preserved for the next turn.
func (a *Agent) runToolRound(ctx context.Context, h *llm.StreamHandlers, toolCalls []llm.ToolCall) (toolRoundOutcome, string) {
	if a.toolCallsParallelEligible(toolCalls) {
		// Mutating tools (patch_file included) are never parallel-eligible,
		// so the patch-turn stop cannot occur on this path.
		if a.executeToolCallsParallel(ctx, h, toolCalls) {
			// The cancelled round's assistant message and tool results were
			// appended in memory; persist them now. Without this flush the
			// session is left CLEAN with unsaved state, and the shutdown /
			// eviction sweep (FlushPending writes only dirty sessions) and
			// the TUI's /resume (flushes only when dirty) would silently
			// lose the round. Mirrors the sequential cancel path above.
			a.FlushSession()
			return toolRoundCancelled, ""
		}
		return toolRoundContinue, ""
	}
	// One image sink per call, drained only after the round's last tool
	// message (appendRoundImages): an image user message between two tool
	// messages of one assistant tool_calls batch breaks the strict pairing
	// providers enforce. Sinks of calls that never ran stay empty.
	sinks := make([]*imageSink, len(toolCalls))
	for i := range sinks {
		sinks[i] = &imageSink{}
	}
	for i, tc := range toolCalls {
		if ctx.Err() != nil {
			// Preserve a valid tool-call/result protocol for the next turn.
			a.appendCanceledToolResults(toolCalls[i:])
			a.appendRoundImages(sinks)
			finishStreamUI(h)
			a.FlushSession()
			return toolRoundCancelled, ""
		}
		if h.OnToolCall != nil {
			h.OnToolCall(tc)
		}
		if h.OnToolExecute != nil {
			h.OnToolExecute(tc.Name)
		}
		ev := a.newTurnEvent(TurnPhaseTool)
		ev.Tool = tc.Name
		a.fireTurnEvent(ev)

		toolCtx := toolCallContext(ctx, h, tc, sinks[i])
		res, errTool := a.executeTool(toolCtx, tc)
		if errors.Is(errTool, errPatchTurnStop) {
			// The model is stuck in a patch retry loop: stop the turn. This
			// call's result is appended normally; any remaining calls in the
			// round get cancelled placeholders so the tool-call/result
			// protocol stays valid, and the caller returns stopMsg to the
			// host instead of letting the model write another attempt.
			res = formatToolError(res, errTool)
			a.deliverToolResult(h, tc, res, false)
			a.appendCanceledToolResults(toolCalls[i+1:])
			a.appendRoundImages(sinks)
			finishStreamUI(h)
			a.FlushSession()
			return toolRoundStopped, res
		}
		if ctx.Err() != nil {
			// errors.Is, not ==: a tool that wraps the cancellation
			// (fmt.Errorf("...: %w", ctx.Err())) must still read as
			// cancelled, not as a real error.
			if errTool != nil && !errors.Is(errTool, context.Canceled) {
				res = formatToolError(res, errTool)
			} else {
				res = toolCanceledMsg
			}
			a.deliverToolResult(h, tc, res, false)
			a.appendCanceledToolResults(toolCalls[i+1:])
			a.appendRoundImages(sinks)
			finishStreamUI(h)
			a.FlushSession()
			return toolRoundCancelled, ""
		}
		success := errTool == nil
		if errTool != nil {
			res = formatToolError(res, errTool)
		}
		a.deliverToolResult(h, tc, res, success)
	}
	a.appendRoundImages(sinks)
	return toolRoundContinue, ""
}

func finishStreamUI(h *llm.StreamHandlers) {
	if h != nil && h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
}
func ensureToolCallIDs(toolCalls []llm.ToolCall) {
	for j := range toolCalls {
		if toolCalls[j].ID == "" {
			toolCalls[j].ID = newToolCallID()
		}
	}
}

var (
	toolCallIDMu   sync.Mutex
	toolCallIDSeq  uint64
	toolCallIDSeed = uint64(time.Now().UnixNano())
)

func newToolCallID() string {
	toolCallIDMu.Lock()
	toolCallIDSeq++
	seq := toolCallIDSeq
	toolCallIDMu.Unlock()
	return fmt.Sprintf("call_%x_%x", toolCallIDSeed, seq)
}

// stabilizeToolArgs ensures every unstabilized tool call has its ArgsStr set.
// Skipped messages with ArgsStabilized=true — this turns an O(total_tool_calls)
// scan into O(new_tool_calls) per turn.
// It mutates Messages in place (ArgsStabilized, ToolCall ArgsStr), so it runs
// under statsMu to exclude concurrent clones (ContextStats/SnapshotMessages).
func (a *Agent) stabilizeToolArgs() {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	for i := range a.Messages {
		if a.Messages[i].ArgsStabilized {
			continue
		}
		for j := range a.Messages[i].ToolCalls {
			llm.StabilizeToolCallArgs(&a.Messages[i].ToolCalls[j])
		}
		a.Messages[i].ArgsStabilized = true
	}
}
func formatToolError(result string, err error) string {
	if result == "" {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Error: %v\n\nOutput:\n%s", err, result)
}
func (a *Agent) appendToolResult(tc llm.ToolCall, result string) {
	if a.Context != nil {
		result = a.capToolResult(tc.Name, result)
	}
	a.appendMessage(llm.Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: tc.ID,
		CreatedAt:  time.Now().Truncate(time.Millisecond),
	})
}

// StreamProcessInput streams tokens to the handlers as they arrive.
// It returns the final accumulated response or an error.
func (a *Agent) StreamProcessInput(ctx context.Context, input string, h *llm.StreamHandlers) (string, error) {
	return a.StreamProcessInputWithImages(ctx, input, nil, h)
}

// StreamProcessInputWithImages is StreamProcessInput with optional
// user-attached images (vision input). images is nil for text-only prompts.
func (a *Agent) StreamProcessInputWithImages(ctx context.Context, input string, images []llm.ImageInput, h *llm.StreamHandlers) (out string, err error) {
	// Downscale user-attached images before they join the transcript: a
	// pasted screenshot is re-uploaded on every subsequent request, so the
	// attached size is paid once per round (see image_scale.go).
	a.appendMessage(llm.Message{
		Role:      "user",
		Content:   input,
		Images:    normalizeImages(images),
		CreatedAt: time.Now().Truncate(time.Millisecond),
	})
	// If the session doesn't have a label yet, derive one from the first user message.
	if a.SessionLabelSnapshot() == "" {
		a.setSessionLabel(llm.SessionLabel(a.Messages))
	}
	if err := a.requireModelSelected(ctx); err != nil {
		// The turn cannot start: the just-appended user message is dropped
		// instead of being sent to a provider with no usable model. Roll it
		// back in memory WITHOUT writing anything: the session must stay
		// exactly as it was. A fresh session must not get a file at all
		// (the old pre-check flush created one, and this rollback flush then
		// overwrote it with an empty snapshot — the "saved as an empty
		// session" bug, where a 0-message session appeared in the list and
		// became the restore target on the next startup), and a session
		// with prior content already has that content on disk.
		a.truncateMessages(1)
		// Re-derive the label from what remains only when it was derived
		// from the conversation: a session whose only message failed must
		// not keep a stale title. A deliberate rename (RenameSession /
		// session_rename) is authoritative — re-deriving it would clear the
		// rename marker and the next save would silently replace the user's
		// custom title with the first-message text.
		a.statsMu.RLock()
		renamed := a.labelRenamed
		a.statsMu.RUnlock()
		if !renamed {
			a.setSessionLabel(llm.SessionLabel(a.Messages))
		}
		a.resetSaveTracking()
		return "", err
	}
	// Persist the user message before the turn runs so a failed/cancelled
	// turn does not drop it. Deliberately AFTER the model check: a turn
	// that cannot start must not write anything (see the rollback above) —
	// the message is dropped there, so persisting it first would leave an
	// empty session file behind once it was rolled back.
	a.FlushSession()

	if h == nil {
		h = &llm.StreamHandlers{}
	}
	// Chain the turn-status observer (if one is installed) onto the
	// context-maintenance / stream-hiccup callbacks, then bracket the whole
	// turn with observer events: Started fires only once the turn is really
	// running (the model check above passed), Ended on every exit from here
	// on. The deferred event reads the named result so a failed turn reports
	// its error.
	h = a.observeStreamHandlers(h)
	a.fireTurnEvent(a.newTurnEvent(TurnPhaseStarted))
	defer func() {
		ev := a.newTurnEvent(TurnPhaseEnded)
		ev.Err = err
		ev.Cancelled = err != nil && ctx.Err() != nil
		a.fireTurnEvent(ev)
	}()
	// Per-turn patch retry budget: a model that fails patch_file three times
	// in a row within this turn is looping; the turn is stopped instead of
	// letting it retry indefinitely.
	a.patchTurnStrikes.Store(0)
	// Per-turn context-window refusal recovery budget (Phase 3): at most
	// one forced-compaction + retry per turn.
	a.overflowRetries.Store(0)
	// Ghost-round recovery budget: at most one automatic retry of a round
	// the model ended without usable output. The counter tracks
	// CONSECUTIVE ghosts and resets after any successful round (see the
	// tool-call branch); the turn-start reset covers the first round.
	a.ghostRetries.Store(0)
	for first := true; ; first = false {
		result, err := a.runTurn(ctx, h, first)
		if err != nil {
			// Phase 3: a provider context-window refusal recovers in-loop
			// (forced compaction + one retry) instead of aborting the turn.
			// Non-overflow errors, a cancelled context, and an exhausted
			// recovery budget all fall through to the original error path.
			if retry, terminal := a.handleOverflowError(ctx, h, err); retry {
				continue
			} else if terminal != nil {
				return "", terminal
			}
			return "", err
		}

		if len(result.ToolCalls) == 0 {
			finishStreamUI(h)
			// Deliberately no OnRecoverPartialStream here, unlike the tool
			// branch below: the callback exists to reset UI state after a
			// stream error mid-tool-call, and no consumer is registered for
			// content recovery (the TUI wires a no-op, the web server wires
			// nothing). Round-end events stay single-fired — firing it here
			// too would double-deliver on every recovered content turn.
			// A result with no content, no refusal, and no tool calls is a
			// truncated turn (e.g. finish_reason="length" after consuming the
			// output budget on reasoning, or a "stop" right after
			// reasoning-only chunks). Persisting it would leave a ghost
			// assistant message that renders as an empty reply, pollutes later
			// turns, and becomes a fork point. Providers emit this
			// transiently, so the round is retried once in-loop before the
			// error surfaces to the user. The budget counts CONSECUTIVE
			// ghost rounds: any successful round (notably a tool-call
			// round, which continues the loop) resets it, so a long turn
			// gets a fresh retry after each recovery — only back-to-back
			// ghosts exhaust the cap. Nothing was appended for the ghost
			// round, so the retried request re-sends the identical view.
			// The provider-reported finish reason is included when
			// known so exhausted retries are diagnosable without
			// provider-side logs ("length" = output budget exhausted,
			// "stop" = stream ended after reasoning-only chunks).
			if result.Content == "" && result.Refusal == "" {
				if a.ghostRetries.Add(1) <= 1 {
					continue
				}
				if result.FinishReason == "" {
					return "", fmt.Errorf("model returned no output (response was truncated mid-reasoning); please try again")
				}
				return "", fmt.Errorf("model returned no output (finish_reason=%q; response was truncated mid-reasoning); please try again", result.FinishReason)
			}
			a.appendMessage(llm.Message{
				Role:      "assistant",
				Content:   result.Content,
				Reasoning: result.Reasoning,
				Refusal:   result.Refusal,
				CreatedAt: time.Now().Truncate(time.Millisecond),
				Model:     result.Model,
			})
			a.notifyMessageAppended(h, "assistant")
			a.FlushSession()
			if result.Content != "" {
				return result.Content, nil
			}
			// Refusal is user-visible when the model declined without content.
			return result.Refusal, nil
		}

		// A tool-call round is a successful round: the model produced
		// usable output, so the consecutive-ghost counter restarts. Without
		// this, a long multi-round turn would spend its single retry on an
		// early transient ghost and hard-fail on a later one.
		a.ghostRetries.Store(0)

		if h.OnStreamEnd != nil {
			h.OnStreamEnd()
		}

		if result.PartialStream && h.OnRecoverPartialStream != nil {
			h.OnRecoverPartialStream()
		}

		ensureToolCallIDs(result.ToolCalls)
		for i := range result.ToolCalls {
			llm.StabilizeToolCallArgs(&result.ToolCalls[i])
		}

		a.appendMessage(llm.Message{
			Role:      "assistant",
			Content:   result.Content,
			Reasoning: result.Reasoning,
			Refusal:   result.Refusal,
			ToolCalls: result.ToolCalls,
			CreatedAt: time.Now().Truncate(time.Millisecond),
			Model:     result.Model,
		})
		a.notifyMessageAppended(h, "assistant")

		switch outcome, stopMsg := a.runToolRound(ctx, h, result.ToolCalls); outcome {
		case toolRoundStopped:
			// A patch_file failure ended the turn (marker-only diff or the
			// per-turn mismatch budget was exhausted): return the final tool
			// result as the turn outcome so the host shows why the turn
			// ended without calling the model again.
			return stopMsg, nil
		case toolRoundCancelled:
			return "", ctx.Err()
		}
		a.persistSession()
	}
}
func (a *Agent) appendCanceledToolResults(toolCalls []llm.ToolCall) {
	const msg = "Tool execution was skipped because the user cancelled the operation."
	for _, tc := range toolCalls {
		a.appendToolResult(tc, msg)
	}
}

// toolCallsParallelEligible reports whether every tool call in the batch can
// run concurrently within the turn: all are builtin read-only tools and none
// is shadowed by an MCP tool of the same name (MCP side effects are unknown,
// so MCP tools always stay sequential).
func (a *Agent) toolCallsParallelEligible(toolCalls []llm.ToolCall) bool {
	if len(toolCalls) < 2 {
		return false
	}
	if a.MCPRegistry != nil {
		if names := a.MCPRegistry.ToolNames(); names != nil {
			for _, tc := range toolCalls {
				if _, ok := names[tc.Name]; ok {
					return false
				}
			}
		}
	}
	for _, tc := range toolCalls {
		if !parallelSafeTools[tc.Name] {
			return false
		}
	}
	return true
}

// executeToolCallsParallel runs every tool call concurrently, bounded by
// maxParallelTools, then fires OnToolResult callbacks and appends results in
// model order so the tool-call/result protocol stays valid (every tool_call
// gets a matching tool result). The round's images are appended only after
// the last result (appendRoundImages). It returns true when the turn was
// cancelled during execution; in that case completed results are preserved
// and interrupted calls read as cancelled.
func (a *Agent) executeToolCallsParallel(ctx context.Context, h *llm.StreamHandlers, toolCalls []llm.ToolCall) bool {
	type execResult struct {
		res  string
		err  error
		done bool // true when the tool returned a result (before batch cancellation)
	}
	results := make([]execResult, len(toolCalls))
	// Per-tool-call image sinks, mirroring the sequential path: read_image
	// handlers collect images into their call's sink, and the round's sinks
	// are drained in model order after the LAST tool result below
	// (appendRoundImages) — the transcript reads tool(result) × N →
	// user(image) …, never an image between two tool messages.
	sinks := make([]*imageSink, len(toolCalls))
	for i := range sinks {
		sinks[i] = &imageSink{}
	}

	if ctx.Err() != nil {
		// The caller already appended the assistant tool_calls message, so
		// the batch must still be closed before returning (mirrors the
		// sequential loop-top cancel): one cancelled placeholder per call.
		a.appendCanceledToolResults(toolCalls)
		return true
	}
	for i := range toolCalls {
		tc := toolCalls[i]
		if h.OnToolCall != nil {
			h.OnToolCall(tc)
		}
		if h.OnToolExecute != nil {
			h.OnToolExecute(tc.Name)
		}
		ev := a.newTurnEvent(TurnPhaseTool)
		ev.Tool = tc.Name
		a.fireTurnEvent(ev)
	}

	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for i := range toolCalls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = execResult{err: context.Canceled}
				return
			}
			defer func() { <-sem }()
			// Attach the live-output sink (if any) and the per-call image
			// sink to this call's context, exactly like the sequential path
			// in runToolRound, so read-only commands and searches running in
			// parallel stream intermediate chunks to the UI tagged with this
			// call's identity (id/name).
			tc := toolCalls[i]
			toolCtx := toolCallContext(ctx, h, tc, sinks[i])
			res, err := a.executeTool(toolCtx, tc)
			results[i] = execResult{res: res, err: err, done: true}
		}(i)
	}
	wg.Wait()

	cancelled := ctx.Err() != nil
	for i, tc := range toolCalls {
		res, errTool := results[i].res, results[i].err
		// Mirror runToolRound's per-call semantics: a tool that completed
		// before the cancellation keeps its real result; only a call that
		// was interrupted (returned — or returned an error wrapping —
		// context.Canceled; errors.Is covers the wrapped form) or never
		// ran (still waiting to start when the batch finished) reads as
		// cancelled.
		if errors.Is(errTool, context.Canceled) || (cancelled && !results[i].done) {
			a.deliverToolResult(h, tc, toolCanceledMsg, false)
			continue
		}
		success := errTool == nil
		if errTool != nil {
			res = formatToolError(res, errTool)
		}
		a.deliverToolResult(h, tc, res, success)
	}
	// The batch is closed (every call has a result above); the images come
	// last so no user message splits the tool messages of the assistant's
	// tool_calls message.
	a.appendRoundImages(sinks)
	return cancelled
}

// RepairOrphanToolCalls appends cancelled tool-result placeholders for any
// trailing assistant tool_calls that lack matching tool messages. Call on
// cancel/shutdown so the next turn keeps a valid tool-call/result protocol.
// Returns true when messages were modified.
//
// The scan is a single backward pass: seen tracks the tool-result IDs in the
// contiguous run of tool messages immediately after the current position
// (matching the previous per-message forward scan, which broke at the first
// non-tool message). Any other message resets the run.
func (a *Agent) RepairOrphanToolCalls() bool {
	if a == nil || len(a.Messages) == 0 {
		return false
	}
	modified := false
	seen := make(map[string]struct{})
	for i := len(a.Messages) - 1; i >= 0; i-- {
		msg := a.Messages[i]
		switch {
		case msg.Role == "tool":
			if id := msg.ToolCallID; id != "" {
				seen[id] = struct{}{}
			}
		case msg.Role == "assistant" && len(msg.ToolCalls) > 0:
			var missing []llm.ToolCall
			for _, tc := range msg.ToolCalls {
				if _, ok := seen[tc.ID]; !ok {
					missing = append(missing, tc)
				}
			}
			if len(missing) > 0 {
				a.appendCanceledToolResults(missing)
				modified = true
			}
			// This assistant message is itself a non-tool message, so the
			// tool-result run it just consumed does not apply to any earlier
			// assistant tool-call message (the old forward scan broke on it).
			seen = make(map[string]struct{})
		default:
			seen = make(map[string]struct{})
		}
	}
	return modified
}

// turnCounters groups the retry/strike counters the agent loop uses to bound
// model failure loops (patch retries, context-window overflow recovery,
// ghost rounds).
type turnCounters struct {
	// patchFailStreak counts consecutive patch_file failures so the agent loop
	// can steer the model away from retrying the same stale diff indefinitely.
	patchFailStreak atomic.Int32

	// patchTurnStrikes counts consecutive patch_file mismatch failures within
	// a single turn so the agent loop can hard-stop a model stuck in a patch
	// retry loop. Reset at the start of every turn; reaching the limit aborts
	// the turn (runToolRound returns toolRoundStopped), unlike
	// patchFailStreak which only decorates the error with advice.
	patchTurnStrikes atomic.Int32

	// patchStrikeKey remembers the failure report of the last patch_file
	// mismatch so patchTurnStrikes only accumulates while the SAME diff keeps
	// failing (same target, same mismatch). A model iterating across
	// different files or diffs is making progress and must not be stopped.
	patchStrikeKey atomic.Value // string

	// overflowRetries counts the provider context-window refusals already
	// recovered in the current turn (Phase 3, handleOverflowError). Reset
	// at the start of every turn; capped at 1 — a second refusal in the
	// same turn (or a failed forced compaction) returns the actionable
	// terminal error instead of retrying again.
	overflowRetries atomic.Int32

	// ghostRetries counts CONSECUTIVE truncated-turn (ghost) rounds — a
	// stream that ends with reasoning but no content, refusal, or tool
	// calls — since the last successful round. Providers emit this
	// transiently (e.g. a finish_reason="stop" right after
	// reasoning-only chunks), so the round is retried once in-loop instead
	// of failing the turn. Any successful round (content, refusal, or tool
	// calls) resets the counter, so a long multi-round turn gets a fresh
	// retry after each recovery; the cap only stops a model that ghosts
	// back-to-back — a second consecutive ghost round surfaces the
	// "model returned no output" error to the user.
	ghostRetries atomic.Int32
}
