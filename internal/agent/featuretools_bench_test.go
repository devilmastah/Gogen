package agent

import (
	"testing"

	"gogen/internal/llm"
)

// BenchmarkFeatureToolDefs measures the cost of building every feature tool
// definition (the JSON-schema construction that dominates featureTools).
// The review's P6b claims the feature tool list is rebuilt ~11 times per
// round; this benchmark exists to say whether that costs enough to cache.
func BenchmarkFeatureToolDefs(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		defs := []llm.Tool{
			boardToolDef(),
			subagentToolDef(true, 4),
			subagentForkToolDef(),
			listAgentsToolDef(),
			getAgentResultToolDef(),
			waitAgentToolDef(),
			sendMessageToolDef(),
			interruptAgentToolDef(),
			skillsToolDef(),
			terminalToolDef(),
			bashPersistentToolDef(),
		}
		if len(defs) == 0 {
			b.Fatal("no defs")
		}
	}
}

// BenchmarkFeatureToolsPolicy measures the gating function itself (feature
// on/off checks, manager lookups, closure creation) with no feature wired.
func BenchmarkFeatureToolsPolicy(b *testing.B) {
	a := &Agent{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := a.featureTools(); len(got) != 0 {
			b.Fatalf("unexpected tools: %d", len(got))
		}
	}
}
