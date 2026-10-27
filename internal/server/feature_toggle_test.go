package server

import (
	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/projectfile"
	"gogen/internal/session"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFeatureToggleViaConfigWS drives the live board/subagent toggles through
// the existing config WS message in PROJECT mode (no global-mode gate): the
// workspace + session-agent flags flip, a config push reaches attached
// clients, and the effective config is persisted so the toggle survives a
// restart.
func TestFeatureToggleViaConfigWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

	if cfg.Board != "off" || cfg.Subagent != "off" {
		t.Fatalf("initial config push flags: board=%q subagent=%q, want off/off", cfg.Board, cfg.Subagent)
	}

	// Toggle board on (project mode — must NOT hit the working-dir gate).
	if err := conn.WriteJSON(WSMessage{Type: "config", Board: "on", SessionID: sid}); err != nil {
		t.Fatalf("send board on: %v", err)
	}
	boardCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.Board == "on" })
	if boardCfg.Subagent != "off" {
		t.Fatalf("board toggle should not touch subagent: %q", boardCfg.Subagent)
	}

	// Workspace + agent flags flipped (sweep runs in a goroutine).
	waitForCond(t, 5*time.Second, func() bool { return s.ws.GetBoardEnabled() && a.BoardEnabled() })

	// Persisted for the next start. yaml.v3 emits on/off values quoted
	// (`board: "on"`), which parses identically to the unquoted spelling.
	cfgPath := filepath.Join(dir, ".gogen", "gogen.conf")
	waitForCond(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return false
		}
		return strings.Contains(string(data), "board: on") || strings.Contains(string(data), `board: "on"`)
	})

	// Toggle back off; the config push reflects it.
	if err := conn.WriteJSON(WSMessage{Type: "config", Board: "off", SessionID: sid}); err != nil {
		t.Fatalf("send board off: %v", err)
	}
	offCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.Board == "off" })
	if offCfg.SessionID != sid {
		t.Fatalf("config push session id = %q, want %q", offCfg.SessionID, sid)
	}
	waitForCond(t, 5*time.Second, func() bool { return !s.ws.GetBoardEnabled() && !a.BoardEnabled() })
}

// TestFeatureToggleSubagentAndDepth verifies the subagent flag and the
// nesting-depth limit ride the same config message.
func TestFeatureToggleSubagentAndDepth(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

	if err := conn.WriteJSON(WSMessage{Type: "config", Subagent: "on", SubagentMaxDepth: 3, SubagentMaxConcurrent: 2, SessionID: sid}); err != nil {
		t.Fatalf("send subagent on: %v", err)
	}
	subCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.Subagent == "on" })
	if subCfg.SubagentMaxDepth != 3 {
		t.Fatalf("config push depth = %d, want 3", subCfg.SubagentMaxDepth)
	}
	if subCfg.SubagentMaxConcurrent != 2 {
		t.Fatalf("config push concurrent limit = %d, want 2", subCfg.SubagentMaxConcurrent)
	}
	waitForCond(t, 5*time.Second, func() bool {
		return s.ws.GetSubagentEnabled() && a.SubagentsEnabled() && a.SubagentMaxDepth() == 3 &&
			a.SubagentMaxConcurrent() == 2 && s.ws.GetSubagentMaxConcurrent() == 2
	})
}

// TestFeatureToggleSkillsAndTerminal verifies the optional-tool flags (the
// skill tool; the persistent-terminal feature tools) ride the same config
// message as the board/subagent flags: they apply live to the shared flag
// store every session agent reads, and they persist for the next start.
func TestFeatureToggleSkillsAndTerminal(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

	if cfg.Skills != "off" || cfg.Terminal != "off" {
		t.Fatalf("initial config push flags: skills=%q terminal=%q, want off/off", cfg.Skills, cfg.Terminal)
	}

	if err := conn.WriteJSON(WSMessage{Type: "config", Skills: "on", Terminal: "on", SessionID: sid}); err != nil {
		t.Fatalf("send skills/terminal on: %v", err)
	}
	onCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.Skills == "on" && m.Terminal == "on"
	})
	if onCfg.Board != "off" || onCfg.Subagent != "off" {
		t.Fatalf("skills/terminal toggle must not touch other flags: board=%q subagent=%q", onCfg.Board, onCfg.Subagent)
	}
	// The shared store is what per-turn tool derivation reads: the initial
	// agent must see the change with no sweep, and so must a later session.
	waitForCond(t, 5*time.Second, func() bool {
		return s.ws.GetSkillsEnabled() && s.ws.GetTerminalEnabled() &&
			a.SkillsEnabled() && a.TerminalEnabled()
	})

	// Persisted for the next start (both keys are written back).
	cfgPath := filepath.Join(dir, ".gogen", "gogen.conf")
	waitForCond(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return false
		}
		// yaml.v3 quotes on/off values (`skills: "on"`), which parses
		// identically to the unquoted spelling.
		s := string(data)
		return (strings.Contains(s, "skills: on") || strings.Contains(s, `skills: "on"`)) &&
			(strings.Contains(s, "terminal: on") || strings.Contains(s, `terminal: "on"`))
	})

	if err := conn.WriteJSON(WSMessage{Type: "config", Skills: "off", Terminal: "off", SessionID: sid}); err != nil {
		t.Fatalf("send skills/terminal off: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.Skills == "off" && m.Terminal == "off"
	})
	waitForCond(t, 5*time.Second, func() bool {
		return !s.ws.GetSkillsEnabled() && !s.ws.GetTerminalEnabled() && !a.SkillsEnabled() && !a.TerminalEnabled()
	})
}

// TestFeatureToggleInvalidValueRejected verifies invalid on/off spellings
// and negative depths are rejected without changing any state.
func TestFeatureToggleInvalidValueRejected(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID

	if err := conn.WriteJSON(WSMessage{Type: "config", Board: "maybe", SessionID: sid}); err != nil {
		t.Fatalf("send invalid board: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "settings" || resp.Success || !strings.Contains(resp.Content, "invalid board value") {
		t.Fatalf("invalid board notice = %+v", resp)
	}
	if s.ws.GetBoardEnabled() {
		t.Fatal("invalid board value must not enable the flag")
	}

	if err := conn.WriteJSON(WSMessage{Type: "config", SubagentMaxDepth: -1, SessionID: sid}); err != nil {
		t.Fatalf("send negative depth: %v", err)
	}
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "settings" || resp.Success || !strings.Contains(resp.Content, "subagentMaxDepth") {
		t.Fatalf("negative depth notice = %+v", resp)
	}

	if err := conn.WriteJSON(WSMessage{Type: "config", SubagentMaxConcurrent: -1, SessionID: sid}); err != nil {
		t.Fatalf("send negative concurrent limit: %v", err)
	}
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "settings" || resp.Success || !strings.Contains(resp.Content, "subagentMaxConcurrent") {
		t.Fatalf("negative concurrent limit notice = %+v", resp)
	}
}

// TestConfigWorkingDirGatePreserved verifies the restructure did not loosen
// the working-dir branch: in project mode a config message carrying a new
// working dir is still rejected with the historical error.
func TestConfigWorkingDirGatePreserved(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID

	if err := conn.WriteJSON(WSMessage{Type: "config", WorkingDir: t.TempDir(), SessionID: sid}); err != nil {
		t.Fatalf("send working dir: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "workspace" || !strings.Contains(resp.Content, "only allowed in global mode") {
		t.Fatalf("unexpected notice: %+v", resp)
	}
}

// TestFeatureTogglePersistPreservesSecrets verifies the live-toggle persist
// never drops secrets that are already stored in the config file: a rewrite
// with IncludeSecrets=false would silently remove openai_api_key / MCP envs
// and break the next start.
func TestFeatureTogglePersistPreservesSecrets(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".gogen")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "gogen.conf")
	seed := "openai_api_key: sk-secret-123\nopenai_model: test-model\n"
	if err := os.WriteFile(cfgPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	// The in-memory effective config carries the same key the file has (a
	// real toggle uses s.config with the loaded key).
	s.config = &config.Config{OpenAIKey: "sk-secret-123", Board: "off", Subagent: "off"}
	s.persistConfig(s.config)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sk-secret-123") {
		t.Fatalf("persist dropped the stored api key:\n%s", data)
	}
}

// TestFeatureTogglePersistKeepsEnvSecretsOut verifies a key that only ever
// came from the environment stays out of the config file (IncludeSecrets
// stays false when the file has no secrets).
func TestFeatureTogglePersistKeepsEnvSecretsOut(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	s.config = &config.Config{OpenAIKey: "sk-env-only", Board: "on"}
	s.persistConfig(s.config)
	data, err := os.ReadFile(filepath.Join(dir, ".gogen", "gogen.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-env-only") {
		t.Fatalf("env-only key must not be persisted:\n%s", data)
	}
}

// TestFeatureTogglePersistPreservesSecretsGlobalMode covers the global-mode
// branch: the global config is PLAIN YAML (config.yaml, no front matter), so
// the secrets check must parse it as pure YAML — the front-matter loader
// alone would report no secrets and the toggle would drop the stored key.
func TestFeatureTogglePersistPreservesSecretsGlobalMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(projectfile.GlobalConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := "openai_api_key: sk-global-secret\nopenai_model: test-model\n"
	if err := os.WriteFile(projectfile.GlobalConfigPath(), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	a.GlobalMode = true
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := newTestServer(t, a, &config.Config{OpenAIKey: "sk-global-secret"})
	s.persistConfig(s.config)
	data, err := os.ReadFile(projectfile.GlobalConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sk-global-secret") {
		t.Fatalf("global persist dropped the stored api key:\n%s", data)
	}
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
