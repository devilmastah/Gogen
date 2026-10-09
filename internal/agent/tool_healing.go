package agent

import (
	"fmt"
	"maps"
	"strings"
)

// toolNameAliases maps tool names that models have actually emitted in
// sessions (usually borrowed from other agent ecosystems) to the canonical
// GoGen tool whose schema the observed arguments matched. healToolCall
// applies them before any routing or plan-mode gating.
var toolNameAliases = map[string]string{
	"exec_command": "execute_command", // observed: {"command": ...}
	"grep":         "search_code",     // observed: {"pattern": ...}
}

// argAliases maps a canonical tool name to misspelled parameter names that
// models have actually emitted, each mapped to the canonical schema
// property. Observed slips:
//
//   - replace_in_file: the model mirrors the replace_in_file TOOL name into
//     the parameter name (38 occurrences in session history, all with the
//     replacement text under that key);
//   - execute_command: the model uses the Unix shorthand "cmd" for the
//     command string, which otherwise fails the dispatch with
//     `missing required argument "command"`.
var argAliases = map[string]map[string]string{
	"replace_in_file": {
		"replace_in_file": "replace",
	},
	"execute_command": {
		"cmd": "command",
	},
}

// healToolCall normalizes a model-emitted tool call before dispatch so that
// known naming slips are accepted instead of failing with "missing required
// argument" / "unknown tool":
//
//   - a misspelled tool name is remapped to its canonical tool
//     (toolNameAliases);
//   - a misspelled parameter is renamed to the canonical schema property
//     (argAliases), but only when the canonical key is ABSENT — if both are
//     present the canonical value wins and the call is left untouched.
//
// The input map is never mutated: a healed call returns a copy, so the
// persisted message (and its prompt-cache-stable ArgsStr) stays
// byte-identical. The returned note is non-empty when anything was healed;
// executeTool prefixes it to the tool result so the model sees the
// canonical form and stops slipping.
func healToolCall(name string, args map[string]any) (string, map[string]any, string) {
	var notes []string
	healed := false
	// cloned tracks the copy-on-write: a tool-name heal (which never touches
	// args) must not let the first argument rename edit the caller's map.
	cloned := false

	if canon, ok := toolNameAliases[name]; ok {
		notes = append(notes, fmt.Sprintf("tool %q accepted as %q", name, canon))
		name, healed = canon, true
	}
	for alias, canon := range argAliases[name] {
		if _, hasCanon := args[canon]; hasCanon {
			continue // canonical key present: it wins, nothing to heal
		}
		val, hasAlias := args[alias]
		if !hasAlias {
			continue
		}
		if !cloned {
			args = maps.Clone(args)
			cloned = true
		}
		delete(args, alias)
		args[canon] = val
		notes = append(notes, fmt.Sprintf("parameter %q accepted as %q", alias, canon))
		healed = true
	}
	if !healed {
		return name, args, ""
	}
	return name, args, "note: healed tool call (" + strings.Join(notes, "; ") + ")\n"
}
