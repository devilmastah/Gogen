package agent

import (
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// TestSmartCompactBaseIsCachedPerRound pins the per-round caching of the
// conversation total: StopStream runs on every streamed chunk, so it must not
// walk every message count (and, without a fresh provider baseline, rebuild and
// hash the system suffix) per chunk. The reset — called at each round start —
// is the only place the base is recomputed, which is also the only place the
// total can change.
func TestSmartCompactBaseIsCachedPerRound(t *testing.T) {
	prov := llmtest.NewMockProvider()
	a := NewAgent(prov, NewExecutor(t.TempDir()), contextmgr.NewManager(prov, contextmgr.Settings{
		ContextLimit:         20000,
		CompactReserveTokens: 0,
		SmartCompact:         true,
	}))
	if !a.smartCompactEnabled() {
		t.Fatal("smart compact should be enabled for this test")
	}

	// The total needs a COMPLETE per-message count cache (compactionTokenTotal
	// reports -1 otherwise); seed it the way a restore/turn would.
	seedCounts := func() {
		a.statsMu.Lock()
		a.tokenCounts = contextmgr.CountTokens(a.Messages)
		if a.tokenCounts == nil {
			a.tokenCounts = []int{}
		}
		a.statsMu.Unlock()
	}
	seedCounts()

	// Size the filler from the live budget: the system-prompt + tool overhead
	// baked into the total is environment-dependent, so the test must cross the
	// stop point rather than assume a fixed number of words.
	stopAt := a.Context.SmartCompactStopAt()
	base := a.compactionTokenTotal()
	if base < 0 || base >= stopAt {
		t.Skipf("baseline %d already at/over the stop point %d (wire overhead)", base, stopAt)
	}

	h, reset := a.armSmartCompactStream(&llm.StreamHandlers{})
	reset() // first round start
	if h.StopStream() {
		t.Fatal("an under-budget conversation must not stop the stream")
	}

	// Grow the conversation the way a round boundary would, but WITHOUT the
	// reset: the cached base must not move mid-round.
	filler := strings.Repeat("word ", (stopAt-base)+256) // ~1 token per word
	a.appendMessage(llm.Message{Role: "user", Content: filler})
	seedCounts()
	if h.StopStream() {
		t.Fatal("StopStream saw a mid-round append: the base must be per-round")
	}

	// A new round recomputes it, and now the budget is exceeded.
	reset()
	if !h.StopStream() {
		t.Fatal("the next round must see the new conversation total")
	}
}
