package mcp

import (
	"testing"

	"gogen/internal/config"
)

// TestNewManagerStartsAllServers pins the parallel-start fix: servers start
// concurrently (each with its own init budget) and every one of them is
// registered. Sequentially, the total startup was the SUM of the per-server
// budgets while callers only wait MaxInitTimeout (the slowest single one), so a
// third slow server was never waited for.
func TestNewManagerStartsAllServers(t *testing.T) {
	t.Setenv("GOGEN_MCP_TEST_HELPER", "1")
	base := helperCommand()
	alpha, beta := base, base
	alpha.Name, beta.Name = "alpha", "beta"

	m, err := NewManager([]config.MCPServerConfig{alpha, beta})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() { _ = m.Close() }()

	if len(m.clients) != 2 {
		t.Fatalf("clients = %d, want 2 (both servers started)", len(m.clients))
	}
	// Registration happens in configured order, and both servers' tools are in
	// the registry under their per-server names.
	names := m.reg.ToolNames()
	for _, want := range []string{"mcp_alpha_tool_a", "mcp_alpha_tool_b", "mcp_beta_tool_a", "mcp_beta_tool_b"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("registry missing %q (got %v)", want, names)
		}
	}
}
