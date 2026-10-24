package projectfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPServerTimeoutKeys pins the per-server timeout knobs end to end:
// `init_timeout_secs` / `tool_timeout_secs` are read from the project file,
// survive Merge into the runtime config, and are re-emitted by --save-config
// (they are not secrets, so they persist without --save-config-secrets).
func TestMCPServerTimeoutKeys(t *testing.T) {
	os.Unsetenv("GOGEN_MCP_SERVERS")
	os.Unsetenv("GOGEN_MCP")

	body := "mcp: on\nmcp_servers:\n" +
		"  - name: rea\n    command: npx\n    args: [\"-y\", \"rea-agents\", \"mcp\"]\n" +
		"    init_timeout_secs: 120\n    tool_timeout_secs: 900\n"
	pf, err := ParseConfigFile("gogen.conf", body)
	if err != nil {
		t.Fatalf("ParseConfigFile: %v", err)
	}

	cfg := Merge(pf, FlagOverrides{})
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("MCPServers = %+v, want one entry", cfg.MCPServers)
	}
	got := cfg.MCPServers[0]
	if got.InitTimeoutSecs != 120 || got.ToolTimeoutSecs != 900 {
		t.Fatalf("timeouts = init %d / tool %d, want 120 / 900", got.InitTimeoutSecs, got.ToolTimeoutSecs)
	}

	// --save-config re-emits both keys; without secrets only env is stripped.
	dir := t.TempDir()
	path := filepath.Join(dir, "gogen.conf")
	if err := SaveConfig(path, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"init_timeout_secs: 120", "tool_timeout_secs: 900"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("saved config missing %q:\n%s", want, data)
		}
	}
}

// TestMergeMCPServersEnvCarriesTimeouts pins the JSON env form
// (GOGEN_MCP_SERVERS), which overrides the file: its entries must carry the
// same timeouts, otherwise a rea entry added through the env would silently
// fall back to the client defaults.
func TestMergeMCPServersEnvCarriesTimeouts(t *testing.T) {
	t.Setenv("GOGEN_MCP_SERVERS",
		`[{"name":"rea","command":"npx","args":["-y","rea-agents","mcp"],"tool_timeout_secs":600}]`)

	cfg := Merge(nil, FlagOverrides{})
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].ToolTimeoutSecs != 600 {
		t.Fatalf("env-form server = %+v, want tool_timeout_secs 600", cfg.MCPServers)
	}
}

// TestMCPServerTimeoutsDefaultToZero pins that an entry without the keys keeps
// the client defaults rather than baking a value into the config: 0 means
// "use the client default", matching every other unset numeric key.
func TestMCPServerTimeoutsDefaultToZero(t *testing.T) {
	os.Unsetenv("GOGEN_MCP_SERVERS")
	pf, err := ParseConfigFile("gogen.conf", "mcp_servers:\n  - name: rea\n    command: npx\n")
	if err != nil {
		t.Fatalf("ParseConfigFile: %v", err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if got := cfg.MCPServers[0]; got.InitTimeoutSecs != 0 || got.ToolTimeoutSecs != 0 {
		t.Fatalf("timeouts = %d / %d, want 0 / 0 (client defaults)", got.InitTimeoutSecs, got.ToolTimeoutSecs)
	}

	// And they stay out of a regenerated config: an unset key must not be
	// baked in as 0 (the file convention is "absent = default").
	path := filepath.Join(t.TempDir(), "gogen.conf")
	if err := SaveConfig(path, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "init_timeout_secs") || strings.Contains(string(data), "tool_timeout_secs") {
		t.Fatalf("saved config baked in an unset timeout:\n%s", data)
	}
}
