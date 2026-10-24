package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gogen/internal/config"
)

// mcpTestTimeout bounds a test_mcp probe (spawn + handshake + tools/list).
// It is generous on purpose: the common launcher is `npx -y <package>`, which
// has to download the package on a cold cache before it answers at all, so a
// short bound reported a working server (e.g. rea-agents) as broken. The probe
// is user-initiated and shows a "Testing…" note while it runs.
const mcpTestTimeout = 90 * time.Second

// wsHandleTestMCP runs the MCP connectivity + tools test against a
// THROWAWAY stdio process (test_mcp) — never registered, never wired to the
// shared registry. The reply is an mcp_test message carrying
// ok/latency/tools/error.
func wsHandleTestMCP(req *wsRequest) {
	s, ws, msg := req.server, req.conn, req.msg
	mcpReq := msg.MCPTest
	if mcpReq == nil {
		writeNoticeError(ws, "mcp", "Error: missing MCP test request")
		return
	}
	server := config.MCPServerConfig{Name: mcpReq.Name, Command: mcpReq.Command, Args: mcpReq.Args, Env: mcpReq.Env}
	// Testing a registered server by name resolves its STORED command/args/
	// env from the runtime overlay (the client only sees envSet, never the
	// env values).
	if mcpReq.Name != "" && mcpReq.Command == "" {
		rc := s.ws.GetRuntimeConfig()
		found := false
		for _, m := range rc.MCPServers {
			if m.Name == mcpReq.Name {
				server = m
				found = true
				break
			}
		}
		if !found {
			writeNoticeError(ws, "mcp", fmt.Sprintf("Error: MCP server %q is not configured", mcpReq.Name))
			return
		}
	}
	if strings.TrimSpace(server.Command) == "" {
		writeNoticeError(ws, "mcp", "Error: MCP server command is required")
		return
	}
	go func() {
		start := time.Now()
		reply := func(res MCPTestResult) {
			_ = ws.writeJSON(WSMessage{Type: "mcp_test", MCPTestResult: &res})
		}
		ctx, cancel := context.WithTimeout(context.Background(), mcpTestTimeout)
		defer cancel()
		tools, err := s.mcpTestFn(ctx, server)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			reply(MCPTestResult{Error: err.Error(), LatencyMs: latency})
			return
		}
		out := make([]MCPTestTool, 0, len(tools))
		for _, t := range tools {
			out = append(out, MCPTestTool{Name: t.Name, Description: t.Description})
		}
		reply(MCPTestResult{OK: true, LatencyMs: latency, Tools: out})
	}()
}

// mcpEntries projects the configured MCP server list for the client: name,
// command, args, and envSet — env values (which can hold secrets) are never
// pushed.
func (s *Server) mcpEntries() []MCPEntry {
	r := s.ws.GetRuntimeConfig()
	if len(r.MCPServers) == 0 {
		return nil
	}
	out := make([]MCPEntry, 0, len(r.MCPServers))
	for _, m := range r.MCPServers {
		out = append(out, MCPEntry{
			Name:    m.Name,
			Command: m.Command,
			Args:    append([]string(nil), m.Args...),
			EnvSet:  len(m.Env) > 0,
		})
	}
	return out
}
