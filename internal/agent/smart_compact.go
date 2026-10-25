package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// reasoningExcerptRunes is the tail of a cut thinking round kept with the
// tool call. The rest of that round is not stored; the summary carries the
// earlier history.
const reasoningExcerptRunes = 1500

const smartCompactStuckMessage = "The conversation is near the context window and compaction could not free enough space. Try /compact or start a new session."

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
		total := a.compactionTokenTotal()
		if total < 0 {
			return false
		}
		return total+int(gen.Load()/4) >= stopAt
	}
	return &local, func() { gen.Store(0) }
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
	beforeN := len(a.Messages)
	beforeBlob := summaryBlob(a.Messages)
	a.runAutoCompact(ctx, h)
	shrunk := len(a.Messages) < beforeN || summaryBlob(a.Messages) != beforeBlob
	// A no-op that still counts as success (nothing to summarize, or
	// auto-compact is off) would otherwise cut the next think immediately
	// and compact again. Record the failure so the next cut stops.
	if !shrunk && a.compactAttemptDue() && !a.emergencyCompactDue(a.compactionTokenTotal()) {
		a.noteCompactFailure(errors.New("smart compact did not shrink the conversation"))
	}
	if !complete && !res.BudgetStop {
		// The stream finished. A broken tool call takes the normal path,
		// which reports the argument error and continues.
		return smartCompactNone
	}
	res.Reasoning = reasoningExcerpt(res.Reasoning)
	if complete {
		if shrunk {
			a.annotateCompactionSummary(pausedToolBlock(res.ToolCalls, res.Reasoning))
		}
		return smartCompactExecute
	}
	res.ToolCalls = nil
	if !shrunk {
		if strings.TrimSpace(res.Content) == "" {
			res.Content = smartCompactStuckMessage
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
		res.Content = smartCompactStuckMessage
	}
	return smartCompactHalt
}

func (a *Agent) runAutoCompact(ctx context.Context, h *llm.StreamHandlers) {
	if a == nil || a.Context == nil {
		return
	}
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
	total := a.compactionTokenTotal()
	emergency := a.emergencyCompactDue(total)
	keep := a.Context.CompactKeepRecentMessages()
	if emergency {
		needed := total - a.Context.HardLimit() + emergencySummaryAllowance
		if k := a.Context.ChooseCompactKeep(a.Messages, counts, needed); k > 0 {
			keep = k
		}
	}
	a.compactToFit(ctx, pinned, counts, keep, emergency)
}

func (a *Agent) annotateCompactionSummary(block string) bool {
	block = strings.TrimSpace(block)
	if block == "" {
		return false
	}
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if strings.HasPrefix(a.Messages[i].Content, contextmgr.SummaryPrefix) {
			a.Messages[i].Content += "\n\n" + block
			if a.tokenCounts != nil && i < len(a.tokenCounts) {
				a.tokenCounts[i] = contextmgr.ComputeMessageTokens(a.Messages[i])
			}
			return true
		}
	}
	return false
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

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
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
		if strings.HasPrefix(m.Content, contextmgr.SummaryPrefix) {
			b.WriteString(m.Content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
