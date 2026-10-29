package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/textutil"
)

// reasoningExcerptRunes is the tail of a cut thinking round kept with the
// tool call. The rest of that round is not stored; the summary carries the
// earlier history.
const reasoningExcerptRunes = 1500

const smartCompactStuckMessage = "The conversation is near the context window and compaction could not free enough space. Try /compact or start a new session."

// smartCompactAutoOffMessage ends a cut turn when auto-compaction is off
// (compact_threshold: 0). Nothing was summarized and nothing will be, so the
// generic "compaction could not free enough space" wording would report an
// attempt that never happened: compact_threshold 0 means "never compact
// automatically" (README), and the way on from here is a manual /compact, a
// higher threshold, or a fresh session.
const smartCompactAutoOffMessage = "The conversation is near the context window and auto-compaction is off (compact_threshold: 0), so nothing was summarized. Run /compact to summarize the history, raise compact_threshold, or start a new session."

type smartCompactAction int

const (
	smartCompactNone smartCompactAction = iota
	// smartCompactExecute means the history was compacted (or compaction
	// was skipped because it just failed) and the tool batch is complete.
	// The caller runs those calls.
	smartCompactExecute
	// smartCompactContinue means the stream was cut before a tool call
	// finished. The caller appends the note and asks the model to go on
	// from the summary, without ending the turn.
	smartCompactContinue
	// smartCompactHalt ends the turn. Compaction could not free room and
	// there is no complete tool call to run.
	smartCompactHalt
)

func (a *Agent) smartCompactEnabled() bool {
	return a != nil && a.Context != nil && a.Context.SmartCompact()
}

// SetSmartCompact turns smart compact on or off for this session. The
// change is live for the next model call.
func (a *Agent) SetSmartCompact(on bool) {
	if a == nil || a.Context == nil {
		return
	}
	a.Context.UpdateSettingsFunc(func(s contextmgr.Settings) contextmgr.Settings {
		s.SmartCompact = on
		return s
	})
}

// armSmartCompactStream counts generated text for this turn and stops the
// stream when the conversation plus that text reaches the window headroom.
// The returned handlers are a copy, so a host that reuses its struct on the
// next turn is not wrapped twice. reset clears the counter for the next
// model round.
func (a *Agent) armSmartCompactStream(h *llm.StreamHandlers) (*llm.StreamHandlers, func()) {
	if h == nil {
		h = &llm.StreamHandlers{}
	}
	local := *h
	var gen atomic.Int64
	wrap := func(orig func(string)) func(string) {
		return func(tok string) {
			gen.Add(int64(len(tok)))
			if orig != nil {
				orig(tok)
			}
		}
	}
	local.OnThinkingToken = wrap(h.OnThinkingToken)
	local.OnToken = wrap(h.OnToken)
	origArgs := h.OnToolCallArgsDelta
	local.OnToolCallArgsDelta = func(index int, id, name, argsDelta string) {
		gen.Add(int64(len(argsDelta)))
		if origArgs != nil {
			origArgs(index, id, name, argsDelta)
		}
	}
	prevStop := h.StopStream
	// base is the conversation total at the START of the current round. Round
	// boundaries are the only places messages are appended, so it cannot change
	// DURING a round — and StopStream is consulted on every streamed chunk,
	// where compactionTokenTotal's walk over every message count (plus, without
	// a fresh provider baseline, a system-suffix rebuild and hash) was pure
	// repetition. -1 means "not applicable" (feature off, or no stop point).
	var base atomic.Int64
	base.Store(-1)
	recomputeBase := func() {
		if !a.smartCompactEnabled() || a.Context == nil || a.Context.SmartCompactStopAt() <= 0 {
			base.Store(-1)
			return
		}
		base.Store(int64(a.compactionTokenTotal()))
	}
	local.StopStream = func() bool {
		if prevStop != nil && prevStop() {
			return true
		}
		if !a.smartCompactEnabled() || a.Context == nil {
			return false
		}
		stopAt := a.Context.SmartCompactStopAt()
		if stopAt <= 0 {
			return false
		}
		total := int(base.Load())
		if total < 0 {
			return false
		}
		return total+int(gen.Load()/4) >= stopAt
	}
	// The reset runs at every round start, which is exactly when the cached
	// base can legitimately change.
	return &local, func() {
		gen.Store(0)
		recomputeBase()
	}
}

// replyExceedsBudget reports whether appending this assistant round would
// put the conversation at or over the compact budget. A threshold of 0
// disables the check; the thinking-stream cut still uses the window.
func (a *Agent) replyExceedsBudget(res *llm.StreamResult) bool {
	if res == nil || !a.smartCompactEnabled() || a.Context == nil || !a.Context.AutoCompactEnabled() {
		return false
	}
	total := a.compactionTokenTotal()
	if total < 0 {
		return false
	}
	extra := contextmgr.ComputeMessageTokens(llm.Message{
		Role:      "assistant",
		Content:   res.Content,
		Reasoning: res.Reasoning,
		ToolCalls: res.ToolCalls,
	})
	return total+extra >= a.Context.CompactBudget()
}

// smartCompactBeforeTools compacts before a finished tool batch is run when
// smart compact is on and this round is over the compact budget. A stream
// cut (BudgetStop) takes the same path: a complete tool call is run after
// the summary, and a cut that landed mid-argument is dropped.
func (a *Agent) smartCompactBeforeTools(ctx context.Context, h *llm.StreamHandlers, res *llm.StreamResult) smartCompactAction {
	if res == nil || !a.smartCompactEnabled() {
		return smartCompactNone
	}
	// A finished answer is delivered as usual. This only steps in before a
	// tool batch, or when the stream itself was cut to leave room to summarize.
	if !res.BudgetStop && len(res.ToolCalls) == 0 {
		return smartCompactNone
	}
	if !res.BudgetStop && !a.replyExceedsBudget(res) {
		return smartCompactNone
	}

	total := a.compactionTokenTotal()
	if !a.compactAttemptDue() && !a.emergencyCompactDue(total) {
		return a.smartCompactGiveUp(res)
	}

	complete := toolBatchExecutable(res.ToolCalls)
	// Same gate as a user-message compact. A threshold of 0 or a short
	// history does not call the summarizer and does not arm the shared
	// backoff. A finished tool call still runs below.
	shrunk := a.compactIfDue(ctx, h)
	if !complete && !res.BudgetStop {
		// The stream finished. A broken tool call takes the normal path,
		// which reports the argument error and continues.
		return smartCompactNone
	}
	res.Reasoning = reasoningExcerpt(res.Reasoning)
	if complete {
		if shrunk {
			block := pausedToolBlock(res.ToolCalls, res.Reasoning)
			if !a.annotateCompactionSummary(block) {
				// No summary takes the note (deep dive keeps the one before
				// the request unchanged): the tool-call message carries it.
				if strings.TrimSpace(res.Content) == "" {
					res.Content = block
				} else {
					res.Content += "\n\n" + block
				}
			}
		}
		return smartCompactExecute
	}
	res.ToolCalls = nil
	if !shrunk {
		if strings.TrimSpace(res.Content) == "" {
			res.Content = a.smartCompactHaltMessage()
		}
		return smartCompactHalt
	}
	if strings.TrimSpace(res.Content) == "" && res.Reasoning == "" {
		res.Content = "[Paused to compact the conversation. Continue from the summary.]"
	}
	note := "[Paused before execution]\nThe previous step was cut off before a tool call finished."
	if res.Reasoning != "" {
		note += "\nReasoning excerpt: " + res.Reasoning
	}
	if !a.annotateCompactionSummary(note) && strings.TrimSpace(res.Content) == "" {
		res.Content = note
	}
	return smartCompactContinue
}

// smartCompactGiveUp runs when compaction was just tried and is backing off.
// A complete tool batch still runs. A stream cut with no complete call ends
// the turn instead of thinking again against the same full window.
func (a *Agent) smartCompactGiveUp(res *llm.StreamResult) smartCompactAction {
	if !res.BudgetStop {
		return smartCompactNone
	}
	res.Reasoning = reasoningExcerpt(res.Reasoning)
	if toolBatchExecutable(res.ToolCalls) {
		return smartCompactExecute
	}
	res.ToolCalls = nil
	if strings.TrimSpace(res.Content) == "" {
		res.Content = a.smartCompactHaltMessage()
	}
	return smartCompactHalt
}

// smartCompactHaltMessage is the note that ends a cut turn which freed no
// room: the auto-compaction-off wording when the user disabled compaction
// (compact_threshold: 0), the generic compaction-failure wording otherwise.
// Both halt paths (a closed gate and a failure backoff) share it, so the note
// always matches what actually happened.
func (a *Agent) smartCompactHaltMessage() string {
	if a != nil && a.Context != nil && !a.Context.AutoCompactEnabled() {
		return smartCompactAutoOffMessage
	}
	return smartCompactStuckMessage
}

// compactIfDue runs one automatic compaction when the normal gate or the
// emergency tier says so. prepareMessages and smart compact both use it, so
// a threshold of 0 and the failure backoff stay one policy. It reports
// whether the history shrank. A closed gate does not record a failure.
//
// compact_threshold 0 means "never compact automatically" and closes BOTH
// tiers, at both boundaries (a user message and a smart-compact tool loop).
// The emergency tier used to ignore the switch and summarize once the
// conversation reached the window reserve, which contradicted the setting
// (and the README) for a user who disabled compaction on purpose. The
// window-driven rescue is not this tier: preflightForcedCompact still steps
// in when the outgoing request would actually be refused, and a
// smart-compact cut halts the turn with smartCompactAutoOffMessage.
//
// Two-tier trigger. The normal tier fires at CompactBudget and honors the
// failure backoff. The emergency tier fires at the hard window
// (ContextLimit - CompactReserveTokens, where the provider refuses the
// request) and bypasses the backoff, so a large message arriving during
// backoff still gets a compaction attempt before the request is refused.
// The progress guard (compactionGuards.emergency) stops a hot loop of
// repeated failures at an unchanged message count.
func (a *Agent) compactIfDue(ctx context.Context, h *llm.StreamHandlers) bool {
	if a == nil || a.Context == nil {
		return false
	}
	if !a.Context.AutoCompactEnabled() {
		return false
	}
	total := a.compactionTokenTotal()
	emergency := a.emergencyCompactDue(total)
	if !((a.shouldCompactUsingCounts() && a.compactAttemptDue()) || emergency) {
		return false
	}
	beforeN := len(a.Messages)
	beforeBlob := summaryBlob(a.Messages)
	if h != nil && h.OnCompacting != nil {
		h.OnCompacting()
	}
	var pinned map[int]struct{}
	if a.PinManager != nil {
		pinned = a.PinManager.PinnedSet()
	}
	// Pass the cached per-message counts so the summarization request can
	// be sized without re-tokenizing the middle.
	a.statsMu.RLock()
	counts := append([]int(nil), a.tokenCounts...)
	a.statsMu.RUnlock()
	keep := a.Context.CompactKeepRecentMessages()
	if emergency {
		// The standard split may not save enough (the preserved tail can
		// itself be most of the deficit): lower the keep stepwise, floor 1
		// (the current user prompt is always preserved verbatim), so the
		// summarized middle covers the deficit. The old messages are
		// included in the middle — summarized, never dropped. -1 (no
		// compactable middle) keeps the configured keep, which makes the
		// attempt a documented no-op.
		needed := total - a.Context.HardLimit() + contextmgr.EmergencySummaryAllowance
		if k := a.Context.ChooseCompactKeep(a.Messages, counts, needed, pinned); k > 0 {
			keep = k
		}
	}
	a.compactToFit(ctx, pinned, counts, keep, emergency)
	return len(a.Messages) < beforeN || summaryBlob(a.Messages) != beforeBlob
}

// annotateCompactionSummary appends block to the summary NoteSummaryIndex
// picks and reports whether one took it.
func (a *Agent) annotateCompactionSummary(block string) bool {
	block = strings.TrimSpace(block)
	if block == "" {
		return false
	}
	deepDive := a.Context != nil && a.Context.DeepDive()
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	i := contextmgr.NoteSummaryIndex(a.Messages, deepDive)
	if i < 0 {
		return false
	}
	a.Messages[i].Content += "\n\n" + block
	if a.tokenCounts != nil && i < len(a.tokenCounts) {
		a.tokenCounts[i] = contextmgr.ComputeMessageTokens(a.Messages[i])
	}
	return true
}

func toolBatchExecutable(calls []llm.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, tc := range calls {
		if tc.Name == "" || tc.ArgsError != "" {
			return false
		}
	}
	return true
}

func reasoningExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= reasoningExcerptRunes {
		return s
	}
	return "..." + string(r[len(r)-reasoningExcerptRunes:])
}

// clipRunes cuts s to n runes and appends "..." — the marker sits BEYOND the
// budget, unlike textutil.TruncateRunesEllipsis, because this bounds displayed
// argument text rather than filling a fixed-width field: keeping the rune
// count exact would silently shorten what the model gets to see by three.
func clipRunes(s string, n int) string {
	if t := textutil.TruncateRunes(s, n); t != s {
		return t + "..."
	}
	return s
}

func pausedToolBlock(calls []llm.ToolCall, reasoning string) string {
	var b strings.Builder
	b.WriteString("[Paused before execution]")
	for _, tc := range calls {
		args := strings.TrimSpace(tc.ArgsStr)
		if args == "" {
			args = "{}"
		}
		fmt.Fprintf(&b, "\n- %s %s", tc.Name, clipRunes(args, reasoningExcerptRunes))
	}
	if ex := strings.TrimSpace(reasoning); ex != "" {
		b.WriteString("\nReasoning excerpt: ")
		b.WriteString(ex)
	}
	return b.String()
}

func summaryBlob(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if contextmgr.IsCompactionSummary(m.Content) {
			b.WriteString(m.Content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
