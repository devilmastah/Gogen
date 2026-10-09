package tui

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

// refEffortProvider returns reasoning-effort sets keyed by the FULL model
// routing ref (profile + id), so a test can prove the models modal resolves
// each row's chips against that row's OWNING profile. A ref the table does
// not hold (including a bare-ID ref with an empty profile) gets the default
// closed set.
type refEffortProvider struct {
	current string
	catalog []llm.ModelInfo
	efforts map[llm.ModelRef][]string
}

func (p *refEffortProvider) GenerateResponse(context.Context, []llm.Message, map[string]struct{}, []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}

func (p *refEffortProvider) GenerateResponseStream(context.Context, []llm.Message, map[string]struct{}, []llm.Tool, *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (p *refEffortProvider) ModelContextLimit(context.Context) (int, error) { return 100000, nil }

func (p *refEffortProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return p.catalog, nil
}

func (p *refEffortProvider) SetModel(id string) error { p.current = id; return nil }

func (p *refEffortProvider) ModelName() string { return p.current }

func (p *refEffortProvider) SetThinkingLevel(string) {}

func (p *refEffortProvider) ModelReasoningEfforts(ref llm.ModelRef) []string {
	if e, ok := p.efforts[ref]; ok {
		return e
	}
	return llm.DefaultReasoningEfforts
}

// Regression: a model ID listed by several profiles must offer the effort set
// ITS OWN row's profile accepts. The chip row used to resolve by bare ID
// (ThinkingLevelsForModel(mdl.ID)), which resolves the owner by first-listing
// — so the second profile's row offered the first profile's set.
func TestModelsModalThinkingChipsUseRowProfile(t *testing.T) {
	const shared = "shared-model"
	p := &refEffortProvider{
		current: shared,
		catalog: []llm.ModelInfo{
			{ID: shared, Provider: "default", Current: true},
			{ID: shared, Provider: "second"},
		},
		efforts: map[llm.ModelRef][]string{
			{Profile: "default", ID: shared}: {"low"},
			{Profile: "second", ID: shared}:  {"high", "max"},
		},
	}
	base := func(cursor int) *Model {
		return &Model{
			agent:       &agent.Agent{Provider: p},
			modelList:   p.catalog,
			modelCursor: cursor,
			thinkingSel: agent.ThinkingOff,
			width:       100,
			height:      40,
		}
	}

	t.Run("second profile's row uses its own efforts", func(t *testing.T) {
		out := stripANSI(base(1).renderModelsModal())
		if !strings.Contains(out, "[H]") || !strings.Contains(out, "[Max]") {
			t.Fatalf("second row missing its own chip set [H][Max]:\n%s", out)
		}
		if strings.Contains(out, "[L]") {
			t.Fatalf("second row leaked the first profile's [L] chip:\n%s", out)
		}
	})

	t.Run("first profile's row keeps its own efforts", func(t *testing.T) {
		out := stripANSI(base(0).renderModelsModal())
		if !strings.Contains(out, "[L]") {
			t.Fatalf("first row missing its own [L] chip:\n%s", out)
		}
		if strings.Contains(out, "[Max]") {
			t.Fatalf("first row leaked the second profile's [Max] chip:\n%s", out)
		}
	})
}
