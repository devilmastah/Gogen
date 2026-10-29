package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TurnContext summarizes context window usage for the current conversation.
type TurnContext struct {
	Snapshot         contextmgr.ContextSnapshot
	LastUsage        *llm.Usage
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

// recordTurnUsage stores the API usage and records a baseline so ContextStats
// can use the API's exact prompt_tokens for the context indicator.
func (a *Agent) recordTurnUsage(u *llm.Usage) {
	if u == nil {
		return
	}
	a.statsMu.Lock()
	a.lastTurnUsage = u
	// Record baseline for accurate context stats: the API's PromptTokens
	// is the exact token count for the view (system prompt + canonical msgs)
	// at this point. Subsequent ContextStats calls will use this as the
	// authoritative baseline and only estimate messages added afterward.
	if u.PromptTokens > 0 {
		a.apiBaselinePromptTokens = u.PromptTokens
		a.apiBaselineMsgCount = len(a.Messages)
	}
	a.statsMu.Unlock()
}

// clearTurnUsage clears last-turn API counters and the baseline used by
// ContextStats. Call when messages are compacted, the model is switched, or
// the session is reset — any time the stored usage no longer represents the
// current conversation state.
func (a *Agent) clearTurnUsage() {
	a.statsMu.Lock()
	a.lastTurnUsage = nil
	a.apiBaselinePromptTokens = 0
	a.apiBaselineMsgCount = 0
	a.statsMu.Unlock()
}

// ContextStats is a read-only probe of current context usage.
// It must not mutate Messages, compact history, or call the provider.
// Safe to call concurrently with a running turn: the shared state it reads
// (Messages, cached token counts, API usage baseline, project profile) is
// snapshotted under Agent.statsMu before tokenization, which happens without
// holding any server lock. Tokenization is slow, so avoid holding
// Server.turnMu across the call where practical; the web /context
// path still runs it under turnMu, which is safe.
//
// When an API baseline is available, Snapshot.Used reflects the API's exact
// prompt_tokens for the messages that were in the last request, plus local
// estimates for any messages appended since then.
func (a *Agent) ContextStats(ctx context.Context) TurnContext {
	// Abort before expensive tokenization when the caller already cancelled
	// (web/TUI interrupt). Returning a minimal snapshot keeps cancel paths
	// from stalling inside OnStart/OnRoundStart context probes.
	if ctx != nil && ctx.Err() != nil {
		a.statsMu.RLock()
		n := len(a.Messages)
		a.statsMu.RUnlock()
		return TurnContext{
			Snapshot: contextmgr.ContextSnapshot{MessageCount: n},
		}
	}

	// Snapshot the shared state (message list, cached token counts, API usage
	// baseline, project profile) under the lock, then release before
	// tokenizing. A concurrent turn goroutine may append messages, extend the
	// cached counts, or record new API usage while ContextStats runs (web
	// readers do not hold turnMu). The message clone is deep so
	// tokenization cannot race in-place stabilization on the live array.
	a.statsMu.RLock()
	msgs := cloneMessagesShallow(a.Messages)
	counts := append([]int(nil), a.tokenCounts...)
	countsEpoch := a.countsEpoch
	lastUsage := a.lastTurnUsage
	baselinePromptTokens, baselineMsgCount := a.apiBaselinePromptTokens, a.apiBaselineMsgCount
	projectProfile := a.projectProfile
	// WorkingDir/Mode are written under statsMu (SetWorkingDir, SetMode), so
	// read them under the same lock: ContextStats runs without the turn lock
	// (web probes) and must not race a concurrent working-dir change or
	// mode switch (data race on a.WorkingDir / a.Mode).
	workingDir := a.WorkingDir
	mode := a.Mode
	a.statsMu.RUnlock()

	view := msgs
	if a.Context != nil {
		// Use cached profile only — do not run DetectProjectProfile here.
		view = buildSystemView(msgs, a.systemPromptBase(workingDir), a.ProjectFilePath, a.EffectiveGuidelines(), projectProfile, mode, a.mcpToolHint())
	}

	var snap contextmgr.ContextSnapshot
	if a.Context != nil {
		snap, counts = a.contextSnapshot(msgs, view, counts, countsEpoch)
	} else {
		snap = contextmgr.ContextSnapshot{
			MessageCount: len(msgs),
		}
	}

	// If we have an API baseline, use it as the authoritative count for
	// messages that were in the last request, then add local estimates
	// for any messages appended since then.
	applyAPIBaseline(&snap, msgs, counts, lastUsage, baselinePromptTokens, baselineMsgCount, a.Context != nil)

	stats := TurnContext{
		Snapshot:  snap,
		LastUsage: lastUsage,
	}

	// Attach last-request API counters for detail views.
	if lastUsage != nil && lastUsage.PromptTokens > 0 {
		stats.PromptTokens = lastUsage.PromptTokens
		stats.CompletionTokens = lastUsage.CompletionTokens
		stats.CachedTokens = lastUsage.CachedTokens
	}

	return stats
}

// contextSnapshot computes the context snapshot, tokenizing the full
// conversation when the counts cache is cold (or extending it for a
// mid-probe burst of appends) and publishing the result under the epoch
// guard so subsequent probes skip re-tokenization. It returns the resolved
// per-message counts too — the caller's API-baseline math needs them.
func (a *Agent) contextSnapshot(msgs, view []llm.Message, counts []int, countsEpoch uint64) (contextmgr.ContextSnapshot, []int) {
	if counts == nil {
		// No cached counts (fresh session, or a compaction/restore just
		// cleared them): compute them once for this snapshot, then publish
		// the result so subsequent probes skip re-tokenization entirely.
		// CountTokens shares one memo across all messages (repeated
		// tool-call names/IDs encode once), which matters most on a large
		// restored session. The computed counts stay valid for the cloned
		// snapshot even if the live list changes before the store; the
		// epoch guard keeps stale counts from ever being published.
		counts = contextmgr.CountTokens(msgs)
		snap := a.Context.SnapshotWithCounts(msgs, view, counts)
		a.publishTokenCounts(counts, len(msgs), countsEpoch)
		return snap, counts
	}
	if len(counts) < len(msgs) {
		// The cache is a valid prefix but messages were appended since it
		// was last extended (ContextStats can race a burst of appends).
		// Compute counts for only the missing suffix locally (one batched
		// pass with a shared memo) and publish the extension under the
		// epoch guard.
		extended := make([]int, len(msgs))
		copy(extended, counts)
		copy(extended[len(counts):], contextmgr.CountTokens(msgs[len(counts):]))
		snap := a.Context.SnapshotWithCounts(msgs, view, extended)
		a.publishTokenCounts(extended, len(msgs), countsEpoch)
		return snap, extended
	}
	// Complete cache: the fast path — no tokenization at all.
	return a.Context.SnapshotWithCounts(msgs, view, counts), counts
}

// publishTokenCounts backfills the in-memory token-count cache so the next
// save or context probe reuses freshly computed counts. The epoch guard
// drops the result if the message list changed underneath us.
func (a *Agent) publishTokenCounts(counts []int, msgCount int, countsEpoch uint64) {
	a.statsMu.Lock()
	if a.countsEpoch == countsEpoch && len(a.tokenCounts) < msgCount {
		a.tokenCounts = append(a.tokenCounts, counts[len(a.tokenCounts):]...)
	}
	a.statsMu.Unlock()
}

// ensureTokenCounts backfills the per-message token-count cache when it does
// not cover every message, using one batched pass with a shared memo. A cold
// cache — a restored session whose snapshot carried no counts, or a cache
// dropped by a cap pass — would otherwise make every round
// re-tokenize the whole view in outgoingViewEstimate and
// shouldCompactUsingCounts; backfilling once restores the cached fast paths.
//
// It runs on the turn goroutine (the only writer of Messages) after the
// boundary compaction decision but before the pre-flight measurement; the
// boundary decision deliberately sees the incomplete cache so the emergency
// tier keeps its documented "total -1 → skip" behavior. The epoch guard still
// drops the result if a concurrent reshape (restore/fork/rollback) moved the
// list underneath us.
func (a *Agent) ensureTokenCounts() {
	if a.Context == nil {
		return
	}
	a.statsMu.RLock()
	n := len(a.Messages)
	complete := n == 0 || (a.tokenCounts != nil && len(a.tokenCounts) == n)
	if complete {
		a.statsMu.RUnlock()
		return
	}
	// cloneMessagesShallow, not a bare slice copy: the counting below runs
	// after statsMu is released, and this is the documented way to read the
	// messages outside the lock (unstabilized ToolCalls are deep-copied so
	// the count cannot race an in-place stabilization).
	msgs := cloneMessagesShallow(a.Messages)
	countsEpoch := a.countsEpoch
	a.statsMu.RUnlock()
	a.publishTokenCounts(contextmgr.CountTokens(msgs), len(msgs), countsEpoch)
}

// applyAPIBaseline uses the last request's exact prompt_tokens as the
// authoritative count for the messages that were in that request, adding
// local estimates for anything appended since. hasContext gates the same way
// the original inline check did (no context manager, no baseline).
func applyAPIBaseline(snap *contextmgr.ContextSnapshot, msgs []llm.Message, counts []int, lastUsage *llm.Usage, baselinePromptTokens, baselineMsgCount int, hasContext bool) {
	if !hasContext || lastUsage == nil || baselinePromptTokens <= 0 || baselineMsgCount <= 0 {
		return
	}
	baseline := baselinePromptTokens
	if n := len(msgs); n > baselineMsgCount {
		// The per-message counts (counts) already cover every message in
		// msgs after the switch above — sum the post-baseline suffix
		// instead of re-tokenizing it (a.Context.EstimateTokens would
		// walk the same messages a second time).
		for _, c := range counts[baselineMsgCount:] {
			baseline += c
		}
	} else if n < baselineMsgCount {
		// The conversation shrank since the baseline was recorded (a
		// truncation/rollback that did not clear the usage baseline): the
		// API count describes a larger request and would over-report.
		// Keep the locally computed estimate instead.
		return
	}
	snap.Used = baseline
	if snap.Limit > 0 {
		snap.Percent = float64(baseline) / float64(snap.Limit)
	}
}

// HandleContextCommand processes /context.
func (a *Agent) HandleContextCommand(ctx context.Context, input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed != "/context" && trimmed != "context" {
		return "", false
	}
	return FormatContextDetail(a.ContextStats(ctx)), true
}

func FormatContextBrief(stats TurnContext) string {
	snap := stats.Snapshot
	if snap.Limit <= 0 && snap.Used <= 0 {
		return ""
	}
	line := fmt.Sprintf("context: %s / %s", formatTokenCount(snap.Used), formatTokenCount(snap.Limit))
	if snap.Limit > 0 {
		pct := int(snap.Percent * 100)
		if pct > 100 {
			pct = 100
		}
		line += fmt.Sprintf(" (%d%%)", pct)
	}
	if stats.CachedTokens > 0 {
		line += fmt.Sprintf(" · %s cached", formatTokenCount(stats.CachedTokens))
	}
	return line
}

// AppendContextBrief adds an estimated context usage line when stats are available.
func AppendContextBrief(ctx context.Context, a *Agent, message string) string {
	if line := FormatContextBrief(a.ContextStats(ctx)); line != "" {
		return message + "\n" + line
	}
	return message
}

func FormatContextDetail(stats TurnContext) string {
	snap := stats.Snapshot
	var b strings.Builder

	fmt.Fprintf(&b, "Context (estimated)\n")
	fmt.Fprintf(&b, "  Used:     %s / %s", formatTokenCount(snap.Used), formatTokenCount(snap.Limit))
	if snap.Limit > 0 {
		pct := int(snap.Percent * 100)
		if pct > 100 {
			pct = 100
		}
		fmt.Fprintf(&b, "  (%d%%)", pct)
	}
	b.WriteString("\n")
	if snap.CompactDisabled {
		fmt.Fprintf(&b, "  Compact:  off\n")
	} else if snap.CompactAt > 0 {
		fmt.Fprintf(&b, "  Compact:  auto at %s\n", formatTokenCount(snap.CompactAt))
	}
	fmt.Fprintf(&b, "  Messages: %d\n", snap.MessageCount)
	if snap.ToolTruncated {
		b.WriteString("  Tool truncation: some results capped\n")
	}
	if stats.PromptTokens > 0 || stats.CompletionTokens > 0 {
		fmt.Fprintf(&b, "  Last turn: %s in / %s out",
			formatTokenCount(stats.PromptTokens),
			formatTokenCount(stats.CompletionTokens))
		if stats.CachedTokens > 0 {
			fmt.Fprintf(&b, " (%s cached)", formatTokenCount(stats.CachedTokens))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatTokenCount(n int) string {
	if n <= 0 {
		return "—"
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	whole := n / 1000
	frac := (n % 1000) / 100
	if frac == 0 {
		return fmt.Sprintf("%dk", whole)
	}
	return fmt.Sprintf("%d.%dk", whole, frac)
}

// cloneMessagesShallow copies a message slice so the result can be read after
// statsMu is released without racing the turn goroutine's in-place
// stabilization (stabilizeToolArgs rewrites ToolCall ArgsStr under statsMu).
// Only unstabilized messages need their ToolCalls deep-copied: stabilization
// is the sole writer of ToolCall fields, it skips messages already marked
// ArgsStabilized, and the wire serializer (messagesToChat) only pins ArgsStr
// for calls whose ArgsStr is still empty/invalid — which stabilization has
// already made valid and trimmed. A message with ArgsStabilized=true therefore
// has ToolCalls that are never mutated again, so sharing their slice is as
// safe as copying it and avoids O(total tool calls) allocation on every
// ContextStats probe / persist snapshot. Callers must hold statsMu (R or W).
func cloneMessagesShallow(msgs []llm.Message) []llm.Message {
	out := append([]llm.Message(nil), msgs...)
	for i := range out {
		if out[i].ArgsStabilized {
			continue
		}
		if len(out[i].ToolCalls) > 0 {
			out[i].ToolCalls = append([]llm.ToolCall(nil), out[i].ToolCalls...)
		}
	}
	return out
}

// sessionStats groups the state ContextStats/SnapshotMessages read without
// the session turnMu: the statsMu lock, the per-message token cache, and the
// provider prompt_tokens baseline.
type sessionStats struct {
	// statsMu serializes the agent state that ContextStats/SnapshotMessages
	// read without the session turnMu: Messages, the cached token counts
	// (tokenCounts), the API-usage baseline (lastTurnUsage,
	// apiBaseline*), projectProfile, UsageAccum, and SessionLabel. Every
	// mutation of these fields from any goroutine must take statsMu. Leaf
	// lock: while holding it, never call out to code that takes turnMu.
	// The reverse order does occur — server paths call
	// SessionLabelSnapshot/ContextStats while holding turnMu — so
	// statsMu critical sections must stay short and never block on I/O or
	// other locks.
	statsMu sync.RWMutex
	// apiBaselinePromptTokens and apiBaselineMsgCount let ContextStats use the
	// API's exact prompt_tokens as the authoritative baseline for Snapshot.Used,
	// only estimating messages added after the last API round.
	apiBaselinePromptTokens, apiBaselineMsgCount int

	// tokenCounts caches per-message token estimates aligned 1:1 with
	// Messages[0:len(tokenCounts)] (a prefix). When len(tokenCounts) ==
	// len(Messages) every message has a cached count and ContextStats /
	// ShouldCompact can avoid re-tokenizing the whole conversation. The cache
	// is filled incrementally: appendMessage extends a complete cache, and
	// ContextStats / doPersist backfill the missing suffix on demand. It is
	// cleared (nil) whenever the message list is replaced wholesale
	// (compaction, restore, fork, reset, rollback).
	//
	// countsEpoch is bumped on every wholesale message-list change so a
	// concurrent ContextStats that computed counts for an older snapshot can
	// detect the list moved under it and skip publishing stale counts.
	tokenCounts []int
	countsEpoch uint64
}
