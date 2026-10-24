package projectfile

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"

	"gogen/internal/config"
	"gogen/internal/onoff"
)

// Merge builds the effective runtime config: env > flags > file > defaults.
// CLI flag overrides (FlagOverrides) are applied only when the corresponding
// environment variable is not set, so env vars cannot be overridden by flags.
func Merge(pf *ProjectFile, flags FlagOverrides) *config.Config {
	def := config.Defaults()
	var file FileConfig
	if pf != nil {
		file = pf.Config
	}

	cfg := &config.Config{
		OpenAIKey:                 mergeString("OPENAI_API_KEY", file.OpenAIAPIKey, def.OpenAIKey),
		OpenAIModel:               mergeString("OPENAI_MODEL", file.OpenAIModel, def.OpenAIModel),
		OpenAIURL:                 mergeString("OPENAI_BASE_URL", file.OpenAIBaseURL, def.OpenAIURL),
		WorkingDir:                mergeString("GOGEN_WORKING_DIR", file.WorkingDir, def.WorkingDir),
		ContextLimit:              mergeInt("GOGEN_CONTEXT_LIMIT", file.ContextLimit, def.ContextLimit),
		CompactThreshold:          mergeFloatOpt("GOGEN_COMPACT_THRESHOLD", file.CompactThreshold, def.CompactThreshold),
		CompactKeepRecentMessages: mergeCompactKeepRecentMessages(file, def.CompactKeepRecentMessages),
		MaxToolResultBytes:        mergeIntOpt("GOGEN_MAX_TOOL_RESULT_BYTES", file.MaxToolResultBytes, def.MaxToolResultBytes),
		CompactReserveTokens:      mergeIntOpt("GOGEN_COMPACT_RESERVE_TOKENS", file.CompactReserveTokens, def.CompactReserveTokens),
		CompactLastResort:         mergeString("GOGEN_COMPACT_LAST_RESORT", file.CompactLastResort, def.CompactLastResort),
		CommandSafetyMode:         mergeString("GOGEN_COMMAND_SAFETY", file.CommandSafety, def.CommandSafetyMode),
		CommandAllowlist:          mergeString("GOGEN_COMMAND_ALLOWLIST", file.CommandAllowlist, def.CommandAllowlist),
		DeleteApproval:            mergeString("GOGEN_DELETE_APPROVAL", file.DeleteApproval, def.DeleteApproval),
		TreeSitter:                mergeString("GOGEN_TREESITTER", file.TreeSitter, def.TreeSitter),
		TreeSitterLangs:           mergeString("GOGEN_TREESITTER_LANGS", file.TreeSitterLangs, def.TreeSitterLangs),
		CLIVerbose:                mergeBool("GOGEN_CLI_VERBOSE", file.CLIVerbose, def.CLIVerbose),
		DebugLog:                  mergeString("GOGEN_DEBUG_LOG", file.DebugLog, def.DebugLog),
		DebugSession:              mergeString("GOGEN_DEBUG_SESSION", file.DebugSession, def.DebugSession),
		MCP:                       mergeString("GOGEN_MCP", file.MCP, def.MCP),
		MCPToolMode:               mergeString("GOGEN_MCP_TOOL_MODE", file.MCPToolMode, def.MCPToolMode),
		DebugCompareMessages:      mergeBool("GOGEN_DEBUG_COMPARE_MESSAGES", file.DebugCompareMessages, def.DebugCompareMessages),
		MCPServers:                mergeMCPServers(file),
		OpenAIProviders:           mergeOpenAIProviders(file),
		TestCommand:               mergeString("", file.TestCommand, ""),
		LintCommand:               mergeString("", file.LintCommand, ""),
		WebBind:                   mergeString("GOGEN_WEB_BIND", file.WebBind, def.WebBind),
		WebAllowedOrigins:         mergeString("GOGEN_WEB_ALLOWED_ORIGINS", file.WebAllowedOrigins, def.WebAllowedOrigins),
		WebAuthToken:              mergeString("GOGEN_WEB_TOKEN", file.WebAuthToken, def.WebAuthToken),
		WebTLSCertFile:            mergeString("GOGEN_WEB_TLS_CERT", file.WebTLSCertFile, def.WebTLSCertFile),
		WebTLSKeyFile:             mergeString("GOGEN_WEB_TLS_KEY", file.WebTLSKeyFile, def.WebTLSKeyFile),
		SessionMaxCount:           mergeInt("GOGEN_SESSION_MAX_COUNT", file.SessionMaxCount, def.SessionMaxCount),
		SessionMaxAgeDays:         mergeIntKeepNegative("GOGEN_SESSION_MAX_AGE_DAYS", file.SessionMaxAgeDays, def.SessionMaxAgeDays),
		WebMaxActiveSessions:      mergeInt("GOGEN_WEB_MAX_ACTIVE_SESSIONS", file.WebMaxActiveSessions, def.WebMaxActiveSessions),
		WebApprovalHoldSecs:       mergeInt("GOGEN_WEB_APPROVAL_HOLD_SECS", file.WebApprovalHoldSecs, def.WebApprovalHoldSecs),
		WebFetch:                  mergeString("GOGEN_WEB_FETCH", file.WebFetch, def.WebFetch),
		WebSearch:                 mergeString("GOGEN_WEB_SEARCH", file.WebSearch, def.WebSearch),
		OutputSpill:               mergeString("GOGEN_OUTPUT_SPILL", file.OutputSpill, def.OutputSpill),
		WebSearchBackend:          mergeString("GOGEN_WEB_SEARCH_BACKEND", file.WebSearchBackend, def.WebSearchBackend),
		WebSearchAPIKey:           mergeString("GOGEN_WEB_SEARCH_API_KEY", file.WebSearchAPIKey, def.WebSearchAPIKey),
		WebAllowedDomains:         mergeString("GOGEN_WEB_ALLOWED_DOMAINS", file.WebAllowedDomains, def.WebAllowedDomains),
		WebFetchMode:              mergeString("GOGEN_WEB_FETCH_MODE", file.WebFetchMode, def.WebFetchMode),
		CommandSandbox:            mergeString("GOGEN_COMMAND_SANDBOX", file.CommandSandbox, def.CommandSandbox),
		CommandIdleTimeoutSecs:    mergeCommandIdleTimeout(file, def.CommandIdleTimeoutSecs),
		PreserveReasoning:         mergeString("GOGEN_PRESERVE_REASONING", file.PreserveReasoning, def.PreserveReasoning),
		Board:                     mergeString("GOGEN_BOARD", file.Board, def.Board),
		Subagent:                  mergeString("GOGEN_SUBAGENT", file.Subagent, def.Subagent),
		SubagentMaxDepth:          mergeInt("GOGEN_SUBAGENT_MAX_DEPTH", file.SubagentMaxDepth, def.SubagentMaxDepth),
		SubagentMaxConcurrent:     mergeInt("GOGEN_SUBAGENT_MAX_CONCURRENT", file.SubagentMaxConcurrent, def.SubagentMaxConcurrent),
		SubagentModel:             mergeString("GOGEN_SUBAGENT_MODEL", file.SubagentModel, def.SubagentModel),
		SubagentThinkingLevel:     mergeString("GOGEN_SUBAGENT_THINKING_LEVEL", file.SubagentThinkingLevel, def.SubagentThinkingLevel),
		ReviewAgent:               mergeString("GOGEN_REVIEW_AGENT", file.ReviewAgent, def.ReviewAgent),
		ReviewAgentModel:          mergeString("GOGEN_REVIEW_AGENT_MODEL", file.ReviewAgentModel, def.ReviewAgentModel),
		ReviewAgentThinkingLevel:  mergeString("GOGEN_REVIEW_AGENT_THINKING_LEVEL", file.ReviewAgentThinkingLevel, def.ReviewAgentThinkingLevel),
		BoardStartPrompt:          mergeString("GOGEN_BOARD_START_PROMPT", file.BoardStartPrompt, def.BoardStartPrompt),
		BoardReviewPrompt:         mergeString("GOGEN_BOARD_REVIEW_PROMPT", file.BoardReviewPrompt, def.BoardReviewPrompt),
		SystemPrompt:              mergeString("GOGEN_SYSTEM_PROMPT", file.SystemPrompt, def.SystemPrompt),
		SubagentPrompt:            mergeString("GOGEN_SUBAGENT_PROMPT", file.SubagentPrompt, def.SubagentPrompt),
		AgentInstructions:         mergeString("GOGEN_AGENT_INSTRUCTIONS", file.AgentInstructions, def.AgentInstructions),
		Skills:                    mergeString("GOGEN_SKILLS", file.Skills, def.Skills),
		Terminal:                  mergeString("GOGEN_TERMINAL", file.Terminal, def.Terminal),
		JobNotices:                mergeString("GOGEN_JOB_NOTICES", file.JobNotices, def.JobNotices),
		Automations:               mergeString("GOGEN_AUTOMATIONS", file.Automations, def.Automations),
		StatusDisplay:             mergeString("GOGEN_STATUS_DISPLAY", file.StatusDisplay, def.StatusDisplay),
		StatusDisplayHost:         mergeString("GOGEN_STATUS_DISPLAY_HOST", file.StatusDisplayHost, def.StatusDisplayHost),
		StatusDisplayToken:        mergeString("GOGEN_STATUS_DISPLAY_TOKEN", file.StatusDisplayToken, def.StatusDisplayToken),
		StatusDisplaySource:       mergeString("GOGEN_STATUS_DISPLAY_SOURCE", file.StatusDisplaySource, def.StatusDisplaySource),
	}

	if flags.WorkingDir != "" {
		if _, ok := os.LookupEnv("GOGEN_WORKING_DIR"); !ok {
			cfg.WorkingDir = flags.WorkingDir
		}
	}
	if flags.OpenAIURL != "" {
		if _, ok := os.LookupEnv("OPENAI_BASE_URL"); !ok {
			cfg.OpenAIURL = flags.OpenAIURL
		}
	}
	if flags.CLIVerbose != nil {
		if _, ok := os.LookupEnv("GOGEN_CLI_VERBOSE"); !ok {
			cfg.CLIVerbose = *flags.CLIVerbose
		}
	}
	if flags.WebBind != "" {
		if _, ok := os.LookupEnv("GOGEN_WEB_BIND"); !ok {
			cfg.WebBind = flags.WebBind
		}
	}

	return cfg
}

func mergeMCPServers(file FileConfig) []config.MCPServerConfig {
	if _, ok := os.LookupEnv("GOGEN_MCP_SERVERS"); ok {
		raw := os.Getenv("GOGEN_MCP_SERVERS")
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		var servers []config.MCPServerConfig
		if err := json.Unmarshal([]byte(raw), &servers); err != nil {
			log.Printf("warning: GOGEN_MCP_SERVERS is not a valid JSON array; ignoring it: %v", err)
			return nil
		}
		return servers
	}
	if file.MCPServers != nil {
		out := make([]config.MCPServerConfig, len(file.MCPServers))
		for i, s := range file.MCPServers {
			out[i] = config.MCPServerConfig{
				Name:            s.Name,
				Command:         s.Command,
				Args:            append([]string(nil), s.Args...),
				Env:             cloneStringMap(s.Env),
				InitTimeoutSecs: s.InitTimeoutSecs,
				ToolTimeoutSecs: s.ToolTimeoutSecs,
			}
		}
		return out
	}
	return nil
}

// mergeOpenAIProviders merges the registered OpenAI-compatible provider
// list: the GOGEN_OPENAI_PROVIDERS JSON env var wins when set, otherwise the
// file's openai_providers list is used (converted to config entries).
// An explicitly empty env value clears the list.
func mergeOpenAIProviders(file FileConfig) []config.OpenAIProviderConfig {
	if _, ok := os.LookupEnv("GOGEN_OPENAI_PROVIDERS"); ok {
		raw := os.Getenv("GOGEN_OPENAI_PROVIDERS")
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		var providers []config.OpenAIProviderConfig
		if err := json.Unmarshal([]byte(raw), &providers); err != nil {
			log.Printf("warning: GOGEN_OPENAI_PROVIDERS is not a valid JSON array; ignoring it: %v", err)
			return nil
		}
		return providers
	}
	if file.OpenAIProviders != nil {
		out := make([]config.OpenAIProviderConfig, len(file.OpenAIProviders))
		for i, p := range file.OpenAIProviders {
			out[i] = config.OpenAIProviderConfig{
				Name:    p.Name,
				BaseURL: p.BaseURL,
				APIKey:  p.APIKey,
				Model:   p.Model,
			}
		}
		return out
	}
	return nil
}

// mergeCompactKeepRecentMessages merges the compact_keep_recent_messages
// setting, honoring the pre-rename spellings for back-compat:
// GOGEN_KEEP_RECENT_MESSAGES (env) and keep_recent_messages (file, already
// aliased onto CompactKeepRecentMessages by parseYAMLFrontMatter). The
// renamed key/env win when both are present; the legacy env var logs a
// deprecation warning instead of being silently dropped.
func mergeCompactKeepRecentMessages(file FileConfig, def int) int {
	if v, ok := envInt("GOGEN_COMPACT_KEEP_RECENT_MESSAGES", def); ok {
		return v
	}
	if v, ok := os.LookupEnv("GOGEN_KEEP_RECENT_MESSAGES"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			log.Printf("warning: GOGEN_KEEP_RECENT_MESSAGES=%q is not a valid integer; using default %d", v, def)
			return def
		}
		log.Printf("warning: GOGEN_KEEP_RECENT_MESSAGES is deprecated; use GOGEN_COMPACT_KEEP_RECENT_MESSAGES (value %d used)", n)
		return n
	}
	if file.CompactKeepRecentMessages != nil {
		return *file.CompactKeepRecentMessages
	}
	return def
}

// mergeCommandIdleTimeout merges the command_idle_timeout_secs setting,
// honoring the pre-rename GOGEN_COMMAND_TIMEOUT_SECS env var for back-compat
// (the file key command_timeout_secs is already aliased onto
// CommandIdleTimeoutSecs by parseYAMLFrontMatter). The renamed env var wins
// when both are set; the legacy env var logs a deprecation warning instead
// of being silently dropped. Precedence: env > file > default.
func mergeCommandIdleTimeout(file FileConfig, def int) int {
	if v, ok := envInt("GOGEN_COMMAND_IDLE_TIMEOUT_SECS", def); ok {
		return v
	}
	if v, ok := os.LookupEnv("GOGEN_COMMAND_TIMEOUT_SECS"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			log.Printf("warning: GOGEN_COMMAND_TIMEOUT_SECS=%q is not a valid integer; using default %d", v, def)
			return def
		}
		log.Printf("warning: GOGEN_COMMAND_TIMEOUT_SECS is deprecated; use GOGEN_COMMAND_IDLE_TIMEOUT_SECS (value %d used)", n)
		return n
	}
	return config.Effective(file.CommandIdleTimeoutSecs, def)
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func envInt(envKey string, def int) (int, bool) {
	v, ok := os.LookupEnv(envKey)
	if !ok {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n, true
	}
	log.Printf("warning: %s=%q is not a valid integer; using default %d", envKey, v, def)
	return def, true
}

func envFloat(envKey string, def float64) (float64, bool) {
	v, ok := os.LookupEnv(envKey)
	if !ok {
		return 0, false
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f, true
	}
	log.Printf("warning: %s=%q is not a valid number; using default %g", envKey, v, def)
	return def, true
}

func envBool(envKey string, def bool) (bool, bool) {
	v, ok := os.LookupEnv(envKey)
	if !ok {
		return false, false
	}
	on, ok := onoff.Parse(v)
	if !ok {
		log.Printf("warning: %s=%q is not a valid boolean; using default %t", envKey, v, def)
		return def, true
	}
	return on, true
}

// mergeString returns the env value when set, the file value when non-empty,
// or the default.
func mergeString(envKey string, fileVal, def string) string {
	if v, ok := os.LookupEnv(envKey); ok {
		return v
	}
	if fileVal != "" {
		return fileVal
	}
	return def
}

// mergeInt returns the env value when set, the file value when non-zero, or
// the default.
func mergeInt(envKey string, fileVal, def int) int {
	if v, ok := envInt(envKey, def); ok {
		return v
	}
	return config.Effective(fileVal, def)
}

// mergeIntKeepNegative is mergeInt for the one field where a negative value
// is meaningful: session_max_age_days -1 = "keep sessions forever" (the
// store's retention sentinel). config.Effective would map any value <= 0 to
// the default, which would silently convert a persisted -1 back to the
// default on the next start.
func mergeIntKeepNegative(envKey string, fileVal, def int) int {
	if v, ok := envInt(envKey, def); ok {
		return v
	}
	if fileVal < 0 {
		return fileVal
	}
	return config.Effective(fileVal, def)
}

// mergeIntOpt is like mergeInt for pointer fields: a non-nil pointer (an
// explicitly present key, including an explicit 0) wins over the default.
func mergeIntOpt(envKey string, fileVal *int, def int) int {
	if v, ok := envInt(envKey, def); ok {
		return v
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

// mergeFloatOpt is like mergeFloat for pointer fields.
func mergeFloatOpt(envKey string, fileVal *float64, def float64) float64 {
	if v, ok := envFloat(envKey, def); ok {
		return v
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

// mergeBool returns the env value when set, the file value when true, or the
// default. Both boolean settings default to false, so an explicit false in
// the file is equivalent to absent.
func mergeBool(envKey string, fileVal, def bool) bool {
	if v, ok := envBool(envKey, def); ok {
		return v
	}
	if fileVal {
		return true
	}
	return def
}
