package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/projectfile"
)

// TestProjectAndAgentSettingsLive drives the settings that used to be
// config-file-only through the runtime-config WS channel: the workspace
// profile commands (test_command / lint_command), the two remaining live
// agent feature flags (agent_instructions / job_notices) and the MCP
// exposure mode (mcp_tool_mode).
//
// It pins the three things each of them needs: validation + the config
// echo, the LIVE target (each session agent re-detects its project profile,
// re-renders its instruction section, and the workspace follows the
// job-notice flag), and persistence (the next start keeps the value).
func TestProjectAndAgentSettingsLive(t *testing.T) {
	dir := t.TempDir()
	// agent_instructions renders the workspace instruction files from the
	// CURRENT working dir; the sweep must pick this up when the flag flips.
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("PROJECT-INSTRUCTION-MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	msg := WSMessage{
		Type: "config",
		ConfigFields: []string{
			"testCommand", "lintCommand", "agentInstructions", "jobNotices", "mcpToolMode",
		},
		TestCommand:       "make test",
		LintCommand:       "make vet",
		AgentInstructions: "on",
		JobNotices:        "on",
		MCPToolMode:       "inline",
	}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("send config: %v", err)
	}

	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return len(m.ConfigFields) == 0 && m.MCPToolMode == "inline"
	})
	if cfg.TestCommand != "make test" || cfg.LintCommand != "make vet" {
		t.Fatalf("config echo commands = %q/%q, want make test / make vet", cfg.TestCommand, cfg.LintCommand)
	}
	if cfg.AgentInstructions != "on" || cfg.JobNotices != "on" {
		t.Fatalf("config echo flags: agentInstructions=%q jobNotices=%q, want on/on", cfg.AgentInstructions, cfg.JobNotices)
	}

	// Live targets: the session agent owns the commands (its cached project
	// profile is dropped so the next turn re-detects with them), the
	// instruction section is re-rendered, and the job-notice flag reached
	// the workspace.
	if a.TestCommand != "make test" || a.LintCommand != "make vet" {
		t.Fatalf("agent commands = %q/%q, want make test / make vet", a.TestCommand, a.LintCommand)
	}
	if !a.InstructionsEnabled() {
		t.Fatal("agent_instructions did not reach the session agent")
	}
	if got := a.EffectiveGuidelines(); !strings.Contains(got, "PROJECT-INSTRUCTION-MARKER") {
		t.Fatalf("agent guidelines missing the workspace instructions:\n%s", got)
	}
	if !s.ws.GetJobNotices() {
		t.Fatal("job_notices did not reach the workspace")
	}

	// The runtime overlay is the source of truth for the push, new sessions,
	// and persistence.
	if r := s.ws.GetRuntimeConfig(); r.TestCommand != "make test" || r.LintCommand != "make vet" ||
		!r.AgentInstructionsEnabled() || !r.JobNoticesEnabled() {
		t.Fatalf("runtime overlay not updated: %+v", r)
	}

	// Persisted for the next start.
	data, err := os.ReadFile(projectfile.DefaultSavePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"test_command: make test", "lint_command: make vet",
		`agent_instructions: "on"`, `job_notices: "on"`, "mcp_tool_mode: inline",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected %q in persisted config:\n%s", want, data)
		}
	}

	// Clearing the commands back to auto-detection and turning the flags off
	// must undo the live state too (explicit empty values are legal because
	// the fields are named in ConfigFields).
	clear := WSMessage{
		Type:              "config",
		ConfigFields:      []string{"testCommand", "lintCommand", "agentInstructions", "jobNotices", "mcpToolMode"},
		AgentInstructions: "off",
		JobNotices:        "off",
		MCPToolMode:       "nested",
	}
	if err := conn.WriteJSON(clear); err != nil {
		t.Fatalf("send clear: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return len(m.ConfigFields) == 0 && m.MCPToolMode == "nested" && m.JobNotices == "off"
	})
	if a.TestCommand != "" || a.LintCommand != "" {
		t.Fatalf("cleared commands = %q/%q, want empty (auto-detect)", a.TestCommand, a.LintCommand)
	}
	if a.InstructionsEnabled() {
		t.Fatal("agent_instructions off did not reach the session agent")
	}
	if got := a.EffectiveGuidelines(); strings.Contains(got, "PROJECT-INSTRUCTION-MARKER") {
		t.Fatalf("instructions still rendered after disabling:\n%s", got)
	}
	if s.ws.GetJobNotices() {
		t.Fatal("job_notices off did not reach the workspace")
	}
}

// TestNewSessionSeedsLiveSettings verifies a session created AFTER a
// settings change inherits it: the workspace seeds the project commands and
// the MCP exposure mode from the LIVE runtime overlay (not the immutable
// startup config) and the optional-tool flags from the shared store.
func TestNewSessionSeedsLiveSettings(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	if err := conn.WriteJSON(WSMessage{
		Type: "config",
		// skills is a FEATURE flag (a top-level config-message field), not a
		// ConfigFields runtime option — naming it there would be rejected as
		// an unknown option and drop the whole request.
		ConfigFields:      []string{"testCommand", "lintCommand", "agentInstructions", "mcpToolMode"},
		TestCommand:       "go test ./pkg/...",
		LintCommand:       "go vet ./pkg/...",
		AgentInstructions: "on",
		MCPToolMode:       "inline",
		Skills:            "on",
	}); err != nil {
		t.Fatalf("send config: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return len(m.ConfigFields) == 0 && m.Skills == "on" && m.MCPToolMode == "inline"
	})

	child := s.ws.NewSessionAgent(nil, "child-session")
	if child.TestCommand != "go test ./pkg/..." || child.LintCommand != "go vet ./pkg/..." {
		t.Fatalf("new session commands = %q/%q, want the configured overrides", child.TestCommand, child.LintCommand)
	}
	if !child.InstructionsEnabled() {
		t.Fatal("new session did not inherit agent_instructions")
	}
	if child.MCPNested() {
		t.Fatal("new session did not inherit mcp_tool_mode: inline")
	}
	if !child.SkillsEnabled() {
		t.Fatal("new session did not read the shared skills flag")
	}
}
