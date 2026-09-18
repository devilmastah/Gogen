package agent

import (
	"context"
	"reflect"
	"testing"

	"gogen/internal/llm"
)

// effortStub is a minimal provider exposing per-model reasoning efforts.
type effortStub struct{ model string }

func (s *effortStub) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}

func (s *effortStub) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (s *effortStub) ModelContextLimit(_ context.Context) (int, error) { return 128000, nil }

func (s *effortStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) { return nil, nil }

func (s *effortStub) SetModel(string) error { return nil }

func (s *effortStub) ModelName() string { return s.model }

func (s *effortStub) SetThinkingLevel(string) {}

func (s *effortStub) ModelReasoningEfforts(ref llm.ModelRef) []string {
	switch ref.ID {
	case "glm-5.2":
		return []string{"high", "max"}
	case "glm-5":
		return nil // toggle-only: no effort control
	default:
		return llm.DefaultReasoningEfforts // unknown model
	}
}

// TestAvailableThinkingLevels pins the per-model selection surface: "off"
// plus the model's accepted efforts in canonical order. Toggle-only models
// yield just "off"; unknown models fall back to the default set.
func TestAvailableThinkingLevels(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  []ThinkingLevel
	}{
		{name: "known-model", model: "glm-5.2", want: []ThinkingLevel{ThinkingOff, ThinkingHigh, ThinkingLevel("max")}},
		{name: "toggle-only", model: "glm-5", want: []ThinkingLevel{ThinkingOff}},
		{name: "unknown-model", model: "selfhosted", want: []ThinkingLevel{ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{Provider: &effortStub{model: tc.model}}
			if got := a.AvailableThinkingLevels(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("available = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsThinkingLevelActive pins policy B: a stored value the model does not
// accept is kept but reported inactive (not sent); "off" is never active.
func TestIsThinkingLevelActive(t *testing.T) {
	cases := []struct {
		level  ThinkingLevel
		active bool
	}{
		{ThinkingOff, false},
		{ThinkingHigh, true},         // ∈ {high, max}
		{ThinkingLevel("max"), true}, // ∈ {high, max}
		{ThinkingLow, false},         // ∉ {high, max} → inactive
		{ThinkingMedium, false},
	}
	for _, tc := range cases {
		a := &Agent{Provider: &effortStub{model: "glm-5.2"}, ThinkingLevel: tc.level}
		if got := a.IsThinkingLevelActive(); got != tc.active {
			t.Fatalf("level %q active = %v, want %v", tc.level, got, tc.active)
		}
	}
}

// TestHandleThinkingCommandRejectsUnavailableLevel guards /think against
// storing a value the current model does not accept: the command must reject
// it and leave the stored level unchanged.
func TestHandleThinkingCommandRejectsUnavailableLevel(t *testing.T) {
	a := &Agent{Provider: &effortStub{model: "glm-5.2"}, ThinkingLevel: ThinkingHigh}
	if got, handled := a.HandleThinkingCommand("/think low"); !handled {
		t.Fatal("/think low not handled")
	} else if a.ThinkingLevel != ThinkingHigh {
		t.Fatalf("level changed to %q despite rejection (want %q)", a.ThinkingLevel, ThinkingHigh)
	} else if got == "" {
		t.Fatal("expected rejection message")
	}
	if got, handled := a.HandleThinkingCommand("/think max"); !handled {
		t.Fatal("/think max not handled")
	} else if a.ThinkingLevel != ThinkingLevel("max") {
		t.Fatalf("level = %q, want max", a.ThinkingLevel)
	} else if got == "" {
		t.Fatal("expected confirmation message")
	}
}
