package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

func hasContent(msgs []llm.Message, content string) bool {
	for _, m := range msgs {
		if m.Content == content {
			return true
		}
	}
	return false
}

// seedDominantRequest builds a deep-dive history around a request that is
// most of the window: head, session summary, the request, a progress
// summary, and work after it that the layout around the request can
// summarize, so that layout succeeds and still leaves the request verbatim.
func seedDominantRequest(a *Agent, request string, after ...string) {
	a.SetDeepDive(true)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"})
	for _, content := range after {
		a.appendMessage(llm.Message{Role: "assistant", Content: content})
	}
	_ = a.ContextStats(context.Background())
}

// TestDeepDiveOverflowSummarizesDominantRequest pins the overflow path when
// the estimate already says the request is over the window: the provider
// refused, deep dive summarizes the request in that pass, and the one retry
// is spent on a history that fits.
func TestDeepDiveOverflowSummarizesDominantRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 300000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000))

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if !retry || err != nil {
		t.Fatalf("retry = %v, err = %v; want a retry", retry, err)
	}
	if hasContent(a.Messages, big) {
		t.Fatal("the oversized request is still verbatim")
	}
	for _, m := range a.Messages {
		if contextmgr.IsCondensedMessage(m.Content) || strings.HasPrefix(m.Content, contextmgr.ProgressSummaryPrefix) {
			t.Fatalf("want the single summary, got a deep-dive or condensed message: %.60q", m.Content)
		}
	}
	if total, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); total >= limit {
		t.Fatalf("view after recovery = %d tokens, window %d", total, limit)
	}
}

// TestOverflowHugeHeadReachesLastResort pins the overflow path's fit check
// in default mode: a huge first message with history after it is shrunk by
// the forced compaction and still does not fit, so last-resort
// condensation runs before the retry is spent.
func TestOverflowHugeHeadReachesLastResort(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 300000)
	a.appendMessage(llm.Message{Role: "user", Content: big})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
	_ = a.ContextStats(context.Background())

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if !retry || err != nil {
		t.Fatalf("retry = %v, err = %v; want a retry", retry, err)
	}
	condensed := false
	for _, m := range a.Messages {
		condensed = condensed || contextmgr.IsCondensedMessage(m.Content)
	}
	if hasContent(a.Messages, big) || !condensed {
		t.Fatal("the oversized head was not condensed")
	}
	if total, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); total >= limit {
		t.Fatalf("view after recovery = %d tokens, window %d", total, limit)
	}
}

// TestEmergencyDeepDiveDominantRequest pins the emergency tier's fit
// fallback: the layout around a request that is most of the window
// summarizes the work at every keep and stays over the hard limit, so the
// tier summarizes the request too instead of recording a failure.
func TestEmergencyDeepDiveDominantRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 120000)
	seedDominantRequest(a, big, strings.Repeat("y", 6000), "tail1", strings.Repeat("z", 10000))
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if hasContent(a.Messages, big) {
		t.Fatal("the dominant request is still verbatim")
	}
	if total := a.compactionTokenTotal(); total >= ctxMgr.HardLimit() {
		t.Fatalf("total after the emergency tier = %d, hard limit %d", total, ctxMgr.HardLimit())
	}
	if backoff, guard := compactState(a); !backoff.IsZero() || guard != 0 {
		t.Fatalf("recorded a failure: backoff %v guard %d", backoff, guard)
	}
}

// TestCompactHistoryNothingToCompact pins that /compact (CompactHistory,
// shared by the TUI and the web UI) does not report success when the
// compaction changed nothing: in deep dive, the regions around the request
// already are one summary each.
func TestCompactHistoryNothingToCompact(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	seedDominantRequest(a, "the request", "tail one", "tail two")
	before := append([]llm.Message(nil), a.Messages...)

	err := a.CompactHistory(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nothing to compact") {
		t.Fatalf("err = %v, want nothing to compact", err)
	}
	if len(a.Messages) != len(before) || provider.calls != 0 {
		t.Fatalf("messages %d -> %d, %d summarizer calls", len(before), len(a.Messages), provider.calls)
	}
}

// TestNormalTierKeepsLargeRequest pins that the fit fallback belongs to the
// emergency tier only: below the hard limit, the normal tier keeps a large
// request verbatim (the point of deep dive) even when it stays over the
// budget.
func TestNormalTierKeepsLargeRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 80000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000), "tail1", "tail2")
	budget, hard := ctxMgr.CompactBudget(), ctxMgr.HardLimit()
	if total := a.compactionTokenTotal(); total < budget || total >= hard {
		t.Fatalf("test setup: total %d not in [budget %d, hard limit %d)", total, budget, hard)
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 2, false)
	if !hasContent(a.Messages, big) {
		t.Fatal("the normal tier summarized the request")
	}
}

// TestDeepDiveOverflowRefusalUnderEstimate pins the refusal the run loop
// actually produces. Pre-flight compacts only when the estimate is already
// over the window, so a provider refusal arrives with the estimate under
// it. Deep dive still summarizes the request in that one pass. A layout
// that kept the request would spend the retry on a request the provider
// has rejected, and the second refusal would end the turn.
func TestDeepDiveOverflowRefusalUnderEstimate(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 60000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000))
	if est, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); est >= limit {
		t.Fatalf("test setup: estimate %d is not under the window %d", est, limit)
	}

	retry, err := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if !retry || err != nil {
		t.Fatalf("retry = %v, err = %v; want a retry", retry, err)
	}
	if hasContent(a.Messages, big) {
		t.Fatal("the refused request is still verbatim")
	}
	if layoutKeptRequest(a.Messages) {
		t.Fatal("the deep-dive layout kept the request")
	}
	if n := atomic.LoadInt64(&provider.calls); n != 1 {
		t.Fatalf("summarizer calls = %d, want the one single-summary pass", n)
	}
	if total, limit := a.outgoingViewEstimate(a.rebuildView()), ctxMgr.ContextLimit(); total >= limit {
		t.Fatalf("view after recovery = %d tokens, window %d", total, limit)
	}
}

// TestPreflightDoesNotResummarizeASingleSummary pins that the pre-flight
// fallback does not run when the first pass already used the single summary.
// The request is the head, so there is no progress summary, and a second
// pass would only re-summarize that summary.
func TestPreflightDoesNotResummarizeASingleSummary(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	a.SetDeepDive(true)
	a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 300000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 4000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("z", 4000)})
	_ = a.ContextStats(context.Background())
	if est := a.outgoingViewEstimate(a.rebuildView()); est < ctxMgr.ContextLimit() {
		t.Fatalf("test setup: estimate %d under the window", est)
	}

	if !a.runForcedSummarization(context.Background(), nil, false) {
		t.Fatal("expected the history to shrink")
	}
	if n := atomic.LoadInt64(&provider.calls); n != 1 {
		t.Fatalf("summarizer calls = %d, want 1", n)
	}
}

// TestPreflightFallbackFailureIsRecorded pins that a failed single-summary
// fallback, after the layout kept the request, is recorded like the forced
// path's other failures. Publishing the layout resets the backoff, so a
// failure that is only logged would be tried again next round.
func TestPreflightFallbackFailureIsRecorded(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	provider.failAfter = 2
	big := strings.Repeat("x", 300000)
	seedDominantRequest(a, big, strings.Repeat("y", 8000))

	if !a.runForcedSummarization(context.Background(), nil, false) {
		t.Fatal("the layout pass should have shrunk the history")
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("the fallback published a summary of the request")
	}
	if backoff, guard := compactState(a); backoff.IsZero() || guard == 0 {
		t.Fatalf("failure not recorded: backoff %v guard %d", backoff, guard)
	}
}

// TestOverflowCancelSkipsLastResort pins that a cancel during the forced
// pass returns the cancel and does not condense. Last resort would
// otherwise run on the cancelled context and the turn would still retry.
func TestOverflowCancelSkipsLastResort(t *testing.T) {
	a, _, provider := newEmergencyTestAgent(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider.cancel = cancel
	big := strings.Repeat("x", 300000)
	a.appendMessage(llm.Message{Role: "user", Content: big})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	_ = a.ContextStats(context.Background())

	retry, err := a.handleOverflowError(ctx, nil, llm.ErrContextWindowExceeded)
	if retry || !errors.Is(err, context.Canceled) {
		t.Fatalf("retry = %v, err = %v; want the cancel", retry, err)
	}
	if !hasContent(a.Messages, big) {
		t.Fatal("last resort condensed the head")
	}
	if _, lastResort := guardsState(a); lastResort != 0 {
		t.Fatalf("last-resort guard = %d, want 0", lastResort)
	}
}

// TestNormalTierOverHardLimitKeepsRequest pins the emergency gate on its
// own. The total is at or above the hard limit, but the tier is the normal
// one, so the request stays verbatim. Removing the emergency condition
// would summarize it.
func TestNormalTierOverHardLimitKeepsRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 120000)
	seedDominantRequest(a, big, strings.Repeat("y", 6000), "tail1", strings.Repeat("z", 10000))
	if total := a.compactionTokenTotal(); total < ctxMgr.HardLimit() {
		t.Fatalf("test setup: total %d under the hard limit %d", total, ctxMgr.HardLimit())
	}

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, false)
	if !hasContent(a.Messages, big) {
		t.Fatal("the normal tier summarized the request")
	}
	if total := a.compactionTokenTotal(); total < ctxMgr.CompactBudget() {
		t.Fatalf("test setup: compaction reached the budget (%d) before the fallback check", total)
	}
}

// TestEmergencyBelowHardLimitKeepsRequest pins the hard-limit gate on its
// own. The tier is emergency, but the total stays under the hard limit, so
// the request stays verbatim. Removing the hard-limit condition would
// summarize it.
func TestEmergencyBelowHardLimitKeepsRequest(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	big := strings.Repeat("x", 100000)
	seedDominantRequest(a, big, strings.Repeat("y", 6000), "tail1", strings.Repeat("z", 10000))
	budget, hard := ctxMgr.CompactBudget(), ctxMgr.HardLimit()

	a.compactToFit(context.Background(), nil, cachedCounts(t, a), 1, true)
	if !hasContent(a.Messages, big) {
		t.Fatal("the emergency tier summarized the request below the hard limit")
	}
	if total := a.compactionTokenTotal(); total < budget || total >= hard {
		t.Fatalf("test setup: total %d left [budget %d, hard limit %d) before the fallback check", total, budget, hard)
	}
}
