package server

import (
	"testing"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/llm/llmtest"
)

// newMCPTestWorkspace builds a minimal workspace around a startup config.
func newMCPTestWorkspace(t *testing.T, cfg *config.Config) *Workspace {
	t.Helper()
	dir := t.TempDir()
	return &Workspace{
		Exec:       agent.NewExecutor(dir),
		Config:     cfg,
		WorkingDir: dir,
		// The live overlay starts as a copy of the startup config, exactly as
		// newWorkspaceFromAgent seeds it; otherwise this test would compare
		// against an empty overlay and prove nothing.
		runtime: runtimeSeed(cfg),
		ProviderFactory: func() llm.LLMProvider {
			return llmtest.NewMockProvider()
		},
	}
}

// TestSessionAgentMCPToolingFromStartupConfig pins where the model-facing MCP
// decision comes from: the STARTUP config (which is what built the MCP
// manager), not the runtime overlay and not the live registry.
//
// The settings modal marks `mcp` as restart-scoped, so a live toggle must not
// add a gateway whose servers were never started (a tool that could only fail)
// nor remove a gateway whose servers are still running. Deriving it from the
// runtime overlay would do both, and deriving it from the registry would move
// the request prefix mid-session and defeat prompt caching. (The tool list
// derived from these flags is covered in internal/agent: gateway gating and the
// attach-stability test.)
func TestSessionAgentMCPToolingFromStartupConfig(t *testing.T) {
	cfg := config.Defaults()
	cfgPtr := &cfg
	cfg.MCP = "on"
	cfg.MCPServers = []config.MCPServerConfig{{Name: "rea", Command: "npx", Args: []string{"-y", "rea-agents", "mcp"}}}
	ws := newMCPTestWorkspace(t, cfgPtr)

	a := ws.NewSessionAgent(nil, "sess-mcp-1")
	if !a.MCPConfigured() {
		t.Fatal("startup config has MCP on with a server; the session should know it")
	}
	if !a.MCPNested() {
		t.Fatal("default mcp_tool_mode should resolve to nested")
	}
	if a.MCPRegistry != nil {
		t.Fatal("no manager was started in this test; the registry must stay nil")
	}

	// A live `mcp: off` toggle (restart-scoped in the UI) must not change it.
	live := ws.GetRuntimeConfig()
	live.MCP = "off"
	ws.SetRuntimeConfig(live)

	a2 := ws.NewSessionAgent(nil, "sess-mcp-2")
	if !a2.MCPConfigured() {
		t.Fatal("runtime overlay for a restart-scoped toggle must not change the session's MCP wiring")
	}
	if !a2.MCPNested() {
		t.Fatal("runtime overlay must not change the exposure mode")
	}
}

// TestSessionAgentMCPToolingDisabled pins the other direction: with MCP off in
// the startup config nothing MCP-shaped appears, even if the live overlay turns
// it on (again: restart-scoped).
func TestSessionAgentMCPToolingDisabled(t *testing.T) {
	cfg := config.Defaults()
	cfgPtr := &cfg // mcp: off
	ws := newMCPTestWorkspace(t, cfgPtr)

	live := ws.GetRuntimeConfig()
	live.MCP = "on"
	live.MCPServers = []config.MCPServerConfig{{Name: "rea", Command: "npx"}}
	ws.SetRuntimeConfig(live)

	a := ws.NewSessionAgent(nil, "sess-mcp-off")
	if a.MCPConfigured() {
		t.Fatal("runtime overlay must not enable MCP for a session whose servers were never started")
	}
}
