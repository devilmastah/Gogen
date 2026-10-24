package projectfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/config"
)

// TestMCPToolModeDefault pins the default exposure mode: nested (the single
// `mcp` gateway tool), because a fixed tool surface keeps the request prefix —
// and therefore the provider's prompt cache — stable.
func TestMCPToolModeDefault(t *testing.T) {
	os.Unsetenv("GOGEN_MCP_TOOL_MODE")
	def := config.Defaults()
	if def.MCPToolMode != config.MCPToolModeNested {
		t.Fatalf("default mcp_tool_mode = %q, want %q", def.MCPToolMode, config.MCPToolModeNested)
	}
	cfg := Merge(nil, FlagOverrides{})
	if cfg.MCPToolMode != config.MCPToolModeNested || !cfg.MCPNested() {
		t.Fatalf("merged default = %q (nested=%v), want nested", cfg.MCPToolMode, cfg.MCPNested())
	}
}

// TestMCPToolModePrecedence pins env > file > default for the mode, and that an
// unknown value falls back to nested rather than breaking MCP entirely.
func TestMCPToolModePrecedence(t *testing.T) {
	t.Run("file value", func(t *testing.T) {
		os.Unsetenv("GOGEN_MCP_TOOL_MODE")
		pf, err := ParseConfigFile("gogen.conf", "mcp: on\nmcp_tool_mode: inline\n")
		if err != nil {
			t.Fatalf("ParseConfigFile: %v", err)
		}
		cfg := Merge(pf, FlagOverrides{})
		if cfg.MCPNested() {
			t.Fatalf("mcp_tool_mode: inline did not survive Merge (got %q)", cfg.MCPToolMode)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		t.Setenv("GOGEN_MCP_TOOL_MODE", config.MCPToolModeInline)
		pf, err := ParseConfigFile("gogen.conf", "mcp: on\nmcp_tool_mode: nested\n")
		if err != nil {
			t.Fatalf("ParseConfigFile: %v", err)
		}
		if cfg := Merge(pf, FlagOverrides{}); cfg.MCPNested() {
			t.Fatalf("env did not override the file value (got %q)", cfg.MCPToolMode)
		}
	})

	t.Run("unknown value falls back to nested", func(t *testing.T) {
		os.Unsetenv("GOGEN_MCP_TOOL_MODE")
		pf, err := ParseConfigFile("gogen.conf", "mcp_tool_mode: sideways\n")
		if err != nil {
			t.Fatalf("ParseConfigFile: %v", err)
		}
		if cfg := Merge(pf, FlagOverrides{}); !cfg.MCPNested() {
			t.Fatalf("unknown mode should fall back to nested, got %q", cfg.MCPToolMode)
		}
	})
}

// TestSaveConfigMCPToolMode pins --save-config round-tripping: an explicit
// non-default mode is written, and the default is omitted (never baked in).
func TestSaveConfigMCPToolMode(t *testing.T) {
	os.Unsetenv("GOGEN_MCP_TOOL_MODE")

	inline := Merge(nil, FlagOverrides{})
	inline.MCPToolMode = config.MCPToolModeInline
	path := filepath.Join(t.TempDir(), "inline.conf")
	if err := SaveConfig(path, "", inline, "", WriteOptions{}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "mcp_tool_mode: inline") {
		t.Fatalf("saved config missing the explicit mode:\n%s", data)
	}

	def := Merge(nil, FlagOverrides{})
	path = filepath.Join(t.TempDir(), "default.conf")
	if err := SaveConfig(path, "", def, "", WriteOptions{}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "mcp_tool_mode") {
		t.Fatalf("saved config baked in the default mode:\n%s", data)
	}
}
