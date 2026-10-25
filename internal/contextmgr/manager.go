package contextmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gogen/internal/config"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
)

// SummaryPrefix marks messages produced by compaction. Summaries are stored
// as assistant-role messages today; the prefix also identifies legacy
// user-role summaries, which must never be treated as real user messages
// (firstUserIndex, PinManager).
const SummaryPrefix = "[Session summary — earlier conversation condensed]\n"
const maxSummarizeDepth = 8

// CondensedMessagePrefix marks a message that the last-resort condensation
// (Phase 0e) replaced in place: the original was too large for the context
// window and was archived to the session's archive sidecar. The marker
// makes a condensed message ineligible for a second condensation (the
// idempotency guard that keeps the last-resort path from re-firing on the
// same message) and identifies it to the UI.
const CondensedMessagePrefix = "[Message condensed — the original was too large for the context window and was archived]\n"

// ErrSummaryNotSmaller is returned by CompactPinned/CompactPinnedWithKeep when
// the framed summary (SummaryPrefix + summary) is not strictly smaller than
// the summarized middle. The compaction is refused without mutating history:
// a summary as big as what it replaced saves nothing, and in the emergency
// fit loop a non-shrinking iteration would never converge — refusing it
// guarantees every accepted iteration strictly shrinks the conversation.
var ErrSummaryNotSmaller = errors.New("summary not smaller than the summarized region")

// defaultMinMiddleTokens is the smallest middle (estimated tokens) worth
// summarizing. Compacting a smaller middle makes the model echo the one or
// two messages it is asked to summarize — a conversation reply, not a recap —
// so the compaction is refused instead. Manager.minMiddleTokens holds the
// effective value (tunable to 0 in tests that exercise tiny histories).
const defaultMinMiddleTokens = 500

// WarnThreshold is the fraction of the context limit at which the
// near-compact warning (web banner, statusbar color) appears. It is
// deliberately decoupled from the auto-compaction trigger (CompactThreshold)
// so the UI warns before compaction fires, giving the user lead time to
// compact manually. The TUI statusbar and the web banner both read this
// constant so the thresholds cannot drift apart.
const WarnThreshold = 0.75

// summaryFraming is the shared opening line of both summarization prompts
// (summaryInstruction and the summarizeText prompt). The framing keeps the
// model from continuing the conversation — an earlier instruction without it
// made models reply to the head question instead of summarizing.
const summaryFraming = "This is a summarization task, not a conversation turn."

// summaryPreserveList is the shared preservation list embedded in both
// summarization prompts.
const summaryPreserveList = `Preserve:
- The user's original goal and any changes to it
- Files touched and why they matter
- Key findings from tool results (errors, line numbers, search hits)
- Technical decisions made
- Errors encountered and how they were fixed
- Pending work and the current state`

// summaryInstruction is the trailing USER-role message appended to the
// summarization request. The request carries the wire prefix (system/
// enrichment messages) plus everything up to the compaction tail start, so
// the provider can serve the bulk of it from its prompt cache (the prefix
// is byte-identical to the last real turn). The instruction is a user
// message because a trailing system message is not portable: OpenAI-style
// endpoints tolerate it, but Anthropic rejects system-role messages inside
// the message list and strict Jinja chat templates (Qwen3 and similar)
// raise "System message must be at the beginning" with a 500. The task
// framing (summaryFraming) keeps the model from continuing the conversation
// — an earlier trailing user instruction without that framing made models
// reply to the head question instead of summarizing. The model is asked to
// summarize only the middle — the first user message (the head) and the tail
// are preserved verbatim by construction, so they are excluded here and the
// cut messages are never mentioned.
const summaryInstruction = summaryFraming + ` Do not reply to any question in the transcript above. Do not continue the conversation. Do not quote or repeat any message verbatim.

Summarize everything after the first user message in the transcript above. The first user message and any leading context before it are preserved verbatim and excluded from this summary.

` + summaryPreserveList + `

Be concise but keep the facts the agent needs to continue without re-reading the summarized messages. Write a concise factual recap in the third person covering only what is in the transcript. Do not invent information. The recap will be inserted into the conversation in place of the summarized messages.

Output only the recap text — no preamble, no markdown headings, no dialogue.`

// Settings controls context window management.
type Settings struct {
	ContextLimit              int
	CompactThreshold          float64
	CompactKeepRecentMessages int
	MaxToolResultBytes        int
	CompactReserveTokens      int
	// CompactLastResort is the last-resort condensation mode (Phase 0e)
	// for a message that cannot fit the context window even after all
	// compaction: "condense" (default) condenses the message in place via
	// the summarizer; "error" makes the caller return a diagnostic instead.
	// normalizeSettings clamps any other value to "condense".
	CompactLastResort string
	// SmartCompact compacts during a tool loop, before the next tool runs,
	// once the conversation is over the compact budget. It also stops a
	// thinking stream while there is still room to summarize. Off (the
	// default) leaves compaction at a user-message boundary.
	SmartCompact bool
}

// DefaultSettings returns defaults; ContextLimit 0 means resolve from the provider at runtime.
func DefaultSettings() Settings {
	return Settings{
		ContextLimit:              0,
		CompactThreshold:          config.DefaultCompactThreshold,
		CompactKeepRecentMessages: config.DefaultCompactKeepRecentMessages,
		MaxToolResultBytes:        config.DefaultMaxToolResultBytes,
		CompactReserveTokens:      config.DefaultCompactReserveTokens,
		CompactLastResort:         config.DefaultCompactLastResort,
	}
}

// Manager builds LLM views and compacts canonical conversation history.
type Manager struct {
	Settings Settings
	Provider llm.LLMProvider
	// minMiddleTokens is the smallest middle (estimated tokens) worth
	// summarizing (see defaultMinMiddleTokens); tests may set it to 0.
	minMiddleTokens int
	// requireSummaryShrink enables the smaller-summary guard in
	// CompactPinnedWithKeep (see ErrSummaryNotSmaller). NewManager enables
	// it; tests may disable it to exercise tiny histories whose framed
	// summary cannot possibly shrink the middle.
	requireSummaryShrink bool
	mu                   sync.RWMutex
	limitResolved        bool
	manualContextLimit   int
	// lastLimitAttempt is when the last probe FAILED to resolve the window.
	// EnsureContextLimit throttles retries to limitRetryInterval so an
	// unresolvable model (no provider n_ctx, no models.dev entry) does not
	// pay a bounded provider probe on every round of a turn while staying
	// unresolved (and displayed as unknown) instead of being pinned to the
	// display fallback. Guarded by mu.
	lastLimitAttempt time.Time
}

func NewManager(provider llm.LLMProvider, settings Settings) *Manager {
	settings = normalizeSettings(settings)
	manual := 0
	if settings.ContextLimit > 0 {
		manual = settings.ContextLimit
	}
	return &Manager{
		Settings:             settings,
		Provider:             provider,
		minMiddleTokens:      defaultMinMiddleTokens,
		requireSummaryShrink: true,
		manualContextLimit:   manual,
		// A manually configured limit is resolved by construction (same
		// semantics as UpdateSettings): it needs no provider probe.
		limitResolved: settings.ContextLimit > 0,
	}
}

// limitRetryInterval throttles re-probes of an unresolved context window
// (see lastLimitAttempt). A var so tests can shrink it.
var limitRetryInterval = 30 * time.Second

// normalizeSettings clamps invalid values to defaults. 0 is meaningful for
// these fields (see Settings docs): compact_threshold 0 disables
// auto-compaction, compact_keep_recent_messages 0 keeps no recent messages on
// compaction, max_tool_result_bytes 0 removes the truncation cap,
// compact_reserve_tokens 0 reserves no tokens. Only negative values are
// invalid and fall back to defaults.
func normalizeSettings(settings Settings) Settings {
	def := DefaultSettings()
	if settings.CompactThreshold < 0 || settings.CompactThreshold > 1 {
		settings.CompactThreshold = def.CompactThreshold
	}
	if settings.CompactKeepRecentMessages < 0 {
		settings.CompactKeepRecentMessages = def.CompactKeepRecentMessages
	}
	if settings.MaxToolResultBytes < 0 {
		settings.MaxToolResultBytes = def.MaxToolResultBytes
	}
	if settings.CompactReserveTokens < 0 {
		settings.CompactReserveTokens = def.CompactReserveTokens
	}
	settings.CompactLastResort = normalizeCompactLastResort(settings.CompactLastResort)
	return settings
}

// normalizeCompactLastResort clamps the last-resort condensation mode to the
// two known values: "error" passes through (case-insensitive), everything
// else — including an empty value — is "condense" (the default).
func normalizeCompactLastResort(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "error") {
		return "error"
	}
	return config.DefaultCompactLastResort
}

// UpdateSettings replaces the context-management settings at runtime (the
// web settings modal). Zero values are meaningful and pass through unchanged
// (same semantics as NewManager); a ContextLimit of 0 returns to provider
// resolution. All reads are internally synchronized (m.mu), so a running
// turn sees the new settings on its next snapshot/compact check.
//
// The ContextLimit's manual/resolved distinction is preserved when the
// incoming value is UNCHANGED (the per-field settings push sends each
// session's current values back for fields it does not touch): a
// provider-resolved limit (set via SetContextLimit on restore) must not
// become a manual pin — that would stop RefreshAfterModelChange from
// re-resolving for a new model — and a manual pin must not silently become
// auto. Only an explicitly changed value re-derives the state: > 0 pins a
// manual limit, 0 returns to provider resolution.
func (m *Manager) UpdateSettings(settings Settings) {
	settings = normalizeSettings(settings)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applySettingsLocked(settings)
}

// UpdateSettingsFunc applies fn to the current settings and stores the merged
// result atomically: the read (previous values) and the write (merged result)
// happen under one m.mu critical section. With separate SettingsSnapshot +
// UpdateSettings calls, two concurrent single-field updates (the web settings
// modal's per-field push from several tabs) can interleave so the second
// caller's snapshot predates the first caller's write, silently reverting the
// first field. fn must mutate only the settings it owns; the merged result
// gets the same ContextLimit semantics as UpdateSettings (an unchanged limit
// preserves the manual/resolved distinction, an explicitly changed value
// re-derives it).
func (m *Manager) UpdateSettingsFunc(fn func(Settings) Settings) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applySettingsLocked(normalizeSettings(fn(m.Settings)))
}

// applySettingsLocked stores next and re-derives the ContextLimit
// manual/resolved state when the value changed. Caller holds m.mu.
func (m *Manager) applySettingsLocked(next Settings) {
	limitChanged := next.ContextLimit != m.Settings.ContextLimit
	m.Settings = next
	if !limitChanged {
		return
	}
	m.manualContextLimit = 0
	// A manual limit is resolved by construction; 0 returns to provider
	// resolution (EnsureContextLimit re-probes on the next turn).
	m.limitResolved = next.ContextLimit > 0
	// An explicitly changed limit re-derives the state: drop any retry
	// throttle left by the previous (failed) resolution.
	m.lastLimitAttempt = time.Time{}
	if next.ContextLimit > 0 {
		m.manualContextLimit = next.ContextLimit
	}
}

// SettingsSnapshot returns a copy of the current settings (web settings
// modal display). Internally synchronized.
func (m *Manager) SettingsSnapshot() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings
}

// RefreshAfterModelChange updates the context limit for the newly selected model.
// Provider I/O runs without holding m.mu so Snapshot/ContextStats are not stalled
// behind a slow /v1/models call.
func (m *Manager) RefreshAfterModelChange(ctx context.Context) {
	m.mu.Lock()
	if m.manualContextLimit > 0 {
		m.Settings.ContextLimit = m.manualContextLimit
		m.limitResolved = true
		m.mu.Unlock()
		return
	}
	// Keep the current value as the display fallback (a restored session's
	// pre-warmed limit, or the previous model's window) but clear the
	// resolved flag: the probe below must not be skipped, and if it cannot
	// resolve the new window the value stays marked unresolved and is
	// retried on a later turn instead of masquerading as the real window.
	m.limitResolved = false
	m.mu.Unlock()
	m.resolveContextLimit(ctx)
}

// EnsureContextLimit resolves ContextLimit from the provider until it is
// known. A manual override (GOGEN_CONTEXT_LIMIT, or a value pinned through
// UpdateSettings) is resolved by construction and skips the probe;
// RefreshAfterModelChange preserves that override. An unresolved value (see
// resolveContextLimit) is re-probed, so a window that was unknown when the
// session started — a cold models.dev registry, a briefly unreachable
// endpoint — is picked up on a later turn instead of being pinned to the
// display fallback for the session's lifetime. Provider I/O runs without
// holding m.mu.
func (m *Manager) EnsureContextLimit(ctx context.Context) {
	m.mu.Lock()
	skip := m.limitResolved
	if !skip && !m.lastLimitAttempt.IsZero() && time.Since(m.lastLimitAttempt) < limitRetryInterval {
		// A recent probe could not resolve the window: do not pay another
		// provider probe on every round; the throttle keeps the attempt
		// cost bounded while the limit stays reported as unknown.
		skip = true
	}
	m.mu.Unlock()
	if skip {
		return
	}
	m.resolveContextLimit(ctx)
}

// resolveContextLimit fetches the provider limit without holding m.mu, then
// stores the result. Concurrent resolvers are safe: the first successful
// store wins unless a manual override lands mid-flight.
//
// A provider that cannot resolve the window (it returns 0 — see
// llm.LLMProvider.ModelContextLimit) does NOT get the 128k display fallback
// stored as if it were the real window: whatever value is on display is kept
// and limitResolved stays false, so ContextSnapshot.LimitResolved reports
// "unknown" (wire consumers then fall back to the model list's per-model
// limit) and EnsureContextLimit retries later.
func (m *Manager) resolveContextLimit(ctx context.Context) {
	limit := 0
	if m.Provider != nil {
		if n, err := m.Provider.ModelContextLimit(ctx); err == nil && n > 0 {
			limit = n
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.manualContextLimit > 0 {
		m.Settings.ContextLimit = m.manualContextLimit
		m.limitResolved = true
		return
	}
	if m.limitResolved {
		// Another resolver landed a real window while this probe ran.
		return
	}
	if limit <= 0 {
		// Unknown: keep the display fallback, stay unresolved (retry after
		// limitRetryInterval).
		m.lastLimitAttempt = time.Now()
		return
	}
	m.Settings.ContextLimit = limit
	m.limitResolved = true
	m.lastLimitAttempt = time.Time{}
}

// ContextLimit returns the resolved context window size.
func (m *Manager) ContextLimit() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.ContextLimit
}

// ResolvedContextLimit returns the context window when it has actually been
// resolved for the current model (provider probe, restored snapshot, or a
// manual pin) and 0 when Settings.ContextLimit is only the display fallback
// for an unresolved window. Persistence uses it so a session snapshot never
// stores a guess — or another model's window kept during a failed refresh —
// as the session's resolved limit, which a later restore would pre-warm as
// fact.
func (m *Manager) ResolvedContextLimit() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.limitResolved {
		return 0
	}
	return m.Settings.ContextLimit
}

// CompactBudget returns the token budget at which auto-compaction triggers
// (CompactThreshold fraction of the context limit, minus the reserve). The
// caller can compare a cached token total against this to avoid re-tokenizing
// the whole conversation.
func (m *Manager) CompactBudget() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.compactBudgetLocked()
}

// HardLimit returns the token count at which the provider will refuse a
// request: the context window minus the post-compaction reserve. The
// emergency compaction tier (Agent.emergencyCompactDue) fires when the
// conversation total reaches this, bypassing the failure backoff.
func (m *Manager) HardLimit() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.ContextLimit - m.Settings.CompactReserveTokens
}

// CompactLastResort returns the last-resort condensation mode (Phase 0e):
// "condense" or "error" (see Settings.CompactLastResort). Internally
// synchronized.
func (m *Manager) CompactLastResort() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.CompactLastResort
}

// SmartCompact reports whether compaction also runs inside a tool loop.
func (m *Manager) SmartCompact() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.SmartCompact
}

// SmartCompactStopAt is the in-flight token count at which a thinking
// stream should stop so a summary still fits. It is the context window
// minus the reserve and the summary output allowance. 0 means the window
// is not known yet, so the stream is left alone.
func (m *Manager) SmartCompactStopAt() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		return 0
	}
	stop := limit - m.Settings.CompactReserveTokens - summaryOutputAllowance
	if stop < 1 {
		return 1
	}
	return stop
}

// ChooseCompactKeep returns the number of recent messages to preserve
// verbatim so that the summarized middle (everything between the first user
// message and the preserved tail) covers at least needed tokens. It starts
// at the configured keep and lowers it stepwise — the lowering floor is 1
// (the current user prompt is preserved verbatim), a worst-case option
// reached only when the standard split cannot save enough tokens. An
// explicitly configured keep of 0 is honored as-is (no tail). needed <= 0
// returns the configured keep unchanged. Returns -1 when no middle exists
// (too few messages, no user message to anchor the head, or an incomplete
// count cache) — the caller should then not attempt a compaction at all.
func (m *Manager) ChooseCompactKeep(messages []llm.Message, counts []int, needed int) int {
	m.mu.RLock()
	keep := m.Settings.CompactKeepRecentMessages
	m.mu.RUnlock()
	if needed <= 0 {
		return keep
	}
	headIdx, ok := firstUserIndex(messages)
	if !ok || len(messages) < headIdx+3 {
		return -1
	}
	if counts == nil || len(counts) < len(messages) {
		return -1
	}
	if keep > len(messages)-2 {
		keep = len(messages) - 2
	}
	for keep > 1 {
		tailStart := len(messages) - keep
		middle := 0
		for i := headIdx + 1; i < tailStart; i++ {
			middle += counts[i] - imageTokenEstimate(messages[i])
		}
		if middle >= needed {
			break
		}
		keep--
	}
	return keep
}

// CompactKeepRecentMessages returns how many recent messages are preserved
// verbatim during compaction.
func (m *Manager) CompactKeepRecentMessages() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.CompactKeepRecentMessages
}

// AutoCompactEnabled reports whether automatic compaction is enabled
// (compact_threshold > 0). A threshold of 0 disables auto-compaction; manual
// compaction via Compact/CompactHistory is unaffected.
func (m *Manager) AutoCompactEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.CompactThreshold > 0
}

// SetContextLimit sets the context window size directly, bypassing provider
// resolution. Used when restoring a session snapshot so the limit is available
// synchronously before the async provider refresh completes.
func (m *Manager) SetContextLimit(limit int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Settings.ContextLimit = limit
	m.limitResolved = true
	m.lastLimitAttempt = time.Time{}
}

const toolResultTruncationMarker = "\n… truncated ("

// HasTruncationMarker reports whether content carries the standard
// tool-result truncation marker ANYWHERE. It is the cheap "looks capped"
// signal: usage stats, and the conservative rule that a later cap must not
// rewrite a body that may already be partial (a false positive leaves an
// oversized body alone, which the spill preview needs — its marker sits
// mid-body, before the retained tail). Callers that must know whether the
// body's data beyond the cut still exists — i.e. whether re-salvaging it is
// worthwhile — use the precise HasTruncatedTail instead.
func HasTruncationMarker(content string) bool {
	return strings.Contains(content, toolResultTruncationMarker)
}

// HasTruncatedTail reports whether content was capped by a truncation
// producer: the standard marker opens the body's LAST line, which is the
// shape every producer writes (truncateToolResult appends it inside the
// budget, and the per-tool cap footers in internal/agent append it last).
// The precise form of HasTruncationMarker for decisions that must not fire
// on a body that merely quotes the marker text (a file being read, a session
// snapshot, a log line): such a body was not truncated, and only this form
// distinguishes it from a genuinely capped one.
func HasTruncatedTail(content string) bool {
	i := strings.LastIndex(content, toolResultTruncationMarker)
	if i < 0 {
		return false
	}
	return !strings.Contains(content[i+len(toolResultTruncationMarker):], "\n")
}

// TruncateToolResult caps tool output stored in canonical history / LLM views.
func (m *Manager) TruncateToolResult(content string) string {
	m.mu.RLock()
	max := m.Settings.MaxToolResultBytes
	m.mu.RUnlock()
	return truncateToolResult(content, max)
}

// truncateToolResult caps content to max bytes and appends a truncation
// marker. The marker's length is reserved up front so the capped result fits
// within max bytes (Truncate with MarkerInBudget; the previous behavior
// returned max + marker, exceeding the budget). Content already carrying the
// marker is returned unchanged (idempotency: a later pass must not
// re-truncate an already-capped result).
func truncateToolResult(content string, max int) string {
	if max <= 0 || len(content) <= max {
		return content
	}
	if strings.Contains(content, toolResultTruncationMarker) {
		return content
	}
	marker := fmt.Sprintf("\n… truncated (%d bytes total)", len(content))
	return Truncate(content, max, TruncateOptions{Marker: marker, MarkerInBudget: true})
}

// ContextSnapshot summarizes context window usage for display.
type ContextSnapshot struct {
	Limit           int
	Used            int
	Stored          int
	CompactAt       int
	MessageCount    int
	ToolTruncated   bool
	CompactDisabled bool // auto-compaction disabled (compact_threshold = 0)
	NearCompact     bool
	// WarnNearCompact is true when usage reached the warning point
	// (min(75% of the limit, CompactAt)) and auto-compaction is enabled.
	// Unlike NearCompact it tracks the warning threshold, not the trigger.
	WarnNearCompact bool
	Percent         float64
	// LimitResolved is true when Limit is a real value for this session
	// (provider probe, restore snapshot, or manual pin) and false when it
	// is the DefaultContextLimit display fallback for an unresolved window.
	// Wire consumers use it to keep "unknown" from masquerading as 128k.
	LimitResolved bool
}

// Snapshot estimates token usage for canonical history and the LLM view.
func (m *Manager) Snapshot(canonical, llmView []llm.Message) ContextSnapshot {
	stored := m.EstimateTokens(canonical)
	return m.snapshot(canonical, llmView, stored)
}

// SnapshotWithCounts is like Snapshot but uses pre-computed token counts for
// the canonical messages, avoiding re-tokenization. The counts slice must
// correspond 1:1 to canonical messages (not including system prompt). This
// is significantly faster for large restored sessions where the token counts
// were persisted alongside the messages.
func (m *Manager) SnapshotWithCounts(canonical, llmView []llm.Message, canonicalCounts []int) ContextSnapshot {
	stored := 0
	for _, c := range canonicalCounts {
		stored += c
	}
	return m.snapshot(canonical, llmView, stored)
}

// snapshot is the shared implementation for Snapshot and SnapshotWithCounts.
// stored is the pre-computed token count for canonical messages.
func (m *Manager) snapshot(canonical, llmView []llm.Message, stored int) ContextSnapshot {
	m.mu.RLock()
	limit := m.Settings.ContextLimit
	limitResolved := m.limitResolved
	if limit <= 0 {
		limit = config.DefaultContextLimit
	}
	compactAt := m.compactBudgetLocked()
	m.mu.RUnlock()

	// Estimate tokens for system prompt / enrichment messages (the prefix
	// in llmView that is not part of canonical). This is typically 1 message
	// and very cheap to tokenize compared to the full history.
	sysTokens := 0
	if n := len(llmView) - len(canonical); n > 0 {
		sysTokens = m.EstimateTokens(llmView[:n])
	}
	used := stored + sysTokens

	warnAt := 0
	if compactAt > 0 {
		warnAt = int(float64(limit) * WarnThreshold)
		if warnAt > compactAt {
			warnAt = compactAt // small windows: warn exactly when compaction would trigger
		}
	}
	snap := m.buildSnapshot(limit, compactAt, warnAt, stored, used, len(canonical), hasTruncatedToolResults(canonical))
	snap.LimitResolved = limitResolved
	return snap
}

// buildSnapshot creates a ContextSnapshot from computed values.
func (m *Manager) buildSnapshot(limit, compactAt, warnAt, stored, used, msgCount int, truncated bool) ContextSnapshot {
	snap := ContextSnapshot{
		Limit:           limit,
		Used:            used,
		Stored:          stored,
		CompactAt:       compactAt,
		MessageCount:    msgCount,
		ToolTruncated:   truncated,
		CompactDisabled: compactAt <= 0,
		NearCompact:     compactAt > 0 && used >= compactAt,
		WarnNearCompact: compactAt > 0 && used >= warnAt,
	}
	if limit > 0 {
		snap.Percent = float64(used) / float64(limit)
	}
	return snap
}

func hasTruncatedToolResults(messages []llm.Message) bool {
	for _, msg := range messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, toolResultTruncationMarker) {
			return true
		}
	}
	return false
}

func (m *Manager) compactBudgetLocked() int {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = config.DefaultContextLimit
	}
	if m.Settings.CompactThreshold <= 0 {
		return 0 // auto-compaction disabled
	}
	budget := int(float64(limit) * m.Settings.CompactThreshold)
	budget -= m.Settings.CompactReserveTokens
	if budget < 1000 {
		budget = 1000
	}
	return budget
}

// ShouldCompactWithOverhead reports whether messages exceed the compaction
// threshold (EstimateTokens computes fresh each call — safe to call every
// turn) after adding overhead — the
// wire cost of the system prompt and tool definitions, which messages does
// not include — to the estimate before comparing against the budget. Pass
// zero when the estimate already covers the full request (e.g. a fresh
// provider prompt_tokens baseline, which includes the overhead).
func (m *Manager) ShouldCompactWithOverhead(messages []llm.Message, overhead int) bool {
	m.mu.RLock()
	keep := m.Settings.CompactKeepRecentMessages
	budget := m.compactBudgetLocked()
	m.mu.RUnlock()
	if budget <= 0 {
		return false // auto-compaction disabled
	}
	if len(messages) <= keep+1 {
		return false
	}
	return m.EstimateTokens(messages)+overhead >= budget
}

// EnsureToolResultsCapped mutates messages so every tool body fits MaxToolResultBytes.
// Safe to call every turn; only rewrites oversized bodies (one-time sticky rewrite).
func (m *Manager) EnsureToolResultsCapped(messages []llm.Message) bool {
	m.mu.RLock()
	max := m.Settings.MaxToolResultBytes
	m.mu.RUnlock()
	if max <= 0 {
		return false
	}
	changed := false
	for i := range messages {
		if messages[i].Role != "tool" || messages[i].Content == "" {
			continue
		}
		if strings.Contains(messages[i].Content, toolResultTruncationMarker) {
			continue
		}
		if len(messages[i].Content) <= max {
			continue
		}
		messages[i].Content = truncateToolResult(messages[i].Content, max)
		changed = true
	}
	return changed
}

// NoTailKeep is the CompactOptions.Keep value that means "no preserved tail:
// everything after the head is summarized".
const NoTailKeep = -1

// CompactOptions configures a compaction call. Zero-value fields use defaults:
// Keep=0 (zero) uses the configured CompactKeepRecentMessages; Forced=false
// applies the minMiddleTokens guard; nil Pinned/Counts/ViewPrefix/Tools/
// AllowedTools mean absent.
type CompactOptions struct {
	// Keep is the number of recent messages to preserve verbatim.
	// Zero (the zero value) means "use the configured CompactKeepRecentMessages".
	// NoTailKeep (-1) means no preserved tail. Positive values are literal.
	Keep int

	// Forced bypasses the minMiddleTokens guard (emergency compaction).
	// When forced and the last message is a tool result, the trailing tool
	// group is preserved verbatim as the tail.
	Forced bool

	// Pinned message indices to preserve in the tail.
	Pinned map[int]struct{}

	// Counts carries per-message token counts for a prefix of messages
	// (the agent's cached counts). When it covers the region being compacted,
	// the summarization request is sized from those counts instead of
	// re-tokenizing the middle with the tokenizer.
	Counts []int

	// ViewPrefix carries the system/enrichment messages that precede
	// canonical history on the wire. It is prepended to the summarization
	// request so the conversation prefix stays byte-identical to the last
	// turn and the provider's prompt cache covers the bulk of the request.
	ViewPrefix []llm.Message

	// Tools and AllowedTools carry the tool definitions and allowlist the
	// agent sends on real turns. The summarization request sends the same
	// pair (the provider filters Tools through AllowedTools, exactly like a
	// turn) so the rendered prompt prefix — system + tools + messages —
	// stays byte-identical to the last turn. Without the tools the rendered
	// prompt diverges right after the system message and the provider's
	// prompt cache covers none of the middle. nil = no tools (the
	// summarization request then cannot reuse the turn's cache).
	Tools        []llm.Tool
	AllowedTools map[string]struct{}
}

// Compact replaces the middle of canonical history with an LLM-generated
// summary. It preserves the first user message and the most recent messages
// (per opts.Keep or the configured default). Pinned indices are preserved in
// the tail and remapped for the compacted history.
//
// A deterministic executed-call roster is appended to the summary content:
// one line per tool call in the summarized middle, carrying the call id, the
// tool name, and the first line of the matching result (see
// buildExecutedCallRoster), so the model's recap cannot drop the ids a later
// turn needs to address results.
//
// The smaller-summary guard (ErrSummaryNotSmaller) refuses a compaction whose
// framed summary is not strictly smaller than the region it replaces. No-op
// paths (too few messages, no user message, empty middle) return the input
// unchanged with a nil error; forced callers enforce the strict-shrink
// requirement (len(result) < len(messages)) externally.
func (m *Manager) Compact(ctx context.Context, messages []llm.Message, opts CompactOptions) ([]llm.Message, map[int]struct{}, error) {
	keep := opts.Keep
	switch {
	case keep == 0:
		m.mu.RLock()
		keep = m.Settings.CompactKeepRecentMessages
		m.mu.RUnlock()
	case keep == NoTailKeep:
		keep = 0
	}
	return m.compactPinned(ctx, messages, keep, opts)
}

// compactPinned is the shared implementation of Compact. opts.Forced
// bypasses the minMiddleTokens guard (emergency compaction).
func (m *Manager) compactPinned(ctx context.Context, messages []llm.Message, keep int, opts CompactOptions) ([]llm.Message, map[int]struct{}, error) {
	viewPrefix, counts, pinned, forced := opts.ViewPrefix, opts.Counts, opts.Pinned, opts.Forced
	if len(messages) <= keep+1 {
		return messages, copyIntSet(pinned), nil
	}

	headIdx, ok := firstUserIndex(messages)
	if !ok {
		// No user message to preserve as the head; compacting would drop
		// messages without anything to anchor the summary.
		return messages, copyIntSet(pinned), nil
	}
	tailStart := adjustCompactTailStart(messages, len(messages)-keep)
	if forced && keep == 0 && len(messages) > 0 && messages[len(messages)-1].Role == "tool" {
		// Forced compaction mid-tool-loop: the trailing tool group (the
		// last assistant tool-call message and its results) is
		// protocol-required — the model needs the fresh results to
		// continue the in-flight round, so it survives verbatim. Walking
		// back over the trailing tool messages makes the tail start at
		// the calling assistant message; every tool result in the tail
		// then has its calling assistant message in the tail too.
		tailStart = adjustCompactTailStart(messages, len(messages)-1)
	}
	tailStart = extendTailForPins(messages, headIdx, tailStart, pinned)
	if tailStart <= headIdx+1 {
		return messages, copyIntSet(pinned), nil
	}

	oldTailStart := tailStart
	head := []llm.Message{cloneMessage(messages[headIdx])}
	middle := messages[headIdx+1 : tailStart]
	tail := cloneMessages(messages[tailStart:])

	// Cached per-message counts (a prefix of messages) let the middle be
	// sized without re-tokenizing it. knownTokens covers everything from
	// messages[0] up to the tail start — the entire messages-derived portion
	// of the summarization request (pre-head, first user message, middle) —
	// and falls back to -1 (unknown) when the cache does not reach tailStart.
	// Cached counts include per-image estimates, but the request strips
	// images; subtract them so image sessions don't overestimate into the
	// fallback path.
	knownTokens := -1
	if counts != nil && len(counts) >= tailStart {
		knownTokens = 0
		for i := 0; i < tailStart; i++ {
			knownTokens += counts[i] - imageTokenEstimate(messages[i])
		}
	}

	// Refuse to summarize a trivially small middle: asking a model to recap
	// one or two short messages produces an echo of those messages (a
	// conversation reply) rather than a summary — worse than not compacting.
	middleTokens := -1
	if counts != nil && len(counts) >= tailStart {
		middleTokens = 0
		for i := headIdx + 1; i < tailStart; i++ {
			middleTokens += counts[i] - imageTokenEstimate(messages[i])
		}
	}
	if middleTokens < 0 {
		middleTokens = m.EstimateTokens(middle)
		for _, msg := range middle {
			middleTokens -= imageTokenEstimate(msg)
		}
	}
	if !forced && middleTokens < m.minMiddleTokens {
		return nil, nil, fmt.Errorf("not enough history to compact (%d messages in the middle)", len(middle))
	}

	// The summarization request carries the wire prefix (viewPrefix), the
	// messages before the middle (pre-head + the preserved first user
	// message), and the middle to summarize; the instruction tells the model
	// to summarize only the middle — head and tail are preserved verbatim by
	// construction below.
	summary, err := m.summarizeMiddle(ctx, viewPrefix, messages[:headIdx+1], middle, knownTokens, opts.Tools, opts.AllowedTools)
	if err != nil {
		return nil, nil, err
	}

	// Deterministic executed-call index appended after the model's recap: the
	// recap can drop call ids and the handles returned in tool results
	// (background job ids above all), and a later turn must still be able to
	// address them (background_job status/list, send_message, spill
	// retrieval). See buildExecutedCallRoster.
	recap := summary
	if roster := buildExecutedCallRoster(middle); roster != "" {
		recap += "\n\n" + roster
	}

	// Smaller-summary guard: refuse a compaction whose framed summary is not
	// strictly smaller than the region it replaces. The comparison is
	// text-only and fair: middleTokens already excludes per-image estimates
	// and the framed message carries no images. Refusing (1) stops useless
	// compactions and (2) guarantees the emergency fit loop terminates —
	// every accepted iteration strictly shrinks. The guard applies to the
	// flattened-text fallback as well: a depth-capped truncated echo of the
	// middle is exactly what it refuses. The framed content INCLUDES the
	// executed-call roster, so the index cannot push the replacement past
	// the size of the region it replaces.
	if m.requireSummaryShrink {
		framed := llm.Message{Role: "assistant", Content: SummaryPrefix + recap}
		if framedTokens := m.EstimateTokens([]llm.Message{framed}); framedTokens >= middleTokens {
			return nil, nil, fmt.Errorf("%w (summary %d tokens, middle %d tokens)",
				ErrSummaryNotSmaller, framedTokens, middleTokens)
		}
	}

	compact := make([]llm.Message, 0, headIdx+1+1+len(tail))
	if headIdx > 0 {
		compact = append(compact, cloneMessages(messages[:headIdx])...)
	}
	compact = append(compact, head...)
	compact = append(compact, llm.Message{
		Role:    "assistant",
		Content: SummaryPrefix + recap,
	})
	compact = append(compact, tail...)

	newPinned := remapPinsAfterCompact(pinned, headIdx, oldTailStart, len(compact)-len(tail))
	// The token cache is keyed by content fingerprint (not pointer), so
	// the newly allocated copies of preserved messages still hit the same
	// cache entries. Old entries for summarised-away middle messages are
	// harmless (bounded by session size and valid for their key).
	return compact, newPinned, nil
}

// extendTailForPins pulls the tail start earlier so every pinned index is preserved.
func extendTailForPins(messages []llm.Message, headIdx, tailStart int, pinned map[int]struct{}) int {
	if len(pinned) == 0 {
		return tailStart
	}
	for idx := range pinned {
		if idx <= headIdx || idx >= len(messages) {
			continue
		}
		if idx < tailStart {
			tailStart = idx
		}
	}
	return adjustCompactTailStart(messages, tailStart)
}

func remapPinsAfterCompact(pinned map[int]struct{}, headIdx, oldTailStart, newTailStart int) map[int]struct{} {
	if len(pinned) == 0 {
		return nil
	}
	out := make(map[int]struct{}, len(pinned))
	for idx := range pinned {
		if idx < 0 {
			continue
		}
		if idx <= headIdx {
			out[idx] = struct{}{}
			continue
		}
		if idx >= oldTailStart {
			out[newTailStart+(idx-oldTailStart)] = struct{}{}
		}
		// Pins that fell in the summarized middle are dropped (should not happen
		// when extendTailForPins ran first).
	}
	return out
}

func copyIntSet(in map[int]struct{}) map[int]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

// IsCompactionSummary reports whether content is a compaction summary
// message. Summaries are stored as assistant-role messages today; the check
// also covers legacy user-role summaries, which must not be treated as real
// user messages (firstUserIndex, PinManager).
func IsCompactionSummary(content string) bool {
	return strings.HasPrefix(content, SummaryPrefix)
}

// IsCondensedMessage reports whether content is a last-resort-condensed
// message (Phase 0e, see CondensedMessagePrefix). A condensed message is
// NOT a compaction summary: it keeps its original role and position, and a
// condensed user message still anchors the head (firstUserIndex).
func IsCondensedMessage(content string) bool {
	return strings.HasPrefix(content, CondensedMessagePrefix)
}

// firstUserIndex returns the index of the first real user message (skipping
// compaction summaries — stored as assistant-role messages prefixed with
// SummaryPrefix today, user-role in legacy sessions) and whether one exists.
// ok is false when the conversation has no user message at all — callers
// must not compact in that case.
func firstUserIndex(messages []llm.Message) (int, bool) {
	for i, msg := range messages {
		if msg.Role == "user" && !IsCompactionSummary(msg.Content) {
			return i, true
		}
	}
	return 0, false
}

// summarizeMiddle produces a summary of the middle of the conversation.
// viewPrefix is the wire context that precedes canonical history
// (system/enrichment messages) and prefix is messages[:headIdx+1] (pre-head
// plus the preserved first user message); the preferred path sends
// viewPrefix + prefix + middle + summaryInstruction as one request so the
// provider prompt cache covers the conversation prefix. When the request
// would not fit in the context window, it falls back to the legacy
// flattened-text recursive summarization.
//
// knownTokens, when non-negative, is the cached token count of prefix+middle
// (the messages-derived portion of the request); only the wire prefix and
// the instruction are then tokenized fresh, avoiding a full re-tokenization
// of the middle on every compaction. tools/allowedTools are the turn's tool
// set (see CompactOptions.Tools): they ride on the request so the rendered
// prefix matches the last turn, and their token cost is included in the
// budget check so the request cannot be pushed over the window.
func (m *Manager) summarizeMiddle(ctx context.Context, viewPrefix, prefix, middle []llm.Message, knownTokens int, tools []llm.Tool, allowedTools map[string]struct{}) (string, error) {
	if len(middle) == 0 {
		return "", nil
	}
	m.mu.RLock()
	budget := m.summaryRequestBudgetLocked()
	m.mu.RUnlock()

	// Trailing USER-role instruction: a task directive, not a chat turn.
	// The strong framing in summaryInstruction keeps the model from
	// continuing the conversation. A trailing system message would be
	// non-portable (rejected by Anthropic and by strict Jinja chat
	// templates such as Qwen3's "System message must be at the
	// beginning"), so the instruction rides on the universally legal user
	// role. The conversation prefix stays byte-identical — INCLUDING the
	// tool definitions, which are sent on the request (tools/allowedTools,
	// see CompactOptions.Tools) — so the provider prompt cache covers the
	// bulk of the request.
	instruction := llm.Message{Role: "user", Content: summaryInstruction}
	req := make([]llm.Message, 0, len(viewPrefix)+len(prefix)+len(middle)+1)
	// Summarization requests must not carry user images: the summary model
	// may not support vision, and image bytes are irrelevant to a recap.
	req = append(req, stripImages(viewPrefix)...)
	req = append(req, stripImages(prefix)...)
	req = append(req, stripImages(middle)...)
	req = append(req, instruction)
	// Count the request exactly once and reuse the count for the budget
	// check and both debuglog entries. (The map literal passed to
	// debuglog.Write is evaluated eagerly even when debug logging is off, so
	// a second EstimateTokens call here would duplicate the full
	// tokenization of the conversation prefix on every compaction.)
	reqTokens := knownTokens
	if reqTokens < 0 {
		reqTokens = m.EstimateTokens(req)
	} else {
		// knownTokens covers prefix+middle with image estimates already
		// subtracted (matching the stripped request); only the wire prefix
		// and the one-message instruction need fresh counts.
		reqTokens += m.EstimateTokens(stripImages(viewPrefix))
		reqTokens += m.EstimateTokens([]llm.Message{instruction})
	}
	// The tool definitions ride on the request (prompt-cache prefix) but
	// are not in req: count them in both branches or the budget check
	// under-reports the outgoing size. Counted once and reused by the
	// debuglog entry (its map literal is evaluated eagerly).
	toolTokens := 0
	if len(tools) > 0 {
		toolTokens = EstimateToolTokens(tools)
	}
	reqTokens += toolTokens
	if reqTokens <= budget {
		debuglog.Write("contextmgr/summarize", "continuation-summary request", "", map[string]any{
			"path":           "primary",
			"middleMessages": len(middle),
			"requestTokens":  reqTokens,
			"toolTokens":     toolTokens,
			"budget":         budget,
		})
		summary, err := m.summarizeRequest(ctx, req, allowedTools, tools)
		if err == nil {
			return summary, nil
		}
		debuglog.Write("contextmgr/summarize", "continuation-summary request failed; using flattened-text fallback", "", map[string]any{
			"path":           "fallback-after-error",
			"error":          err.Error(),
			"middleMessages": len(middle),
		})
		return m.summarizeMessagesDepth(ctx, middle, 0)
	}
	debuglog.Write("contextmgr/summarize", "summary request exceeds window; using flattened-text fallback", "", map[string]any{
		"path":           "fallback",
		"middleMessages": len(middle),
		"requestTokens":  reqTokens,
		"budget":         budget,
	})
	return m.summarizeMessagesDepth(ctx, middle, 0)
}

// stripImages returns a copy of msgs with every user message's images
// removed, leaving all other fields (and the original slice) untouched.
// Only the summarization request is affected: stripping images from the
// request prefix breaks the byte-identical prompt-cache prefix only for
// sessions that attach images; the main conversation requests keep their
// images and their cache behavior.
func stripImages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		m.Images = nil
		out[i] = m
	}
	return out
}

// summaryRequestBudgetLocked is the token budget for the continuation-summary
// request: the full context window minus the post-compaction reserve. Unlike
// maxSummaryInputTokensLocked (which budgets the flattened middle text for the
// legacy path), this leaves room for the summary output while keeping the
// request under the window. Callers must hold m.mu (R or W).
func (m *Manager) summaryRequestBudgetLocked() int {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = config.DefaultContextLimit
	}
	budget := limit - m.Settings.CompactReserveTokens
	if budget < 2000 {
		budget = 2000
	}
	return budget
}

func (m *Manager) summarizeMessagesDepth(ctx context.Context, messages []llm.Message, depth int) (string, error) {
	if depth >= maxSummarizeDepth {
		m.mu.RLock()
		maxTool := m.Settings.MaxToolResultBytes
		maxIn := m.maxSummaryInputTokensLocked()
		m.mu.RUnlock()
		text := renderMessagesForSummary(messages, maxTool)
		return truncateForSummary(text, maxIn), nil
	}
	return m.summarizeMessages(ctx, messages, depth)
}

func (m *Manager) summarizeMessages(ctx context.Context, messages []llm.Message, depth int) (string, error) {
	m.mu.RLock()
	maxTool := m.Settings.MaxToolResultBytes
	maxIn := m.maxSummaryInputTokensLocked()
	m.mu.RUnlock()
	text := renderMessagesForSummary(messages, maxTool)
	if m.EstimateTokens([]llm.Message{{Content: text}}) <= maxIn {
		return m.summarizeText(ctx, text)
	}
	if len(messages) == 1 {
		return m.summarizeText(ctx, truncateForSummary(text, maxIn))
	}

	mid := len(messages) / 2
	left, err := m.summarizeMessagesDepth(ctx, messages[:mid], depth+1)
	if err != nil {
		return "", err
	}
	right, err := m.summarizeMessagesDepth(ctx, messages[mid:], depth+1)
	if err != nil {
		return "", err
	}
	merged := "Earlier segment summary:\n" + left + "\n\nLater segment summary:\n" + right
	if m.EstimateTokens([]llm.Message{{Content: merged}}) <= maxIn {
		return m.summarizeText(ctx, merged)
	}
	return merged, nil
}

// summaryOutputAllowance is the token budget assumed for the summary output
// when sizing the flattened single-call path: the recap is short (a few
// hundred to ~2k tokens), and the allowance keeps input + output under the
// window. Unlike the continuation-summary request, the flattened call
// carries no conversation prefix, so it can safely use the full window
// minus the reserve — the pre-fix limit/2 budget forced middles that fit
// the window into the recursive (multi-call) path instead of one call.
const summaryOutputAllowance = 4000

func (m *Manager) maxSummaryInputTokensLocked() int {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = config.DefaultContextLimit
	}
	budget := limit - m.Settings.CompactReserveTokens - summaryOutputAllowance
	if budget < 2000 {
		budget = 2000
	}
	return budget
}

// summarizeRequest calls the provider with the full continuation-summary
// request (wire prefix + middle + summaryInstruction) and extracts the
// summary text from the response. allowedTools/tools are the turn's tool
// set: sending them (as the turn does) keeps the rendered prompt prefix
// byte-identical so the provider's prompt cache covers the middle.
func (m *Manager) summarizeRequest(ctx context.Context, request []llm.Message, allowedTools map[string]struct{}, tools []llm.Tool) (string, error) {
	resp, err := m.Provider.GenerateResponse(ctx, request, allowedTools, tools)
	if err != nil {
		return "", fmt.Errorf("context summarization failed: %w", err)
	}
	return extractSummary(resp)
}

// extractSummary picks the summary text out of a summarization response,
// falling back from Content to Refusal to Reasoning (some local models only
// emit reasoning for short completions). It returns an error when all three
// are empty.
func extractSummary(resp llm.Response) (string, error) {
	summary := resp.Content
	if summary == "" {
		summary = resp.Refusal
	}
	if summary == "" {
		summary = resp.Reasoning
	}
	if summary == "" {
		return "", fmt.Errorf("context summarization returned empty summary")
	}
	return summary, nil
}

func renderMessagesForSummary(messages []llm.Message, maxToolBytes int) string {
	toolNames := toolNamesFromMessages(messages)
	var b strings.Builder
	for _, msg := range messages {
		writeMessageForSummary(&b, msg, maxToolBytes, toolNames)
	}
	return b.String()
}

func toolNamesFromMessages(messages []llm.Message) map[string]string {
	names := make(map[string]string)
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.ID != "" && tc.Name != "" {
				names[tc.ID] = tc.Name
			}
		}
	}
	return names
}

func adjustCompactTailStart(messages []llm.Message, start int) int {
	if start <= 0 || start >= len(messages) {
		return start
	}
	for start > 0 && messages[start].Role == "tool" {
		start--
	}
	return start
}

// summarizeTextPrompt builds the system-role prompt for the flattened
// segment summarization. It reuses summaryFraming and summaryPreserveList so
// the shared guidance cannot drift from summaryInstruction.
func summarizeTextPrompt(segment string) string {
	return summaryFraming + ` Do not reply to the segment below, do not continue the conversation, and do not quote it verbatim.

Summarize the conversation segment below. ` + summaryPreserveList + `

Be concise but keep facts the agent needs to continue without re-reading everything.
Do not invent information. Output only the summary text.

Conversation segment:
` + segment
}

func (m *Manager) summarizeText(ctx context.Context, segment string) (string, error) {
	if strings.TrimSpace(segment) == "" {
		return "", nil
	}

	resp, err := m.Provider.GenerateResponse(ctx, []llm.Message{
		{Role: "system", Content: summarizeTextPrompt(segment)},
	}, nil, nil)
	if err != nil {
		return "", fmt.Errorf("context summarization failed: %w", err)
	}
	return extractSummary(resp)
}

// CondenseMessage summarizes the content of a single message — the
// last-resort condensation (Phase 0e) for a message that cannot fit the
// context window. The message is rendered with its role label and summarized
// via the same summarizer path as compaction (summarizeText). When the
// rendered content does not fit the summary input budget (maxIn), it is
// truncated deterministically (the depth-cap fallback) instead of calling
// the provider — a message that big cannot be sent to the provider anyway,
// so the truncated echo is the best that is possible.
func (m *Manager) CondenseMessage(ctx context.Context, msg llm.Message) (string, error) {
	if m.Provider == nil {
		return "", errors.New("no provider configured for condensation")
	}
	m.mu.RLock()
	maxTool := m.Settings.MaxToolResultBytes
	maxIn := m.maxSummaryInputTokensLocked()
	m.mu.RUnlock()
	text := renderMessagesForSummary([]llm.Message{msg}, maxTool)
	if m.EstimateTokens([]llm.Message{{Content: text}}) <= maxIn {
		return m.summarizeText(ctx, text)
	}
	return truncateForSummary(text, maxIn), nil
}

func writeMessageForSummary(b *strings.Builder, msg llm.Message, maxToolBytes int, toolNames map[string]string) {
	switch msg.Role {
	case "user":
		fmt.Fprintf(b, "USER: %s\n", msg.Content)
	case "assistant":
		if msg.Content != "" {
			fmt.Fprintf(b, "ASSISTANT: %s\n", msg.Content)
		} else if msg.Refusal != "" {
			fmt.Fprintf(b, "ASSISTANT REFUSAL: %s\n", msg.Refusal)
		}
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(b, "TOOL CALL: %s(%v)\n", tc.Name, tc.Args)
		}
	case "tool":
		content := msg.Content
		if maxToolBytes > 0 && len(content) > maxToolBytes {
			content = Truncate(content, maxToolBytes, TruncateOptions{
				Marker: fmt.Sprintf(" …(%d chars total)", len(msg.Content)),
			})
		}
		label := msg.ToolCallID
		if name := toolNames[msg.ToolCallID]; name != "" {
			label = name + " (" + msg.ToolCallID + ")"
		}
		fmt.Fprintf(b, "TOOL RESULT (%s): %s\n", label, content)
	}
}

// executedCallRosterHeader introduces the deterministic executed-call index
// that compactPinned appends to every summary. The wording is part of the
// persisted transcript (a later compaction sees the roster inside the
// previous summary's content), so treat it as a format.
const executedCallRosterHeader = "[Executed tool calls in the summarized span — id, tool, result head]"

// maxCallRosterHeadBytes caps the result-head portion of one roster entry.
// The head is the first non-empty line of the tool result — where tools put
// their returned handles (background job ids, subagent ids, terminal sends,
// spill locators) — so the cap only trims unusually long lines.
const maxCallRosterHeadBytes = 160

// maxCallRosterBytes bounds the whole roster. Without it an extreme middle
// (a huge run of tiny calls) could make the replacement larger than the
// region it replaces: the smaller-summary guard would then refuse every
// compaction and the conversation could not shrink at the hard window. A
// roster over the cap stops early and names the number of calls omitted.
const maxCallRosterBytes = 8 * 1024

// buildExecutedCallRoster renders the deterministic executed-call index
// appended to a compaction summary. The model's recap may drop call-level
// detail — and with it the ids a later turn needs to address results
// (background job ids above all, plus subagent ids, terminal sends, and
// spill locators) — so the roster is generated from the summarized messages,
// never by the model: one line per tool call in the middle, in model order,
// carrying the wire call id, the tool name, and the first line of the
// matching tool result. Call ids are never truncated.
//
// The roster is part of the framed summary the smaller-summary guard
// measures (see compactPinned), so it can never silently push the
// replacement past the size of the region it replaces.
func buildExecutedCallRoster(middle []llm.Message) string {
	return buildExecutedCallRosterBudget(middle, maxCallRosterBytes)
}

// buildExecutedCallRosterBudget is buildExecutedCallRoster with an explicit
// byte budget: the omission path is only reachable with a budget far below
// the const, so tests drive it directly.
func buildExecutedCallRosterBudget(middle []llm.Message, budget int) string {
	heads := resultHeadsByCallID(middle)
	entries := make([]string, 0, len(middle))
	for _, msg := range middle {
		if msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			entries = append(entries, callRosterEntry(tc, heads[tc.ID]))
		}
	}
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d executed\n", executedCallRosterHeader, len(entries))
	for i, entry := range entries {
		line := "- " + entry + "\n"
		if i > 0 && b.Len()+len(line) > budget {
			fmt.Fprintf(&b, "- … %d more executed call(s) omitted\n", len(entries)-i)
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// resultHeadsByCallID maps each call id to the first non-empty line of its
// tool result. The first result wins (a duplicated id is protocol-invalid
// and the roster only needs one head).
func resultHeadsByCallID(middle []llm.Message) map[string]string {
	var heads map[string]string
	for _, msg := range middle {
		if msg.Role != "tool" || msg.ToolCallID == "" {
			continue
		}
		if heads == nil {
			heads = make(map[string]string)
		}
		if _, seen := heads[msg.ToolCallID]; seen {
			continue
		}
		heads[msg.ToolCallID] = firstResultLine(msg.Content)
	}
	return heads
}

// firstResultLine returns the first non-empty line of a tool result,
// rune-safe capped at maxCallRosterHeadBytes. Whitespace-only lines are
// skipped so a result that opens with a blank line still contributes its
// handle.
func firstResultLine(content string) string {
	for len(content) > 0 {
		line := content
		if i := strings.IndexByte(content, '\n'); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = ""
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return Truncate(line, maxCallRosterHeadBytes, TruncateOptions{Marker: "…"})
	}
	return ""
}

// callRosterEntry renders one executed call as "id name: result-head". The
// placeholders keep a malformed call (missing id or name) listable instead
// of silently dropped from the index.
func callRosterEntry(tc llm.ToolCall, head string) string {
	id := tc.ID
	if id == "" {
		id = "unknown-id"
	}
	name := tc.Name
	if name == "" {
		name = "unknown-tool"
	}
	if head == "" {
		return id + " " + name
	}
	return id + " " + name + ": " + head
}

// cloneMessage deep-copies the persisted parts of a message so compaction can
// preserve them verbatim. Images (vision input), CreatedAt, and Model are
// value fields that must survive: stripping them from the preserved head/tail
// would silently drop image context after the first compaction and wipe
// assistant model attribution (web UI chips) from preserved bubbles.
func cloneMessage(msg llm.Message) llm.Message {
	out := llm.Message{
		Role:           msg.Role,
		Content:        msg.Content,
		Reasoning:      msg.Reasoning,
		Refusal:        msg.Refusal,
		ToolCallID:     msg.ToolCallID,
		CreatedAt:      msg.CreatedAt,
		Model:          msg.Model,
		ArgsStabilized: msg.ArgsStabilized,
	}
	if len(msg.Images) > 0 {
		out.Images = make([]llm.ImageInput, len(msg.Images))
		copy(out.Images, msg.Images)
	}
	if len(msg.ToolCalls) > 0 {
		out.ToolCalls = make([]llm.ToolCall, len(msg.ToolCalls))
		copy(out.ToolCalls, msg.ToolCalls)
		for i := range out.ToolCalls {
			if msg.ToolCalls[i].Args != nil {
				out.ToolCalls[i].Args = make(map[string]any, len(msg.ToolCalls[i].Args))
				for k, v := range msg.ToolCalls[i].Args {
					out.ToolCalls[i].Args[k] = cloneArgValue(v)
				}
			}
			// ArgsStr is copied verbatim, so ArgsJSONValid stays truthful;
			// carrying it avoids re-stabilizing preserved tool calls next turn.
			out.ToolCalls[i].ArgsJSONValid = msg.ToolCalls[i].ArgsJSONValid
		}
	}
	return out
}

func cloneArgValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = cloneArgValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = cloneArgValue(val)
		}
		return out
	default:
		return v
	}
}

func cloneMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	for i, msg := range messages {
		out[i] = cloneMessage(msg)
	}
	return out
}
