package contextmgr

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// segmentResult is one deep-dive segment after compaction: its replacement
// messages, whether a new summary was written, and why it was left as it was
// when it was not (nil for an empty or untouched segment).
type segmentResult struct {
	msgs       []llm.Message
	summarized bool
	skip       error
}

// deepDiveRefusal is a deep-dive compaction that summarized neither region.
// Unlike a summarizer failure, the single-summary layout may still succeed.
type deepDiveRefusal struct{ error }

func (r deepDiveRefusal) Unwrap() error { return r.error }

// compactAroundRequest is the deep-dive layout (Settings.DeepDive). The latest
// real user request at reqIdx stays verbatim, and the two regions around it
// are summarized separately:
//
//	pre-head, first user message, session summary (what came before the
//	request), the request, progress summary (the work since), tail
//
// A region that already is exactly one summary of its kind is left alone, so
// during one long turn only the progress summary is rewritten and the prefix
// up to the request stays byte-identical (prompt cache, no repeated
// re-summarizing of older history). A new request moves everything before it
// into the session summary at the next compaction.
//
// Each region keeps the single-summary guards: the minimum size unless
// forced, the smaller-summary guard, and its own executed-call roster. The
// compaction is refused only when neither region produced a summary.
func (m *Manager) compactAroundRequest(ctx context.Context, messages []llm.Message, headIdx, reqIdx, tailStart int, opts CompactOptions) ([]llm.Message, map[int]struct{}, error) {
	before, err := m.compactSegment(ctx, messages, headIdx+1, reqIdx, SummaryPrefix, summaryInstruction, opts)
	if err != nil {
		return nil, nil, err
	}
	since, err := m.compactSegment(ctx, messages, reqIdx+1, tailStart, ProgressSummaryPrefix, progressSummaryInstruction, opts)
	if err != nil {
		return nil, nil, err
	}
	if !before.summarized && !since.summarized {
		if since.skip != nil {
			return nil, nil, deepDiveRefusal{since.skip}
		}
		if before.skip != nil {
			return nil, nil, deepDiveRefusal{before.skip}
		}
		return nil, nil, deepDiveRefusal{fmt.Errorf("not enough history to compact (the regions around the current request are already summarized)")}
	}

	tail := cloneMessages(messages[tailStart:])
	compact := make([]llm.Message, 0, headIdx+1+len(before.msgs)+1+len(since.msgs)+len(tail))
	compact = append(compact, cloneMessages(messages[:headIdx+1])...)
	compact = append(compact, before.msgs...)
	compact = append(compact, cloneMessage(messages[reqIdx]))
	compact = append(compact, since.msgs...)
	compact = append(compact, tail...)
	return compact, remapPinsAfterCompact(opts.Pinned, headIdx, tailStart, len(compact)-len(tail)), nil
}

// compactSegment summarizes messages[start:end] into one message framed with
// prefix, using instruction. The summarization request carries messages[:start]
// as context, which is the wire history up to this region, so the provider's
// prompt cache covers it. Only a summarizer failure is returned as err; a
// region that is empty, already one summary, too small, or would not shrink
// comes back unchanged with skip set when it was a refusal.
func (m *Manager) compactSegment(ctx context.Context, messages []llm.Message, start, end int, prefix, instruction string, opts CompactOptions) (segmentResult, error) {
	seg := messages[start:end]
	if len(seg) == 0 {
		return segmentResult{}, nil
	}
	if isOnlySummary(seg, prefix) {
		return segmentResult{msgs: cloneMessages(seg)}, nil
	}
	unchanged := segmentResult{msgs: cloneMessages(seg)}

	segTokens := -1
	knownTokens := -1
	if opts.Counts != nil && len(opts.Counts) >= end {
		segTokens = middleTokensBetween(messages, opts.Counts, start, end)
		knownTokens = middleTokensBetween(messages, opts.Counts, 0, end)
	}
	if segTokens < 0 {
		segTokens = m.EstimateTokens(seg)
		for _, msg := range seg {
			segTokens -= imageTokenEstimate(msg)
		}
	}
	if !opts.Forced && segTokens < m.minMiddleTokens {
		unchanged.skip = fmt.Errorf("not enough history to compact (%d messages in the region)", len(seg))
		return unchanged, nil
	}

	summary, err := m.summarizeMiddle(ctx, opts.ViewPrefix, messages[:start], seg, knownTokens, opts.Tools, opts.AllowedTools, instruction)
	if err != nil {
		return segmentResult{}, err
	}
	recap := summary
	if roster := buildExecutedCallRoster(seg); roster != "" {
		recap += "\n\n" + roster
	}
	framed := llm.Message{Role: "assistant", Content: prefix + recap}
	if m.requireSummaryShrink {
		if framedTokens := m.EstimateTokens([]llm.Message{framed}); framedTokens >= segTokens {
			unchanged.skip = fmt.Errorf("%w (summary %d tokens, region %d tokens)", ErrSummaryNotSmaller, framedTokens, segTokens)
			return unchanged, nil
		}
	}
	return segmentResult{msgs: []llm.Message{framed}, summarized: true}, nil
}

// isOnlySummary reports whether seg is exactly one summary framed with prefix.
func isOnlySummary(seg []llm.Message, prefix string) bool {
	return len(seg) == 1 && strings.HasPrefix(seg[0].Content, prefix)
}

// deepDiveFreeable is the summarizable token weight of messages[headIdx+1:tailStart]
// for an emergency compaction in deep dive, sized as compactPinned will run
// it. The verbatim request, a region that already is one summary of its
// kind, and a region under minTokens stay as they are, so they free nothing.
// A split where no region would be summarized or no message would be
// dropped counts as freeing nothing, so the keep is lowered until the layout
// around the request frees enough; only when no split does is the request
// summarized, by compactPinned's fallback to the single summary. The caller
// budgets one summary (EmergencySummaryAllowance); when both regions are
// summarized, the second summary's allowance is charged here.
func deepDiveFreeable(messages []llm.Message, counts []int, headIdx, tailStart, minTokens int) int {
	reqIdx := deepDiveRequestIndex(messages, headIdx, tailStart)
	if reqIdx < 0 {
		return middleTokensBetween(messages, counts, headIdx+1, tailStart)
	}
	summaries, dropped, freed := 0, 0, 0
	region := func(start, end int, prefix string) {
		if start >= end || isOnlySummary(messages[start:end], prefix) {
			return
		}
		tokens := middleTokensBetween(messages, counts, start, end)
		if tokens < minTokens {
			return
		}
		summaries++
		dropped += end - start - 1
		freed += tokens
	}
	region(headIdx+1, reqIdx, SummaryPrefix)
	region(reqIdx+1, tailStart, ProgressSummaryPrefix)
	if summaries == 0 || dropped == 0 {
		return 0
	}
	if summaries > 1 {
		freed -= EmergencySummaryAllowance
	}
	return freed
}
