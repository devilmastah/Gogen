package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// recordingMCPRegistry is a fake MCP registry that keeps its definitions and
// records dispatched calls, so the gateway's list/describe/call actions can be
// asserted end to end.
type recordingMCPRegistry struct {
	defs  []llm.Tool
	calls []mcpCall
	// err, when non-nil, is returned by CallTool (a server-side tool error).
	err error
	// out, when non-empty, is returned as the tool output.
	out string
}

type mcpCall struct {
	name string
	args map[string]any
}

func (r *recordingMCPRegistry) Definitions() []llm.Tool { return r.defs }

func (r *recordingMCPRegistry) ToolNames() map[string]struct{} {
	out := make(map[string]struct{}, len(r.defs))
	for _, d := range r.defs {
		out[d.Name] = struct{}{}
	}
	return out
}

func (r *recordingMCPRegistry) CallTool(_ context.Context, name string, args map[string]any) (string, error) {
	r.calls = append(r.calls, mcpCall{name: name, args: args})
	if r.err != nil {
		return r.out, r.err
	}
	if r.out != "" {
		return r.out, nil
	}
	return "ok: " + name, nil
}

// gatewayRegistry builds a registry whose tools look like a reverse-engineering
// server's catalog.
func gatewayRegistry() *recordingMCPRegistry {
	return &recordingMCPRegistry{defs: []llm.Tool{
		{
			Name:        "mcp_rea_analyze_binary",
			Description: "Analyze a native binary and return pseudocode.\nLong second paragraph that list output must not include.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Binary path"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "mcp_rea_inspect_wasm",
			Description: "Inspect a WASM artifact.",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}}
}

// newGatewayAgent returns an agent with the MCP exposure policy applied.
func newGatewayAgent(t *testing.T, configured, nested bool, reg MCPToolRegistry) *Agent {
	t.Helper()
	a := &Agent{WorkingDir: t.TempDir(), Mode: ModeAct}
	a.SetMCPTooling(configured, nested)
	if reg != nil {
		a.SetMCPRegistry(reg)
	}
	return a
}

func toolNameList(defs []llm.Tool) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func containsName(defs []llm.Tool, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func gatewayCount(defs []llm.Tool) int {
	n := 0
	for _, name := range mcpGatewayToolNames() {
		if containsName(defs, name) {
			n++
		}
	}
	return n
}

func gatewayAllowed(a *Agent) int {
	n := 0
	allowed := a.AllowedToolNames()
	for _, name := range mcpGatewayToolNames() {
		if _, ok := allowed[name]; ok {
			n++
		}
	}
	return n
}

// TestMCPGatewayToolGating pins the single gating policy across all three tool
// surfaces: in nested mode the gateway is the ONLY MCP tool the model sees (its
// fixed definition is what keeps the request prefix stable), in inline mode the
// server definitions are inlined and no gateway exists, and with no MCP servers
// configured neither appears.
func TestMCPGatewayToolGating(t *testing.T) {
	tests := []struct {
		name        string
		configured  bool
		nested      bool
		wantGateway bool
		wantInlined bool
	}{
		{name: "no MCP servers", configured: false, nested: true},
		{name: "nested", configured: true, nested: true, wantGateway: true},
		{name: "inline", configured: true, nested: false, wantInlined: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := gatewayRegistry()
			a := newGatewayAgent(t, tt.configured, tt.nested, reg)
			defs := a.llmTools()

			wantCount := 0
			if tt.wantGateway {
				wantCount = len(mcpGatewayToolNames())
			}
			if got := gatewayCount(defs); got != wantCount {
				t.Fatalf("gateway tools present = %d, want %d (tools: %v)", got, wantCount, toolNameList(defs))
			}
			if got := containsName(defs, "mcp_rea_analyze_binary"); got != tt.wantInlined {
				t.Fatalf("inlined server tool present = %v, want %v (tools: %v)", got, tt.wantInlined, toolNameList(defs))
			}
			if got := gatewayAllowed(a); got != wantCount {
				t.Fatalf("gateway tools allowed = %d, want %d", got, wantCount)
			}
			if _, ok := a.AllowedToolNames()["mcp_rea_analyze_binary"]; ok != tt.wantInlined {
				t.Fatalf("inlined tool allowed = %v, want %v", ok, tt.wantInlined)
			}
		})
	}
}

// TestMCPGatewayDisabledInPlanMode pins parity with inlined MCP tools, which
// plan mode refuses: the gateway runs server tools, so it is act-only.
func TestMCPGatewayDisabledInPlanMode(t *testing.T) {
	a := newGatewayAgent(t, true, true, gatewayRegistry())
	if gatewayAllowed(a) != len(mcpGatewayToolNames()) {
		t.Fatal("gateway should be allowed in act mode")
	}
	a.Mode = ModePlan
	if gatewayAllowed(a) != 0 {
		t.Fatal("gateway must not be allowed in plan mode")
	}
	for _, name := range mcpGatewayToolNames() {
		if err := a.checkPlanMode(name); err == nil {
			t.Fatalf("plan mode should refuse %s", name)
		} else if !errors.Is(err, ErrPlanModeBlocked) {
			t.Fatalf("error = %v, want ErrPlanModeBlocked", err)
		}
	}
}

// TestMCPToolSetStableAcrossRegistryAttach is the prompt-cache regression: MCP
// init is asynchronous, so the registry appears mid-session. In nested mode the
// model-facing tool list AND the system-prompt hint must be byte-identical
// before and after that attach — otherwise the request prefix changes between
// turn 1 and turn 2 and the provider's cache (tools and system prompt lead the
// prefix) is invalidated for the whole conversation.
func TestMCPToolSetStableAcrossRegistryAttach(t *testing.T) {
	a := newGatewayAgent(t, true, true, nil)
	if a.MCPRegistry != nil {
		t.Fatal("test setup: registry should start detached")
	}
	// systemPromptPrefix needs a conversation (it folds the system prompt into
	// the leading message rather than inventing one for an empty session).
	a.Messages = []llm.Message{{Role: "user", Content: "analyze this binary"}}

	before, err := json.Marshal(a.llmTools())
	if err != nil {
		t.Fatal(err)
	}
	beforeHint := a.mcpToolHint()
	beforePrompt := a.systemPromptPrefix()

	// The async attach: a server finishes its handshake with a large catalog.
	reg := gatewayRegistry()
	for i := 0; i < 30; i++ {
		reg.defs = append(reg.defs, llm.Tool{
			Name:        fmt.Sprintf("mcp_rea_extra_%02d", i),
			Description: "Extra analysis tool",
			Parameters:  map[string]any{"type": "object"},
		})
	}
	a.SetMCPRegistry(reg)

	after, err := json.Marshal(a.llmTools())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("registry attach changed the tool list (cache bust):\nbefore %s\nafter  %s", before, after)
	}
	if got := a.mcpToolHint(); got != beforeHint {
		t.Fatalf("registry attach changed the MCP hint: %q → %q", beforeHint, got)
	}
	afterPrompt := a.systemPromptPrefix()
	if beforePrompt == nil || afterPrompt == nil || beforePrompt[0].Content != afterPrompt[0].Content {
		t.Fatal("registry attach changed the system prompt (cache bust)")
	}
}

// TestMCPGatewayList pins the discovery output: sorted names with a one-line
// summary, query filtering, and a bounded answer for a large catalog.
func TestMCPGatewayList(t *testing.T) {
	a := newGatewayAgent(t, true, true, gatewayRegistry())

	out, err := handleMCPList(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"2 MCP tool(s) available", "mcp_rea_analyze_binary", "mcp_rea_inspect_wasm", "Analyze a native binary"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Long second paragraph") {
		t.Fatalf("list output should carry only the first description line:\n%s", out)
	}
	if strings.Index(out, "mcp_rea_analyze_binary") > strings.Index(out, "mcp_rea_inspect_wasm") {
		t.Fatalf("list output is not sorted by name:\n%s", out)
	}

	t.Run("query filters", func(t *testing.T) {
		out, err := handleMCPList(context.Background(), a, map[string]any{"query": "wasm"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "mcp_rea_inspect_wasm") || strings.Contains(out, "mcp_rea_analyze_binary") {
			t.Fatalf("query filter = %q, want only the WASM tool", out)
		}
	})

	t.Run("query with no match", func(t *testing.T) {
		out, err := handleMCPList(context.Background(), a, map[string]any{"query": "nonexistent"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "No MCP tool matches") || !strings.Contains(out, "2 tools are available") {
			t.Fatalf("no-match output = %q", out)
		}
	})

	t.Run("large catalog is bounded", func(t *testing.T) {
		reg := &recordingMCPRegistry{}
		for i := 0; i < mcpListLimit+5; i++ {
			reg.defs = append(reg.defs, llm.Tool{Name: fmt.Sprintf("mcp_srv_tool_%02d", i), Description: "x"})
		}
		big := newGatewayAgent(t, true, true, reg)
		out, err := handleMCPList(context.Background(), big, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "and 5 more") {
			t.Fatalf("bounded list must report the remainder:\n%s", out)
		}
		if got := strings.Count(out, "mcp_srv_tool_"); got != mcpListLimit {
			t.Fatalf("listed %d tools, want the %d-tool cap", got, mcpListLimit)
		}
	})

	t.Run("registry not attached yet", func(t *testing.T) {
		waiting := newGatewayAgent(t, true, true, nil)
		out, err := handleMCPList(context.Background(), waiting, map[string]any{})
		if err == nil || out != "" {
			t.Fatalf("list with no registry = (%q, %v), want a retryable error", out, err)
		}
		if !strings.Contains(err.Error(), "no MCP server is connected yet") {
			t.Fatalf("error = %v, want a 'not connected yet' hint", err)
		}
	})
}

// TestMCPGatewayDescribe pins the schema lookup, including name resolution
// without the mcp_ prefix.
func TestMCPGatewayDescribe(t *testing.T) {
	a := newGatewayAgent(t, true, true, gatewayRegistry())

	out, err := handleMCPDescribe(context.Background(), a, map[string]any{"tool": "rea_analyze_binary"})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	for _, want := range []string{"mcp_rea_analyze_binary", "Arguments (JSON schema)", `"path"`, "Binary path"} {
		if !strings.Contains(out, want) {
			t.Fatalf("describe output missing %q:\n%s", want, out)
		}
	}
}

// TestMCPGatewayCall pins dispatch: object and JSON-string arguments, name
// resolution, and the error paths.
func TestMCPGatewayCall(t *testing.T) {
	t.Run("object arguments", func(t *testing.T) {
		reg := gatewayRegistry()
		a := newGatewayAgent(t, true, true, reg)
		out, err := handleMCPCall(context.Background(), a, map[string]any{
			"tool":  "mcp_rea_analyze_binary",
			"input": map[string]any{"path": "/bin/ls"},
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if out != "ok: mcp_rea_analyze_binary" {
			t.Fatalf("output = %q", out)
		}
		if len(reg.calls) != 1 || reg.calls[0].name != "mcp_rea_analyze_binary" || reg.calls[0].args["path"] != "/bin/ls" {
			t.Fatalf("dispatched call = %+v", reg.calls)
		}
	})

	t.Run("JSON string arguments", func(t *testing.T) {
		reg := gatewayRegistry()
		a := newGatewayAgent(t, true, true, reg)
		if _, err := handleMCPCall(context.Background(), a, map[string]any{
			"tool":  "rea_analyze_binary",
			"input": `{"path":"/bin/cat"}`,
		}); err != nil {
			t.Fatalf("call: %v", err)
		}
		if len(reg.calls) != 1 || reg.calls[0].args["path"] != "/bin/cat" {
			t.Fatalf("dispatched call = %+v", reg.calls)
		}
	})

	t.Run("absent arguments", func(t *testing.T) {
		reg := gatewayRegistry()
		a := newGatewayAgent(t, true, true, reg)
		if _, err := handleMCPCall(context.Background(), a, map[string]any{"tool": "mcp_rea_inspect_wasm"}); err != nil {
			t.Fatalf("call: %v", err)
		}
		if len(reg.calls) != 1 || len(reg.calls[0].args) != 0 {
			t.Fatalf("dispatched call = %+v", reg.calls)
		}
	})

	t.Run("server error keeps its text", func(t *testing.T) {
		reg := gatewayRegistry()
		reg.err = errors.New("analysis failed")
		reg.out = "provider not configured"
		a := newGatewayAgent(t, true, true, reg)
		out, err := handleMCPCall(context.Background(), a, map[string]any{"tool": "mcp_rea_analyze_binary"})
		if err == nil {
			t.Fatal("expected an error")
		}
		if out != "provider not configured" {
			t.Fatalf("output = %q, want the server text preserved", out)
		}
		if !strings.Contains(err.Error(), "mcp_rea_analyze_binary") {
			t.Fatalf("error = %v, want the tool name for context", err)
		}
	})

	t.Run("unknown tool", func(t *testing.T) {
		a := newGatewayAgent(t, true, true, gatewayRegistry())
		_, err := handleMCPCall(context.Background(), a, map[string]any{"tool": "nope"})
		if err == nil || !strings.Contains(err.Error(), "unknown MCP tool") {
			t.Fatalf("error = %v, want unknown-tool", err)
		}
	})

	t.Run("ambiguous suffix", func(t *testing.T) {
		reg := &recordingMCPRegistry{defs: []llm.Tool{
			{Name: "mcp_a_analyze", Parameters: map[string]any{"type": "object"}},
			{Name: "mcp_b_analyze", Parameters: map[string]any{"type": "object"}},
		}}
		a := newGatewayAgent(t, true, true, reg)
		_, err := handleMCPCall(context.Background(), a, map[string]any{"tool": "analyze"})
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("error = %v, want ambiguity", err)
		}
	})

	t.Run("arguments alias", func(t *testing.T) {
		reg := gatewayRegistry()
		a := newGatewayAgent(t, true, true, reg)
		if _, err := handleMCPCall(context.Background(), a, map[string]any{
			"tool":      "mcp_rea_analyze_binary",
			"arguments": map[string]any{"path": "/bin/alias"},
		}); err != nil {
			t.Fatalf("call: %v", err)
		}
		if len(reg.calls) != 1 || reg.calls[0].args["path"] != "/bin/alias" {
			t.Fatalf("dispatched call = %+v", reg.calls)
		}
	})

	t.Run("input wins over arguments", func(t *testing.T) {
		reg := gatewayRegistry()
		a := newGatewayAgent(t, true, true, reg)
		if _, err := handleMCPCall(context.Background(), a, map[string]any{
			"tool":      "mcp_rea_analyze_binary",
			"input":     map[string]any{"path": "/bin/input"},
			"arguments": map[string]any{"path": "/bin/arguments"},
		}); err != nil {
			t.Fatalf("call: %v", err)
		}
		if len(reg.calls) != 1 || reg.calls[0].args["path"] != "/bin/input" {
			t.Fatalf("dispatched call = %+v", reg.calls)
		}
	})

	t.Run("missing tool", func(t *testing.T) {
		a := newGatewayAgent(t, true, true, gatewayRegistry())
		if _, err := handleMCPCall(context.Background(), a, map[string]any{}); err == nil {
			t.Fatal("expected an error for a missing tool")
		}
		if _, err := handleMCPDescribe(context.Background(), a, map[string]any{}); err == nil {
			t.Fatal("expected an error for a missing tool")
		}
	})
}

// TestMCPShadowingKeepsToolOrder pins the second cache fix: an MCP tool that
// shadows a builtin replaces the definition AT THE SAME INDEX. The previous
// remove-and-append behavior shifted every tool before it, so one name
// collision reordered the whole request prefix.
func TestMCPShadowingKeepsToolOrder(t *testing.T) {
	base := newGatewayAgent(t, false, false, nil)
	before := toolNameList(base.llmTools())
	idx := -1
	for i, name := range before {
		if name == "read_file" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("test setup: read_file missing from the builtin list")
	}

	reg := &recordingMCPRegistry{defs: []llm.Tool{{
		Name:        "read_file",
		Description: "mcp read_file",
		Parameters:  map[string]any{"type": "object"},
	}}}
	shadowed := newGatewayAgent(t, true, false, reg)
	after := toolNameList(shadowed.llmTools())

	if len(after) != len(before) {
		t.Fatalf("tool count changed: %d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("tool %d moved: %q → %q (shadowing must not reorder)", i, before[i], after[i])
		}
	}
	if got := shadowed.llmTools()[idx].Description; got != "mcp read_file" {
		t.Fatalf("definition at index %d = %q, want the MCP definition", idx, got)
	}
}
