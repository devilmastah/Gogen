package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// SetDeepDive sets context_mode for this session: on is deep_dive, off is
// default. The compaction layout and the system prompt's context advice
// follow it from the next model call.
func (a *Agent) SetDeepDive(on bool) {
	if a == nil || a.Context == nil {
		return
	}
	a.Context.UpdateSettingsFunc(func(s contextmgr.Settings) contextmgr.Settings {
		s.DeepDive = on
		return s
	})
}

// baselineState is a consistent snapshot of the per-message token cache and
// the provider prompt_tokens baseline, taken under a single statsMu read
// lock so both compaction checks agree on the same conversation state.
type baselineState struct {
	msgs             []llm.Message
	counts           []int
	complete         bool
	baselineFresh    bool
	baselineTokens   int
	baselineMsgCount int
}

// snapshotBaselineState captures the per-message token cache and the
// provider prompt_tokens baseline under statsMu. The baseline is fresh when
// the provider's prompt_tokens were recorded for a list no larger than the
// current one (the list only grew since that request). A shrunken list
// (rollback) leaves the baseline stale — the API count would over-report, so
// the local estimate is used instead.
func (a *Agent) snapshotBaselineState() baselineState {
	a.statsMu.RLock()
	counts := a.tokenCounts
	msgs := a.Messages
	baselineTokens, baselineMsgCount := a.apiBaselinePromptTokens, a.apiBaselineMsgCount
	a.statsMu.RUnlock()
	return baselineState{
		msgs:             msgs,
		counts:           counts,
		complete:         counts != nil && len(counts) == len(msgs),
		baselineFresh:    baselineTokens > 0 && baselineMsgCount > 0 && baselineMsgCount <= len(msgs),
		baselineTokens:   baselineTokens,
		baselineMsgCount: baselineMsgCount,
	}
}

// shouldCompactUsingCounts reports whether auto-compaction should trigger,
// summing the cached per-message token counts when they are complete to avoid
// re-tokenizing the whole conversation on every turn. Falls back to
// Manager.ShouldCompactWithOverhead (a full EstimateTokens pass) when the
// cache is empty or incomplete (e.g. after a cap pass — capToolResultsForTurn
// or capToolResultsForCompact — drops it; compaction and session restore
// publish fresh counts).
//
// Wire overhead accounting: the per-message counts cover the canonical
// messages only — the system prompt and tool definitions (10-30k tokens)
// ride on every request but are not in a.Messages. When the provider
// prompt_tokens baseline is fresh, it already includes that overhead, so it
// must NOT be added again (double-count trap). When the baseline is absent
// (post-compaction, post-restore, first turn), the wire overhead is added so
// the trigger fires on time instead of tens of thousands of tokens late.
func (a *Agent) shouldCompactUsingCounts() bool {
	if a.Context == nil {
		return false
	}
	snap := a.snapshotBaselineState()
	msgs := snap.msgs
	if !snap.complete {
		overhead := 0
		if !snap.baselineFresh {
			overhead = a.wireOverheadTokens()
		}
		return a.Context.ShouldCompactWithOverhead(msgs, overhead)
	}
	if !a.Context.AutoCompactEnabled() {
		return false
	}
	if len(msgs) <= a.Context.CompactKeepRecentMessages()+1 {
		return false
	}
	return a.compactionTokenTotal() >= a.Context.CompactBudget()
}

// compactionTokenTotal returns the estimated total token count of the
// conversation (system prompt + tool definitions + canonical messages),
// using the same accounting as shouldCompactUsingCounts: the provider's
// exact prompt_tokens baseline when fresh, otherwise the cached per-message
// counts plus the wire overhead. Returns -1 when the per-message count
// cache is incomplete (e.g. after a cap pass drops it) so callers can fall
// back to the full-estimate path.
func (a *Agent) compactionTokenTotal() int {
	snap := a.snapshotBaselineState()
	if !snap.complete {
		return -1
	}
	total := 0
	for _, c := range snap.counts {
		total += c
	}
	// Use the provider's exact prompt_tokens for messages in the last request
	// (cl100k misjudges other tokenizers); estimate only the suffix appended
	// since. The provider count already includes the system prompt and tool
	// definitions, so the wire overhead is NOT added again (double-count
	// trap). Cleared by clearTurnUsage after a compaction.
	if snap.baselineFresh {
		local := 0
		for _, c := range snap.counts[snap.baselineMsgCount:] {
			local += c
		}
		total = snap.baselineTokens + local
	} else {
		// Baseline absent (post-compaction, post-restore, first turn): the
		// local counts cover the canonical messages only, so add the wire
		// overhead (system prompt + tool definitions) they omit.
		total += a.wireOverheadTokens()
	}
	return total
}

// overheadKey identifies a wire-overhead computation: the hash of the system
// prompt content plus a generation of the model-facing tool set. Two calls
// with an equal key must see the same (system prompt, tool definitions)
// content, so the cached token estimate can be reused without re-deriving
// it.
//
// The tool side is deliberately a generation rather than a content
// fingerprint: hashing the tool definitions (contextmgr.ToolDefinitionString
// json.Marshals every parameter schema) on each turn was the dominant cost of
// this cache even on a hit. Every mutation that can change llmTools() bumps
// the generation instead — noteToolsChanged for the agent's own
// reconfigurations, FeatureFlags.generation for a live shared-board/subagent
// toggle — so a stable tool set costs one integer compare per turn.
type overheadKey struct {
	sysHash      uint64
	toolGen      uint64
	flagsVersion uint64
	flags        *FeatureFlags
}

// wireOverheadTokens returns the estimated wire token cost of everything a
// provider request carries besides the canonical messages: the system prompt
// (or only its enrichment suffix when the history already carries a system
// message, whose base content is in the per-message counts) plus all tool
// definitions. The result is cached and recomputed only when the system
// prompt or the model-facing tool set changes (see overheadKey), so the
// per-round prepareMessages call does not re-tokenize or re-serialize the
// tool set.
func (a *Agent) wireOverheadTokens() int {
	var sysContent string
	if prefix := a.systemPromptPrefix(); prefix != nil {
		sysContent = prefix[0].Content
	} else {
		// History carries a system message: only the enrichment suffix is
		// wire overhead (the base content is counted in the messages).
		sysContent = buildSystemSuffix(a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode, a.mcpToolHint())
	}
	h := fnv.New64a()
	h.Write([]byte(sysContent))
	flags := a.flags()
	key := overheadKey{
		sysHash:      h.Sum64(),
		toolGen:      a.toolGen.Load(),
		flagsVersion: flags.generation(),
		flags:        flags,
	}
	a.overheadMu.Lock()
	if key != a.overheadCacheKey {
		tools := a.llmTools()
		tokens := contextmgr.EstimateToolTokens(tools)
		if sysContent != "" && a.Context != nil {
			tokens += a.Context.EstimateTokens([]llm.Message{{Role: "system", Content: sysContent}})
		}
		a.overheadCacheKey = key
		a.overheadTokens = tokens
	}
	t := a.overheadTokens
	a.overheadMu.Unlock()
	return t
}

// compactAttemptDue reports whether the auto-compaction failure backoff has expired.
func (a *Agent) compactAttemptDue() bool {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return time.Now().After(a.compactBackoffUntil)
}

// emergencyCompactDue reports whether the EMERGENCY compaction tier should
// fire: the conversation total has reached the hard window
// (ContextLimit - CompactReserveTokens, where the provider would refuse the
// request) and the progress guard allows a new attempt. Unlike the normal
// tier it ignores the failure backoff — a provider refusal is worse than a
// redundant summarization call — but it refuses to retry a compaction that
// already failed at a message count >= the current one
// (compactionGuards.emergency), so a permanently broken summarization path
// cannot hot-loop an expensive failure on every turn. It also requires at
// least 3 messages: with fewer, there is nothing between the starting
// prompt and the current one to summarize. total < 0 (incomplete count
// cache) never fires: the normal tier's full-estimate fallback covers that
// state.
func (a *Agent) emergencyCompactDue(total int) bool {
	if a.Context == nil || total < 0 {
		return false
	}
	hardLimit := a.Context.HardLimit()
	if hardLimit <= 0 || total < hardLimit {
		return false
	}
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	if len(a.Messages) < 3 {
		return false
	}
	return a.compactionGuards.emergency < len(a.Messages)
}

// noteProgressFailure records a failed compaction-tier attempt at the
// current message count: the tier's progress guard suppresses further
// attempts until the conversation grows past that count (a retry at the
// same count would hit the same failure).
func (a *Agent) noteProgressFailure(guard *int) {
	a.statsMu.Lock()
	if n := len(a.Messages); n > *guard {
		*guard = n
	}
	a.statsMu.Unlock()
}

// noteProgressSuccess resets one compaction tier's progress guard. The
// caller passes the specific tier: the reset semantics are per-tier
// (emergency is reset on any successful compaction, lastResort only on a
// successful condensation), so this is never a blanket reset.
func (a *Agent) noteProgressSuccess(guard *int) {
	a.statsMu.Lock()
	*guard = 0
	a.statsMu.Unlock()
}

// noteCompactFailure records a failed auto-compaction and backs off the next attempt exponentially.
func (a *Agent) noteCompactFailure(err error) {
	log.Printf("auto-compaction failed (backing off): %v", err)
	a.statsMu.Lock()
	if a.compactBackoffDelay == 0 {
		a.compactBackoffDelay = 30 * time.Second
	} else {
		a.compactBackoffDelay *= 2
		if a.compactBackoffDelay > 10*time.Minute {
			a.compactBackoffDelay = 10 * time.Minute
		}
	}
	a.compactBackoffUntil = time.Now().Add(a.compactBackoffDelay)
	a.statsMu.Unlock()
}

// noteCompactSuccess resets the auto-compaction failure backoff and the
// emergency-tier progress guard (NOT the last-resort guard: a successful
// compaction says nothing about a previously failed condensation).
func (a *Agent) noteCompactSuccess() {
	a.statsMu.Lock()
	a.compactBackoffUntil = time.Time{}
	a.compactBackoffDelay = 0
	a.compactionGuards.emergency = 0
	a.statsMu.Unlock()
}

// resetCompactionState clears the conversation-scoped compaction bookkeeping:
// the failure backoff and BOTH per-tier progress guards. Called when the
// conversation is replaced wholesale (session reset / restore): the guards
// record a message count from the OLD conversation, and leaving them set
// would suppress the emergency tier and last-resort condensation on the new
// one until it grew past that stale count — while the backoff (up to 10 min)
// would suppress the normal tier. The wire-overhead cache is deliberately
// untouched (it tracks the system prompt / tool set, not the conversation).
func (a *Agent) resetCompactionState() {
	a.statsMu.Lock()
	a.compactBackoffUntil = time.Time{}
	a.compactBackoffDelay = 0
	a.compactionGuards.emergency = 0
	a.compactionGuards.lastResort = 0
	a.statsMu.Unlock()
}

// CompactHistory manually compacts conversation history at a task boundary.
func (a *Agent) CompactHistory(ctx context.Context) error {
	if a.Context == nil {
		return fmt.Errorf("context management is not configured")
	}
	if len(a.Messages) <= a.Context.CompactKeepRecentMessages()+1 {
		return fmt.Errorf("not enough history to compact (%d messages)", len(a.Messages))
	}
	a.statsMu.RLock()
	cachedCounts := append([]int(nil), a.tokenCounts...)
	a.statsMu.RUnlock()
	compacted, newPins, err := a.Context.Compact(ctx, a.Messages, contextmgr.CompactOptions{
		ViewPrefix:   a.systemPromptPrefix(),
		Counts:       cachedCounts,
		Pinned:       pinnedSet(a.PinManager),
		Tools:        a.llmTools(),
		AllowedTools: a.AllowedToolNames(),
	})
	if err != nil {
		return err
	}
	// A compaction with nothing to summarize returns the history unchanged:
	// in deep dive, when the regions around the current request are already
	// summaries, and in either mode when there is no user message or a pin
	// pulls the tail back to the head. Saying it compacted would be wrong,
	// and publishing would drop the provider's token count for nothing.
	if len(compacted) == len(a.Messages) && summaryBlob(compacted) == summaryBlob(a.Messages) {
		return fmt.Errorf("nothing to compact: compaction left the history unchanged")
	}
	// Publish the compacted history together with its freshly computed token
	// counts (cheap — compaction shrank the conversation) so the cached
	// shouldCompactUsingCounts path stays valid on the next turn.
	counts := contextmgr.CountTokens(compacted)
	a.replaceMessagesWithCounts(compacted, counts)
	if a.PinManager != nil {
		a.PinManager.ReplacePins(newPins)
	}
	// lastTurnUsage is no longer representative after compaction.
	a.clearTurnUsage()
	a.resetSaveTracking()
	return nil
}

// compactionState groups the auto-compaction bookkeeping: the failure
// backoff, the per-tier progress guards, and the cached wire-overhead token
// estimate (overheadMu).
type compactionState struct {
	// Auto-compaction failure backoff (guarded by statsMu): doubles per
	// failure (30s → 10min cap), reset on success.
	compactBackoffUntil time.Time
	compactBackoffDelay time.Duration

	// compactionGuards records, per compaction tier, the message count at
	// which the last attempt failed (guarded by statsMu). An attempt is
	// suppressed while the message count has not grown past the recorded
	// count (the progress guard against a hot loop of identical failures).
	// The RESET semantics are per-tier and deliberately asymmetric:
	// emergency is reset on ANY successful compaction (noteCompactSuccess),
	// lastResort only on a successful last-resort condensation
	// (lastResortCondense). Never reset both at once.
	compactionGuards struct {
		emergency  int
		lastResort int
	}

	// overheadMu guards overheadCacheKey/overheadTokens: the cached wire
	// overhead (system prompt + tool definitions) in tokens. Recomputed only
	// when the system prompt or the model-facing tool set generation changes
	// (see overheadKey), so the per-round prepareMessages call never
	// re-tokenizes or re-serializes the tool set. Deliberately not statsMu:
	// tokenizing is CPU-heavy and the cache is consulted only on the turn
	// goroutine (shouldCompactUsingCounts), not by lock-free readers.
	overheadMu       sync.Mutex
	overheadCacheKey overheadKey
	overheadTokens   int
}
