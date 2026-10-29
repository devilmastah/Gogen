package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/projectfile"
)

// TestRuntimeConfigLiveViaWS drives the settings-modal runtime options
// through a real WebSocket: each applied value lands in the runtime target
// (executor, context manager, session store), the config echo carries it,
// and the persisted file contains it.
func TestRuntimeConfigLiveViaWS(t *testing.T) {
	dir := t.TempDir()
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
			"commandSafety", "commandAllowlist", "deleteApproval", "commandApproval", "commandSandbox", "commandIdleTimeoutSecs",
			"contextLimit", "compactThreshold", "compactKeepRecentMessages", "maxToolResultBytes", "outputSpill", "compactReserveTokens", "compactLastResort", "smartCompact", "contextMode",
			"webFetch", "webSearch", "treesitter", "preserveReasoning",
			"sessionMaxCount", "sessionMaxAgeDays", "webApprovalHoldSecs",
			"subagentModel", "subagentThinkingLevel", "boardStartPrompt", "systemPrompt", "subagentPrompt",
		},
		CommandSafetyMode:         "allowlist",
		CommandAllowlist:          "ls, cat",
		DeleteApproval:            "off",
		CommandApproval:           "off",
		CommandSandbox:            "bwrap",
		CommandIdleTimeoutSecs:    45,
		ContextLimitConfig:        20000,
		CompactThreshold:          0.5,
		CompactKeepRecentMessages: 3,
		MaxToolResultBytes:        65536,
		OutputSpill:               "off",
		CompactReserveTokens:      1000,
		CompactLastResort:         "error",
		SmartCompact:              "on",
		ContextMode:               "deep_dive",
		WebFetch:                  "off",
		WebSearch:                 "off",
		TreeSitter:                "off",
		PreserveReasoning:         "on",
		SessionMaxCount:           4,
		SessionMaxAgeDays:         9,
		WebApprovalHoldSecs:       7,
		SubagentModel:             strPtr("gpt-4o-mini"),
		SubagentThinkingLevel:     strPtr("high"),
		BoardStartPrompt:          "board tmpl {title}",
		SystemPrompt:              "sys tmpl {working_dir}",
		SubagentPrompt:            "sub tmpl {job}",
	}
	t.Cleanup(func() {
		// The prompt setters are package-global: restore the built-ins so
		// no other test observes this test's templates.
		agent.ConfigureSystemPrompt("")
		agent.ConfigureSubagentPrompt("")
		// The spill gate is package-global too: this test applied "off".
		agent.ConfigureOutputSpill(true)
	})
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("send config: %v", err)
	}

	// The broadcast echoes the applied values.
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return len(m.ConfigFields) == 0 && m.CommandSafetyMode == "allowlist"
	})
	if cfg.CommandAllowlist != "ls, cat" || cfg.DeleteApproval != "off" || cfg.CommandApproval != "off" || cfg.CommandSandbox != "bwrap" ||
		cfg.CommandIdleTimeoutSecs != 45 ||
		cfg.ContextLimitConfig != 20000 || cfg.CompactThreshold != 0.5 ||
		cfg.CompactKeepRecentMessages != 3 || cfg.MaxToolResultBytes != 65536 || cfg.CompactReserveTokens != 1000 ||
		cfg.CompactLastResort != "error" || cfg.SmartCompact != "on" || cfg.ContextMode != "deep_dive" ||
		cfg.OutputSpill != "off" ||
		cfg.WebFetch != "off" || cfg.WebSearch != "off" || cfg.TreeSitter != "off" ||
		cfg.PreserveReasoning != "on" || cfg.SessionMaxCount != 4 || cfg.SessionMaxAgeDays != 9 ||
		cfg.WebApprovalHoldSecs != 7 || cfg.SubagentModel == nil || *cfg.SubagentModel != "gpt-4o-mini" ||
		cfg.SubagentThinkingLevel == nil || *cfg.SubagentThinkingLevel != "high" ||
		cfg.BoardStartPrompt != "board tmpl {title}" || cfg.SystemPrompt != "sys tmpl {working_dir}" ||
		cfg.SubagentPrompt != "sub tmpl {job}" {
		t.Fatalf("config echo missing live values: %+v", cfg)
	}

	// Runtime targets.
	if got := s.ws.Exec.CommandGuardMode(); got != "allowlist" {
		t.Fatalf("executor guard mode = %q, want allowlist", got)
	}
	if s.ws.Exec.DeleteApprovalRequired() {
		t.Fatal("delete approval should be off")
	}
	if s.ws.Exec.CommandApprovalRequired() {
		t.Fatal("command approval should be off")
	}
	if s.ws.Exec.SandboxMode() != "bwrap" {
		t.Fatalf("sandbox = %q, want bwrap", s.ws.Exec.SandboxMode())
	}
	if s.ws.Exec.IdleTimeoutDuration() != 45*time.Second {
		t.Fatalf("idle timeout = %v, want 45s", s.ws.Exec.IdleTimeoutDuration())
	}
	snap := a.Context.SettingsSnapshot()
	if snap.ContextLimit != 20000 || snap.CompactThreshold != 0.5 || snap.CompactKeepRecentMessages != 3 ||
		snap.MaxToolResultBytes != 65536 || snap.CompactReserveTokens != 1000 || snap.CompactLastResort != "error" || !snap.SmartCompact || !snap.DeepDive || !a.Context.DeepDive() {
		t.Fatalf("context settings = %+v, want applied values", snap)
	}
	if s.ws.Store.MaxCount() != 4 || s.ws.Store.MaxAgeDays() != 9 {
		t.Fatalf("store retention = %d/%d, want 4/9", s.ws.Store.MaxCount(), s.ws.Store.MaxAgeDays())
	}
	if s.ws.ApprovalHold() != 7*time.Second {
		t.Fatalf("approval hold = %v, want 7s", s.ws.ApprovalHold())
	}
	if got := s.ws.GetRuntimeConfig().SubagentModel; got != "gpt-4o-mini" {
		t.Fatalf("runtime subagentModel = %q, want gpt-4o-mini", got)
	}
	if got := s.ws.GetRuntimeConfig().SubagentThinkingLevel; got != "high" {
		t.Fatalf("runtime subagentThinkingLevel = %q, want high", got)
	}
	if agent.OutputSpillEnabled() {
		t.Fatal("spill gate should be off after applying outputSpill=off")
	}
	// Live prompt setters: the next turn's system prompt and subagent jobs
	// resolve the configured templates.
	if got := agent.SystemPrompt("/w"); got != "sys tmpl /w" {
		t.Fatalf("live system prompt = %q, want the configured template", got)
	}
	if got := agent.FormatSubagentJob("j"); got != "sub tmpl j" {
		t.Fatalf("live subagent job = %q, want the configured template", got)
	}

	// Persisted file contains the values (persist happens before the
	// broadcast in the handler goroutine).
	data, err := os.ReadFile(projectfile.DefaultSavePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"command_safety: allowlist", "command_allowlist: ls, cat", `delete_approval: "off"`, "command_idle_timeout_secs: 45", "context_limit: 20000", "session_max_count: 4", "web_approval_hold_secs: 7", `output_spill: "off"`, "context_mode: deep_dive", "subagent_model: gpt-4o-mini", "subagent_thinking_level: high", "board_start_prompt: board tmpl {title}", "system_prompt: sys tmpl {working_dir}", "subagent_prompt: sub tmpl {job}"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected %q in persisted config:\n%s", want, data)
		}
	}

	// Clearing back to "inherit" (empty value via ConfigFields) is applied
	// and pushed — the explicit-empty mechanism distinguishes it from "not
	// provided".
	empty := ""
	clear := WSMessage{Type: "config", ConfigFields: []string{"subagentModel", "subagentThinkingLevel"}, SubagentModel: &empty, SubagentThinkingLevel: &empty}
	if err := conn.WriteJSON(clear); err != nil {
		t.Fatalf("send config clear subagentModel: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return len(m.ConfigFields) == 0 && m.SubagentModel != nil && *m.SubagentModel == "" &&
			m.SubagentThinkingLevel != nil && *m.SubagentThinkingLevel == ""
	})
	if got := s.ws.GetRuntimeConfig().SubagentModel; got != "" {
		t.Fatalf("cleared subagentModel = %q, want empty (inherit)", got)
	}
	if got := s.ws.GetRuntimeConfig().SubagentThinkingLevel; got != "" {
		t.Fatalf("cleared subagentThinkingLevel = %q, want empty (inherit)", got)
	}

	// Clearing the prompts: the push resolves the empty values back to the
	// built-in defaults (the fields stay pre-populated for editing).
	clearPrompts := WSMessage{Type: "config", ConfigFields: []string{"boardStartPrompt", "systemPrompt", "subagentPrompt"}}
	if err := conn.WriteJSON(clearPrompts); err != nil {
		t.Fatalf("send config clear prompts: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return len(m.ConfigFields) == 0 &&
			m.BoardStartPrompt == agent.DefaultBoardStartPrompt &&
			m.SystemPrompt == agent.DefaultSystemPromptTemplate() &&
			m.SubagentPrompt == agent.DefaultSubagentPrompt
	})
	if got := s.ws.GetRuntimeConfig().BoardStartPrompt; got != "" {
		t.Fatalf("cleared boardStartPrompt = %q, want empty (built-in)", got)
	}

	// Saving the default text verbatim stores NOTHING: the runtime overlay
	// stays empty and the config file stays clean (no bake-in).
	defMsg := WSMessage{Type: "config", ConfigFields: []string{"boardStartPrompt"}, BoardStartPrompt: agent.DefaultBoardStartPrompt}
	if err := conn.WriteJSON(defMsg); err != nil {
		t.Fatalf("send config default prompt: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return len(m.ConfigFields) == 0 && m.BoardStartPrompt == agent.DefaultBoardStartPrompt
	})
	if got := s.ws.GetRuntimeConfig().BoardStartPrompt; got != "" {
		t.Fatalf("saving the default text stored %q, want empty", got)
	}
	data, err = os.ReadFile(projectfile.DefaultSavePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "board_start_prompt") {
		t.Fatalf("default prompt text was baked into the config:\n%s", data)
	}
}

// TestRuntimeConfigRejectsInvalid pins validation: an invalid value rejects
// the WHOLE request (nothing applied) with a settings notice.
func TestRuntimeConfigRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	bad := []WSMessage{
		{Type: "config", ConfigFields: []string{"commandSafety"}, CommandSafetyMode: "maybe"},
		{Type: "config", ConfigFields: []string{"commandIdleTimeoutSecs"}, CommandIdleTimeoutSecs: -1},
		{Type: "config", ConfigFields: []string{"compactThreshold"}, CompactThreshold: 1.5},
		{Type: "config", ConfigFields: []string{"webFetch"}, WebFetch: "sometimes"},
		{Type: "config", ConfigFields: []string{"deleteApproval"}, DeleteApproval: "maybe"},
		{Type: "config", ConfigFields: []string{"subagentModel"}, SubagentModel: strPtr("   ")},
		{Type: "config", ConfigFields: []string{"subagentThinkingLevel"}, SubagentThinkingLevel: strPtr("   ")},
		{Type: "config", ConfigFields: []string{"systemPrompt"}, SystemPrompt: strings.Repeat("x", maxPromptTemplateLen+1)},
		{Type: "config", ConfigFields: []string{"nope"}},
	}
	for _, m := range bad {
		if err := conn.WriteJSON(m); err != nil {
			t.Fatalf("send config: %v", err)
		}
		resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
		if resp.Kind != "settings" || resp.Success {
			t.Fatalf("invalid config %+v: notice = %+v, want settings error", m.ConfigFields, resp)
		}
	}
	// Nothing was applied: the guard is still the default blocklist.
	if got := s.ws.Exec.CommandGuardMode(); got != "blocklist" {
		t.Fatalf("invalid request mutated the guard: %q", got)
	}
}

// TestSessionMaxAgeDaysKeepForever pins the -1 "keep sessions forever"
// sentinel: accepted through the settings modal (unlike other int options,
// which reject negatives), applied to the store's retention, persisted to
// the config file, and re-read as -1 by the merge path on the next start.
func TestSessionMaxAgeDaysKeepForever(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	msg := WSMessage{Type: "config", ConfigFields: []string{"sessionMaxAgeDays"}, SessionMaxAgeDays: -1}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("send config: %v", err)
	}
	// The handler applies the value on the server read loop; wait for the
	// store retention to flip to the keep-forever sentinel.
	if s.ws.Store == nil {
		t.Fatal("store nil")
	}
	waitFor(t, 5*time.Second, func() bool { return s.ws.Store.MaxAgeDays() == -1 })
	if got := s.ws.Store.MaxAgeDays(); got != -1 {
		t.Fatalf("store maxAgeDays = %d, want -1 (keep forever)", got)
	}
	// The persisted config round-trips the sentinel (the merge path must
	// preserve negatives, not map them to the default).
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(projectfile.DefaultSavePath(dir))
		return err == nil && strings.Contains(string(data), "session_max_age_days: -1")
	})
	// The merge path (used on the next start) must preserve the negative
	// sentinel instead of mapping it to the default.
	pf, err := projectfile.Load(projectfile.DefaultSavePath(dir))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if merged := projectfile.Merge(pf, projectfile.FlagOverrides{}); merged.SessionMaxAgeDays != -1 {
		t.Fatalf("merged session_max_age_days = %d, want -1", merged.SessionMaxAgeDays)
	}

	// -2 is still rejected (only -1 is the keep-forever sentinel).
	bad := WSMessage{Type: "config", ConfigFields: []string{"sessionMaxAgeDays"}, SessionMaxAgeDays: -2}
	if err := conn.WriteJSON(bad); err != nil {
		t.Fatalf("send config: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "settings" || resp.Success {
		t.Fatalf("-2 notice = %+v, want settings error", resp)
	}
}

// TestRestartStagedConfigViaWS pins the set + prompt restart tier: the
// values land in the runtime overlay and the persisted file, the running
// server config is untouched, a success notice lists the staged settings,
// and the next config push carries them in restartRequired.
func TestRestartStagedConfigViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	if err := conn.WriteJSON(WSMessage{
		Type:         "config",
		ConfigFields: []string{"webBind", "webAuthToken", "mcp"},
		WebBind:      "0.0.0.0:9090",
		WebAuthToken: "new-token",
		MCP:          "on",
	}); err != nil {
		t.Fatalf("send config: %v", err)
	}

	// Success notice listing the staged settings.
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" && m.Success })
	if resp.Kind != "settings" || !strings.Contains(resp.Content, "web_bind") || !strings.Contains(resp.Content, "web_auth_token") {
		t.Fatalf("restart notice = %+v, want success listing web_bind/web_auth_token", resp)
	}

	// Runtime overlay staged; running server config untouched.
	r := s.ws.GetRuntimeConfig()
	if r.WebBind != "0.0.0.0:9090" || r.WebAuthToken != "new-token" || r.MCP != "on" {
		t.Fatalf("runtime overlay = %+v, want staged values", r)
	}
	if s.config.WebBind != "" || s.config.WebAuthToken != "" || s.config.MCP != "" {
		t.Fatalf("running server config mutated: %+v", s.config)
	}

	// The config push advertises the pending restart fields.
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && len(m.RestartRequired) > 0
	})
	joined := strings.Join(cfg.RestartRequired, ",")
	if !strings.Contains(joined, "web_bind") || !strings.Contains(joined, "web_auth_token") || !strings.Contains(joined, "mcp") {
		t.Fatalf("restartRequired = %v, want web_bind/web_auth_token/mcp", cfg.RestartRequired)
	}
	if cfg.WebBind != "0.0.0.0:9090" {
		t.Fatalf("push webBind = %q, want staged value", cfg.WebBind)
	}
	if cfg.WebAuthTokenSet != true {
		t.Fatal("webAuthTokenSet should be true (the token itself is never pushed)")
	}

	// Persisted file carries the staged values for the next start.
	data, err := os.ReadFile(projectfile.DefaultSavePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"web_bind: 0.0.0.0:9090", "web_auth_token: new-token", `mcp: "on"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected %q in persisted config:\n%s", want, data)
		}
	}
}

// TestDefaultProfileSaveViaWS pins provider_save with name "default": the
// default profile (legacy fields) is editable but not deletable, and the
// provider push reflects the new values.
func TestDefaultProfileSaveViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	if err := conn.WriteJSON(WSMessage{Type: "provider_save", ProviderOp: &ProviderOpRequest{
		Name: "default", BaseURL: "https://custom.example.com/v1", APIKey: "new-default-key", Model: "gpt-5",
	}}); err != nil {
		t.Fatalf("send provider_save default: %v", err)
	}
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if len(m.Providers) == 0 {
			return false
		}
		return m.Providers[0].BaseURL == "https://custom.example.com/v1"
	})
	if !cfg.Providers[0].APIKeySet || cfg.Providers[0].Deletable {
		t.Fatalf("default entry = %+v, want key set + not deletable", cfg.Providers[0])
	}

	r := s.ws.GetRuntimeConfig()
	if r.OpenAIURL != "https://custom.example.com/v1" || r.OpenAIKey != "new-default-key" || r.OpenAIModel != "gpt-5" {
		t.Fatalf("runtime default profile = %+v, want applied values", r)
	}
	// The model edit applies LIVE: the workspace default (which seeds new
	// sessions) must follow, not just the persisted overlay.
	if got := s.ws.DefaultModel(); got != "gpt-5" {
		t.Fatalf("workspace default model = %q, want gpt-5 (live edit)", got)
	}

	// A blank key on the default profile keeps the stored key.
	if err := conn.WriteJSON(WSMessage{Type: "provider_save", ProviderOp: &ProviderOpRequest{Name: "default", BaseURL: "https://custom.example.com/v1"}}); err != nil {
		t.Fatalf("send provider_save default (blank key): %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return len(m.Providers) > 0 && m.Providers[0].APIKeySet })
	if r := s.ws.GetRuntimeConfig(); r.OpenAIKey != "new-default-key" {
		t.Fatalf("blank key overwrote the stored default key: %q", r.OpenAIKey)
	}

	// Deleting the default profile is still refused.
	if err := conn.WriteJSON(WSMessage{Type: "provider_delete", ProviderOp: &ProviderOpRequest{Name: "default"}}); err != nil {
		t.Fatalf("send provider_delete default: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "provider" || resp.Success {
		t.Fatalf("default-delete notice = %+v, want provider error", resp)
	}
}

// TestEffectiveConfigOverlaysRuntime pins the persistence snapshot: live
// runtime values (incl. restart-staged) are overlaid onto the startup
// config, so a persist never reverts a modal change.
func TestEffectiveConfigOverlaysRuntime(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	if s.effectiveConfig() == nil {
		t.Fatal("effectiveConfig nil with a startup config")
	}
	r := s.ws.GetRuntimeConfig()
	r.CommandSafetyMode = "allowlist"
	r.WebBind = "0.0.0.0:9090"
	r.SessionMaxCount = 6
	s.ws.SetRuntimeConfig(r)

	eff := s.effectiveConfig()
	if eff.CommandSafetyMode != "allowlist" || eff.WebBind != "0.0.0.0:9090" || eff.SessionMaxCount != 6 {
		t.Fatalf("effectiveConfig = %+v, want runtime overlay", eff)
	}
	// The startup config itself is untouched.
	if s.config.CommandSafetyMode != "" || s.config.WebBind != "" {
		t.Fatalf("startup config mutated: %+v", s.config)
	}
}

// TestWorkspaceRuntimeSeed pins the overlay seeding: nil cfg → zero config,
// non-nil → a copy of the startup config.
func TestWorkspaceRuntimeSeed(t *testing.T) {
	if got := runtimeSeed(nil); got.CommandSafetyMode != "" || got.SessionMaxCount != 0 {
		t.Fatalf("runtimeSeed(nil) = %+v, want zero", got)
	}
	cfg := &config.Config{CommandSafetyMode: "allowlist", SessionMaxCount: 3}
	r := runtimeSeed(cfg)
	if r.CommandSafetyMode != "allowlist" || r.SessionMaxCount != 3 {
		t.Fatalf("runtimeSeed = %+v", r)
	}
	cfg.CommandSafetyMode = "off"
	if r.CommandSafetyMode != "allowlist" {
		t.Fatal("runtimeSeed shares the source struct")
	}
}

// strPtr is a small helper for the pointer-typed WSMessage fields.
func strPtr(s string) *string { return &s }
