package contextmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// segmentResult is one deep-dive segment after compaction: its replacement
// messages, whether a new summary was written, and skip when a summary was
// written but refused as not smaller (nil when the segment was left alone).
type segmentResult struct {
	msgs       []llm.Message
	summarized bool
	skip       error
}

// deepDiveRefusal is a deep-dive compaction that summarized neither region
// because a summary came back no smaller. Unlike a summarizer failure, the
// single-summary layout may still succeed.
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
// forced, the smaller-summary guard, and its own executed-call roster. When
// neither region produced a summary, the history comes back unchanged, or
// refused when a summary was not smaller.
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
		// A summary that came back no smaller cost a summarizer call, so the
		// caller backs off, as in the single-summary layout. Otherwise
		// nothing was attempted: the regions are single summaries or under
		// the minimum size, the normal state shortly after a compaction. The
		// history then comes back unchanged with no error, so the caller
		// tries a smaller tail instead of backing off.
		for _, seg := range []segmentResult{since, before} {
			if errors.Is(seg.skip, ErrSummaryNotSmaller) {
				return nil, nil, deepDiveRefusal{seg.skip}
			}
		}
		return messages, copyIntSet(opts.Pinned), nil
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
// prompt cache covers it. For the progress region that is the original
// history up to and including the request, unsummarized: the same size as
// the single-summary request, and when it does not fit the summary budget,
// summarizeMiddle uses its flattened fallback, which the prompt cache does
// not cover. Only a summarizer failure is returned as err. A region
// planSegment leaves alone comes back unchanged; one whose summary would not
// shrink comes back unchanged with skip set.
func (m *Manager) compactSegment(ctx context.Context, messages []llm.Message, start, end int, prefix, instruction string, opts CompactOptions) (segmentResult, error) {
	seg := messages[start:end]
	plan := m.planSegment(messages, opts.Counts, start, end, prefix, m.minMiddleTokens, opts.Forced)
	unchanged := segmentResult{msgs: cloneMessages(seg)}
	if !plan.summarize {
		return unchanged, nil
	}
	segTokens := plan.tokens
	knownTokens := -1
	if opts.Counts != nil && len(opts.Counts) >= end {
		knownTokens = middleTokensBetween(messages, opts.Counts, 0, end)
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

// segmentPlan is what a deep-dive compaction does with one region: whether
// it is summarized, and its token weight with images excluded (0 when the
// region is left alone before it is measured).
type segmentPlan struct {
	summarize bool
	tokens    int
}

// planSegment holds the region rules shared by compactSegment and
// deepDiveFreeable, so the emergency sizing frees what the compaction does.
// An empty region and a region that already is one summary of its kind are
// left alone, and so is a region under minTokens unless forced. The weight
// comes from counts when they cover the region, otherwise from an estimate.
func (m *Manager) planSegment(messages []llm.Message, counts []int, start, end int, prefix string, minTokens int, forced bool) segmentPlan {
	seg := messages[start:end]
	if len(seg) == 0 || isOnlySummary(seg, prefix) {
		return segmentPlan{}
	}
	var tokens int
	if counts != nil && len(counts) >= end {
		tokens = middleTokensBetween(messages, counts, start, end)
	} else {
		tokens = m.EstimateTokens(seg)
		for _, msg := range seg {
			tokens -= imageTokenEstimate(msg)
		}
	}
	return segmentPlan{summarize: forced || tokens >= minTokens, tokens: tokens}
}

// isOnlySummary reports whether seg is exactly one summary framed with prefix.
func isOnlySummary(seg []llm.Message, prefix string) bool {
	return len(seg) == 1 && strings.HasPrefix(seg[0].Content, prefix)
}

// deepDiveFreeable is the summarizable token weight of messages[headIdx+1:tailStart]
// for an emergency compaction in deep dive, sized as compactPinned will run
// it. The verbatim request, and a region planSegment leaves alone (already
// one summary of its kind, or under minTokens), free nothing.
// A split where no region would be summarized or no message would be
// dropped counts as freeing nothing, so the keep is lowered until the layout
// around the request frees enough; only when no split does is the request
// summarized, by compactPinned's fallback to the single summary. The caller
// budgets one summary (EmergencySummaryAllowance); when both regions are
// summarized, the second summary's allowance is charged here.
func (m *Manager) deepDiveFreeable(messages []llm.Message, counts []int, headIdx, tailStart, minTokens int) int {
	reqIdx := deepDiveRequestIndex(messages, headIdx, tailStart)
	if reqIdx < 0 {
		return middleTokensBetween(messages, counts, headIdx+1, tailStart)
	}
	summaries, dropped, freed := 0, 0, 0
	region := func(start, end int, prefix string) {
		plan := m.planSegment(messages, counts, start, end, prefix, minTokens, false)
		if !plan.summarize {
			return
		}
		summaries++
		dropped += end - start - 1
		freed += plan.tokens
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
