package server

import (
	"context"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

// wsEffortStub reports per-model efforts for the WS validation tests.
type wsEffortStub struct{ model string }

func (s *wsEffortStub) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}

func (s *wsEffortStub) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (s *wsEffortStub) ModelContextLimit(_ context.Context) (int, error) { return 128000, nil }

func (s *wsEffortStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) { return nil, nil }

func (s *wsEffortStub) SetModel(string) error { return nil }

func (s *wsEffortStub) ModelName() string { return s.model }

func (s *wsEffortStub) SetThinkingLevel(string) {}

func (s *wsEffortStub) ModelReasoningEfforts(ref llm.ModelRef) []string {
	if ref.ID == "glm-5.2" {
		return []string{"high", "max"}
	}
	return llm.DefaultReasoningEfforts // unknown model
}

func (s *wsEffortStub) ModelDescription(ref llm.ModelRef) string {
	if ref.ID == "glm-5.2" {
		return "Open flagship GLM for agentic engineering"
	}
	return "" // unknown model
}

// TestIsValidThinkingLevel pins the set_thinking_level validation: ""/off are
// always accepted (omit), and any other value is accepted only when it is in
// the current model's effective accepted set.
func TestIsValidThinkingLevel(t *testing.T) {
	s := &Server{}
	a := &agent.Agent{Provider: &wsEffortStub{model: "glm-5.2"}}
	cases := []struct {
		v  string
		ok bool
	}{
		{"", true},      // nothing selected → omit
		{"off", true},   // off always valid
		{"high", true},  // ∈ {high, max}
		{"max", true},   // ∈ {high, max}
		{"low", false},  // vocabulary word but ∉ model set
		{"none", false}, // vocabulary word but ∉ model set
		{"bogus", false},
		{"Max", true}, // trimmed + case-insensitive
	}
	for _, tc := range cases {
		if got := s.isValidThinkingLevel(a, tc.v); got != tc.ok {
			t.Fatalf("isValidThinkingLevel(%q) = %v, want %v", tc.v, got, tc.ok)
		}
	}
}

// TestModelEntriesIncludeDescription pins the models-list payload: each entry
// carries its own description and reasoning efforts for hover tooltips.
func TestModelEntriesIncludeDescription(t *testing.T) {
	s := &Server{}
	entries := s.modelEntries([]llm.ModelInfo{
		{ID: "glm-5.2", ContextLimit: 200000, Current: true, Description: "Open flagship GLM for agentic engineering", ReasoningEfforts: []string{"high", "max"}},
		{ID: "selfhosted"},
	})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Description != "Open flagship GLM for agentic engineering" {
		t.Fatalf("description = %q", entries[0].Description)
	}
	if len(entries[0].ReasoningEfforts) != 2 || entries[0].ReasoningEfforts[0] != "high" {
		t.Fatalf("reasoningEfforts = %v", entries[0].ReasoningEfforts)
	}
	if entries[1].Description != "" || len(entries[1].ReasoningEfforts) != 0 {
		t.Fatalf("unknown model should carry no description/efforts, got desc=%q efforts=%v", entries[1].Description, entries[1].ReasoningEfforts)
	}
}

// TestAgentConfigMsgCarriesModelDescription pins the config echo: the current
// model's description (and efforts) reach the client for the toolbar tooltip.
func TestAgentConfigMsgCarriesModelDescription(t *testing.T) {
	a := &agent.Agent{
		Provider: &wsEffortStub{model: "glm-5.2"},
		Executor: &agent.Executor{WorkingDir: "."},
	}
	msg := agentConfigMsgBasic(a)
	if msg.Model != "glm-5.2" {
		t.Fatalf("model = %q", msg.Model)
	}
	if msg.ModelDescription != "Open flagship GLM for agentic engineering" {
		t.Fatalf("modelDescription = %q", msg.ModelDescription)
	}
	if len(msg.ReasoningEfforts) != 2 || msg.ReasoningEfforts[0] != "high" {
		t.Fatalf("reasoningEfforts = %v", msg.ReasoningEfforts)
	}
}

// refEffortStub is a wsEffortStub that routes by (profile, ID), so the
// config echo's ModelProvider is testable with a non-default owner.
type refEffortStub struct {
	*wsEffortStub
	ref llm.ModelRef
}

func (s *refEffortStub) SetModelRef(ref llm.ModelRef) error {
	s.ref = ref
	return nil
}

func (s *refEffortStub) CurrentModelRef() llm.ModelRef { return s.ref }

// TestAgentConfigMsgCarriesModelProvider pins the config echo's routing key:
// ModelProvider names the profile serving the current model, so the client's
// current-model check can match (provider, id). A bare Model alone would
// mark every catalog row that lists the same ID (two providers listing
// deepseek-flash would both light up in the picker).
func TestAgentConfigMsgCarriesModelProvider(t *testing.T) {
	p := &refEffortStub{
		wsEffortStub: &wsEffortStub{model: "deepseek-flash"},
		ref:          llm.ModelRef{Profile: "deepseek", ID: "deepseek-flash"},
	}
	a := &agent.Agent{
		Provider: p,
		Executor: &agent.Executor{WorkingDir: "."},
	}
	msg := agentConfigMsgBasic(a)
	if msg.Model != "deepseek-flash" {
		t.Fatalf("model = %q", msg.Model)
	}
	if msg.ModelProvider != "deepseek" {
		t.Fatalf("modelProvider = %q, want deepseek", msg.ModelProvider)
	}

	// A default-profile model reports the default profile name (an empty
	// ref profile normalizes to "default", as the client's fallback does).
	p.ref = llm.ModelRef{Profile: "", ID: "gpt-4o"}
	msg = agentConfigMsgBasic(a)
	if msg.ModelProvider != "default" {
		t.Fatalf("modelProvider = %q, want default", msg.ModelProvider)
	}
}
