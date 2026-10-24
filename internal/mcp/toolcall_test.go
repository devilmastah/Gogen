package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gogen/internal/config"
)

// TestMCPPagedServerHelperProcess is the re-exec'd helper that fakes a
// paginated MCP stdio server shaped like a real analysis server: tools/list
// spreads its catalog over two pages, and one tool returns an image part
// instead of text.
func TestMCPPagedServerHelperProcess(t *testing.T) {
	if os.Getenv("GOGEN_MCP_PAGED_HELPER") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.Method == "notifications/initialized" {
			continue // notification: no reply
		}
		var resp map[string]any
		switch req.Method {
		case "initialize":
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{"tools": map[string]any{}},
				},
			}
		case "tools/list":
			page := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "page-one-tool", "description": "Page one", "inputSchema": map[string]any{"type": "object"}},
					},
				},
			}
			if req.Params.Cursor == "" {
				page["result"].(map[string]any)["nextCursor"] = "page-2"
			} else {
				page["result"].(map[string]any)["tools"] = []map[string]any{
					{"name": "page-two-tool", "description": "Page two", "inputSchema": map[string]any{"type": "object"}},
				}
			}
			resp = page
		case "tools/call":
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "image", "mimeType": "image/png", "data": "iVBORw0KGgo="},
					},
					"structuredContent": map[string]any{"artifact": "screenshot.png"},
				},
			}
		default:
			continue
		}
		b, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		fmt.Fprintln(os.Stdout, string(b))
	}
	os.Exit(0)
}

func pagedHelperCommand() config.MCPServerConfig {
	return config.MCPServerConfig{
		Name:    "paged",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPPagedServerHelperProcess"},
	}
}

// TestManagerFollowsToolsListPagination pins that a paginated tools/list is
// followed to the last page: stopping at the first page would silently hide
// every tool after it, which for a large catalog (REA) is most of the toolset.
func TestManagerFollowsToolsListPagination(t *testing.T) {
	t.Setenv("GOGEN_MCP_PAGED_HELPER", "1")
	m, err := NewManager([]config.MCPServerConfig{pagedHelperCommand()})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() { _ = m.Close() }()

	names := m.Registry().ToolNames()
	for _, want := range []string{"mcp_paged_page_one_tool", "mcp_paged_page_two_tool"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("tool %q missing from the registry (got %v)", want, names)
		}
	}
}

// TestCallToolRendersNonTextContent pins the end-to-end behavior for a result
// whose only part is an image: the model must receive a placeholder naming the
// type and size rather than an empty tool result (which reads as "the tool did
// nothing" and triggers a retry loop).
func TestCallToolRendersNonTextContent(t *testing.T) {
	t.Setenv("GOGEN_MCP_PAGED_HELPER", "1")
	m, err := NewManager([]config.MCPServerConfig{pagedHelperCommand()})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() { _ = m.Close() }()

	out, err := m.Registry().CallTool(context.Background(), "mcp_paged_page_one_tool", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("image-only result reached the model as an empty tool result")
	}
	if !strings.Contains(out, "image/png") || !strings.Contains(out, "bytes") {
		t.Fatalf("placeholder = %q, want the mime type and size", out)
	}
}

// TestCallToolUsesConfiguredTimeout pins that the server entry's
// tool_timeout_secs reaches the client: a tool that answers after the package
// default would previously have been cancelled. The helper answers instantly,
// so this asserts the resolved value on the live client rather than the wait.
func TestCallToolUsesConfiguredTimeout(t *testing.T) {
	t.Setenv("GOGEN_MCP_PAGED_HELPER", "1")
	server := pagedHelperCommand()
	server.ToolTimeoutSecs = 1234
	m, err := NewManager([]config.MCPServerConfig{server})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() { _ = m.Close() }()

	if len(m.clients) != 1 {
		t.Fatalf("expected 1 live client, got %d", len(m.clients))
	}
	want := 1234 * time.Second
	if got := m.clients[0].callTimeout; got != want {
		t.Fatalf("client callTimeout = %s, want %s (from tool_timeout_secs)", got, want)
	}
}
