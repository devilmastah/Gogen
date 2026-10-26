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

// TestMCPEntriesListsRunningHostServer pins the settings list: configured
// servers stay, a host server is appended only when mcp was on at startup,
// and an entry with no command is left out.
func TestMCPEntriesListsRunningHostServer(t *testing.T) {
	fetch := config.MCPServerConfig{Name: "fetch", Command: "npx", Env: map[string]string{"TOKEN": "secret"}}
	vscode := config.MCPServerConfig{Name: "vscode", Command: "node", Args: []string{"bridge.js"}, Env: map[string]string{"GOGEN_VSCODE_BRIDGE_TOKEN": "secret"}}
	blank := config.MCPServerConfig{Name: "blank"}
	tests := []struct {
		name    string
		mcp     string
		wantLen int
		host    bool
	}{
		{name: "mcp on lists the host server", mcp: "on", wantLen: 2, host: true},
		{name: "mcp off leaves the host server out", mcp: "off", wantLen: 1, host: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.MCP = tt.mcp
			cfg.MCPServers = []config.MCPServerConfig{fetch}
			cfg.HostMCPServers = []config.MCPServerConfig{vscode, blank}
			s := &Server{ws: newMCPTestWorkspace(t, &cfg)}
			got := s.mcpEntries()
			if len(got) != tt.wantLen {
				t.Fatalf("entries = %+v, want %d", got, tt.wantLen)
			}
			if got[0].Name != "fetch" || got[0].Host || !got[0].EnvSet {
				t.Fatalf("configured entry = %+v", got[0])
			}
			if tt.host {
				if got[1].Name != "vscode" || !got[1].Host || !got[1].EnvSet || len(got[1].Args) != 1 {
					t.Fatalf("host entry = %+v", got[1])
				}
			}
		})
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
