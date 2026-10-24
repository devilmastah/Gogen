package config

import "strings"
import "testing"

func TestMCPEnabledOptIn(t *testing.T) {
	if ((*Config)(nil)).MCPEnabled() {
		t.Fatal("nil config should not enable MCP")
	}
	def := Defaults()
	if def.MCPEnabled() {
		t.Fatal("default MCP should be off")
	}
	for _, on := range []string{"on", "ON", "1", "true"} {
		c := Config{MCP: on}
		if !c.MCPEnabled() {
			t.Fatalf("MCP=%q should enable", on)
		}
	}
	for _, off := range []string{"", "off", "0", "false", "maybe"} {
		c := Config{MCP: off}
		if c.MCPEnabled() {
			t.Fatalf("MCP=%q should not enable", off)
		}
	}
}

func TestTreeSitterEnabled(t *testing.T) {
	if !((*Config)(nil)).TreeSitterEnabled() {
		t.Fatal("nil config should enable tree-sitter (default on)")
	}
	def := Defaults()
	if !def.TreeSitterEnabled() {
		t.Fatal("default tree-sitter should be on")
	}
	for _, on := range []string{"on", "ON", "1", "true", "", "random"} {
		c := Config{TreeSitter: on}
		if !c.TreeSitterEnabled() {
			t.Fatalf("TreeSitter=%q should enable (default on)", on)
		}
	}
	for _, off := range []string{"off", "OFF", "0", "false"} {
		c := Config{TreeSitter: off}
		if c.TreeSitterEnabled() {
			t.Fatalf("TreeSitter=%q should disable", off)
		}
	}
}

func TestWebFetchEnabled(t *testing.T) {
	if ((*Config)(nil)).WebFetchEnabled() {
		t.Fatal("nil config should not enable web fetch")
	}
	def := Defaults()
	if !def.WebFetchEnabled() {
		t.Fatal("default web fetch should be on")
	}
	for _, on := range []string{"on", "ON", "1", "true"} {
		c := Config{WebFetch: on}
		if !c.WebFetchEnabled() {
			t.Fatalf("WebFetch=%q should enable", on)
		}
	}
	for _, off := range []string{"", "off", "0", "false", "maybe"} {
		c := Config{WebFetch: off}
		if c.WebFetchEnabled() {
			t.Fatalf("WebFetch=%q should not enable", off)
		}
	}
}

func TestWebSearchEnabled(t *testing.T) {
	if ((*Config)(nil)).WebSearchEnabled() {
		t.Fatal("nil config should not enable web search")
	}
	def := Defaults()
	if !def.WebSearchEnabled() {
		t.Fatal("default web search should be on")
	}
	for _, on := range []string{"on", "ON", "1", "true"} {
		c := Config{WebSearch: on}
		if !c.WebSearchEnabled() {
			t.Fatalf("WebSearch=%q should enable", on)
		}
	}
	for _, off := range []string{"", "off", "0", "false", "maybe"} {
		c := Config{WebSearch: off}
		if c.WebSearchEnabled() {
			t.Fatalf("WebSearch=%q should not enable", off)
		}
	}
}

func TestOutputSpillEnabled(t *testing.T) {
	if !((*Config)(nil)).OutputSpillEnabled() {
		t.Fatal("nil config should enable output spill (default on)")
	}
	def := Defaults()
	if !def.OutputSpillEnabled() {
		t.Fatal("default output spill should be on")
	}
	for _, on := range []string{"on", "ON", "1", "true", "", "random"} {
		c := Config{OutputSpill: on}
		if !c.OutputSpillEnabled() {
			t.Fatalf("OutputSpill=%q should enable (default on)", on)
		}
	}
	for _, off := range []string{"off", "OFF", "0", "false"} {
		c := Config{OutputSpill: off}
		if c.OutputSpillEnabled() {
			t.Fatalf("OutputSpill=%q should disable", off)
		}
	}
}

func TestWebToolsEnabled(t *testing.T) {
	if ((*Config)(nil)).WebToolsEnabled() {
		t.Fatal("nil config should not enable web tools")
	}

	tests := []struct {
		name   string
		fetch  string
		search string
		want   bool
	}{
		{"both off", "off", "off", false},
		{"fetch on", "on", "off", true},
		{"search on", "off", "on", true},
		{"both on", "on", "on", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{WebFetch: tt.fetch, WebSearch: tt.search}
			got := c.WebToolsEnabled()
			if got != tt.want {
				t.Errorf("WebToolsEnabled(fetch=%q, search=%q) = %v, want %v", tt.fetch, tt.search, got, tt.want)
			}
		})
	}
}

func TestBoardEnabledOptIn(t *testing.T) {
	if ((*Config)(nil)).BoardEnabled() {
		t.Fatal("nil config should not enable board")
	}
	def := Defaults()
	if def.BoardEnabled() {
		t.Fatal("default board should be off")
	}
	for _, on := range []string{"on", "ON", "1", "true"} {
		c := Config{Board: on}
		if !c.BoardEnabled() {
			t.Fatalf("Board=%q should enable", on)
		}
	}
	for _, off := range []string{"", "off", "0", "false", "maybe"} {
		c := Config{Board: off}
		if c.BoardEnabled() {
			t.Fatalf("Board=%q should not enable", off)
		}
	}
}

func TestSubagentEnabledOptIn(t *testing.T) {
	if ((*Config)(nil)).SubagentEnabled() {
		t.Fatal("nil config should not enable subagents")
	}
	def := Defaults()
	if def.SubagentEnabled() {
		t.Fatal("default subagent should be off")
	}
	for _, on := range []string{"on", "ON", "1", "true"} {
		c := Config{Subagent: on}
		if !c.SubagentEnabled() {
			t.Fatalf("Subagent=%q should enable", on)
		}
	}
	for _, off := range []string{"", "off", "0", "false", "maybe"} {
		c := Config{Subagent: off}
		if c.SubagentEnabled() {
			t.Fatalf("Subagent=%q should not enable", off)
		}
	}
}

func TestSubagentDepth(t *testing.T) {
	if got := ((*Config)(nil)).SubagentDepth(); got != DefaultSubagentMaxDepth {
		t.Fatalf("nil config depth = %d, want default %d", got, DefaultSubagentMaxDepth)
	}
	zero := Config{}
	if got := zero.SubagentDepth(); got != DefaultSubagentMaxDepth {
		t.Fatalf("zero depth = %d, want default %d", got, DefaultSubagentMaxDepth)
	}
	neg := Config{SubagentMaxDepth: -3}
	if got := neg.SubagentDepth(); got != DefaultSubagentMaxDepth {
		t.Fatalf("negative depth = %d, want default %d", got, DefaultSubagentMaxDepth)
	}
	three := Config{SubagentMaxDepth: 3}
	if got := three.SubagentDepth(); got != 3 {
		t.Fatalf("depth 3 = %d, want 3", got)
	}
	def := Defaults()
	if got := def.SubagentDepth(); got != DefaultSubagentMaxDepth {
		t.Fatalf("defaults depth = %d, want %d", got, DefaultSubagentMaxDepth)
	}
}

func TestSubagentLimit(t *testing.T) {
	if got := ((*Config)(nil)).SubagentLimit(); got != DefaultSubagentMaxConcurrent {
		t.Fatalf("nil config limit = %d, want default %d", got, DefaultSubagentMaxConcurrent)
	}
	zero := Config{}
	if got := zero.SubagentLimit(); got != DefaultSubagentMaxConcurrent {
		t.Fatalf("zero limit = %d, want default %d", got, DefaultSubagentMaxConcurrent)
	}
	neg := Config{SubagentMaxConcurrent: -2}
	if got := neg.SubagentLimit(); got != DefaultSubagentMaxConcurrent {
		t.Fatalf("negative limit = %d, want default %d", got, DefaultSubagentMaxConcurrent)
	}
	two := Config{SubagentMaxConcurrent: 2}
	if got := two.SubagentLimit(); got != 2 {
		t.Fatalf("limit 2 = %d, want 2", got)
	}
	def := Defaults()
	if got := def.SubagentLimit(); got != DefaultSubagentMaxConcurrent {
		t.Fatalf("defaults limit = %d, want %d", got, DefaultSubagentMaxConcurrent)
	}
	if def.SubagentMaxConcurrent != 4 {
		t.Fatalf("default SubagentMaxConcurrent = %d, want 4", def.SubagentMaxConcurrent)
	}
}

func TestConfigDefaults(t *testing.T) {
	d := Defaults()
	if d.CompactThreshold != 0.85 {
		t.Errorf("default compact threshold = %f, want 0.85", d.CompactThreshold)
	}
	if d.MaxToolResultBytes != 262144 {
		t.Errorf("default max tool result bytes = %d, want 262144", d.MaxToolResultBytes)
	}
	if d.CommandIdleTimeoutSecs != 300 {
		t.Errorf("default command idle timeout = %d, want 300", d.CommandIdleTimeoutSecs)
	}
	if d.SessionMaxCount != 50 {
		t.Errorf("default session max count = %d, want 50", d.SessionMaxCount)
	}
	if d.SessionMaxAgeDays != 30 {
		t.Errorf("default session max age days = %d, want 30", d.SessionMaxAgeDays)
	}
	if d.CommandSafetyMode != "blocklist" {
		t.Errorf("default command safety mode = %q, want blocklist", d.CommandSafetyMode)
	}
	if d.DeleteApproval != "required" {
		t.Errorf("default delete approval = %q, want required", d.DeleteApproval)
	}
	if d.CommandApproval != "required" {
		t.Errorf("default command approval = %q, want required", d.CommandApproval)
	}
	if strings.TrimSpace(d.OpenAIModel) != "" {
		t.Errorf("default openai model should be empty, got %q", d.OpenAIModel)
	}
}

// WebToolsEnabled is test-only surface, moved out of config.go: production
// branches on WebFetchEnabled / WebSearchEnabled individually (network gating
// per tool), nothing reads the combined predicate.

// WebToolsEnabled reports whether either web tool may use the network.
func (c *Config) WebToolsEnabled() bool {
	return c.WebFetchEnabled() || c.WebSearchEnabled()
}
