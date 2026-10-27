package server

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/projectfile"
	"gogen/internal/session"
)

// TestMCPCRUDViaWS drives the MCP server-list editor (mcp_save /
// mcp_delete) through a real WebSocket: the entry lands in the runtime
// overlay, the list is pushed back to every tab (env values never leave the
// server), the config file persists it (secrets forced, since env can hold
// them), a re-save without retyping keeps the stored command/env, host
// servers are refused, and delete removes the row.
func TestMCPCRUDViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	// A host server (GOGEN_MCP_EXTRA) is seeded at CONSTRUCTION: the config
	// is immutable after NewServer, so a test must not assign it later (the
	// config push goroutines read it).
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := newTestServer(t, a, &config.Config{
		HostMCPServers: []config.MCPServerConfig{{Name: "host-only", Command: "x"}},
	})
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	// ── Add a new server ──
	if err := conn.WriteJSON(WSMessage{Type: "mcp_save", MCPOp: &MCPOpRequest{
		Name:    "fetch",
		Command: "npx",
		Args:    []string{"-y", "server-fetch", " "}, // blank arg is dropped
		Env:     map[string]string{"TOKEN": "s3cret"},
	}}); err != nil {
		t.Fatalf("send mcp_save: %v", err)
	}
	notice := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !notice.Success || !strings.Contains(notice.Content, "restart") {
		t.Fatalf("save notice = %+v, want a restart success notice", notice)
	}
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return len(m.MCPServers) > 0 })
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("mcpServers push = %+v, want one row", cfg.MCPServers)
	}
	row := cfg.MCPServers[0]
	if row.Name != "fetch" || row.Command != "npx" || len(row.Args) != 2 || !row.EnvSet || row.Host {
		t.Fatalf("pushed row = %+v, want fetch/npx with 2 args and envSet", row)
	}
	// The stored entry keeps the env values and the cleaned arg list.
	r := s.ws.GetRuntimeConfig()
	if len(r.MCPServers) != 1 || r.MCPServers[0].Env["TOKEN"] != "s3cret" || len(r.MCPServers[0].Args) != 2 {
		t.Fatalf("overlay servers = %+v, want the saved entry", r.MCPServers)
	}

	// Persisted for the next start (env is a secret → forced 0600 write).
	data, err := os.ReadFile(projectfile.DefaultSavePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: fetch") || !strings.Contains(string(data), "TOKEN: s3cret") {
		t.Fatalf("persisted config missing the MCP server:\n%s", data)
	}
	// 0600 is POSIX-only: Windows does not honor POSIX mode bits (Stat
	// reports 0666/0777 for any writable file), so the forced-secret write is
	// pinned on the content above and the mode only where it is meaningful.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(projectfile.DefaultSavePath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("config perms = %o, want 600", perm)
		}
	}

	// ── Re-save without the command/env keeps the stored values ──
	if err := conn.WriteJSON(WSMessage{Type: "mcp_save", MCPOp: &MCPOpRequest{Name: "fetch"}}); err != nil {
		t.Fatalf("send mcp_save (keep): %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	r = s.ws.GetRuntimeConfig()
	if len(r.MCPServers) != 1 || r.MCPServers[0].Command != "npx" || r.MCPServers[0].Env["TOKEN"] != "s3cret" {
		t.Fatalf("re-save wiped the stored values: %+v", r.MCPServers)
	}

	// ── A new entry requires a command ──
	if err := conn.WriteJSON(WSMessage{Type: "mcp_save", MCPOp: &MCPOpRequest{Name: "broken"}}); err != nil {
		t.Fatalf("send mcp_save (no command): %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Success || !strings.Contains(resp.Content, "command is required") {
		t.Fatalf("missing-command notice = %+v", resp)
	}

	// ── Host servers (this process) cannot be edited or deleted ──
	if err := conn.WriteJSON(WSMessage{Type: "mcp_save", MCPOp: &MCPOpRequest{Name: "host-only", Command: "y"}}); err != nil {
		t.Fatalf("send host mcp_save: %v", err)
	}
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Success || !strings.Contains(resp.Content, "GOGEN_MCP_EXTRA") {
		t.Fatalf("host save notice = %+v", resp)
	}
	if err := conn.WriteJSON(WSMessage{Type: "mcp_delete", MCPOp: &MCPOpRequest{Name: "host-only"}}); err != nil {
		t.Fatalf("send host mcp_delete: %v", err)
	}
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Success || !strings.Contains(resp.Content, "GOGEN_MCP_EXTRA") {
		t.Fatalf("host delete notice = %+v", resp)
	}

	// ── Unknown name on delete is an error, the list is untouched ──
	if err := conn.WriteJSON(WSMessage{Type: "mcp_delete", MCPOp: &MCPOpRequest{Name: "nope"}}); err != nil {
		t.Fatalf("send mcp_delete (unknown): %v", err)
	}
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Success || !strings.Contains(resp.Content, "not configured") {
		t.Fatalf("unknown delete notice = %+v", resp)
	}
	if len(s.ws.GetRuntimeConfig().MCPServers) != 1 {
		t.Fatal("unknown delete must not change the list")
	}

	// ── Delete the saved server ──
	if err := conn.WriteJSON(WSMessage{Type: "mcp_delete", MCPOp: &MCPOpRequest{Name: "fetch"}}); err != nil {
		t.Fatalf("send mcp_delete: %v", err)
	}
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if !resp.Success || !strings.Contains(resp.Content, "restart") {
		t.Fatalf("delete notice = %+v", resp)
	}
	if got := s.ws.GetRuntimeConfig().MCPServers; len(got) != 0 {
		t.Fatalf("overlay still has servers after delete: %+v", got)
	}
	// The persisted file no longer carries the row (the write runs off the
	// read loop, after the notice — poll for it).
	waitForCond(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(projectfile.DefaultSavePath(dir))
		return err == nil && !strings.Contains(string(data), "name: fetch")
	})
}
