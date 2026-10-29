package projectfile

import (
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/config"
)

func TestMergeBoolAcceptsOnOffVariants(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want bool
	}{
		{"true", "true", true},
		{"1", "1", true},
		{"on", "on", true},
		{"ON", "ON", true},
		{"yes", "yes", true},
		{"false", "false", false},
		{"0", "0", false},
		{"off", "off", false},
		{"no", "no", false},
		{"invalid falls back to default", "maybe", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOGEN_TEST_BOOL", tt.env)
			got := mergeBool("GOGEN_TEST_BOOL", false, false)
			if got != tt.want {
				t.Errorf("mergeBool(%q) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestMergeIntFallBackToDefaultOnInvalidEnv(t *testing.T) {
	t.Setenv("GOGEN_TEST_INT", "abc")
	if got := mergeInt("GOGEN_TEST_INT", 0, 42); got != 42 {
		t.Errorf("mergeInt invalid env = %d, want default 42", got)
	}
}

func TestMergeBoolEnvOverridesFile(t *testing.T) {
	t.Setenv("GOGEN_TEST_BOOL", "off")
	if got := mergeBool("GOGEN_TEST_BOOL", true, false); got {
		t.Error("env GOGEN_TEST_BOOL=off should override file cli_verbose: true")
	}
}

func TestMergeBoolFileValueUsedWhenEnvUnset(t *testing.T) {
	t.Setenv("GOGEN_TEST_BOOL_UNUSED", "") // ensure a distinct var is unset
	if got := mergeBool("GOGEN_TEST_BOOL", true, false); !got {
		t.Error("file cli_verbose: true should win when env is unset")
	}
}

func TestMergeIntOptPreservesExplicitZero(t *testing.T) {
	os.Unsetenv("GOGEN_TEST_OPT")
	zero := 0
	if got := mergeIntOpt("GOGEN_TEST_OPT", &zero, 12); got != 0 {
		t.Errorf("explicit 0 = %d, want 0", got)
	}
	if got := mergeIntOpt("GOGEN_TEST_OPT", nil, 12); got != 12 {
		t.Errorf("absent = %d, want default 12", got)
	}
	five := 5
	if got := mergeIntOpt("GOGEN_TEST_OPT", &five, 12); got != 5 {
		t.Errorf("explicit 5 = %d, want 5", got)
	}
}

// The pre-rename GOGEN_KEEP_RECENT_MESSAGES env var must keep working, with
// the renamed env var winning when both are set.
func TestMergeLegacyKeepRecentEnv(t *testing.T) {
	t.Setenv("GOGEN_KEEP_RECENT_MESSAGES", "7")
	cfg := Merge(nil, FlagOverrides{})
	if cfg.CompactKeepRecentMessages != 7 {
		t.Fatalf("legacy env not honored: %d, want 7", cfg.CompactKeepRecentMessages)
	}

	t.Setenv("GOGEN_COMPACT_KEEP_RECENT_MESSAGES", "9")
	cfgBoth := Merge(nil, FlagOverrides{})
	if cfgBoth.CompactKeepRecentMessages != 9 {
		t.Fatalf("renamed env should win: %d, want 9", cfgBoth.CompactKeepRecentMessages)
	}
}

// An invalid legacy env value falls back to the default with a warning,
// matching the renamed env's behavior.
func TestMergeLegacyKeepRecentEnvInvalid(t *testing.T) {
	t.Setenv("GOGEN_KEEP_RECENT_MESSAGES", "bogus")
	os.Unsetenv("GOGEN_COMPACT_KEEP_RECENT_MESSAGES")
	cfg := Merge(nil, FlagOverrides{})
	if cfg.CompactKeepRecentMessages != 12 {
		t.Fatalf("invalid legacy env should fall back to default: %d, want 12", cfg.CompactKeepRecentMessages)
	}
}

// The file key aliasing flows through Merge: a project file using the legacy
// key reaches the runtime config even when the new env var is unset.
func TestMergeLegacyKeepRecentFileKey(t *testing.T) {
	os.Unsetenv("GOGEN_COMPACT_KEEP_RECENT_MESSAGES")
	os.Unsetenv("GOGEN_KEEP_RECENT_MESSAGES")
	pf, err := ParseContent("GOGEN.md", "---\nkeep_recent_messages: 20\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.CompactKeepRecentMessages != 20 {
		t.Fatalf("legacy file key not honored through Merge: %d, want 20", cfg.CompactKeepRecentMessages)
	}
}

// The pre-rename GOGEN_COMMAND_TIMEOUT_SECS env var must keep working, with
// the renamed env var winning when both are set.
func TestMergeLegacyCommandTimeoutEnv(t *testing.T) {
	os.Unsetenv("GOGEN_COMMAND_IDLE_TIMEOUT_SECS")
	t.Setenv("GOGEN_COMMAND_TIMEOUT_SECS", "300")
	cfg := Merge(nil, FlagOverrides{})
	if cfg.CommandIdleTimeoutSecs != 300 {
		t.Fatalf("legacy env not honored: %d, want 300", cfg.CommandIdleTimeoutSecs)
	}

	t.Setenv("GOGEN_COMMAND_IDLE_TIMEOUT_SECS", "90")
	cfgBoth := Merge(nil, FlagOverrides{})
	if cfgBoth.CommandIdleTimeoutSecs != 90 {
		t.Fatalf("renamed env should win: %d, want 90", cfgBoth.CommandIdleTimeoutSecs)
	}
}

// An invalid legacy env value falls back to the default with a warning,
// matching the renamed env's behavior.
func TestMergeLegacyCommandTimeoutEnvInvalid(t *testing.T) {
	os.Unsetenv("GOGEN_COMMAND_IDLE_TIMEOUT_SECS")
	t.Setenv("GOGEN_COMMAND_TIMEOUT_SECS", "bogus")
	cfg := Merge(nil, FlagOverrides{})
	if cfg.CommandIdleTimeoutSecs != config.DefaultCommandIdleTimeoutSecs {
		t.Fatalf("invalid legacy env should fall back to default: %d, want %d", cfg.CommandIdleTimeoutSecs, config.DefaultCommandIdleTimeoutSecs)
	}
}

// The file key aliasing flows through Merge: a project file using the legacy
// key reaches the runtime config even when both env vars are unset, and the
// legacy env var still loses to a file value's new-key sibling per the env >
// file precedence.
func TestMergeLegacyCommandTimeoutFileKey(t *testing.T) {
	os.Unsetenv("GOGEN_COMMAND_IDLE_TIMEOUT_SECS")
	os.Unsetenv("GOGEN_COMMAND_TIMEOUT_SECS")
	pf, err := ParseContent("GOGEN.md", "---\ncommand_timeout_secs: 300\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.CommandIdleTimeoutSecs != 300 {
		t.Fatalf("legacy file key not honored through Merge: %d, want 300", cfg.CommandIdleTimeoutSecs)
	}

	// Env (even the legacy spelling) beats the file value.
	t.Setenv("GOGEN_COMMAND_TIMEOUT_SECS", "60")
	cfgEnv := Merge(pf, FlagOverrides{})
	if cfgEnv.CommandIdleTimeoutSecs != 60 {
		t.Fatalf("legacy env should beat file value: %d, want 60", cfgEnv.CommandIdleTimeoutSecs)
	}
}

func TestMergeFloatOptPreservesExplicitZero(t *testing.T) {
	os.Unsetenv("GOGEN_TEST_OPTF")
	zero := 0.0
	if got := mergeFloatOpt("GOGEN_TEST_OPTF", &zero, 0.75); got != 0 {
		t.Errorf("explicit 0 = %v, want 0", got)
	}
	if got := mergeFloatOpt("GOGEN_TEST_OPTF", nil, 0.75); got != 0.75 {
		t.Errorf("absent = %v, want default 0.75", got)
	}
}

// End-to-end: explicit zeros in the file must survive the full Merge path so
// the runtime can honor them (auto-compaction off, no tail kept, no cap, no
// reserve), while absent keys fall back to defaults.
func TestMergePreservesExplicitZeros(t *testing.T) {
	for _, env := range []string{"GOGEN_COMPACT_THRESHOLD", "GOGEN_COMPACT_KEEP_RECENT_MESSAGES", "GOGEN_MAX_TOOL_RESULT_BYTES", "GOGEN_COMPACT_RESERVE_TOKENS"} {
		os.Unsetenv(env)
	}
	zero := 0
	zeroF := 0.0
	pf := &ProjectFile{Config: FileConfig{
		CompactThreshold:          &zeroF,
		CompactKeepRecentMessages: &zero,
		MaxToolResultBytes:        &zero,
		CompactReserveTokens:      &zero,
	}}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.CompactThreshold != 0 || cfg.CompactKeepRecentMessages != 0 || cfg.MaxToolResultBytes != 0 || cfg.CompactReserveTokens != 0 {
		t.Fatalf("explicit zeros not preserved through Merge: threshold=%v keep=%d max=%d reserve=%d",
			cfg.CompactThreshold, cfg.CompactKeepRecentMessages, cfg.MaxToolResultBytes, cfg.CompactReserveTokens)
	}

	pfAbsent := &ProjectFile{Config: FileConfig{}}
	cfgAbsent := Merge(pfAbsent, FlagOverrides{})
	if cfgAbsent.CompactThreshold != 0.85 || cfgAbsent.CompactKeepRecentMessages != 12 || cfgAbsent.MaxToolResultBytes != 262144 || cfgAbsent.CompactReserveTokens != 4000 {
		t.Fatalf("absent keys should fall back to defaults: threshold=%v keep=%d max=%d reserve=%d",
			cfgAbsent.CompactThreshold, cfgAbsent.CompactKeepRecentMessages, cfgAbsent.MaxToolResultBytes, cfgAbsent.CompactReserveTokens)
	}
}

// session_max_age_days -1 is the "keep sessions forever" sentinel: unlike
// every other int option (where <= 0 means "use the default"), a negative
// file value must survive Merge so the store keeps its retention sentinel
// across restarts.
func TestMergePreservesNegativeSessionMaxAgeDays(t *testing.T) {
	os.Unsetenv("GOGEN_SESSION_MAX_AGE_DAYS")
	pf := &ProjectFile{Config: FileConfig{SessionMaxAgeDays: -1}}
	if got := Merge(pf, FlagOverrides{}).SessionMaxAgeDays; got != -1 {
		t.Fatalf("file -1 merged to %d, want -1 (keep forever)", got)
	}
	pfAbsent := &ProjectFile{Config: FileConfig{}}
	if got := Merge(pfAbsent, FlagOverrides{}).SessionMaxAgeDays; got != config.DefaultSessionMaxAgeDays {
		t.Fatalf("absent key merged to %d, want default %d", got, config.DefaultSessionMaxAgeDays)
	}
	// The env override still wins (and a positive env value is unchanged).
	t.Setenv("GOGEN_SESSION_MAX_AGE_DAYS", "30")
	if got := Merge(pf, FlagOverrides{}).SessionMaxAgeDays; got != 30 {
		t.Fatalf("env 30 merged to %d, want 30", got)
	}
}

// The board/subagent feature flags follow the opt-in MCP pattern: env >
// file > defaults, with GOGEN_SUBAGENT_MAX_DEPTH falling back to the default
// when unset/zero.
// TestMergeAgentInstructionsFlag pins the agent_instructions feature flag
// merge: default off, file value applies, env overrides file.
func TestMergeAgentInstructionsFlag(t *testing.T) {
	os.Unsetenv("GOGEN_AGENT_INSTRUCTIONS")
	if Merge(nil, FlagOverrides{}).AgentInstructionsEnabled() {
		t.Fatal("agent_instructions should default off")
	}
	pf, err := ParseContent("GOGEN.md", "---\nagent_instructions: on\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if !Merge(pf, FlagOverrides{}).AgentInstructionsEnabled() {
		t.Fatal("file agent_instructions: on should enable")
	}
	t.Setenv("GOGEN_AGENT_INSTRUCTIONS", "off")
	if Merge(pf, FlagOverrides{}).AgentInstructionsEnabled() {
		t.Fatal("env off should override file on")
	}
}

// TestMergeSkillsFlag pins the skills feature flag merge: default off,
// file value applies, env overrides file.
func TestMergeSkillsFlag(t *testing.T) {
	os.Unsetenv("GOGEN_SKILLS")
	if Merge(nil, FlagOverrides{}).SkillsEnabled() {
		t.Fatal("skills should default off")
	}
	pf, err := ParseContent("GOGEN.md", "---\nskills: on\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if !Merge(pf, FlagOverrides{}).SkillsEnabled() {
		t.Fatal("file skills: on should enable")
	}
	t.Setenv("GOGEN_SKILLS", "off")
	if Merge(pf, FlagOverrides{}).SkillsEnabled() {
		t.Fatal("env off should override file on")
	}
}

// TestMergeJobNoticesFlag pins the job_notices feature flag merge: default
// off, file value applies, env overrides file.
func TestMergeJobNoticesFlag(t *testing.T) {
	os.Unsetenv("GOGEN_JOB_NOTICES")
	if Merge(nil, FlagOverrides{}).JobNoticesEnabled() {
		t.Fatal("job_notices should default off")
	}
	pf, err := ParseContent("GOGEN.md", "---\njob_notices: on\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if !Merge(pf, FlagOverrides{}).JobNoticesEnabled() {
		t.Fatal("file job_notices: on should enable")
	}
	t.Setenv("GOGEN_JOB_NOTICES", "off")
	if Merge(pf, FlagOverrides{}).JobNoticesEnabled() {
		t.Fatal("env off should override file on")
	}
}

// TestMergeAutomationsFlag pins the automations feature flag merge: default
// off, file value applies, env overrides file.
func TestMergeAutomationsFlag(t *testing.T) {
	os.Unsetenv("GOGEN_AUTOMATIONS")
	if Merge(nil, FlagOverrides{}).AutomationsEnabled() {
		t.Fatal("automations should default off")
	}
	pf, err := ParseContent("GOGEN.md", "---\nautomations: on\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if !Merge(pf, FlagOverrides{}).AutomationsEnabled() {
		t.Fatal("file automations: on should enable")
	}
	t.Setenv("GOGEN_AUTOMATIONS", "off")
	if Merge(pf, FlagOverrides{}).AutomationsEnabled() {
		t.Fatal("env off should override file on")
	}
}

// TestMergeOutputSpillFlag pins the output_spill feature flag merge:
// default on, file value applies, env overrides file.
func TestMergeOutputSpillFlag(t *testing.T) {
	os.Unsetenv("GOGEN_OUTPUT_SPILL")
	if !Merge(nil, FlagOverrides{}).OutputSpillEnabled() {
		t.Fatal("output_spill should default on")
	}
	pf, err := ParseContent("GOGEN.md", "---\noutput_spill: off\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if Merge(pf, FlagOverrides{}).OutputSpillEnabled() {
		t.Fatal("file output_spill: off should disable")
	}
	t.Setenv("GOGEN_OUTPUT_SPILL", "on")
	if !Merge(pf, FlagOverrides{}).OutputSpillEnabled() {
		t.Fatal("env on should override file off")
	}
}

// TestMergeStatusDisplay pins the status-display option merge: the gate
// defaults to auto (probe at startup), the host defaults to the firmware's
// mDNS name, the source defaults to auto (derived from the project folder),
// file values apply, and the env overrides them. An unknown gate value
// resolves to auto rather than erroring the whole config.
func TestMergeStatusDisplay(t *testing.T) {
	for _, env := range []string{"GOGEN_STATUS_DISPLAY", "GOGEN_STATUS_DISPLAY_HOST", "GOGEN_STATUS_DISPLAY_TOKEN", "GOGEN_STATUS_DISPLAY_SOURCE"} {
		os.Unsetenv(env)
	}
	cfg := Merge(nil, FlagOverrides{})
	if got := cfg.StatusDisplayMode(); got != config.StatusDisplayAuto {
		t.Fatalf("status_display default = %q, want auto", got)
	}
	if cfg.StatusDisplayHost != config.DefaultStatusDisplayHost || cfg.StatusDisplaySource != config.DefaultStatusDisplaySource {
		t.Fatalf("defaults = host %q source %q, want %q / %q",
			cfg.StatusDisplayHost, cfg.StatusDisplaySource, config.DefaultStatusDisplayHost, config.DefaultStatusDisplaySource)
	}

	pf, err := ParseContent("GOGEN.md", "---\nstatus_display: on\nstatus_display_host: 10.0.0.5\nstatus_display_token: s3cret\nstatus_display_source: gogen-alt\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	fromFile := Merge(pf, FlagOverrides{})
	if got := fromFile.StatusDisplayMode(); got != config.StatusDisplayOn {
		t.Fatalf("file status_display = %q, want on", got)
	}
	if fromFile.StatusDisplayHost != "10.0.0.5" || fromFile.StatusDisplayToken != "s3cret" || fromFile.StatusDisplaySource != "gogen-alt" {
		t.Fatalf("file values not honored: %+v", fromFile)
	}

	pfAuto, err := ParseContent("GOGEN.md", "---\nstatus_display: auto\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(pfAuto, FlagOverrides{}).StatusDisplayMode(); got != config.StatusDisplayAuto {
		t.Fatalf("file status_display = %q, want auto", got)
	}

	t.Setenv("GOGEN_STATUS_DISPLAY", "off")
	t.Setenv("GOGEN_STATUS_DISPLAY_HOST", "statusscreen.local")
	t.Setenv("GOGEN_STATUS_DISPLAY_SOURCE", "gogen")
	fromEnv := Merge(pf, FlagOverrides{})
	if got := fromEnv.StatusDisplayMode(); got != config.StatusDisplayOff {
		t.Fatalf("env status_display = %q, want off (env over file on)", got)
	}
	if fromEnv.StatusDisplayHost != "statusscreen.local" || fromEnv.StatusDisplaySource != "gogen" {
		t.Fatalf("env values should override file: host %q source %q", fromEnv.StatusDisplayHost, fromEnv.StatusDisplaySource)
	}

	// An unknown gate value degrades to auto (discovery), not to an error.
	if got := (&config.Config{StatusDisplay: "sometimes"}).StatusDisplayMode(); got != config.StatusDisplayAuto {
		t.Fatalf("unknown gate = %q, want auto", got)
	}
	// browser is a first-class gate (the web UI sends the updates).
	if got := (&config.Config{StatusDisplay: "browser"}).StatusDisplayMode(); got != config.StatusDisplayBrowser {
		t.Fatalf("browser gate = %q, want browser", got)
	}
}

func TestMergeBoardSubagentFlags(t *testing.T) {
	for _, env := range []string{"GOGEN_BOARD", "GOGEN_SUBAGENT", "GOGEN_SUBAGENT_MAX_DEPTH", "GOGEN_SUBAGENT_MAX_CONCURRENT"} {
		os.Unsetenv(env)
	}

	// Defaults: both off, depth default 1.
	cfg := Merge(nil, FlagOverrides{})
	if cfg.BoardEnabled() || cfg.SubagentEnabled() {
		t.Fatalf("defaults should be off: board=%q subagent=%q", cfg.Board, cfg.Subagent)
	}
	if cfg.SubagentDepth() != config.DefaultSubagentMaxDepth {
		t.Fatalf("default depth = %d, want %d", cfg.SubagentDepth(), config.DefaultSubagentMaxDepth)
	}
	if cfg.SubagentLimit() != config.DefaultSubagentMaxConcurrent {
		t.Fatalf("default concurrent limit = %d, want %d", cfg.SubagentLimit(), config.DefaultSubagentMaxConcurrent)
	}

	// File values.
	pf := &ProjectFile{Config: FileConfig{Board: "on", Subagent: "on", SubagentMaxDepth: 3, SubagentMaxConcurrent: 2}}
	cfgFile := Merge(pf, FlagOverrides{})
	if !cfgFile.BoardEnabled() || !cfgFile.SubagentEnabled() {
		t.Fatalf("file flags not honored: board=%q subagent=%q", cfgFile.Board, cfgFile.Subagent)
	}
	if cfgFile.SubagentDepth() != 3 {
		t.Fatalf("file depth = %d, want 3", cfgFile.SubagentDepth())
	}
	if cfgFile.SubagentLimit() != 2 {
		t.Fatalf("file concurrent limit = %d, want 2", cfgFile.SubagentLimit())
	}

	// Env overrides file.
	t.Setenv("GOGEN_BOARD", "off")
	t.Setenv("GOGEN_SUBAGENT", "off")
	t.Setenv("GOGEN_SUBAGENT_MAX_DEPTH", "5")
	t.Setenv("GOGEN_SUBAGENT_MAX_CONCURRENT", "7")
	cfgEnv := Merge(pf, FlagOverrides{})
	if cfgEnv.BoardEnabled() || cfgEnv.SubagentEnabled() {
		t.Fatalf("env off should override file on: board=%q subagent=%q", cfgEnv.Board, cfgEnv.Subagent)
	}
	if cfgEnv.SubagentDepth() != 5 {
		t.Fatalf("env depth = %d, want 5", cfgEnv.SubagentDepth())
	}
	if cfgEnv.SubagentLimit() != 7 {
		t.Fatalf("env concurrent limit = %d, want 7", cfgEnv.SubagentLimit())
	}

	// Invalid depth env falls back to default.
	t.Setenv("GOGEN_SUBAGENT_MAX_DEPTH", "bogus")
	cfgBad := Merge(nil, FlagOverrides{})
	if cfgBad.SubagentDepth() != config.DefaultSubagentMaxDepth {
		t.Fatalf("invalid env depth = %d, want default %d", cfgBad.SubagentDepth(), config.DefaultSubagentMaxDepth)
	}
	// Invalid concurrent-limit env falls back to default.
	t.Setenv("GOGEN_SUBAGENT_MAX_CONCURRENT", "bogus")
	cfgBadLimit := Merge(nil, FlagOverrides{})
	if cfgBadLimit.SubagentLimit() != config.DefaultSubagentMaxConcurrent {
		t.Fatalf("invalid env concurrent limit = %d, want default %d", cfgBadLimit.SubagentLimit(), config.DefaultSubagentMaxConcurrent)
	}
}

// --save-config must round-trip the new keys so a live toggle survives a
// restart.
func TestSaveConfigRoundTripsBoardSubagent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gogen.conf")
	cfg := config.Defaults()
	cfg.Board = "on"
	cfg.Subagent = "on"
	cfg.SubagentMaxDepth = 4
	cfg.SubagentMaxConcurrent = 6
	cfg.SubagentModel = "gpt-4o-mini"
	cfg.SubagentThinkingLevel = "high"
	if err := SaveConfig(cfgPath, "", &cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	pf, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(pf, FlagOverrides{})
	if !merged.BoardEnabled() || !merged.SubagentEnabled() {
		t.Fatalf("round-trip lost flags: board=%q subagent=%q", merged.Board, merged.Subagent)
	}
	if merged.SubagentDepth() != 4 {
		t.Fatalf("round-trip depth = %d, want 4", merged.SubagentDepth())
	}
	if merged.SubagentLimit() != 6 {
		t.Fatalf("round-trip concurrent limit = %d, want 6", merged.SubagentLimit())
	}
	if merged.SubagentModel != "gpt-4o-mini" {
		t.Fatalf("round-trip subagent_model = %q, want gpt-4o-mini", merged.SubagentModel)
	}
	if merged.SubagentThinkingLevel != "high" {
		t.Fatalf("round-trip subagent_thinking_level = %q, want high", merged.SubagentThinkingLevel)
	}
}

// TestSaveConfigRoundTripsWebAllowedOrigins is a regression test for the
// silent data-loss bug where web_allowed_origins was written by --save-config
// but never parsed back on load (FileConfig had no field and Merge hard-coded
// the file slot to ""). A CORS allow-list set in the web UI or saved to disk
// must survive a restart.
func TestSaveConfigRoundTripsWebAllowedOrigins(t *testing.T) {
	os.Unsetenv("GOGEN_WEB_ALLOWED_ORIGINS")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gogen.conf")
	cfg := config.Defaults()
	cfg.WebAllowedOrigins = "https://example.com https://app.dev"
	if err := SaveConfig(cfgPath, "", &cfg, "", WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	pf, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := pf.Config.WebAllowedOrigins; got != cfg.WebAllowedOrigins {
		t.Fatalf("file WebAllowedOrigins = %q, want %q", got, cfg.WebAllowedOrigins)
	}
	merged := Merge(pf, FlagOverrides{})
	if merged.WebAllowedOrigins != cfg.WebAllowedOrigins {
		t.Fatalf("round-trip WebAllowedOrigins = %q, want %q", merged.WebAllowedOrigins, cfg.WebAllowedOrigins)
	}
	// Env still overrides the persisted file value.
	t.Setenv("GOGEN_WEB_ALLOWED_ORIGINS", "https://override.test")
	if got := Merge(pf, FlagOverrides{}).WebAllowedOrigins; got != "https://override.test" {
		t.Fatalf("env override = %q, want https://override.test", got)
	}
}

// TestMergeSubagentModelPrecedence pins the subagent default model merge:
// empty (default) = inherit, file value applies, env overrides file.
func TestMergeSubagentModelPrecedence(t *testing.T) {
	os.Unsetenv("GOGEN_SUBAGENT_MODEL")
	if got := Merge(nil, FlagOverrides{}).SubagentModel; got != "" {
		t.Fatalf("default subagent_model = %q, want empty (inherit)", got)
	}
	pf := &ProjectFile{Config: FileConfig{SubagentModel: "llama3.1"}}
	if got := Merge(pf, FlagOverrides{}).SubagentModel; got != "llama3.1" {
		t.Fatalf("file subagent_model = %q, want llama3.1", got)
	}
	t.Setenv("GOGEN_SUBAGENT_MODEL", "qwen2.5")
	if got := Merge(pf, FlagOverrides{}).SubagentModel; got != "qwen2.5" {
		t.Fatalf("env subagent_model = %q, want qwen2.5", got)
	}
}

// TestMergeSubagentThinkingLevelPrecedence pins the subagent reasoning-effort
// merge: empty (default) = inherit the parent's level, file value applies,
// env overrides file.
func TestMergeSubagentThinkingLevelPrecedence(t *testing.T) {
	os.Unsetenv("GOGEN_SUBAGENT_THINKING_LEVEL")
	if got := Merge(nil, FlagOverrides{}).SubagentThinkingLevel; got != "" {
		t.Fatalf("default subagent_thinking_level = %q, want empty (inherit)", got)
	}
	pf := &ProjectFile{Config: FileConfig{SubagentThinkingLevel: "high"}}
	if got := Merge(pf, FlagOverrides{}).SubagentThinkingLevel; got != "high" {
		t.Fatalf("file subagent_thinking_level = %q, want high", got)
	}
	t.Setenv("GOGEN_SUBAGENT_THINKING_LEVEL", "low")
	if got := Merge(pf, FlagOverrides{}).SubagentThinkingLevel; got != "low" {
		t.Fatalf("env subagent_thinking_level = %q, want low", got)
	}
}

// TestMergePromptTemplates pins the configurable prompt template merge:
// empty = the built-in default, file value applies, env overrides file.
func TestMergePromptTemplates(t *testing.T) {
	os.Unsetenv("GOGEN_BOARD_START_PROMPT")
	os.Unsetenv("GOGEN_SYSTEM_PROMPT")
	os.Unsetenv("GOGEN_SUBAGENT_PROMPT")
	if got := Merge(nil, FlagOverrides{}).BoardStartPrompt; got != "" {
		t.Fatalf("default board_start_prompt = %q, want empty (built-in)", got)
	}
	pf := &ProjectFile{Config: FileConfig{
		BoardStartPrompt: "board {title}",
		SystemPrompt:     "sys {working_dir}",
		SubagentPrompt:   "sub {job}",
	}}
	cfg := Merge(pf, FlagOverrides{})
	if cfg.BoardStartPrompt != "board {title}" || cfg.SystemPrompt != "sys {working_dir}" || cfg.SubagentPrompt != "sub {job}" {
		t.Fatalf("file prompt templates = %+v", cfg)
	}
	t.Setenv("GOGEN_SYSTEM_PROMPT", "env {working_dir}")
	if got := Merge(pf, FlagOverrides{}).SystemPrompt; got != "env {working_dir}" {
		t.Fatalf("env system_prompt = %q, want env value", got)
	}
}

// ConfigFileHasSecrets must detect stored secrets in every config file
// shape the toggle persist can rewrite: .conf, the plain-YAML global config
// (config.yaml — no front matter), and front-matter .md files. Missing it
// would silently drop openai_api_key / MCP envs on a live toggle.
func TestConfigFileHasSecrets(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "gogen.conf")
	if err := os.WriteFile(confPath, []byte("openai_api_key: sk-conf\nopenai_model: m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(confPath) {
		t.Fatal(".conf with a key should report secrets")
	}

	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("openai_api_key: sk-yaml\nopenai_model: m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(yamlPath) {
		t.Fatal("plain-YAML global config with a key should report secrets")
	}

	mdPath := filepath.Join(dir, "GOGEN.md")
	md := "---\nopenai_api_key: sk-md\nopenai_model: m\n---\n# rules\n"
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(mdPath) {
		t.Fatal("front-matter .md with a key should report secrets")
	}

	envPath := filepath.Join(dir, "env.conf")
	if err := os.WriteFile(envPath, []byte("openai_model: m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ConfigFileHasSecrets(envPath) {
		t.Fatal("config without secrets should report none")
	}
	if ConfigFileHasSecrets(filepath.Join(dir, "missing.conf")) {
		t.Fatal("missing config should report no secrets")
	}

	mcpPath := filepath.Join(dir, "mcp.conf")
	mcp := "mcp: on\nmcp_servers:\n  - name: fetch\n    command: npx\n    args: [\"-y\", \"@modelcontextprotocol/server-fetch\"]\n    env:\n      TOKEN: secret\n"
	if err := os.WriteFile(mcpPath, []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(mcpPath) {
		t.Fatal("MCP server env should report secrets")
	}
}

// An existing but unparseable config is conservatively treated as having
// secrets so a toggle rewrite cannot drop a key from a file it cannot read.
func TestConfigFileHasSecretsUnparseableAssumesSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.conf")
	if err := os.WriteFile(path, []byte("{not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ConfigFileHasSecrets(path) {
		t.Fatal("unparseable existing config should conservatively report secrets")
	}
}

// compact_last_resort follows the env > file > defaults merge rule: the
// last-resort condensation mode (Phase 0e) defaults to "condense", the
// project file key applies, and the env var overrides the file.
func TestMergeCompactLastResort(t *testing.T) {
	os.Unsetenv("GOGEN_COMPACT_LAST_RESORT")
	if got := Merge(nil, FlagOverrides{}).CompactLastResort; got != config.DefaultCompactLastResort {
		t.Fatalf("absent key merged to %q, want default %q", got, config.DefaultCompactLastResort)
	}
	pf, err := ParseContent("GOGEN.md", "---\ncompact_last_resort: error\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(pf, FlagOverrides{}).CompactLastResort; got != "error" {
		t.Fatalf("file value merged to %q, want error", got)
	}
	t.Setenv("GOGEN_COMPACT_LAST_RESORT", "condense")
	if got := Merge(pf, FlagOverrides{}).CompactLastResort; got != "condense" {
		t.Fatalf("env should override the file: %q, want condense", got)
	}
}

// smart_compact follows the same env > file > defaults rule and defaults
// to off, so a config that never mentions it keeps today's compaction.
func TestMergeSmartCompact(t *testing.T) {
	os.Unsetenv("GOGEN_SMART_COMPACT")
	if got := Merge(nil, FlagOverrides{}).SmartCompact; got != config.DefaultSmartCompact {
		t.Fatalf("absent key merged to %q, want default %q", got, config.DefaultSmartCompact)
	}
	pf, err := ParseContent("GOGEN.md", "---\nsmart_compact: on\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(pf, FlagOverrides{}).SmartCompact; got != "on" {
		t.Fatalf("file value merged to %q, want on", got)
	}
	t.Setenv("GOGEN_SMART_COMPACT", "off")
	if got := Merge(pf, FlagOverrides{}).SmartCompact; got != "off" {
		t.Fatalf("env should override the file: %q, want off", got)
	}
}

// context_mode follows the same env > file > defaults rule and defaults to
// "default", so a config that never mentions it keeps today's prompt and
// compaction.
func TestMergeContextMode(t *testing.T) {
	t.Setenv("GOGEN_CONTEXT_MODE", "")
	os.Unsetenv("GOGEN_CONTEXT_MODE")
	if got := Merge(nil, FlagOverrides{}).ContextMode; got != config.DefaultContextMode {
		t.Fatalf("absent key merged to %q, want default %q", got, config.DefaultContextMode)
	}
	pf, err := ParseContent("GOGEN.md", "---\ncontext_mode: deep_dive\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(pf, FlagOverrides{}).ContextMode; got != config.ContextModeDeepDive {
		t.Fatalf("file value merged to %q, want deep_dive", got)
	}
	t.Setenv("GOGEN_CONTEXT_MODE", "default")
	if got := Merge(pf, FlagOverrides{}).ContextMode; got != "default" {
		t.Fatalf("env should override the file: %q, want default", got)
	}
}
