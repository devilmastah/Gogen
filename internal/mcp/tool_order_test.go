package mcp

import (
	"reflect"
	"testing"

	"gogen/internal/config"
	"gogen/internal/llm"
)

// TestNewManagerToolListIsRepeatable pins the property the provider prompt cache
// depends on: two managers built from the SAME config — with servers now
// started in parallel, so completion order varies — must produce byte-identical
// tool definitions in the same order. Anything derived from registration or
// completion order (map iteration, "first writer wins" with a timing-dependent
// first) would change the request prefix between runs and bust the cache.
func TestNewManagerToolListIsRepeatable(t *testing.T) {
	t.Setenv("GOGEN_MCP_TEST_HELPER", "1")
	base := helperCommand()
	alpha, beta := base, base
	alpha.Name, beta.Name = "server-a", "server-b"
	cfgs := []config.MCPServerConfig{beta, alpha} // reverse alphabetical on purpose

	first := toolListOf(t, cfgs)
	for i := 0; i < 3; i++ {
		if got := toolListOf(t, cfgs); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d produced a different tool list\nfirst: %v\ngot:   %v", i+2, names(first), names(got))
		}
	}
	// Sorted by external name: registration order must not leak into the wire
	// order, or a server list edit would reshuffle the whole prefix.
	for i := 1; i < len(first); i++ {
		if first[i-1].Name > first[i].Name {
			t.Fatalf("tool list not sorted: %q before %q", first[i-1].Name, first[i].Name)
		}
	}
}

// toolListOf builds a manager for cfgs and returns its definitions.
func toolListOf(t *testing.T, cfgs []config.MCPServerConfig) []llm.Tool {
	t.Helper()
	m, err := NewManager(cfgs)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defs := m.reg.Definitions()
	_ = m.Close()
	return defs
}

func names(tools []llm.Tool) []string {
	out := make([]string, len(tools))
	for i, tl := range tools {
		out[i] = tl.Name
	}
	return out
}

// TestRegistryDefinitionsSortedByRegistrationIndependence: Definitions() sorts
// by external name no matter what order the servers registered in — the reason
// parallel startup cannot perturb the wire order.
func TestRegistryDefinitionsSortedByRegistrationIndependence(t *testing.T) {
	reg := &Registry{tools: make(map[string]toolEntry)}
	reg.add("zeta", "tool", llm.Tool{Name: "tool", Description: "z"}, nil)
	reg.add("alpha", "tool", llm.Tool{Name: "tool", Description: "a"}, nil)
	reg.add("mid", "other", llm.Tool{Name: "other", Description: "m"}, nil)

	got := names(reg.Definitions())
	want := []string{"mcp_alpha_tool", "mcp_mid_other", "mcp_zeta_tool"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Definitions() order = %v, want %v", got, want)
	}
}
