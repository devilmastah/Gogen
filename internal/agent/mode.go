package agent

import (
	"fmt"
	"strings"
)

// Mode controls whether the agent may mutate the repository.
type Mode int

const (
	ModeAct Mode = iota
	ModePlan
)

// ErrPlanModeBlocked is returned when a tool is disabled in plan mode.
var ErrPlanModeBlocked = fmt.Errorf("plan mode blocked tool")

// planModeAllowedTools is the intentional read-mostly subset for plan mode,
// derived from the PlanAllowed flag on each builtin tool's ToolDef so the
// registry stays the single source of truth. Mutating tools from
// BuiltinTools() are excluded by omission (no PlanAllowed flag).
var planModeAllowedTools = derivePlanModeAllowedTools()

func derivePlanModeAllowedTools() map[string]struct{} {
	out := make(map[string]struct{}, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		if d.PlanAllowed {
			out[d.Definition.Name] = struct{}{}
		}
	}
	return out
}

// builtinToolNames is derived from BuiltinTools() so schema and allowlists stay in sync.
var builtinToolNames = deriveBuiltinToolNames()

func deriveBuiltinToolNames() []string {
	tools := BuiltinTools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

func (m Mode) String() string {
	if m == ModePlan {
		return "plan"
	}
	return "act"
}

// ParseMode parses act/plan strings.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "plan", "Plan", "PLAN":
		return ModePlan, true
	case "act", "Act", "ACT", "":
		return ModeAct, true
	default:
		return ModeAct, false
	}
}

// AllowsTool reports whether the tool may run in this mode.
func (m Mode) AllowsTool(name string) bool {
	if m != ModePlan {
		return true
	}
	_, ok := planModeAllowedTools[name]
	return ok
}

// allowsTool reports whether name may run in the agent's current mode,
// covering the static builtin set plus the dynamic feature tools (whose
// plan-mode flags come from the single gating policy, featureTools).
func (a *Agent) allowsTool(name string) bool {
	if a.Mode != ModePlan {
		return true
	}
	if _, ok := planModeAllowedTools[name]; ok {
		return true
	}
	for _, ft := range a.featureTools() {
		if ft.Name == name && ft.PlanAllowed {
			return true
		}
	}
	return false
}

func (a *Agent) SetMode(m Mode) {
	// Written under statsMu so config snapshots (agentConfigMsgBasic) can
	// read the mode WITHOUT the session turn lock — the web attach handshake
	// must never block on a running turn, which holds turnMu for its entire
	// duration.
	a.statsMu.Lock()
	a.Mode = m
	a.statsMu.Unlock()
	a.FlushSession()
}

// ModeAndThinkingLevel returns the current mode and thinking level under
// statsMu. Safe to call without holding turnMu; used by the web server's
// config snapshot so a mid-turn attach never blocks on the turn lock.
func (a *Agent) ModeAndThinkingLevel() (Mode, ThinkingLevel) {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return a.Mode, a.ThinkingLevel
}

func (a *Agent) checkPlanMode(toolName string) error {
	if a.Mode == ModePlan && !a.allowsTool(toolName) {
		return fmt.Errorf("%w: tool %q is disabled; use /act to implement changes", ErrPlanModeBlocked, toolName)
	}
	if a.Mode == ModePlan && a.isMCPTool(toolName) {
		return fmt.Errorf("%w: MCP tool %q is disabled in plan mode", ErrPlanModeBlocked, toolName)
	}
	return nil
}

func (a *Agent) isMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp_")
}

// AllowedToolNames returns tool names available to the LLM in the current
// mode: the builtin set filtered by mode, the feature tools from the single
// gating policy (featureTools) filtered by their plan-mode flags, and the
// MCP registry tools (act mode only).
func (a *Agent) AllowedToolNames() map[string]struct{} {
	out := make(map[string]struct{})
	for _, name := range builtinToolNames {
		if a.allowsTool(name) {
			out[name] = struct{}{}
		}
	}
	for _, ft := range a.featureTools() {
		if a.Mode != ModePlan || ft.PlanAllowed {
			out[ft.Name] = struct{}{}
		}
	}
	if a.Mode != ModePlan && a.MCPRegistry != nil && !a.MCPNested() {
		// Inlined server tools are exposed (and therefore callable) only in
		// inline mode; nested mode reaches them through mcp_list, mcp_describe, and mcp_call,
		// which the featureTools loop above already added.
		for name := range a.MCPRegistry.ToolNames() {
			out[name] = struct{}{}
		}
	}
	return out
}
