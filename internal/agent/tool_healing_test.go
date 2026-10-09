package agent

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestHealToolCall pins the self-healing contract: known model naming slips
// are renamed to their canonical form, the canonical key always wins when
// both are present, and the input map is never mutated (the persisted
// message must stay byte-identical for prompt-cache stability).
func TestHealToolCall(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		args     map[string]any
		wantTool string
		wantArgs map[string]any
		wantNote bool
	}{
		{
			name:     "replace_in_file param healed to replace",
			tool:     "replace_in_file",
			args:     map[string]any{"path": "a.go", "search": "x", "replace_in_file": "y"},
			wantTool: "replace_in_file",
			wantArgs: map[string]any{"path": "a.go", "search": "x", "replace": "y"},
			wantNote: true,
		},
		{
			name:     "canonical key wins over alias",
			tool:     "replace_in_file",
			args:     map[string]any{"path": "a.go", "search": "x", "replace": "y", "replace_in_file": "z"},
			wantTool: "replace_in_file",
			wantArgs: map[string]any{"path": "a.go", "search": "x", "replace": "y", "replace_in_file": "z"},
			wantNote: false,
		},
		{
			name:     "no alias present is unchanged",
			tool:     "replace_in_file",
			args:     map[string]any{"path": "a.go", "search": "x", "replace": "y"},
			wantTool: "replace_in_file",
			wantArgs: map[string]any{"path": "a.go", "search": "x", "replace": "y"},
			wantNote: false,
		},
		{
			name:     "exec_command healed to execute_command",
			tool:     "exec_command",
			args:     map[string]any{"command": "ls"},
			wantTool: "execute_command",
			wantArgs: map[string]any{"command": "ls"},
			wantNote: true,
		},
		{
			name:     "execute_command cmd param healed to command",
			tool:     "execute_command",
			args:     map[string]any{"cmd": "ls"},
			wantTool: "execute_command",
			wantArgs: map[string]any{"command": "ls"},
			wantNote: true,
		},
		{
			name:     "grep healed to search_code",
			tool:     "grep",
			args:     map[string]any{"pattern": "foo"},
			wantTool: "search_code",
			wantArgs: map[string]any{"pattern": "foo"},
			wantNote: true,
		},
		{
			name:     "unknown tool untouched",
			tool:     "frobnicate",
			args:     map[string]any{"x": 1},
			wantTool: "frobnicate",
			wantArgs: map[string]any{"x": 1},
			wantNote: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := maps.Clone(tt.args)
			gotTool, gotArgs, note := healToolCall(tt.tool, tt.args)
			if gotTool != tt.wantTool {
				t.Errorf("tool = %q, want %q", gotTool, tt.wantTool)
			}
			if !maps.Equal(gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
			if (note != "") != tt.wantNote {
				t.Errorf("note = %q, wantNote=%v", note, tt.wantNote)
			}
			if !maps.Equal(tt.args, orig) {
				t.Errorf("input map was mutated: got %v, want %v", tt.args, orig)
			}
		})
	}
}

// TestHealAliasesMatchSchemas pins the healing tables against the tool
// schemas: every argument-alias target must be a real property of the
// canonical tool's schema, and every tool-name alias target a real builtin
// tool, so the tables cannot drift from builtinToolDefs. (A future alias
// targeting a feature tool must extend this check.)
func TestHealAliasesMatchSchemas(t *testing.T) {
	defs := make(map[string]llm.Tool, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		defs[d.Definition.Name] = d.Definition
	}
	for tool, aliases := range argAliases {
		def, ok := defs[tool]
		if !ok {
			t.Errorf("argAliases references unknown tool %q", tool)
			continue
		}
		props, _ := def.Parameters["properties"].(map[string]any)
		for alias, canon := range aliases {
			if _, ok := props[canon]; !ok {
				t.Errorf("tool %q: alias %q targets %q, which is not in its schema", tool, alias, canon)
			}
		}
	}
	for alias, canon := range toolNameAliases {
		if _, ok := defs[canon]; !ok {
			t.Errorf("toolNameAliases: %q targets %q, which is not a builtin tool", alias, canon)
		}
	}
}

// TestExecuteToolHealsReplaceInFileParam pins the end-to-end self-healing:
// a replace_in_file call that names the replacement under the tool name
// (the observed model slip) is accepted, executes, and reports the healing
// as a note on the tool result.
func TestExecuteToolHealsReplaceInFileParam(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Mode: ModeAct, Executor: &Executor{WorkingDir: dir}}

	args := map[string]any{
		"path":            "a.txt",
		"search":          "old line",
		"replace_in_file": "new line",
	}
	out, err := a.executeTool(context.Background(), llmToolCall("replace_in_file", args))
	if err != nil {
		t.Fatalf("healed call failed: %v", err)
	}
	if !strings.Contains(out, "healed tool call") {
		t.Errorf("result missing healing note: %q", out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "new line\n" {
		t.Errorf("file = %q, want healed replacement applied", got)
	}
	// The caller's args map must stay untouched (prompt-cache stability).
	if _, ok := args["replace"]; ok {
		t.Error("original args map was mutated: gained canonical key")
	}
	if _, ok := args["replace_in_file"]; !ok {
		t.Error("original args map lost the alias key")
	}
}

// TestExecuteToolHealsToolName pins the tool-name healing end-to-end: a
// grep call (foreign name, search_code-shaped args) dispatches to
// search_code instead of failing with "unknown tool".
func TestExecuteToolHealsToolName(t *testing.T) {
	a := &Agent{Mode: ModeAct, Executor: &Executor{WorkingDir: t.TempDir()}}

	out, err := a.executeTool(context.Background(), llmToolCall("grep", map[string]any{
		"pattern": "definitely_not_present_pattern_xyz",
	}))
	if err != nil {
		t.Fatalf("healed call failed: %v", err)
	}
	if !strings.Contains(out, "healed tool call") {
		t.Errorf("result missing healing note: %q", out)
	}
	if strings.Contains(out, "unknown tool") {
		t.Errorf("healed call still reported unknown tool: %q", out)
	}
}

// TestHealToolCallCombinedNameAndArgAliasDoesNotMutate exercises the
// copy-on-write path the shipped tables do not cover: a tool-name heal and an
// argument rename in the same call. The name heal sets the "healed" flag that
// used to skip the clone, after which the rename edited the caller's map — the
// persisted message and its prompt-cache-stable ArgsStr must stay
// byte-identical.
func TestHealToolCallCombinedNameAndArgAliasDoesNotMutate(t *testing.T) {
	const aliasName = "replace_in_file_thing"
	toolNameAliases[aliasName] = "replace_in_file"
	t.Cleanup(func() { delete(toolNameAliases, aliasName) })

	args := map[string]any{"path": "a.go", "search": "x", "replace_in_file": "y"}
	orig := maps.Clone(args)
	gotTool, gotArgs, note := healToolCall(aliasName, args)
	if gotTool != "replace_in_file" {
		t.Fatalf("tool = %q, want replace_in_file", gotTool)
	}
	want := map[string]any{"path": "a.go", "search": "x", "replace": "y"}
	if !maps.Equal(gotArgs, want) {
		t.Fatalf("args = %v, want %v", gotArgs, want)
	}
	if note == "" {
		t.Fatal("expected a healing note")
	}
	if !maps.Equal(args, orig) {
		t.Fatalf("input map was mutated: got %v, want %v", args, orig)
	}
}

// TestExecuteToolHealsExecuteCommandCmdArg pins the end-to-end self-healing
// for a model that names execute_command's command under "cmd" (the Unix
// shorthand): the call is accepted instead of failing with `missing
// required argument "command"`, runs, and reports the healing as a note on
// the tool result.
func TestExecuteToolHealsExecuteCommandCmdArg(t *testing.T) {
	a := &Agent{Mode: ModeAct, Executor: &Executor{WorkingDir: t.TempDir()}}

	args := map[string]any{"cmd": "echo healed-cmd-ok"}
	out, err := a.executeTool(context.Background(), llmToolCall("execute_command", args))
	if err != nil {
		t.Fatalf("healed call failed: %v", err)
	}
	if !strings.Contains(out, "healed tool call") {
		t.Errorf("result missing healing note: %q", out)
	}
	if !strings.Contains(out, "healed-cmd-ok") {
		t.Errorf("healed command output missing: %q", out)
	}
	// The caller's args map must stay untouched (prompt-cache stability).
	if _, ok := args["command"]; ok {
		t.Error("original args map was mutated: gained canonical key")
	}
	if _, ok := args["cmd"]; !ok {
		t.Error("original args map lost the alias key")
	}
}
