package agent

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"gogen/internal/llm"
)

// ThinkingLevel is a literal reasoning_effort wire value or "off" — the zero
// value — which means no thinking parameter is sent to the API. The default
// levels (off/low/medium/high) are the closed fallback set; any other value
// the current model accepts (see ReasoningEffortsProvider) is also a valid
// literal. Values are sent verbatim (never translated) and are only effective
// when the current model accepts them; a stored value the model does not
// accept is kept but not sent (policy B).
type ThinkingLevel string

const (
	ThinkingOff    ThinkingLevel = "off"
	ThinkingLow    ThinkingLevel = "low"
	ThinkingMedium ThinkingLevel = "medium"
	ThinkingHigh   ThinkingLevel = "high"
)

// thinkingInfo holds display metadata for a default thinking level.
type thinkingInfo struct {
	label      string
	shortLabel string
}

// defaultLevels holds display metadata for the closed default set
// (off/low/medium/high). Values outside the defaults — e.g. "max" or "xhigh"
// reported by a model's models.dev entry — fall back to derived labels (see
// Label/ShortLabel) so every accepted value renders.
var defaultLevels = map[ThinkingLevel]thinkingInfo{
	ThinkingOff: {
		label:      "Off",
		shortLabel: "",
	},
	ThinkingLow: {
		label:      "Low",
		shortLabel: "L",
	},
	ThinkingMedium: {
		label:      "Medium",
		shortLabel: "M",
	},
	ThinkingHigh: {
		label:      "High",
		shortLabel: "H",
	},
}

// NormalizeThinkingLevel canonicalizes a thinking-level input: trimmed,
// lowercased, and "" when the input is blank. There is no fixed vocabulary to
// validate against — the set of selectable values is whatever the current
// model accepts (see AvailableThinkingLevels) — so any non-blank token is a
// literal reasoning_effort value.
func NormalizeThinkingLevel(s string) ThinkingLevel {
	return ThinkingLevel(strings.ToLower(strings.TrimSpace(s)))
}

// CurrentModelEfforts returns the reasoning-effort values the current model
// accepts (without "off"): the models.dev registry set when the model is
// known (empty for toggle/budget-only models), else the llama.cpp
// /props-derived set when the provider has probed one, else
// llm.DefaultReasoningEfforts. Never blocks.
func (a *Agent) CurrentModelEfforts() []string {
	if p, ok := a.Provider.(llm.ReasoningEffortsProvider); ok {
		return p.ModelReasoningEfforts(a.CurrentModelRef())
	}
	return llm.DefaultReasoningEfforts
}

// CurrentModelDescription returns the models.dev description of the current
// model, or "" when the provider cannot report one (unknown model, provider
// without registry data). Never blocks.
func (a *Agent) CurrentModelDescription() string {
	if p, ok := a.Provider.(llm.ModelDescriptionProvider); ok {
		return p.ModelDescription(a.CurrentModelRef())
	}
	return ""
}

// ThinkingLevelsForModel returns the thinking levels selectable for
// modelID: "off" (omit) plus the model's accepted reasoning-effort values
// in the provider's order. Toggle-only models (no effort control) yield
// just "off". Never blocks on the network.
func (a *Agent) ThinkingLevelsForModel(modelID string) []ThinkingLevel {
	var efforts []string
	if p, ok := a.Provider.(llm.ReasoningEffortsProvider); ok {
		efforts = p.ModelReasoningEfforts(llm.ModelRef{ID: modelID})
	} else {
		efforts = llm.DefaultReasoningEfforts
	}
	out := make([]ThinkingLevel, 0, len(efforts)+1)
	out = append(out, ThinkingOff)
	for _, e := range efforts {
		if e == "" || e == "off" {
			continue
		}
		out = append(out, ThinkingLevel(e))
	}
	return out
}

// AvailableThinkingLevels returns the thinking levels selectable for the
// current model (ThinkingLevelsForModel(a.CurrentModel())).
func (a *Agent) AvailableThinkingLevels() []ThinkingLevel {
	return a.ThinkingLevelsForModel(a.CurrentModel())
}

// IsThinkingLevelActive reports whether the stored thinking level is currently
// sent to the provider: false when off, or when the value is not in the
// current model's accepted set (policy B keeps it stored but inactive).
func (a *Agent) IsThinkingLevelActive() bool {
	level := a.ThinkingLevel
	if level == "" || level == ThinkingOff {
		return false
	}
	if p, ok := a.Provider.(llm.ReasoningEffortsProvider); ok {
		return slices.Contains(p.ModelReasoningEfforts(a.CurrentModelRef()), string(level))
	}
	return true // provider without effort reporting: assume active
}

func joinThinkingLevels(levels []ThinkingLevel) string {
	parts := make([]string, len(levels))
	for i, l := range levels {
		parts[i] = string(l)
	}
	return strings.Join(parts, ", ")
}

// Label returns a user-friendly label: the default table for the closed
// default set, or a derived title-case of the wire value otherwise, so a
// stored value (even one inactive for the current model) always renders.
func (l ThinkingLevel) Label() string {
	if info, ok := defaultLevels[l]; ok {
		return info.label
	}
	if l == "" {
		return "Off"
	}
	return titleCase(string(l))
}

// ShortLabel returns a compact label for display in toolbars (empty for
// "off" and blank): the default table for the closed default set, or the
// first letter of the derived label otherwise.
func (l ThinkingLevel) ShortLabel() string {
	if info, ok := defaultLevels[l]; ok {
		return info.shortLabel
	}
	if l == "" {
		return ""
	}
	label := l.Label()
	if label == "" {
		return ""
	}
	return string([]rune(label)[:1])
}

// titleCase upper-cases the first letter of s ("max" → "Max"); s must be
// non-empty.
func titleCase(s string) string {
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// SetThinkingLevel sets the agent's thinking level, syncs the provider's
// reasoning-effort state so the two can never diverge, and persists the
// session. The level field itself is written under statsMu (see
// Agent.ModeAndThinkingLevel — config snapshots read it without the turn
// lock, so a mid-turn attach never blocks); the provider sync runs outside
// the lock.
func (a *Agent) SetThinkingLevel(l ThinkingLevel) {
	a.statsMu.Lock()
	a.ThinkingLevel = l
	a.statsMu.Unlock()
	if a.Provider != nil {
		a.Provider.SetThinkingLevel(string(l))
	}
	a.FlushSession()
}

// HandleThinkingCommand processes /think commands. The values offered are the
// current model's accepted reasoning efforts plus "off" (see
// AvailableThinkingLevels); a value the model does not accept is rejected
// rather than stored inactive, so the stored level always matches the wire.
func (a *Agent) HandleThinkingCommand(input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	available := a.AvailableThinkingLevels()
	if trimmed == "/think" || trimmed == "think" {
		msg := fmt.Sprintf("Thinking level: %s", a.ThinkingLevel.Label())
		if a.ThinkingLevel != ThinkingOff && !a.IsThinkingLevelActive() {
			msg += " (inactive for this model — not sent)"
		}
		msg += fmt.Sprintf(". Available: %s", joinThinkingLevels(available))
		return msg, true
	}
	if strings.HasPrefix(trimmed, "/think ") || strings.HasPrefix(trimmed, "think ") {
		parts := strings.SplitN(trimmed, " ", 2)
		if len(parts) < 2 {
			return "", false
		}
		level := NormalizeThinkingLevel(parts[1])
		if level == "" {
			return fmt.Sprintf("Unknown thinking level %q. Available: %s", parts[1], joinThinkingLevels(available)), true
		}
		if level != ThinkingOff && !slices.Contains(available, level) {
			return fmt.Sprintf("Thinking level %q is not available for the current model. Available: %s", parts[1], joinThinkingLevels(available)), true
		}
		a.SetThinkingLevel(level)
		if level == ThinkingOff {
			return "Thinking level set to off (no reasoning effort sent to the API).", true
		}
		return fmt.Sprintf("Thinking level set to %s.", level.Label()), true
	}
	return "", false
}
