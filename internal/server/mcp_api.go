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

// mcpProbeTimeout is the test_mcp deadline. The floor stays mcpTestTimeout
// (a cold npx download). A server that sets init_timeout_secs higher — a
// reverse-engineering launcher that spends that budget inside the handshake —
// must not be reported broken by the shorter hardcoded cap.
func mcpProbeTimeout(server config.MCPServerConfig) time.Duration {
	timeout := mcpTestTimeout
	if server.InitTimeoutSecs > 0 {
		if d := time.Duration(server.InitTimeoutSecs) * time.Second; d > timeout {
			timeout = d
		}
	}
	return timeout
}

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
		// A host server is running from the startup config, not the file.
		// Resolve it here so the probe can use its command without the
		// client ever receiving the env values.
		if !found && s.ws.Config != nil && s.ws.Config.MCPEnabled() {
			for _, m := range s.ws.Config.HostMCPServers {
				if m.Name == mcpReq.Name {
					server = m
					found = true
					break
				}
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
		ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout(server))
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

// mcpEntries projects the MCP servers for the client: the configured list,
// plus a host server that this process actually started. Env values (which
// can hold secrets) are never pushed. mcp off leaves the host server out,
// because it was not started.
func (s *Server) mcpEntries() []MCPEntry {
	r := s.ws.GetRuntimeConfig()
	out := make([]MCPEntry, 0, len(r.MCPServers))
	for _, m := range r.MCPServers {
		out = append(out, mcpEntry(m, false))
	}
	if s.ws.Config != nil && s.ws.Config.MCPEnabled() {
		for _, m := range config.ValidMCPServers(s.ws.Config.HostMCPServers) {
			out = append(out, mcpEntry(m, true))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mcpEntry(m config.MCPServerConfig, host bool) MCPEntry {
	return MCPEntry{
		Name:    m.Name,
		Command: m.Command,
		Args:    append([]string(nil), m.Args...),
		EnvSet:  len(m.Env) > 0,
		Host:    host,
	}
}
