package config

import "testing"

func TestLoadHostMCPServers(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv(HostMCPServersEnv, "")
		if got := LoadHostMCPServers(); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		t.Setenv(HostMCPServersEnv, "{")
		if got := LoadHostMCPServers(); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
	t.Run("one server", func(t *testing.T) {
		t.Setenv(HostMCPServersEnv, `[{"name":"vscode","command":"node","args":["bridge.js"]}]`)
		got := LoadHostMCPServers()
		if len(got) != 1 || got[0].Name != "vscode" || got[0].Command != "node" || len(got[0].Args) != 1 {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestMergeHostMCPServersSkipsTakenName(t *testing.T) {
	configured := []MCPServerConfig{{Name: "vscode", Command: "user"}}
	extra := []MCPServerConfig{
		{Name: "vscode", Command: "host"},
		{Name: "other", Command: "host"},
	}
	got := MergeHostMCPServers(configured, extra)
	if len(got) != 1 || got[0].Name != "other" || got[0].Command != "host" {
		t.Fatalf("got %+v, want the host server whose name is free", got)
	}
}
