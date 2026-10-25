package main

import (
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/mcp"
)

// mcpTestConfig returns a config that enables MCP with one valid server
// entry (the command is never executed: tests inject the constructor).
func mcpTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		MCP:        "on",
		MCPServers: []config.MCPServerConfig{{Name: "slow", Command: "true"}},
	}
}

// TestCloseMCPWhileInitStillRunning reproduces the race window from the
// MCP shutdown ticket: init outlives the bounded wait in closeMCP, so
// closeMCP times out and reads h.mgr while the init goroutine is still
// running; the goroutine then writes h.mgr afterwards. The write is not
// ordered with closeMCP's read (no synchronization edge between them),
// which -race flagged before h.mgr was guarded by the handle mutex.
func TestCloseMCPWhileInitStillRunning(t *testing.T) {
	// Constructor sleeps past the shrunken init wait, so its h.mgr
	// write lands after closeMCP has already read the field and
	// returned.
	origNew := newMCPManager
	t.Cleanup(func() { newMCPManager = origNew })
	newMCPManager = func(servers []config.MCPServerConfig) (*mcp.Manager, error) {
		time.Sleep(30 * time.Millisecond)
		return &mcp.Manager{}, nil
	}

	origWait := mcpInitWait
	t.Cleanup(func() { mcpInitWait = origWait })
	mcpInitWait = 10 * time.Millisecond

	// a is nil: the injected manager's registry is nil, so
	// SetMCPRegistry is never reached.
	h := startMCP(nil, mcpTestConfig(t))

	// closeMCP times out at 10ms and reads mgr (nil) while the
	// constructor is still sleeping; it must return cleanly even
	// though init never completed.
	closeMCP(h)

	// The constructor's write happens at ~30ms; wait for init to
	// finish so the goroutine cannot outlive the test.
	<-h.done
}

// TestCloseMCPAfterInitCompletes covers the fast path: init finished
// before closeMCP is called, so the wait is skipped and the manager
// is closed.
func TestCloseMCPAfterInitCompletes(t *testing.T) {
	origNew := newMCPManager
	t.Cleanup(func() { newMCPManager = origNew })
	newMCPManager = func(servers []config.MCPServerConfig) (*mcp.Manager, error) {
		return &mcp.Manager{}, nil
	}

	h := startMCP(nil, mcpTestConfig(t))
	<-h.done
	closeMCP(h) // must not block or panic
}

// TestStartMCPSkipsHostServersWhenOff pins the off switch in front of a
// host-appended server: the manager is not started, and the note is the
// off branch in startMCP.
func TestStartMCPSkipsHostServersWhenOff(t *testing.T) {
	origNew := newMCPManager
	t.Cleanup(func() { newMCPManager = origNew })
	newMCPManager = func(servers []config.MCPServerConfig) (*mcp.Manager, error) {
		t.Fatal("mcp off must not start host servers")
		return nil, nil
	}
	cfg := &config.Config{
		MCP:            "off",
		HostMCPServers: []config.MCPServerConfig{{Name: "vscode", Command: "node"}},
	}
	h := startMCP(nil, cfg)
	<-h.done
}

// TestStartMCPStartsHostServersWhenOn starts a host server when mcp is on
// even if the config file lists none, and tells the session the gateway is on.
func TestStartMCPStartsHostServersWhenOn(t *testing.T) {
	origNew := newMCPManager
	t.Cleanup(func() { newMCPManager = origNew })
	var got []config.MCPServerConfig
	newMCPManager = func(servers []config.MCPServerConfig) (*mcp.Manager, error) {
		got = append([]config.MCPServerConfig(nil), servers...)
		return &mcp.Manager{}, nil
	}
	a := &agent.Agent{}
	cfg := &config.Config{
		MCP:            "on",
		HostMCPServers: []config.MCPServerConfig{{Name: "vscode", Command: "node"}},
	}
	h := startMCP(a, cfg)
	<-h.done
	if !a.MCPConfigured() {
		t.Fatal("a host server with mcp on should expose the gateway")
	}
	if len(got) != 1 || got[0].Name != "vscode" {
		t.Fatalf("started servers = %+v, want vscode", got)
	}
}

// TestStartMCPSharesRegistryBeforeHandshake pins the web-session bug: the
// workspace copies a.MCPRegistry when the server is built, which is before
// the handshake goroutine returns. The shared registry has to be installed
// synchronously so that copy, and every later session, sees Publish.
func TestStartMCPSharesRegistryBeforeHandshake(t *testing.T) {
	origNew := newMCPManager
	t.Cleanup(func() { newMCPManager = origNew })
	started := make(chan struct{})
	release := make(chan struct{})
	newMCPManager = func(servers []config.MCPServerConfig) (*mcp.Manager, error) {
		close(started)
		<-release
		return &mcp.Manager{}, nil
	}

	a := &agent.Agent{}
	h := startMCP(a, mcpTestConfig(t))
	if a.MCPRegistry == nil {
		t.Fatal("registry must be installed before the handshake finishes")
	}
	if _, ok := a.MCPRegistry.(*agent.PendingMCPRegistry); !ok {
		t.Fatalf("registry type = %T, want *agent.PendingMCPRegistry", a.MCPRegistry)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handshake constructor was not started")
	}
	close(release)
	<-h.done
}
