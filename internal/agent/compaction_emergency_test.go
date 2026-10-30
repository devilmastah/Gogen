package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// countingProvider is a stub provider that counts summarization calls
// (GenerateResponse) and can fail every call, so tests can observe whether
// prepareMessages attempted a compaction and force the failure paths. Note
// one compaction attempt may make several provider calls (the manager falls
// back to flattened-text summarization after a primary-path error), so
// tests assert on "no new calls" / "new calls" deltas, not exact counts,
// unless the summarized text is small enough for one call.
// failAfter, when set, fails once calls reaches that count. cancel, when
// set, cancels that context on the first call.
type countingProvider struct {
	limit     int
	fail      bool
	failAfter int64
	cancel    context.CancelFunc
	calls     int64
}

func (p *countingProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	n := atomic.AddInt64(&p.calls, 1)
	if p.cancel != nil {
		p.cancel()
	}
	if p.fail || (p.failAfter > 0 && n >= p.failAfter) {
		return llm.Response{}, errors.New("summarization unavailable")
	}
	return llm.Response{Content: "summary"}, nil
}

func (p *countingProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (p *countingProvider) ModelContextLimit(_ context.Context) (int, error) { return p.limit, nil }
func (p *countingProvider) SetThinkingLevel(string)                          {}
func (p *countingProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (p *countingProvider) SetModel(string) error { return nil }
func (p *countingProvider) ModelName() string     { return "test-model" }

// newEmergencyTestAgent builds an agent with a small context window so the
// hard window is reachable: limit 20000, threshold 0.85 (budget 13000),
// reserve 4000 (hard limit 16000), keep-recent 2.
func newEmergencyTestAgent(t *testing.T, fail bool) (*Agent, *contextmgr.Manager, *countingProvider) {
	t.Helper()
	provider := &countingProvider{limit: 20000, fail: fail}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              20000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	return a, ctxMgr, provider
}

// seedConversation appends a 6-message conversation (one short head, three
// large middle messages so the compaction middle clears minMiddleTokens,
// two short tail messages), fills the per-message count cache, and pins the
// estimated total to total via a fresh provider baseline — so the trigger
// math is exact regardless of the wire overhead.
func seedConversation(t *testing.T, a *Agent, total int) {
	t.Helper()
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	for i := 0; i < 3; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 2000)})
	}
	for i := 0; i < 2; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	}
	_ = a.ContextStats(context.Background())
	a.recordTurnUsage(&llm.Usage{PromptTokens: total, CompletionTokens: 10})
}

// compactState snapshots the failure backoff deadline and the emergency
// progress guard under statsMu.
func compactState(a *Agent) (backoff time.Time, guard int) {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return a.compactBackoffUntil, a.compactionGuards.emergency
}

// guardsState snapshots both compaction progress guards under statsMu.
func guardsState(a *Agent) (emergency, lastResort int) {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return a.compactionGuards.emergency, a.compactionGuards.lastResort
}

// TestProgressGuardResetAsymmetry pins the per-tier reset semantics of the
// compaction progress guards: a successful COMPACTION (published through
// publishCompaction → noteCompactSuccess) resets the emergency guard only,
// and a successful last-resort CONDENSATION resets the lastResort guard
// only. A blanket "reset all guards" success helper would break both
// assertions.
func TestProgressGuardResetAsymmetry(t *testing.T) {
	// 1. A successful compaction must not clear a recorded last-resort
	// failure.
	a, _, _ := newEmergencyTestAgent(t, false)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.noteProgressFailure(&a.compactionGuards.lastResort)
	if _, lr := guardsState(a); lr != 1 {
		t.Fatalf("setup: lastResort guard = %d, want 1 (message count at failure)", lr)
	}
	// A successful compaction publishes through publishCompaction, which
	// calls noteCompactSuccess.
	a.publishCompaction([]llm.Message{{Role: "user", Content: "hello"}}, nil)
	if e, lr := guardsState(a); lr != 1 {
		t.Fatalf("successful compaction cleared the lastResort guard: %d, want 1", lr)
	} else if e != 0 {
		t.Fatalf("emergency guard = %d, want 0 (untouched)", e)
	}

	// 2. A successful last-resort condensation must not clear a recorded
	// emergency failure: a fresh session whose single message is over the
	// window (no middle to summarize, so forced compaction is a no-op)
	// recovers via the condensation.
	a2, mgr, _, _ := newLastResortTestAgent(t)
	a2.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 2200)})
	_ = a2.ContextStats(context.Background())
	setLimitOverBy(t, a2, mgr, 50)
	a2.noteProgressFailure(&a2.compactionGuards.emergency)
	if e, _ := guardsState(a2); e != 1 {
		t.Fatalf("setup: emergency guard = %d, want 1 (message count at failure)", e)
	}
	view, err := a2.prepareMessages(context.Background(), nil)
	if err != nil {
		t.Fatalf("prepareMessages: %v", err)
	}
	if len(a2.Messages) != 1 || !contextmgr.IsCondensedMessage(a2.Messages[0].Content) {
		t.Fatalf("history = %d message(s), want the condensed message (condensation ran)", len(a2.Messages))
	}
	if e, lr := guardsState(a2); lr != 0 {
		t.Fatalf("lastResort guard = %d, want 0 (reset by the successful condensation)", lr)
	} else if e != 1 {
		t.Fatalf("successful condensation cleared the emergency guard: %d, want 1", e)
	}
	if est := a2.outgoingViewEstimate(view); est >= mgr.ContextLimit() {
		t.Fatalf("estimate after condensation = %d, want < limit %d", est, mgr.ContextLimit())
	}
}

// toolRecordingProvider extends countingProvider with the (allowlist, tools)
// pair the last summarization call received.
type toolRecordingProvider struct {
	countingProvider
	lastTools   []llm.Tool
	lastAllowed map[string]struct{}
}

func (p *toolRecordingProvider) GenerateResponse(ctx context.Context, msgs []llm.Message, allowed map[string]struct{}, tools []llm.Tool) (llm.Response, error) {
	p.lastTools = tools
	p.lastAllowed = allowed
	return p.countingProvider.GenerateResponse(ctx, msgs, allowed, tools)
}

func toolNames(tools []llm.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

// assertTurnToolSetPassed verifies the summarization request carried exactly
// the turn's (AllowedToolNames, llmTools) pair — the prompt-cache fix: with
// the same tool set on the wire, the rendered prefix matches the last turn
// and the provider's prompt cache covers the summarized middle.
func assertTurnToolSetPassed(t *testing.T, a *Agent, p *toolRecordingProvider) {
	t.Helper()
	if len(p.lastTools) == 0 {
		t.Fatal("summarization request carried no tool definitions")
	}
	want := toolNames(a.llmTools())
	got := toolNames(p.lastTools)
	if len(got) != len(want) {
		t.Fatalf("summarization tools = %d, want %d (the turn's set)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("summarization tool[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(p.lastAllowed) != len(a.AllowedToolNames()) {
		t.Fatalf("summarization allowlist = %d entries, want %d (the turn's set)", len(p.lastAllowed), len(a.AllowedToolNames()))
	}
}

// TestCompactHistoryPassesTurnToolSet pins the agent-side half of the
// prompt-cache fix for the manual path: /compact passes the turn's tool set
// to the summarization request.
func TestCompactHistoryPassesTurnToolSet(t *testing.T) {
	provider := &toolRecordingProvider{countingProvider: countingProvider{limit: 20000}}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              20000,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("x", 4000)})
	a.appendMessage(llm.Message{Role: "user", Content: "more"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "ok"})
	_ = a.ContextStats(context.Background())
	if err := a.CompactHistory(context.Background()); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	assertTurnToolSetPassed(t, a, provider)
}

// TestPrepareMessagesCompactionPassesTurnToolSet pins the same for the
// auto-compaction path (prepareMessages -> compactToFit).
func TestPrepareMessagesCompactionPassesTurnToolSet(t *testing.T) {
	provider := &toolRecordingProvider{countingProvider: countingProvider{limit: 20000}}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              20000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, ctxMgr)
	seedConversation(t, a, 14000) // budget = 0.85*20000 - 4000 = 13000: triggers
	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatalf("prepareMessages: %v", err)
	}
	if !contextmgr.IsCompactionSummary(a.Messages[1].Content) {
		t.Fatalf("compaction did not run: message[1] = %q", a.Messages[1].Content)
	}
	assertTurnToolSetPassed(t, a, provider)
}

// cachedCounts returns a copy of the agent's per-message token count cache.
func cachedCounts(t *testing.T, a *Agent) []int {
	t.Helper()
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return append([]int(nil), a.tokenCounts...)
}

// TestEmergencyCompactBypassesBackoff pins the core fix: with the failure
// backoff active and the total past the hard window, the emergency tier
// still attempts a compaction (a provider refusal is worse than a redundant
// summarization call); at an unchanged message count the progress guard
// skips the attempt; when the count grows, the attempt runs again.
func TestEmergencyCompactBypassesBackoff(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, true)
	hard := ctxMgr.HardLimit()
	if hard <= 0 {
		t.Fatalf("HardLimit = %d, want > 0", hard)
	}
	seedConversation(t, a, hard+100)

	// First attempt (normal tier due, no backoff yet) fails and starts the
	// 30s backoff, and the emergency guard records the message count.
	a.prepareMessages(context.Background(), nil)
	calls1 := atomic.LoadInt64(&provider.calls)
	backoff1, guard1 := compactState(a)
	if calls1 == 0 {
		t.Fatal("first attempt: no summarization calls")
	}
	if backoff1.IsZero() || time.Now().After(backoff1) {
		t.Fatalf("expected the failure backoff to be active after the failed attempt (until %v)", backoff1)
	}
	if guard1 != len(a.Messages) {
		t.Fatalf("emergency guard = %d, want %d (message count at failure)", guard1, len(a.Messages))
	}

	// Same message count: the progress guard suppresses the emergency tier
	// and the backoff suppresses the normal tier → no new attempt.
	a.prepareMessages(context.Background(), nil)
	if got := atomic.LoadInt64(&provider.calls); got != calls1 {
		t.Fatalf("same-count retry: %d -> %d summarization calls, want no new calls (progress guard)", calls1, got)
	}
	if backoff2, guard2 := compactState(a); !backoff2.Equal(backoff1) || guard2 != guard1 {
		t.Fatalf("attempt state changed without an attempt: backoff %v->%v guard %d->%d", backoff1, backoff2, guard1, guard2)
	}

	// Count grows: the emergency tier bypasses the still-active backoff.
	a.appendMessage(llm.Message{Role: "user", Content: "another"})
	a.prepareMessages(context.Background(), nil)
	if got := atomic.LoadInt64(&provider.calls); got <= calls1 {
		t.Fatalf("grown-count retry: %d -> %d summarization calls, want a new attempt (emergency bypass)", calls1, got)
	}
	backoff3, guard3 := compactState(a)
	if !backoff3.After(backoff1) {
		t.Fatalf("backoff not advanced by the second failure: %v -> %v", backoff1, backoff3)
	}
	if guard3 != len(a.Messages) {
		t.Fatalf("emergency guard = %d, want %d (message count at second failure)", guard3, len(a.Messages))
	}
}

// TestEmergencyProgressGuardRepeatedFailures pins that repeated failures at
// the same message count do not hot-loop: only the first attempt runs.
func TestEmergencyProgressGuardRepeatedFailures(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, true)
	seedConversation(t, a, ctxMgr.HardLimit()+50)

	a.prepareMessages(context.Background(), nil)
	calls1 := atomic.LoadInt64(&provider.calls)
	if calls1 == 0 {
		t.Fatal("first turn: no summarization calls")
	}
	for i := 0; i < 2; i++ {
		a.prepareMessages(context.Background(), nil)
	}
	if got := atomic.LoadInt64(&provider.calls); got != calls1 {
		t.Fatalf("%d -> %d summarization calls over 3 turns at the same count, want no new calls", calls1, got)
	}
}

// TestBackoffHonoredBelowHardLimit pins the regression guard: below the hard
// window the emergency tier never fires, so the failure backoff is honored
// exactly as before the change (including the retry once it expires). A
// normal-tier failure also must not set the emergency progress guard.
func TestBackoffHonoredBelowHardLimit(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, true)
	budget, hard := ctxMgr.CompactBudget(), ctxMgr.HardLimit()
	if budget+100 >= hard {
		t.Fatalf("test setup: budget %d + 100 not below hard limit %d", budget, hard)
	}
	seedConversation(t, a, budget+100)

	// Normal tier fires (above the threshold budget) and fails → backoff.
	a.prepareMessages(context.Background(), nil)
	calls1 := atomic.LoadInt64(&provider.calls)
	if calls1 == 0 {
		t.Fatal("first attempt: no summarization calls")
	}

	// Below the hard limit the backoff suppresses the next attempt — the
	// pre-change behavior, untouched.
	a.prepareMessages(context.Background(), nil)
	if got := atomic.LoadInt64(&provider.calls); got != calls1 {
		t.Fatalf("below hard limit: %d -> %d summarization calls, want no new calls (backoff honored)", calls1, got)
	}
	if _, guard := compactState(a); guard != 0 {
		t.Fatalf("normal-tier failure set the emergency progress guard to %d, want 0", guard)
	}

	// Once the backoff expires, the normal tier retries.
	a.statsMu.Lock()
	a.compactBackoffUntil = time.Time{}
	a.statsMu.Unlock()
	a.prepareMessages(context.Background(), nil)
	if got := atomic.LoadInt64(&provider.calls); got <= calls1 {
		t.Fatalf("after backoff expiry: %d -> %d summarization calls, want a new attempt", calls1, got)
	}
}

// TestEmergencyOnlyInsideHardWindow pins the regression property: the
// emergency tier fires only at/above the hard window, never at the
// threshold budget — so normal-tier behavior is untouched. It also fires
// inside the hard window even when the normal tier's keep-recent guard
// blocks it (the large-user-message scenario the fix targets).
func TestEmergencyOnlyInsideHardWindow(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, false)
	budget, hard := ctxMgr.CompactBudget(), ctxMgr.HardLimit()

	// At the threshold budget: normal tier fires, emergency tier does not.
	seedConversation(t, a, budget+100)
	if !a.shouldCompactUsingCounts() {
		t.Fatal("normal tier should fire at the threshold budget")
	}
	if a.emergencyCompactDue(a.compactionTokenTotal()) {
		t.Fatal("emergency tier fired below the hard window")
	}

	// Inside the hard window: emergency tier fires.
	seedConversation(t, a, hard+100)
	if !a.emergencyCompactDue(a.compactionTokenTotal()) {
		t.Fatal("emergency tier should fire at the hard window")
	}

	// Inside the hard window with only keep+1 messages: the normal tier's
	// keep-recent guard blocks it, but the emergency tier still fires —
	// the keep is lowered to 1 so a compactable middle exists.
	a2, ctxMgr2, _ := newEmergencyTestAgent(t, false)
	for i := 0; i < 3; i++ {
		a2.appendMessage(llm.Message{Role: "user", Content: "hello"})
	}
	_ = a2.ContextStats(context.Background())
	a2.recordTurnUsage(&llm.Usage{PromptTokens: ctxMgr2.HardLimit() + 100, CompletionTokens: 10})
	if a2.shouldCompactUsingCounts() {
		t.Fatal("normal tier should be blocked by the keep-recent guard at keep+1 messages")
	}
	if !a2.emergencyCompactDue(a2.compactionTokenTotal()) {
		t.Fatal("emergency tier should fire inside the hard window with keep+1 messages")
	}
	if k := ctxMgr2.ChooseCompactKeep(a2.Messages, cachedCounts(t, a2), 1000, nil); k != 1 {
		t.Fatalf("ChooseCompactKeep at keep+1 messages = %d, want 1 (lowered keep)", k)
	}

	// With only 2 messages there is nothing between the starting prompt and
	// the current one to summarize: the emergency tier must not fire.
	a3, ctxMgr3, _ := newEmergencyTestAgent(t, false)
	a3.appendMessage(llm.Message{Role: "user", Content: "start"})
	a3.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 2000)})
	_ = a3.ContextStats(context.Background())
	a3.recordTurnUsage(&llm.Usage{PromptTokens: ctxMgr3.HardLimit() + 500, CompletionTokens: 10})
	if a3.emergencyCompactDue(a3.compactionTokenTotal()) {
		t.Fatal("emergency tier fired with only 2 messages (nothing to summarize)")
	}
}

// TestEmergencyLowersKeepToCoverDeficit pins the worst-case path: when the
// middle at the configured keep cannot save enough tokens, the keep is
// lowered to the floor of 1 — the old messages are included in the
// summarized middle (never dropped), while the starting prompt and the
// current user prompt are preserved verbatim.
func TestEmergencyLowersKeepToCoverDeficit(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	// 5 messages: head, two large middle, one recent, current prompt.
	// At keep=2 the middle is only the two large messages (~1000 tokens),
	// far below the deficit (total - hard + allowance), so the keep must
	// drop to the floor of 1.
	a.appendMessage(llm.Message{Role: "user", Content: "start"})
	a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 4000)})
	a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("y", 4000)})
	a.appendMessage(llm.Message{Role: "user", Content: "recent"})
	a.appendMessage(llm.Message{Role: "user", Content: "current"})
	_ = a.ContextStats(context.Background())
	a.recordTurnUsage(&llm.Usage{PromptTokens: ctxMgr.HardLimit() + 1200, CompletionTokens: 10})

	total := a.compactionTokenTotal()
	needed := total - ctxMgr.HardLimit() + contextmgr.EmergencySummaryAllowance
	if k := ctxMgr.ChooseCompactKeep(a.Messages, cachedCounts(t, a), needed, nil); k != 1 {
		t.Fatalf("ChooseCompactKeep = %d, want 1 (deficit %d exceeds the keep-2 middle)", k, needed)
	}

	a.prepareMessages(context.Background(), nil)
	if got := atomic.LoadInt64(&provider.calls); got == 0 {
		t.Fatal("no summarization calls: the emergency attempt did not run")
	}
	// head + summary + tail(1): the starting prompt and the current user
	// prompt survive verbatim, the old messages live on inside the summary.
	if len(a.Messages) != 3 {
		t.Fatalf("messages after emergency compaction = %d, want 3 (head + summary + tail)", len(a.Messages))
	}
	if a.Messages[0].Content != "start" {
		t.Fatalf("head message = %q, want the starting prompt preserved verbatim", a.Messages[0].Content)
	}
	if a.Messages[2].Content != "current" {
		t.Fatalf("tail message = %q, want the current user prompt preserved verbatim", a.Messages[2].Content)
	}
	if !contextmgr.IsCompactionSummary(a.Messages[1].Content) {
		t.Fatalf("middle message = %q, want a compaction summary", a.Messages[1].Content)
	}
}

// TestEmergencyKeepUnchangedWhenMiddleSufficient pins that the keep is
// lowered only as a worst case: when the middle at the configured keep
// already covers the deficit, the recent tail is preserved verbatim and
// untouched.
func TestEmergencyKeepUnchangedWhenMiddleSufficient(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	// 8 messages: head, five large middle, two recent. At keep=2 the middle
	// (~3500 tokens) covers the small deficit, so the keep stays at 2.
	a.appendMessage(llm.Message{Role: "user", Content: "start"})
	for i := 0; i < 5; i++ {
		a.appendMessage(llm.Message{Role: "user", Content: strings.Repeat("x", 4000)})
	}
	a.appendMessage(llm.Message{Role: "user", Content: "recent"})
	a.appendMessage(llm.Message{Role: "user", Content: "current"})
	_ = a.ContextStats(context.Background())
	a.recordTurnUsage(&llm.Usage{PromptTokens: ctxMgr.HardLimit() + 100, CompletionTokens: 10})

	total := a.compactionTokenTotal()
	needed := total - ctxMgr.HardLimit() + contextmgr.EmergencySummaryAllowance
	if k := ctxMgr.ChooseCompactKeep(a.Messages, cachedCounts(t, a), needed, nil); k != ctxMgr.CompactKeepRecentMessages() {
		t.Fatalf("ChooseCompactKeep = %d, want %d (middle already covers the deficit)",
			k, ctxMgr.CompactKeepRecentMessages())
	}

	a.prepareMessages(context.Background(), nil)
	if got := atomic.LoadInt64(&provider.calls); got == 0 {
		t.Fatal("no summarization calls: the emergency attempt did not run")
	}
	// head + summary + tail(2): both recent messages survive verbatim.
	if len(a.Messages) != 4 {
		t.Fatalf("messages after emergency compaction = %d, want 4 (head + summary + tail 2)", len(a.Messages))
	}
	if a.Messages[2].Content != "recent" || a.Messages[3].Content != "current" {
		t.Fatalf("tail = %q, %q; want both recent messages preserved verbatim",
			a.Messages[2].Content, a.Messages[3].Content)
	}
}

// TestEmergencySuccessResetsBackoffAndGuard pins that a successful
// emergency compaction resets both the failure backoff and the progress
// guard.
func TestEmergencySuccessResetsBackoffAndGuard(t *testing.T) {
	a, ctxMgr, provider := newEmergencyTestAgent(t, true)
	seedConversation(t, a, ctxMgr.HardLimit()+50)

	// Emergency attempt fails → backoff + progress guard set.
	a.prepareMessages(context.Background(), nil)
	backoff1, guard1 := compactState(a)
	if backoff1.IsZero() || guard1 == 0 {
		t.Fatalf("after failed attempt: backoff %v guard %d, want both set", backoff1, guard1)
	}

	// The summarization path recovers and the conversation grows.
	provider.fail = false
	a.appendMessage(llm.Message{Role: "user", Content: "another"})
	before := len(a.Messages)
	a.prepareMessages(context.Background(), nil)
	if len(a.Messages) >= before {
		t.Fatalf("messages after successful compaction = %d, want < %d (compacted)", len(a.Messages), before)
	}
	backoff, guard := compactState(a)
	if !backoff.IsZero() {
		t.Fatalf("backoff not reset after successful compaction: %v", backoff)
	}
	if guard != 0 {
		t.Fatalf("progress guard not reset after successful compaction: %d", guard)
	}
}

// TestEmergencySkippedWhenCountsIncomplete pins that the emergency tier
// never fires on an incomplete count cache (total -1): that state falls
// back to the normal tier's full-estimate path, exactly as before.
func TestEmergencySkippedWhenCountsIncomplete(t *testing.T) {
	a, ctxMgr, _ := newEmergencyTestAgent(t, true)
	seedConversation(t, a, ctxMgr.HardLimit()+100)
	a.replaceMessages(a.Messages) // clears the counts cache
	if got := a.compactionTokenTotal(); got != -1 {
		t.Fatalf("compactionTokenTotal with incomplete cache = %d, want -1", got)
	}
	if a.emergencyCompactDue(a.compactionTokenTotal()) {
		t.Fatal("emergency tier fired on an incomplete count cache")
	}
	a.prepareMessages(context.Background(), nil)
	// Whatever the normal tier's full-estimate fallback decides, the
	// emergency tier must not have run (its guard stays unset).
	if _, guard := compactState(a); guard != 0 {
		t.Fatalf("incomplete-cache attempt set the emergency guard to %d, want 0", guard)
	}
}
