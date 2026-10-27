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
		// client ever receiving the env values. Config is never reassigned
		// after construction, so this read is race-free.
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
		// The page reads the raw host list, not the slice the manager was
		// given. ValidMCPServers is that same usable-entry rule, so a blank
		// command is not shown as a server this process started. The
		// configured rows above come from GetRuntimeConfig. These host rows
		// come from Config, which is never reassigned after construction,
		// so the unlocked read is race-free.
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

// wsHandleMCPSave upserts one MCP server into the runtime overlay
// (mcp_save from the settings modal). The list is RESTART-SCOPED: the MCP
// manager was built from the startup config, so nothing is started or
// stopped here — the entry is persisted for the next start, pushed back to
// every tab immediately, and the client is told to restart. Env values can
// hold secrets, so the persist forces the 0600 secret write (the same
// policy as provider saves).
func wsHandleMCPSave(req *wsRequest) {
	s, ws, msg := req.server, req.conn, req.msg
	op := msg.MCPOp
	if op == nil {
		writeNoticeError(ws, "mcp", "Error: missing MCP save request")
		return
	}
	name := strings.TrimSpace(op.Name)
	if name == "" {
		writeNoticeError(ws, "mcp", "Error: MCP server name is required")
		return
	}
	if s.isHostMCPServer(name) {
		writeNoticeError(ws, "mcp", fmt.Sprintf("Error: MCP server %q is started by this process (GOGEN_MCP_EXTRA) and cannot be edited", name))
		return
	}
	r := s.ws.GetRuntimeConfig()
	idx := -1
	for i, m := range r.MCPServers {
		if m.Name == name {
			idx = i
			break
		}
	}
	entry := config.MCPServerConfig{Name: name}
	entry.Args = cleanMCPArgs(op.Args)
	if idx >= 0 {
		// Edit: blank command / empty env keep the stored values, so a row
		// saved without retyping its secrets cannot wipe them.
		stored := r.MCPServers[idx]
		entry.Command = strings.TrimSpace(op.Command)
		if entry.Command == "" {
			entry.Command = stored.Command
		}
		entry.Env = cloneMCPEnv(stored.Env)
		if len(op.Env) > 0 {
			entry.Env = cloneMCPEnv(op.Env)
		}
		entry.InitTimeoutSecs = stored.InitTimeoutSecs
		entry.ToolTimeoutSecs = stored.ToolTimeoutSecs
	} else {
		entry.Command = strings.TrimSpace(op.Command)
		if entry.Command == "" {
			writeNoticeError(ws, "mcp", "Error: MCP server command is required")
			return
		}
		entry.Env = cloneMCPEnv(op.Env)
	}
	if op.InitTimeoutSecs > 0 {
		entry.InitTimeoutSecs = op.InitTimeoutSecs
	}
	if op.ToolTimeoutSecs > 0 {
		entry.ToolTimeoutSecs = op.ToolTimeoutSecs
	}
	if idx >= 0 {
		r.MCPServers[idx] = entry
	} else {
		r.MCPServers = append(r.MCPServers, entry)
	}
	s.ws.SetRuntimeConfig(r)
	s.persistMCPChange(ws, "Saved")
}

// wsHandleMCPDelete removes one configured MCP server (mcp_delete). Host
// servers (GOGEN_MCP_EXTRA) are process-local and cannot be deleted.
func wsHandleMCPDelete(req *wsRequest) {
	s, ws, msg := req.server, req.conn, req.msg
	op := msg.MCPOp
	if op == nil {
		writeNoticeError(ws, "mcp", "Error: missing MCP delete request")
		return
	}
	name := strings.TrimSpace(op.Name)
	if name == "" {
		writeNoticeError(ws, "mcp", "Error: MCP server name is required")
		return
	}
	if s.isHostMCPServer(name) {
		writeNoticeError(ws, "mcp", fmt.Sprintf("Error: MCP server %q is started by this process (GOGEN_MCP_EXTRA) and cannot be deleted", name))
		return
	}
	r := s.ws.GetRuntimeConfig()
	idx := -1
	for i, m := range r.MCPServers {
		if m.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeNoticeError(ws, "mcp", fmt.Sprintf("Error: MCP server %q is not configured", name))
		return
	}
	r.MCPServers = append(r.MCPServers[:idx], r.MCPServers[idx+1:]...)
	s.ws.SetRuntimeConfig(r)
	s.persistMCPChange(ws, "Deleted")
}

// persistMCPChange persists the effective config (forced secrets: MCP env
// values are user-entered and must survive the restart this change is staged
// for), broadcasts a fresh config to every tab, and reports the restart
// requirement off the read loop.
func (s *Server) persistMCPChange(ws *wsConn, verb string) {
	// MCP env values are user-entered, so the write forces the secret form.
	s.persistAndBroadcast(nil, true)
	writeNotice(ws, "mcp", true, verb+" — restart gogen for MCP changes to take effect")
}

// isHostMCPServer reports whether name belongs to a process-local host
// server (GOGEN_MCP_EXTRA): those are started by the host, never written to
// a config file, and must not be editable from the settings modal.
func (s *Server) isHostMCPServer(name string) bool {
	if s == nil || s.ws == nil || s.ws.Config == nil {
		return false
	}
	for _, m := range s.ws.Config.HostMCPServers {
		if m.Name == name {
			return true
		}
	}
	return false
}

// cleanMCPArgs trims the arg list and drops empty entries (the add form
// splits a typed command line, which can leave blanks on trailing spaces).
func cleanMCPArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cloneMCPEnv copies an MCP env map. The runtime overlay is copied by value
// (GetRuntimeConfig), so nested maps would otherwise be shared between the
// stored entry and the request; cloning keeps the overlay copy-on-write.
func cloneMCPEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}
