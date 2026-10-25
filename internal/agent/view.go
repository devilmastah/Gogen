package agent

import (
	"context"
	"fmt"
	"log"
	"strconv"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// llmTools returns the tool definitions exposed to the model: the built-in
// tools, the feature-gated tools from the single gating policy
// (featureTools), plus any registered MCP server tools. Feature tools are
// appended conditionally (MCP-style) so they have zero registry trace when
// their feature is off.
//
// A registered MCP tool whose name collides with a builtin or feature tool
// SHADOWS it here (its definition is the one the model sees): executeTool
// prefers the registry on name collisions too (featureToolFor), so what the
// model sees and what actually executes always agree — and the model never
// sees duplicate definitions for one name (several APIs reject those).
// Two properties below are load-bearing for provider prompt caching: tool
// definitions lead the request prefix, so any change to this list invalidates
// the whole conversation rather than a tail slice.
//
//   - Nested MCP mode inlines nothing: the single `mcp` gateway tool (from
//     featureTools) is the entire MCP surface, so the list size and its bytes
//     do not depend on how many tools the servers publish or on whether they
//     have connected yet.
//   - A shadowing MCP tool REPLACES the builtin at its own index. Removing the
//     builtin and appending the MCP definition at the end (the previous
//     behavior) shifted every tool before it, so one name collision reordered
//     the whole prefix.
func (a *Agent) llmTools() []llm.Tool {
	var mcpDefs []llm.Tool
	if a.MCPRegistry != nil && !a.MCPNested() {
		mcpDefs = a.MCPRegistry.Definitions()
	}
	// Replacement map: MCP definition by the name it shadows. Consumed as the
	// list is built, so the final loop only appends genuinely new tools.
	replacements := make(map[string]llm.Tool, len(mcpDefs))
	for _, t := range mcpDefs {
		replacements[t.Name] = t
	}
	take := func(name string) (llm.Tool, bool) {
		t, ok := replacements[name]
		if ok {
			delete(replacements, name)
		}
		return t, ok
	}

	features := a.featureTools()
	tools := make([]llm.Tool, 0, len(BuiltinTools())+len(features)+len(mcpDefs))
	for _, t := range BuiltinTools() {
		if replacement, shadowed := take(t.Name); shadowed {
			tools = append(tools, replacement) // same index, MCP schema
			continue
		}
		tools = append(tools, t)
	}
	for _, ft := range features {
		if replacement, shadowed := take(ft.Name); shadowed {
			tools = append(tools, replacement)
			continue
		}
		tools = append(tools, ft.Definition)
	}
	// Non-shadowing server tools keep the registry's (sorted) order and are
	// appended after the builtins, so adding a server never reorders what
	// precedes them.
	for _, t := range mcpDefs {
		if _, pending := replacements[t.Name]; pending {
			tools = append(tools, t)
		}
	}
	return tools
}

// prepareMessages builds the LLM view for the next round, compacting history
// at conversation boundaries when auto-compaction triggers. h carries the
// stream handlers for the round; OnCompacting fires before the summarization
// call so the UI can show compaction progress. It returns an error when the
// pre-flight check cannot make the request fit and the last-resort mode is
// "error" (compact_last_resort=error, Phase 0e): the actionable diagnostic
// is returned instead of letting the provider refuse the request.
func (a *Agent) prepareMessages(ctx context.Context, h *llm.StreamHandlers) ([]llm.Message, error) {
	var view []llm.Message
	if a.Context == nil {
		view = a.Messages
	} else {
		a.Context.EnsureContextLimit(ctx)
		// Compact at a user-message boundary. Compacting while a tool
		// call is still pending can drop that call. A tool result means
		// the call already finished. Smart compact (off by default) also
		// compacts then, once the conversation is over the compact budget,
		// so a long tool loop does not start another think against a full
		// window. Off keeps the user-message boundary only.
		lastRole := ""
		if len(a.Messages) > 0 {
			lastRole = a.Messages[len(a.Messages)-1].Role
		}
		if lastRole == "user" || (lastRole == "tool" && a.smartCompactEnabled()) {
			// Two-tier trigger. The normal tier fires at CompactBudget and
			// honors the failure backoff. The EMERGENCY tier fires at the
			// hard window (ContextLimit - CompactReserveTokens, where the
			// provider refuses the request) and bypasses the backoff, so a
			// large message arriving during backoff still gets a
			// compaction attempt before the request is refused. The
			// progress guard (compactionGuards.emergency) stops a hot loop
			// of repeated failures at an unchanged message count.
			total := a.compactionTokenTotal()
			emergency := a.emergencyCompactDue(total)
			if (a.shouldCompactUsingCounts() && a.compactAttemptDue()) || emergency {
				if h != nil && h.OnCompacting != nil {
					h.OnCompacting()
				}
				var pinned map[int]struct{}
				if a.PinManager != nil {
					pinned = a.PinManager.PinnedSet()
				}
				// Pass the cached per-message counts so the summarization
				// request can be sized without re-tokenizing the middle.
				a.statsMu.RLock()
				counts := append([]int(nil), a.tokenCounts...)
				a.statsMu.RUnlock()
				keep := a.Context.CompactKeepRecentMessages()
				if emergency {
					// The standard split may not save enough (the preserved
					// tail can itself be most of the deficit): lower the
					// keep stepwise, floor 1 (the current user prompt is
					// always preserved verbatim), so the summarized middle
					// covers the deficit. The old messages are included in
					// the middle — summarized, never dropped. -1 (no
					// compactable middle) keeps the configured keep, which
					// makes the attempt a documented no-op.
					needed := total - a.Context.HardLimit() + emergencySummaryAllowance
					if k := a.Context.ChooseCompactKeep(a.Messages, counts, needed); k > 0 {
						keep = k
					}
				}
				a.compactToFit(ctx, pinned, counts, keep, emergency)
			}
		}
		// Cap oversized tool bodies in place on the live message array,
		// spilling the full output exactly like a result arriving now (the
		// forced-compaction stage-1 pass keeps the model-free plain cap). The
		// cached counts are dropped when a body is rewritten. Runs BEFORE the
		// count backfill below: capping drops the cached counts, so backfilling
		// first would be discarded and the pre-flight would re-tokenize the
		// whole view anyway.
		a.capToolResultsForTurn()
		// Backfill a cold per-message count cache (a restored session, or the
		// cache capToolResultsForTurn just dropped) once, after the
		// boundary decision but before the pre-flight measurement, so the
		// pre-flight does not re-tokenize the whole view on every round
		// while the cache stays incomplete. The boundary decision above
		// deliberately runs first: the emergency tier is documented to skip
		// an incomplete cache (total -1) and fall back to the normal tier.
		a.ensureTokenCounts()
		view = a.Messages
	}
	// Stabilize tool args on a.Messages (not view, which may be a copy) so
	// ArgsStabilized is persisted and we skip already-stable messages.
	a.stabilizeToolArgs()

	view = buildSystemView(view, a.WorkingDir, a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode, a.mcpToolHint())

	// Pre-flight: verify the actual outgoing request fits the context
	// window, compacting in place when it would be refused. The boundary
	// check above only sees growth at conversation boundaries; tool
	// results appended mid-loop can push the request past the window
	// between boundaries, and nothing else verifies the outgoing view.
	view, err := a.preflightForcedCompact(ctx, h, view)
	if err != nil {
		return nil, err
	}

	a.recordViewForDrift(view)
	return view, nil
}

// preflightRecheckMargin is how far below the context limit the cached
// total may be before the pre-flight check skips the full view
// tokenization. The cached total (compactionTokenTotal) and the full view
// estimate use the same accounting (per-message counts + wire overhead, or
// the provider's exact prompt_tokens baseline), so a total this far under
// the limit cannot reach it; only within the margin is the view
// tokenized for real. The margin trades a rare redundant tokenization
// against a missed refusal — the provider remains the source of truth.
const preflightRecheckMargin = 2000

// outgoingViewEstimate estimates the wire token cost of the outgoing
// request: the actual view slice (system prompt + canonical history) plus
// the tool definitions. A cheap pre-check (compactionTokenTotal) skips the
// full view tokenization when the conversation is comfortably under the
// limit — tokenizing a large view on every round is too slow; only within
// preflightRecheckMargin of the limit (or on an incomplete count cache) is
// the view tokenized for real. The estimates are approximate (cl100k vs
// the provider's tokenizer): the provider is the source of truth, and
// Phase 3 recovers from a refusal that slips through.
func (a *Agent) outgoingViewEstimate(view []llm.Message) int {
	if a.Context == nil {
		return 0
	}
	if total := a.compactionTokenTotal(); total >= 0 && total < a.Context.ContextLimit()-preflightRecheckMargin {
		return total
	}
	return a.Context.EstimateTokens(view) + contextmgr.EstimateToolTokens(a.llmTools())
}

// rebuildView builds the LLM view from canonical history (system prompt and
// enrichment folded in), the same call prepareMessages makes.
func (a *Agent) rebuildView() []llm.Message {
	return buildSystemView(a.Messages, a.WorkingDir, a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode, a.mcpToolHint())
}

// preflightForcedCompact verifies that the outgoing view fits the context
// window and, when the request would be refused, runs an emergency forced
// compaction in place. It is the last local check before the request
// leaves: it fires only at/above the context limit (the hard window, where
// the alternative is a refused request), never at the compaction budget —
// the boundary tiers own everything below it. The estimates are
// approximate, so the provider remains the source of truth: Phase 3
// recovers from a refusal that slips through.
//
// When the request is still over the window after the forced compaction
// (or the attempt was suppressed/aborted), it hands off to Phase 0e —
// last-resort condensation of the single message that cannot fit. In error
// mode (compact_last_resort=error) the hand-off returns the actionable
// diagnostic as the error instead of letting the provider refuse the
// request; in condense mode a failed condensation leaves the view as-is
// (the provider refusal remains the diagnostic, wrapped by Phase 3).
//
// The forced compaction is two-stage:
//  1. Cap oversized tool results (EnsureToolResultsCapped) — a model-free
//     win; re-measure and stop when capping alone brings the request
//     under the window (no summarization call).
//  2. Summarize with keep=0 (no preserved tail) and the minMiddleTokens
//     guard bypassed: mid-loop there may be only a few messages since the
//     last compaction. The smaller-summary guard still applies.
//
// Strict-shrink requirement: a compaction that does not remove at least
// one message (len(compacted) < len(messages)) is aborted — it saves
// nothing, and a non-shrinking iteration would never converge.
//
// Two guards keep the check from hot-looping: a degenerate window (the
// limit does not exceed the wire overhead alone) can never fit any
// request, so the check stays out of the way — that state is Phase 0e's
// (last-resort condensation) or a misconfiguration, and the provider
// refusal is the diagnostic; and the emergency progress guard
// (compactionGuards.emergency) suppresses a summarization retry at an
// unchanged message count after a failed or aborted attempt (a retry
// would hit the same failure) — growth past that count re-arms it, as in
// the boundary emergency tier.
//
// Protocol safety mid-loop: the tail (the trailing tool group when the
// last message is a tool result, pulled back further by pins, empty
// otherwise) always starts at a non-tool message — adjustCompactTailStart
// walks back over trailing tool messages — so every tool result in the
// tail has its calling assistant message in the tail too, and the summary
// is a plain assistant message with no tool calls.
func (a *Agent) preflightForcedCompact(ctx context.Context, h *llm.StreamHandlers, view []llm.Message) ([]llm.Message, error) {
	if a.Context == nil {
		return view, nil
	}
	limit := a.Context.ContextLimit()
	if limit <= 0 || a.outgoingViewEstimate(view) < limit {
		return view, nil
	}
	// A degenerate window (the limit does not exceed the wire overhead
	// alone) can never fit any request — the system prompt and tool
	// definitions ride on every request — so no local strategy can help
	// and the check stays out of the way: the provider refusal is the
	// diagnostic.
	if limit <= a.wireOverheadTokens() {
		return view, nil
	}
	// The request would be refused. Stage 1: cap oversized tool results —
	// a model-free win. prepareMessages already caps every turn, so this
	// is normally a no-op; it makes the forced compaction self-contained.
	if a.capToolResultsForCompact() {
		view = a.rebuildView()
		if a.outgoingViewEstimate(view) < limit {
			return view, nil
		}
	}
	// Stage 2: forced summarization (keep=0, minMiddleTokens bypassed).
	// The emergency progress guard suppresses a retry at an unchanged
	// message count after a failed or aborted attempt (a retry would hit
	// the same failure); growth past that count re-arms it.
	a.statsMu.RLock()
	guarded := a.compactionGuards.emergency >= len(a.Messages)
	a.statsMu.RUnlock()
	if !guarded {
		if a.runForcedSummarization(ctx, h) {
			view = a.rebuildView()
		}
	}
	if a.outgoingViewEstimate(view) < limit {
		return view, nil
	}
	// Still over after forced compaction (or a suppressed/aborted
	// attempt): Phase 0e — last-resort condensation of the single message
	// that cannot fit the window. In error mode it returns the actionable
	// diagnostic; in condense mode a failed condensation leaves the view
	// as-is (the provider refusal remains the diagnostic, wrapped by
	// Phase 3).
	condensed, err := a.lastResortCondense(ctx, h, view)
	if err != nil {
		return view, err
	}
	if condensed {
		view = a.rebuildView()
	}
	return view, nil
}

// capToolResultsForCompact runs stage 1 of the forced compaction: cap
// oversized tool bodies in place (a model-free win). It returns true when
// a body was rewritten; the cached per-message counts are dropped in that
// case (they are rebuilt on the next ContextStats).
//
// It deliberately uses the plain, no-I/O cap (EnsureToolResultsCapped): the
// forced-compaction path may run in a tight retry loop, and the bodies it
// rewrites are frequently summarized out on the next pass anyway. The
// normal-turn pass that preserves the data is capToolResultsForTurn.
func (a *Agent) capToolResultsForCompact() bool {
	a.statsMu.Lock()
	capped := a.Context.EnsureToolResultsCapped(a.Messages)
	if capped {
		// Tool bodies were rewritten in place, so the cached counts are
		// stale; drop the cache (it is rebuilt on the next ContextStats).
		a.tokenCounts = nil
	}
	a.statsMu.Unlock()
	return capped
}

// capToolResultsForTurn runs the per-turn cap pass with the spill path:
// oversized tool bodies already in history are handled exactly like a result
// arriving now (capToolResult) — the full output is spilled to the session's
// spill dir and the inline body becomes a head+locator+tail preview — with
// the plain cap as the fallback when spilling is unavailable. This is the
// normal-turn counterpart of capToolResultsForCompact, which keeps the
// model-free, no-I/O cap for the forced-compaction stage-1 path.
//
// Spilling writes files, so the pass must NOT run under statsMu (a hot lock
// for ContextStats / history snapshots). It snapshots the candidates under
// the read lock, spills unlocked, then publishes under the write lock guarded
// by countsEpoch: a concurrent reshape (which bumps the epoch) discards the
// pending rewrite instead of applying previews at stale indices, and a
// discarded pass removes the spill files it wrote (they were never published).
// Appends do not shift existing indices, so they do not invalidate the
// candidates.
//
// Returns true when a body was rewritten; the cached per-message counts are
// dropped in that case (they are rebuilt on the next ContextStats).
func (a *Agent) capToolResultsForTurn() bool {
	if a.Context == nil {
		return false
	}
	max := a.Context.SettingsSnapshot().MaxToolResultBytes
	if max <= 0 {
		return false
	}

	type candidate struct {
		idx   int
		orig  string
		label string
	}
	var cands []candidate
	a.statsMu.RLock()
	// The tool-name lookup is built lazily: a turn with no oversized body
	// (the common case) pays only the length scan below. The label only names
	// the spill file, so a missing call falls back to a generic name.
	var names map[string]string
	for i := range a.Messages {
		m := &a.Messages[i]
		if m.Role != "tool" || m.Content == "" || len(m.Content) <= max {
			continue
		}
		// The precise idempotency predicate the arriving-result path uses
		// (alreadyPartial): a spill preview (an intact locator) or a plain cap
		// (the marker terminating the body) is left alone so repeated passes
		// stay no-ops and the prompt prefix stays stable — while a body that
		// merely CONTAINS the marker text (a file quoting it, a log line) is
		// still spilled, exactly like a result arriving now. The coarse
		// HasTruncationMarker the compaction path uses would skip such a body
		// and leave it oversized.
		if alreadyPartial(m.Content) {
			continue
		}
		if names == nil {
			names = make(map[string]string)
			for j := range a.Messages {
				for k := range a.Messages[j].ToolCalls {
					tc := &a.Messages[j].ToolCalls[k]
					if tc.ID != "" && tc.Name != "" {
						names[tc.ID] = tc.Name
					}
				}
			}
		}
		label := names[m.ToolCallID]
		if label == "" {
			label = "tool"
		}
		cands = append(cands, candidate{idx: i, orig: m.Content, label: label})
	}
	epoch := a.countsEpoch
	a.statsMu.RUnlock()

	if len(cands) == 0 {
		return false
	}

	// Spill outside the lock — this is the disk I/O the compaction path
	// deliberately avoids.
	previews := make([]string, len(cands))
	for i := range cands {
		previews[i] = a.capToolResult(cands[i].label, cands[i].orig)
	}

	a.statsMu.Lock()
	applied := a.countsEpoch == epoch
	changed := false
	if applied {
		for i := range cands {
			c := &cands[i]
			p := previews[i]
			if c.idx >= len(a.Messages) || a.Messages[c.idx].Content != c.orig || p == c.orig {
				continue
			}
			a.Messages[c.idx].Content = p
			changed = true
		}
		if changed {
			// Tool bodies were rewritten in place, so the cached counts are
			// stale; drop the cache (it is rebuilt on the next ContextStats).
			a.tokenCounts = nil
		}
	}
	a.statsMu.Unlock()
	if !applied {
		// A reshape bumped the epoch while the spill I/O ran unlocked: the
		// previews are stale and were never published, so remove the files
		// this pass wrote instead of leaking them until session deletion.
		// Outside statsMu — this is disk I/O.
		removeStaleSpills(previews)
	}
	return changed
}

// runForcedSummarization runs stage 2 of the forced compaction: the forced
// summarization (keep=0, minMiddleTokens bypassed) with the strict-shrink
// requirement and the post-success bookkeeping (republish with fresh
// counts, pin remap, clear turn usage, reset save tracking) — the same
// bookkeeping the auto-compaction path applies. It fires h.OnCompacting
// before the summarization call so the UI can show compaction progress.
// It returns true when the history was strictly shrunk and republished.
func (a *Agent) runForcedSummarization(ctx context.Context, h *llm.StreamHandlers) bool {
	if h != nil && h.OnCompacting != nil {
		h.OnCompacting()
	}
	var pinned map[int]struct{}
	if a.PinManager != nil {
		pinned = a.PinManager.PinnedSet()
	}
	a.statsMu.RLock()
	counts := append([]int(nil), a.tokenCounts...)
	a.statsMu.RUnlock()
	compacted, newPins, err := a.Context.Compact(ctx, a.Messages, contextmgr.CompactOptions{
		ViewPrefix:   a.systemPromptPrefix(),
		Counts:       counts,
		Pinned:       pinned,
		Keep:         contextmgr.NoTailKeep,
		Forced:       true,
		Tools:        a.llmTools(),
		AllowedTools: a.AllowedToolNames(),
	})
	if err != nil {
		a.noteCompactFailure(err)
		a.noteProgressFailure(&a.compactionGuards.emergency)
		return false
	}
	if len(compacted) >= len(a.Messages) {
		// Strict-shrink requirement: abort a compaction that removed
		// no message — it saves nothing and would not converge.
		log.Printf("forced compaction aborted: no shrink (%d -> %d messages)", len(a.Messages), len(compacted))
		a.noteProgressFailure(&a.compactionGuards.emergency)
		return false
	}
	// Publish the compacted history with the post-compaction bookkeeping
	// shared with the auto-compaction path (compactToFit).
	a.publishCompaction(compacted, newPins)
	return true
}

// publishCompaction publishes a compacted history with the post-compaction
// bookkeeping: fresh per-message counts (the conversation just shrank, so
// counting it is cheap) published atomically with the messages, pin remap,
// cleared turn usage (lastTurnUsage is no longer representative after
// compaction), and reset save tracking. It returns the new counts so a
// retrying caller (compactToFit) can pass them to the next compaction pass.
func (a *Agent) publishCompaction(compacted []llm.Message, newPins map[int]struct{}) []int {
	counts := contextmgr.CountTokens(compacted)
	a.replaceMessagesWithCounts(compacted, counts)
	if a.PinManager != nil {
		a.PinManager.ReplacePins(newPins)
	}
	a.clearTurnUsage()
	a.resetSaveTracking()
	a.noteCompactSuccess()
	return counts
}

// lastResortCondenseAllowance is the token ceiling assumed for the framed
// condensed message when the pre-check decides whether a condensation is
// worth attempting: a summary of one message is small, and the framed
// marker adds a fixed prefix. The ceiling is deliberately conservative —
// after the summary comes back, the ACTUAL framed size is re-measured
// before the replacement is published, so an over-long summary is refused
// instead of shipped.
const lastResortCondenseAllowance = 2000

// lastResortCondense is Phase 0e — the last-resort condensation for a
// message that cannot fit the context window. It is strictly last-resort:
// callers reach it only after all compaction (the 0d hand-off) has left
// the request provably over the window. It fixes the permanently-stuck
// case — a single user message bigger than the window (e.g. a fresh
// session where the message is the head): firstUserIndex preserves it
// verbatim, there is no middle to summarize, every request is refused.
//
// It finds the single largest condensable message (a text-only user
// message or tool result — not a compaction summary, not already
// condensed) and checks that replacing it with a small framed summary
// would bring the request under the window (the message plus the
// irreducible floor — head, minimal tail, wire overhead — fits). When it
// would, it condenses that message in place via the summarizer, archives
// the original to the session's archive sidecar (Phase 5), and announces
// the condensation in-band (h.OnCondensed).
//
// Returns (true, nil) when a message was condensed in place and
// republished (the caller must rebuild the view). Returns (false, nil)
// when the path does not apply: the request is under the window
// (defensive), no single message is the cause (the floor itself is over —
// the provider refusal stays the diagnostic), the condensation failed or
// produced no shrink, or a failed attempt at this message count is still
// guarded. Returns (false, err) in error mode (compact_last_resort=error):
// err is the actionable diagnostic and the history is left untouched.
func (a *Agent) lastResortCondense(ctx context.Context, h *llm.StreamHandlers, view []llm.Message) (bool, error) {
	if a.Context == nil {
		return false, nil
	}
	limit := a.Context.ContextLimit()
	if limit <= 0 {
		return false, nil
	}
	total := a.outgoingViewEstimate(view)
	if total < limit {
		return false, nil // defensive: strictly last-resort
	}
	msgs, counts := a.messageCounts()
	idx, c := largestCondensableMessage(msgs, counts)
	if idx < 0 {
		return false, nil
	}
	// Fit check: replacing the message with a small framed summary must
	// bring the request under the window.
	if total-c+lastResortCondenseAllowance >= limit {
		return false, nil // the floor itself is over: not a single-message case
	}
	if a.Context.CompactLastResort() != "condense" {
		// Error mode: the actionable diagnostic replaces the raw provider
		// refusal. No attempt is made, so no guard is needed.
		return false, fmt.Errorf("message is ~%d tokens vs a %d-token window; shorten it or start a fresh session (/new)", c, limit)
	}
	// Progress guard: a failed condensation at this message count is not
	// retried until the conversation grows past that count (a retry would
	// hit the same failure).
	a.statsMu.RLock()
	guarded := a.compactionGuards.lastResort >= len(a.Messages)
	a.statsMu.RUnlock()
	if guarded {
		return false, nil
	}

	if h != nil && h.OnCompacting != nil {
		h.OnCompacting()
	}
	summary, err := a.Context.CondenseMessage(ctx, msgs[idx])
	if err != nil {
		log.Printf("last-resort condensation failed: %v", err)
		a.noteProgressFailure(&a.compactionGuards.lastResort)
		return false, nil
	}
	framed := llm.Message{
		Role:       msgs[idx].Role,
		Content:    contextmgr.CondensedMessagePrefix + summary,
		ToolCallID: msgs[idx].ToolCallID,
	}
	framedTok := contextmgr.ComputeMessageTokens(framed)
	if framedTok >= c {
		// No shrink: the summary is as big as the message it replaces.
		// Leave the history untouched (and do not record a failure — a
		// later attempt may produce a smaller summary).
		log.Printf("last-resort condensation aborted: framed summary %d tokens not smaller than the message %d", framedTok, c)
		return false, nil
	}
	if total-c+framedTok >= limit {
		// The actual framed summary is bigger than the allowance the
		// pre-check assumed: condensing would not fit the request. Leave
		// the history untouched.
		log.Printf("last-resort condensation aborted: request would still be over the window (%d - %d + %d >= %d)", total, c, framedTok, limit)
		return false, nil
	}
	// Archive the original (Phase 5 sidecar), condense in place, publish.
	archived := a.archiveShadowedMessage(idx, msgs[idx], c)
	a.condenseMessageAt(idx, framed)
	// lastTurnUsage is no longer representative after condensation.
	a.clearTurnUsage()
	// resetSaveTracking forces a full snapshot on the next save: the
	// in-place content change is invisible to the incremental delta path.
	a.resetSaveTracking()
	a.FlushSession()
	a.noteProgressSuccess(&a.compactionGuards.lastResort)
	if h != nil && h.OnCondensed != nil {
		suffix := " The original was archived."
		if !archived {
			suffix = " Archiving the original failed (session persistence off or sidecar write error)."
		}
		h.OnCondensed(fmt.Sprintf("A message was ~%d tokens vs a %d-token context window; it was condensed to continue.%s", c, limit, suffix))
	}
	return true, nil
}

// messageCounts returns the live messages and their per-message token
// counts: the cached counts when complete, else freshly computed for every
// message. Both slices are copies safe to use after the lock is released.
func (a *Agent) messageCounts() ([]llm.Message, []int) {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	msgs := append([]llm.Message(nil), a.Messages...)
	if a.tokenCounts != nil && len(a.tokenCounts) == len(msgs) {
		return msgs, append([]int(nil), a.tokenCounts...)
	}
	counts := contextmgr.CountTokens(msgs)
	return msgs, counts
}

// largestCondensableMessage returns the index and token count of the
// largest message eligible for last-resort condensation (Phase 0e): a
// text-only user message or tool result that is not a compaction summary
// and not already condensed. Assistant messages are never candidates (their
// tool-call protocol and model attribution must survive verbatim), and
// image-bearing user messages are excluded (the summarizer is text-only).
// Returns (-1, 0) when no message qualifies.
func largestCondensableMessage(msgs []llm.Message, counts []int) (int, int) {
	best, bestTok := -1, 0
	for i, m := range msgs {
		switch m.Role {
		case "user":
			if m.HasImages() || m.Content == "" ||
				contextmgr.IsCompactionSummary(m.Content) || contextmgr.IsCondensedMessage(m.Content) {
				continue
			}
		case "tool":
			if m.Content == "" || contextmgr.IsCondensedMessage(m.Content) {
				continue
			}
		default:
			continue
		}
		if counts[i] > bestTok {
			best, bestTok = i, counts[i]
		}
	}
	return best, bestTok
}

// condenseMessageAt replaces the message at idx in place with the framed
// condensed version and updates the cached count. The index, role, and
// position are preserved — only the content changes — so pins and the
// tool-call/result protocol are untouched. Leaf lock.
func (a *Agent) condenseMessageAt(idx int, framed llm.Message) {
	a.statsMu.Lock()
	a.Messages[idx] = framed
	if a.tokenCounts != nil && len(a.tokenCounts) == len(a.Messages) {
		a.tokenCounts[idx] = contextmgr.ComputeMessageTokens(framed)
	}
	// The content of a cached message changed: bump the epoch so a
	// concurrent ContextStats that computed counts for the older snapshot
	// cannot publish stale numbers.
	a.countsEpoch++
	a.statsMu.Unlock()
}

// handleOverflowError is Phase 3 — the last line of defense that makes
// "stuck" impossible: the pre-flight estimates are approximate (cl100k vs
// the provider's real tokenizer), so a request that looked like it would
// fit can still be refused by the provider. Instead of aborting the turn,
// it recovers in-loop: run the forced compaction (two-stage cap-first,
// keep=0, minMiddleTokens bypassed, strict-shrink) and let the caller
// retry the round once — the next runTurn rebuilds the view from the
// shrunken history.
//
// Returns (true, nil) when the history was shrunk and republished and the
// caller should continue the loop. Returns (false, nil) when the error is
// NOT a context-window refusal or no context manager is configured: the
// caller returns the original error untouched. Returns (false, ctx.Err())
// when the context was cancelled: the user cancelled, and the loop reports
// the cancellation (as the toolRoundCancelled path does) rather than the
// refusal that happened to arrive with it. Returns (false, terminal) when
// recovery is exhausted — a second refusal in the same turn, or a
// failed/aborted forced compaction: terminal is an actionable error that
// wraps (never masks) the original refusal.
//
// Guards against infinite loops: the per-turn counter (overflowRetries,
// reset at Run start) allows at most one recovery per turn, and the
// strict-shrink requirement in runForcedSummarization makes a non-shrinking
// compaction a terminal failure instead of a retry.
func (a *Agent) handleOverflowError(ctx context.Context, h *llm.StreamHandlers, err error) (retry bool, terminal error) {
	if !llm.IsContextWindowError(err) {
		// Strict classifier: non-overflow errors take the untouched
		// err != nil path — even when the context is also cancelled, the
		// original error is surfaced (never masked).
		return false, nil
	}
	if ctx.Err() != nil {
		// No retry on cancellation: the user cancelled, and the run loop
		// surfaces ctx.Err() for that (as the toolRoundCancelled path
		// does).
		return false, ctx.Err()
	}
	if a.Context == nil {
		// Nothing to compact with; surface the original error.
		return false, nil
	}
	if a.overflowRetries.Load() >= 1 {
		// The forced compaction already ran this turn and the shrunken
		// request was refused too: another retry cannot converge.
		return false, a.overflowTerminalError(err)
	}
	// Stage 1: cap oversized tool results — a model-free win. The estimate
	// is wrong here (that is why the provider refused), so capping alone
	// is never trusted to have fixed the request; stage 2 always runs.
	a.capToolResultsForCompact()
	// Stage 2: forced summarization (keep=0, minMiddleTokens bypassed,
	// strict-shrink, post-success bookkeeping identical to the pre-flight
	// path).
	if a.runForcedSummarization(ctx, h) {
		a.overflowRetries.Add(1)
		return true, nil
	}
	// Forced compaction could not shrink the history — the classic case is
	// a fresh session whose single user message is the head (there is no
	// middle to summarize). Phase 0e: last-resort condensation of the one
	// message that cannot fit the window.
	condensed, condErr := a.lastResortCondense(ctx, h, a.rebuildView())
	if condErr != nil {
		// Error mode (compact_last_resort=error): the actionable diagnostic
		// replaces the raw provider refusal.
		return false, condErr
	}
	if condensed {
		a.overflowRetries.Add(1)
		return true, nil
	}
	return false, a.overflowTerminalError(err)
}

// overflowTerminalError builds the actionable terminal error for an
// exhausted Phase 3 recovery: the current size vs the window, what was
// tried, and the /compact and /new suggestions. The original refusal is
// wrapped (never masked) so the provider's own diagnostic stays in the
// chain.
func (a *Agent) overflowTerminalError(cause error) error {
	limit := 0
	if a.Context != nil {
		limit = a.Context.ContextLimit()
	}
	size := a.compactionTokenTotal()
	sizeStr := "unknown"
	if size >= 0 {
		sizeStr = strconv.Itoa(size)
	}
	return fmt.Errorf("context window exceeded: the outgoing request is ~%s tokens but the model window is %d; forced compaction (cap oversized tool results, then summarize the old history) was tried without success. Try /compact to shrink the history, or /new to start a fresh session: %w", sizeStr, limit, cause)
}

// compactToFit runs one auto-compaction pass and, when the post-compaction
// request is still over the compaction budget, repeats it with a shrinking
// preserved tail — keep, keep/2, 1, 0 — up to 3 additional summarization
// calls per turn (port of the harness compactionRetries loop). Each accepted
// pass strictly shrinks the conversation (the smaller-summary guard), so
// the loop always terminates. Extra passes happen only when the first pass
// failed to reach the budget (rare); when the first pass already fits, the
// behavior is exactly the single pass.
//
// Giving up — a failed pass, or an exhausted sequence still over budget —
// records a compact failure so the next turn's attempt is backed off
// instead of immediately repeating the whole sequence.
func (a *Agent) compactToFit(ctx context.Context, pinned map[int]struct{}, counts []int, keep int, emergency bool) {
	// Shrinking-tail sequence: the chosen keep first, then keep/2, 1, 0.
	// Duplicates are dropped so every retry preserves strictly fewer recent
	// messages (a keep of 1 or 0 already leaves at most one / no retries).
	keeps := []int{keep}
	for _, k := range []int{keep / 2, 1, 0} {
		if k < keeps[len(keeps)-1] {
			keeps = append(keeps, k)
		}
	}
	budget := a.Context.CompactBudget()
	for i, k := range keeps {
		// k=0 means "no tail" — map to NoTailKeep so the zero-value
		// CompactOptions.Keep (use default) is not triggered.
		keepOpt := k
		if keepOpt == 0 {
			keepOpt = contextmgr.NoTailKeep
		}
		compacted, newPins, err := a.Context.Compact(ctx, a.Messages, contextmgr.CompactOptions{
			ViewPrefix:   a.systemPromptPrefix(),
			Counts:       counts,
			Pinned:       pinned,
			Keep:         keepOpt,
			Tools:        a.llmTools(),
			AllowedTools: a.AllowedToolNames(),
		})
		if err != nil {
			// A failing summarization call must not be retried on every turn.
			a.noteCompactFailure(err)
			if emergency {
				a.noteProgressFailure(&a.compactionGuards.emergency)
			}
			return
		}
		// Publish with the post-compaction bookkeeping. The fresh
		// counts are carried into the next pass (a nil cache here
		// would make the next turn's shouldCompactUsingCounts fall
		// back to a full EstimateTokens pass; the cache is otherwise
		// only backfilled by ContextStats/doPersist), and the remapped
		// pins become the next pass's input.
		counts = a.publishCompaction(compacted, newPins)
		pinned = newPins
		// Re-measure: stop as soon as the post-compaction request fits.
		total := a.compactionTokenTotal()
		if budget <= 0 || total < budget {
			return
		}
		if i == len(keeps)-1 {
			// Exhausted the shrinking-tail sequence and the request is
			// still over budget: give up into the failure backoff so the
			// next turn does not immediately repeat the whole sequence.
			a.noteCompactFailure(fmt.Errorf("still over budget after %d compaction passes (%d tokens, budget %d)", i+1, total, budget))
			return
		}
	}
}

// systemPromptPrefix returns the system/enrichment messages that precede
// canonical history on the wire (the view minus a.Messages). CompactPinned
// prepends these to the summarization request so the conversation prefix is
// byte-identical to the previous turn and provider prompt caching applies.
// Built without copying the history: the prefix is either empty (the history
// already carries a system message that buildSystemView enriches in place)
// or the single prepended system message.
func (a *Agent) systemPromptPrefix() []llm.Message {
	if len(a.Messages) == 0 {
		return nil
	}
	for _, m := range a.Messages {
		if m.Role == "system" {
			return nil
		}
	}
	return []llm.Message{{
		Role: "system",
		Content: SystemPrompt(a.WorkingDir) +
			buildSystemSuffix(a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode, a.mcpToolHint()),
	}}
}
