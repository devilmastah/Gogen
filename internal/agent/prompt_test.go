package agent

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestSystemPromptTemplate pins the prompt guidance that models rely on:
// the parallel-batching rule, tool-output authority, and the
// compaction/todo guidance. The MCP hint is deliberately NOT part of the
// base template — it is appended only when the session was constructed with
// MCP servers configured (TestMCPHintGateIsConfigDerived).
func TestSystemPromptTemplate(t *testing.T) {
	p := SystemPrompt("/tmp/project")
	for _, want := range []string{
		"at most 4",        // parallel read-only cap
		"ground truth",     // tool output is authoritative
		"context_pin_last", // survive compaction
		"todo",             // task tracking
	} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if strings.Contains(p, mcpGatewayHint) || strings.Contains(p, mcpInlineHint) {
		t.Error("base system prompt advertises MCP tools with no MCP servers configured")
	}
}

// TestMCPHintGateIsConfigDerived pins the gating of the MCP hint and, more
// importantly, its STABILITY: the hint is chosen from construction-time config
// (SetMCPTooling), so attaching a registry later — which happens asynchronously
// for an `npx`-launched server — must not change the system prompt. The system
// prompt leads the request prefix, so a hint that appeared on attach would
// invalidate the provider's prompt cache for the whole conversation.
func TestMCPHintGateIsConfigDerived(t *testing.T) {
	t.Cleanup(resetPromptConfigForTest)
	msgs := []llm.Message{{Role: "user", Content: "hi"}}

	t.Run("hint text follows the exposure mode", func(t *testing.T) {
		tests := []struct {
			name       string
			configured bool
			nested     bool
			want       string
		}{
			{name: "no MCP servers", configured: false, nested: true, want: ""},
			{name: "nested mode", configured: true, nested: true, want: mcpGatewayHint},
			{name: "inline mode", configured: true, nested: false, want: mcpInlineHint},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				a := &Agent{WorkingDir: t.TempDir(), Messages: msgs}
				a.SetMCPTooling(tt.configured, tt.nested)
				p := a.systemPromptPrefix()
				if p == nil {
					t.Fatal("systemPromptPrefix returned nil")
				}
				for _, hint := range []string{mcpGatewayHint, mcpInlineHint} {
					if present := strings.Contains(p[0].Content, hint); present != (hint == tt.want) {
						t.Fatalf("hint %q present = %v, want %v\n%q", hint, present, hint == tt.want, p[0].Content)
					}
				}
			})
		}
	})

	t.Run("attaching a registry does not move the prefix", func(t *testing.T) {
		a := &Agent{WorkingDir: t.TempDir(), Messages: msgs}
		a.SetMCPTooling(true, true)
		before := a.systemPromptPrefix()
		if before == nil {
			t.Fatal("systemPromptPrefix returned nil")
		}
		// The async attach: the registry appears mid-session.
		a.SetMCPRegistry(&fakeMCPRegistry{names: map[string]struct{}{"mcp_x": {}}})
		after := a.systemPromptPrefix()
		if after == nil || after[0].Content != before[0].Content {
			t.Fatalf("registry attach changed the system prompt (prefix cache bust):\nbefore %q\nafter  %q",
				before[0].Content, after[0].Content)
		}
	})

	// buildSystemSuffix order: the hint leads the enrichment suffixes because
	// it qualifies the base prompt.
	suffix := buildSystemSuffix("gogen.md", "rules text", "profile text", ModeAct, mcpGatewayHint)
	hint, profile := strings.Index(suffix, mcpGatewayHint), strings.Index(suffix, "profile text")
	if hint < 0 || profile < 0 || hint > profile {
		t.Fatalf("suffix = %q, want the MCP hint before the project profile", suffix)
	}

	plan := buildSystemSuffix("", "", "", ModePlan, mcpGatewayHint)
	if !strings.Contains(plan, mcpGatewayHint) || !strings.Contains(plan, "MCP tools are disabled until act mode.") {
		t.Fatalf("plan suffix = %q, want the call shape and the act-mode gate", plan)
	}
	if strings.Contains(suffix, "disabled until act mode") {
		t.Fatalf("act suffix advertises the plan-mode gate: %q", suffix)
	}
}

// TestReadFileSchemaDropsDeprecatedAlias pins the schema cleanup: the
// deprecated file_path alias is gone from the LLM-facing schema and path is
// required (it is now the only way to name a file).
func TestReadFileSchemaDropsDeprecatedAlias(t *testing.T) {
	for _, d := range builtinToolDefs {
		if d.Definition.Name != "read_file" {
			continue
		}
		props, ok := d.Definition.Parameters["properties"].(map[string]any)
		if !ok {
			t.Fatal("read_file schema has no properties")
		}
		if _, ok := props["file_path"]; ok {
			t.Error("read_file schema still advertises deprecated file_path alias")
		}
		if _, ok := props["path"]; !ok {
			t.Error("read_file schema missing path")
		}
		req, ok := d.Definition.Parameters["required"].([]string)
		if !ok || len(req) != 1 || req[0] != "path" {
			t.Errorf("read_file required = %v, want [path]", req)
		}
		return
	}
	t.Fatal("read_file tool not found")
}

// TestActionToolsHaveEnums pins the enum constraint on action/kind parameters
// of the consolidated tools, so invalid values fail fast instead of
// round-tripping through handlers.
func TestActionToolsHaveEnums(t *testing.T) {
	byName := make(map[string]llm.Tool, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		byName[d.Definition.Name] = d.Definition
	}
	cases := map[string]string{
		"git":            "action",
		"find_symbol":    "kind",
		"background_job": "action",
		"todo":           "action",
	}
	for tool, param := range cases {
		def, ok := byName[tool]
		if !ok {
			t.Fatalf("tool %q missing from registry", tool)
		}
		props, ok := def.Parameters["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no properties", tool)
		}
		p, ok := props[param].(map[string]any)
		if !ok {
			t.Fatalf("%s: missing %s param", tool, param)
		}
		if _, ok := p["enum"]; !ok {
			t.Errorf("%s: %s param has no enum", tool, param)
		}
	}
}
