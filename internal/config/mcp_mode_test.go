package config

import "testing"

// TestMCPActive pins the config-level MCP predicate that replaces the live
// registry check for the model-facing tool set. It deliberately answers from
// configuration, so it is true from process start — before any server has
// finished its handshake — and never flips mid-session (a flip would rewrite
// the request prefix and invalidate the provider's prompt cache).
func TestMCPActive(t *testing.T) {
	server := MCPServerConfig{Name: "rea", Command: "npx", Args: []string{"-y", "rea-agents", "mcp"}}

	tests := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "nil config", cfg: nil},
		{name: "mcp off", cfg: &Config{MCP: "off", MCPServers: []MCPServerConfig{server}}},
		{name: "mcp on but no servers", cfg: &Config{MCP: "on"}},
		{name: "mcp on with a placeholder entry", cfg: &Config{MCP: "on", MCPServers: []MCPServerConfig{{Name: "rea"}}}},
		{name: "mcp on with a server", cfg: &Config{MCP: "on", MCPServers: []MCPServerConfig{server}}, want: true},
		{name: "mcp on, one valid one placeholder", cfg: &Config{MCP: "on", MCPServers: []MCPServerConfig{{Name: "blank"}, server}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.MCPActive(); got != tt.want {
				t.Fatalf("MCPActive() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMCPNested pins the mode resolution: only an explicit "inline" selects
// inlined definitions; empty and unknown values are nested.
func TestMCPNested(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{mode: "", want: true},
		{mode: MCPToolModeNested, want: true},
		{mode: MCPToolModeInline, want: false},
		{mode: " INLINE ", want: false},
		{mode: "sideways", want: true},
	}
	for _, tt := range tests {
		cfg := &Config{MCPToolMode: tt.mode}
		if got := cfg.MCPNested(); got != tt.want {
			t.Fatalf("MCPNested(%q) = %v, want %v", tt.mode, got, tt.want)
		}
	}
	var nilCfg *Config
	if !nilCfg.MCPNested() {
		t.Fatal("nil config should resolve to nested")
	}
}
