package projectfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/config"
)

// configEnvKeys are the GOGEN_* variables whose merge would override the
// file values asserted below; they are unset so the tests are hermetic
// regardless of the developer's shell environment.
var configEnvKeys = []string{
	"GOGEN_BOARD", "GOGEN_SUBAGENT", "GOGEN_SUBAGENT_MAX_DEPTH", "GOGEN_SUBAGENT_MAX_CONCURRENT",
	"GOGEN_COMMAND_SANDBOX", "GOGEN_COMMAND_IDLE_TIMEOUT_SECS", "GOGEN_COMMAND_TIMEOUT_SECS",
	"GOGEN_SESSION_MAX_COUNT", "GOGEN_SESSION_MAX_AGE_DAYS",
	"GOGEN_WEB_MAX_ACTIVE_SESSIONS", "GOGEN_WEB_APPROVAL_HOLD_SECS",
	"GOGEN_WEB_BIND", "GOGEN_AGENT_INSTRUCTIONS", "GOGEN_SKILLS", "GOGEN_JOB_NOTICES",
}

func unsetConfigEnvs(t *testing.T) {
	t.Helper()
	for _, k := range configEnvKeys {
		os.Unsetenv(k)
	}
}

// TestMergeWebBindPrecedence pins web_bind's merge order — env > CLI flag >
// file > default (the documented precedence) — now that the loader accepts
// the file key, so the settings modal's restart-staged value takes effect.
func TestMergeWebBindPrecedence(t *testing.T) {
	os.Unsetenv("GOGEN_WEB_BIND")
	def := config.Defaults().WebBind
	if got := Merge(nil, FlagOverrides{}).WebBind; got != def {
		t.Fatalf("default web_bind = %q, want %q", got, def)
	}
	pf := &ProjectFile{Config: FileConfig{WebBind: "0.0.0.0:9090"}}
	if got := Merge(pf, FlagOverrides{}).WebBind; got != "0.0.0.0:9090" {
		t.Fatalf("file web_bind = %q, want 0.0.0.0:9090", got)
	}
	// Env overrides file.
	t.Setenv("GOGEN_WEB_BIND", "127.0.0.1:9999")
	if got := Merge(pf, FlagOverrides{}).WebBind; got != "127.0.0.1:9999" {
		t.Fatalf("env web_bind = %q, want 127.0.0.1:9999", got)
	}
	// CLI flag overrides file when env is unset (merge applies the file
	// value, then the flag override applies on top).
	os.Unsetenv("GOGEN_WEB_BIND")
	if got := Merge(pf, FlagOverrides{WebBind: "0.0.0.0:8080"}).WebBind; got != "0.0.0.0:8080" {
		t.Fatalf("flag web_bind = %q, want 0.0.0.0:8080", got)
	}
}

// TestWebListenEnvIsNotSaved pins the sidebar's free port: GOGEN_WEB_LISTEN
// selects the listener for this process and stays out of the merged config,
// so a later save cannot store it as web_bind.
func TestWebListenEnvIsNotSaved(t *testing.T) {
	os.Unsetenv("GOGEN_WEB_BIND")
	t.Setenv(config.WebListenEnv, "127.0.0.1:53244")
	pf := &ProjectFile{Config: FileConfig{WebBind: "0.0.0.0:9090"}}
	if got := Merge(pf, FlagOverrides{}).WebBind; got != "0.0.0.0:9090" {
		t.Fatalf("file web_bind = %q, want 0.0.0.0:9090 with %s set", got, config.WebListenEnv)
	}
	if got := Merge(nil, FlagOverrides{}).WebBind; got != config.Defaults().WebBind {
		t.Fatalf("default web_bind = %q, want %q with %s set", got, config.Defaults().WebBind, config.WebListenEnv)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gogen.conf")
	cfg := Merge(nil, FlagOverrides{})
	if err := SaveConfig(path, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "53244") || strings.Contains(string(data), "web_bind") {
		t.Fatalf("saved config contains the process listen address:\n%s", data)
	}
}

// TestSaveConfigOmitsDefaults verifies that options added by the
// runtime-config / feature-flag work are NOT written when they equal the
// built-in default: a config file must never bake in a default, so future
// default changes reach users who did not customize without them editing
// anything (the same rule preserve_reasoning already follows). Reload of an
// omitted key resolves to the same default via the merge path, so the
// round-trip is transparent.
func TestSaveConfigOmitsDefaults(t *testing.T) {
	unsetConfigEnvs(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gogen.conf")
	cfg := config.Defaults()
	if err := SaveConfig(path, "", &cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, key := range []string{
		"board:", "subagent:", "subagent_max_depth:", "subagent_max_concurrent:",
		"subagent_thinking_level:",
		"command_sandbox:", "command_idle_timeout_secs:", "session_max_count:",
		"session_max_age_days:", "web_max_active_sessions:",
		"web_approval_hold_secs:", "web_bind:",
	} {
		if strings.Contains(body, key) {
			t.Fatalf("default config saved key %q:\n%s", key, body)
		}
	}
	// The omitted keys must round-trip to the same effective values.
	pf, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(pf, FlagOverrides{})
	def := config.Defaults()
	if merged.CommandSandbox != def.CommandSandbox ||
		merged.CommandIdleTimeoutSecs != def.CommandIdleTimeoutSecs ||
		merged.Board != def.Board || merged.Subagent != def.Subagent ||
		merged.SubagentDepth() != config.DefaultSubagentMaxDepth ||
		merged.SubagentLimit() != config.DefaultSubagentMaxConcurrent ||
		merged.SessionMaxCount != def.SessionMaxCount || merged.SessionMaxAgeDays != def.SessionMaxAgeDays ||
		merged.WebMaxActiveSessions != def.WebMaxActiveSessions || merged.WebBind != def.WebBind {
		t.Fatalf("round-trip lost defaults: %+v", merged)
	}
}

// TestSaveConfigOmitsExplicitDefaults pins the chosen semantic: a value
// that EQUALS the current default is treated as "the default" (the file
// format cannot express the distinction anyway) and the key is omitted, so
// a future default change propagates to the user instead of being pinned by
// a stale explicit value.
func TestSaveConfigOmitsExplicitDefaults(t *testing.T) {
	unsetConfigEnvs(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gogen.conf")
	cfg := config.Defaults()
	cfg.Board = "off"
	cfg.Subagent = "off"
	cfg.SubagentMaxDepth = config.DefaultSubagentMaxDepth
	cfg.SubagentMaxConcurrent = config.DefaultSubagentMaxConcurrent
	cfg.CommandSandbox = "off"
	cfg.CommandIdleTimeoutSecs = config.DefaultCommandIdleTimeoutSecs
	cfg.SessionMaxCount = config.DefaultSessionMaxCount
	cfg.SessionMaxAgeDays = config.DefaultSessionMaxAgeDays
	cfg.WebMaxActiveSessions = config.DefaultWebMaxActiveSessions
	cfg.WebBind = "127.0.0.1:8081"
	if err := SaveConfig(path, "", &cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"board:", "subagent:", "subagent_max_depth:", "subagent_max_concurrent:",
		"command_sandbox:", "command_idle_timeout_secs:", "session_max_count:",
		"session_max_age_days:", "web_max_active_sessions:", "web_bind:",
	} {
		if strings.Contains(string(data), key) {
			t.Fatalf("explicit-default value saved key %q:\n%s", key, data)
		}
	}
}

// TestSaveConfigWritesNonDefaults verifies explicitly customized values are
// written and survive a round-trip, including the -1 "keep sessions
// forever" sentinel for session_max_age_days (which must never be omitted
// as if it were the default).
func TestSaveConfigWritesNonDefaults(t *testing.T) {
	unsetConfigEnvs(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gogen.conf")
	cfg := config.Defaults()
	cfg.Board = "on"
	cfg.Subagent = "on"
	cfg.Automations = "on"
	cfg.SubagentMaxDepth = 3
	cfg.SubagentMaxConcurrent = 5
	cfg.CommandSandbox = "bwrap"
	cfg.CommandIdleTimeoutSecs = 900
	cfg.SessionMaxCount = 60
	cfg.SessionMaxAgeDays = -1
	cfg.WebMaxActiveSessions = 4
	cfg.WebApprovalHoldSecs = 5
	cfg.WebBind = "0.0.0.0:9090"
	if err := SaveConfig(path, "", &cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`board: "on"`, `subagent: "on"`, `automations: "on"`, "subagent_max_depth: 3",
		"subagent_max_concurrent: 5",
		"command_sandbox: bwrap", "command_idle_timeout_secs: 900",
		"session_max_count: 60", "session_max_age_days: -1",
		"web_max_active_sessions: 4", "web_approval_hold_secs: 5",
		"web_bind: 0.0.0.0:9090",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("saved config missing %q:\n%s", want, data)
		}
	}
	pf, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(pf, FlagOverrides{})
	if merged.Board != "on" || merged.Subagent != "on" || !merged.AutomationsEnabled() || merged.SubagentDepth() != 3 ||
		merged.SubagentLimit() != 5 ||
		merged.CommandSandbox != "bwrap" || merged.CommandIdleTimeoutSecs != 900 ||
		merged.SessionMaxCount != 60 || merged.SessionMaxAgeDays != -1 ||
		merged.WebMaxActiveSessions != 4 || merged.WebApprovalHoldSecs != 5 ||
		merged.WebBind != "0.0.0.0:9090" {
		t.Fatalf("round-trip lost custom values: %+v", merged)
	}
}

// TestSaveConfigNeverWritesLegacyCommandTimeoutKey pins that --save-config
// regeneration emits only the renamed key: a config whose effective idle
// timeout came from the legacy command_timeout_secs spelling must be
// rewritten under command_idle_timeout_secs, never under the old key.
func TestSaveConfigNeverWritesLegacyCommandTimeoutKey(t *testing.T) {
	unsetConfigEnvs(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gogen.conf")

	// Start from a legacy-spelled file, round-trip it through the effective
	// config, and re-save.
	legacy := "command_timeout_secs: 900\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if pf.Config.CommandIdleTimeoutSecs != 900 {
		t.Fatalf("legacy key not aliased before save: %d, want 900", pf.Config.CommandIdleTimeoutSecs)
	}
	cfg := Merge(pf, FlagOverrides{})
	if err := SaveConfig(path, "", cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "command_idle_timeout_secs: 900") {
		t.Fatalf("saved config missing renamed key:\n%s", body)
	}
	if strings.Contains(body, "command_timeout_secs:") {
		t.Fatalf("legacy key leaked into saved config:\n%s", body)
	}
}
