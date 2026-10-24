package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/mcp"
)

// e2eToolCount is the size of the fake server's catalog: deliberately larger
// than GoGen's whole builtin set, the shape that motivated the gateway (a
// reverse-engineering server publishes dozens of tools).
const e2eToolCount = 60

// TestAgentMCPGatewayHelperProcess is the re-exec'd fake MCP server for
// TestAgentMCPGatewayEndToEnd. It speaks stdio JSON-RPC and publishes a
// REA-shaped catalog: many analysis tools, one echoing tool, and one that
// returns image content only.
func TestAgentMCPGatewayHelperProcess(t *testing.T) {
	if os.Getenv("GOGEN_AGENT_MCP_HELPER") != "1" {
		return
	}
	tools := make([]map[string]any, 0, e2eToolCount+2)
	for i := 0; i < e2eToolCount; i++ {
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("analyze_%02d", i),
			"description": fmt.Sprintf("Analyze artifact %02d of a target.", i),
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Target path"},
				},
				"required": []string{"path"},
			},
		})
	}
	tools = append(tools,
		map[string]any{
			"name":        "say",
			"description": "Echo the given text.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
				"required":   []string{"text"},
			},
		},
		map[string]any{
			"name":        "screenshot",
			"description": "Return a screenshot as image content.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	)

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.Method == "notifications/initialized" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-rea", "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": tools}
		case "tools/call":
			switch req.Params.Name {
			case "say":
				result = map[string]any{"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("echo: %v", req.Params.Arguments["text"])},
				}}
			case "screenshot":
				result = map[string]any{"content": []map[string]any{
					{"type": "image", "mimeType": "image/png", "data": "iVBORw0KGgoAAAANSUhEUg=="},
				}}
			default:
				result = map[string]any{"content": []map[string]any{
					{"type": "text", "text": "ran " + req.Params.Name},
				}}
			}
		default:
			continue
		}
		payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if err != nil {
			continue
		}
		fmt.Fprintln(os.Stdout, string(payload))
	}
	os.Exit(0)
}

// TestAgentMCPGatewayEndToEnd drives the whole path with a real MCP stdio
// subprocess: manager spawn → handshake → catalog → agent registration →
// gateway list/describe/call through executeTool (plan-mode gate and
// name-resolution included). It is the wiring a live REA install exercises,
// minus the model.
func TestAgentMCPGatewayEndToEnd(t *testing.T) {
	t.Setenv("GOGEN_AGENT_MCP_HELPER", "1")
	server := config.MCPServerConfig{
		Name:    "rea",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestAgentMCPGatewayHelperProcess"},
	}
	mgr, err := mcp.NewManager([]config.MCPServerConfig{server})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	if got := len(mgr.Registry().ToolNames()); got != e2eToolCount+2 {
		t.Fatalf("registered %d server tools, want %d", got, e2eToolCount+2)
	}

	baseline := newGatewayAgent(t, false, false, nil)
	baselineTools := len(baseline.llmTools())

	a := newGatewayAgent(t, true, true, mgr.Registry())
	defs := a.llmTools()
	if len(defs) != baselineTools+1 {
		t.Fatalf("nested mode tool count = %d, want %d (builtins + the single gateway)", len(defs), baselineTools+1)
	}
	for _, def := range defs {
		if strings.HasPrefix(def.Name, "mcp_rea_") {
			t.Fatalf("server tool %q was inlined in nested mode", def.Name)
		}
	}

	ctx := context.Background()
	run := func(args map[string]any) (string, error) {
		t.Helper()
		return a.executeTool(ctx, llm.ToolCall{ID: "c1", Name: "mcp", Args: args})
	}

	t.Run("list", func(t *testing.T) {
		out, err := run(map[string]any{"action": "list", "query": "screenshot"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if !strings.Contains(out, "mcp_rea_screenshot") || !strings.Contains(out, "Return a screenshot") {
			t.Fatalf("list output = %q", out)
		}
		if strings.Contains(out, "mcp_rea_analyze") {
			t.Fatalf("query filter leaked unrelated tools: %q", out)
		}
	})

	t.Run("describe", func(t *testing.T) {
		out, err := run(map[string]any{"action": "describe", "tool": "rea_say"})
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		if !strings.Contains(out, "mcp_rea_say") || !strings.Contains(out, `"text"`) {
			t.Fatalf("describe output = %q", out)
		}
	})

	t.Run("call by bare name", func(t *testing.T) {
		out, err := run(map[string]any{
			"action":    "call",
			"tool":      "rea_say",
			"arguments": map[string]any{"text": "hello rea"},
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if out != "echo: hello rea" {
			t.Fatalf("call output = %q", out)
		}
	})

	t.Run("call by full name with JSON-string arguments", func(t *testing.T) {
		out, err := run(map[string]any{
			"action":    "call",
			"tool":      "mcp_rea_say",
			"arguments": `{"text":"json string"}`,
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if out != "echo: json string" {
			t.Fatalf("call output = %q", out)
		}
	})

	t.Run("image-only result reaches the model as a placeholder", func(t *testing.T) {
		out, err := run(map[string]any{"action": "call", "tool": "rea_screenshot"})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !strings.Contains(out, "image/png") || !strings.Contains(out, "bytes") {
			t.Fatalf("image result = %q, want a described placeholder", out)
		}
	})

	t.Run("unknown tool", func(t *testing.T) {
		if _, err := run(map[string]any{"action": "call", "tool": "nope"}); err == nil {
			t.Fatal("expected an unknown-tool error")
		}
	})

	t.Run("plan mode refuses the gateway", func(t *testing.T) {
		a.Mode = ModePlan
		defer func() { a.Mode = ModeAct }()
		if _, err := run(map[string]any{"action": "list"}); err == nil {
			t.Fatal("plan mode must refuse the gateway")
		}
	})

	// The opt-out must still work against a real server: inline mode lists
	// every server tool and registers no gateway.
	t.Run("inline mode inlines the catalog", func(t *testing.T) {
		inline := newGatewayAgent(t, true, false, mgr.Registry())
		defs := inline.llmTools()
		if len(defs) != baselineTools+e2eToolCount+2 {
			t.Fatalf("inline mode tool count = %d, want %d (builtins + the whole catalog)", len(defs), baselineTools+e2eToolCount+2)
		}
		if containsName(defs, mcpToolName) {
			t.Fatal("inline mode must not register the gateway")
		}
		if !containsName(defs, "mcp_rea_say") {
			t.Fatal("inline mode should expose the server tools by name")
		}
	})
}
