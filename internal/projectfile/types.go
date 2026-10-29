package projectfile

// MCPServerEntry describes one MCP stdio server.
type MCPServerEntry struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	// InitTimeoutSecs / ToolTimeoutSecs override the MCP client's default
	// handshake / per-call deadlines for this server (0 = default).
	InitTimeoutSecs int `yaml:"init_timeout_secs"`
	ToolTimeoutSecs int `yaml:"tool_timeout_secs"`
}

// OpenAIProviderEntry describes one registered OpenAI-compatible API
// endpoint in the project file (openai_providers key). An empty base URL
// means the official OpenAI endpoint; an empty API key is valid for
// endpoints that need none.
type OpenAIProviderEntry struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

// FileConfig holds parsed YAML front matter keys.
//
// Field convention: fields whose zero value is meaningful (a real setting
// distinct from "absent") are pointers (*int, *float64); nil means the key
// was not present in the file, so an explicit 0 is preserved through merge.
// All other fields are plain values where the zero value means "use the
// default" (0, "", false).
type FileConfig struct {
	OpenAIAPIKey              string                `yaml:"openai_api_key"`
	OpenAIModel               string                `yaml:"openai_model"`
	OpenAIBaseURL             string                `yaml:"openai_base_url"`
	WorkingDir                string                `yaml:"working_dir"`
	ContextLimit              int                   `yaml:"context_limit"`
	CompactThreshold          *float64              `yaml:"compact_threshold"`            // 0 = auto-compaction disabled
	CompactKeepRecentMessages *int                  `yaml:"compact_keep_recent_messages"` // 0 = keep no recent messages on compaction
	MaxToolResultBytes        *int                  `yaml:"max_tool_result_bytes"`        // 0 = no truncation cap
	CompactReserveTokens      *int                  `yaml:"compact_reserve_tokens"`       // 0 = reserve no tokens
	CompactLastResort         string                `yaml:"compact_last_resort"`          // condense (default) | error
	SmartCompact              string                `yaml:"smart_compact"`                // off (default) | on
	ContextMode               string                `yaml:"context_mode"`                 // default (default) | deep_dive
	CommandSafety             string                `yaml:"command_safety"`
	CommandAllowlist          string                `yaml:"command_allowlist"`
	DeleteApproval            string                `yaml:"delete_approval"`
	CommandApproval           string                `yaml:"command_approval"`
	TreeSitter                string                `yaml:"treesitter"`
	TreeSitterLangs           string                `yaml:"treesitter_langs"`
	CLIVerbose                bool                  `yaml:"cli_verbose"`
	DebugLog                  string                `yaml:"debug_log"`
	DebugSession              string                `yaml:"debug_session"`
	MCP                       string                `yaml:"mcp"`
	MCPToolMode               string                `yaml:"mcp_tool_mode"`
	DebugCompareMessages      bool                  `yaml:"debug_compare_messages"`
	MCPServers                []MCPServerEntry      `yaml:"mcp_servers"`
	OpenAIProviders           []OpenAIProviderEntry `yaml:"openai_providers"`
	TestCommand               string                `yaml:"test_command"`
	LintCommand               string                `yaml:"lint_command"`
	WebFetch                  string                `yaml:"web_fetch"`
	WebSearch                 string                `yaml:"web_search"`
	OutputSpill               string                `yaml:"output_spill"`
	WebSearchBackend          string                `yaml:"web_search_backend"`
	WebSearchAPIKey           string                `yaml:"web_search_api_key"`
	WebAllowedDomains         string                `yaml:"web_allowed_domains"`
	WebAllowedOrigins         string                `yaml:"web_allowed_origins"`
	WebFetchMode              string                `yaml:"web_fetch_mode"`
	WebAuthToken              string                `yaml:"web_auth_token"`
	WebTLSCertFile            string                `yaml:"web_tls_cert_file"`
	WebTLSKeyFile             string                `yaml:"web_tls_key_file"`
	WebBind                   string                `yaml:"web_bind"`
	SessionMaxCount           int                   `yaml:"session_max_count"`
	SessionMaxAgeDays         int                   `yaml:"session_max_age_days"`
	WebMaxActiveSessions      int                   `yaml:"web_max_active_sessions"`
	WebApprovalHoldSecs       int                   `yaml:"web_approval_hold_secs"`
	CommandSandbox            string                `yaml:"command_sandbox"`
	CommandIdleTimeoutSecs    int                   `yaml:"command_idle_timeout_secs"`
	PreserveReasoning         string                `yaml:"preserve_reasoning"`
	Board                     string                `yaml:"board"`
	Subagent                  string                `yaml:"subagent"`
	SubagentMaxDepth          int                   `yaml:"subagent_max_depth"`
	SubagentMaxConcurrent     int                   `yaml:"subagent_max_concurrent"`
	SubagentModel             string                `yaml:"subagent_model"`
	SubagentThinkingLevel     string                `yaml:"subagent_thinking_level"`
	ReviewAgent               string                `yaml:"review_agent"`
	ReviewAgentModel          string                `yaml:"review_agent_model"`
	ReviewAgentThinkingLevel  string                `yaml:"review_agent_thinking_level"`
	BoardStartPrompt          string                `yaml:"board_start_prompt"`
	BoardReviewPrompt         string                `yaml:"board_review_prompt"`
	SystemPrompt              string                `yaml:"system_prompt"`
	SubagentPrompt            string                `yaml:"subagent_prompt"`
	AgentInstructions         string                `yaml:"agent_instructions"`
	Skills                    string                `yaml:"skills"`
	Terminal                  string                `yaml:"terminal"`
	JobNotices                string                `yaml:"job_notices"`
	JoinQueued                string                `yaml:"join_queued"`
	Automations               string                `yaml:"automations"`
	StatusDisplay             string                `yaml:"status_display"`
	StatusDisplayHost         string                `yaml:"status_display_host"`
	StatusDisplayToken        string                `yaml:"status_display_token"`
	StatusDisplaySource       string                `yaml:"status_display_source"`
	// KeepRecentMessages is the pre-rename spelling of
	// compact_keep_recent_messages, accepted for back-compat with a
	// deprecation warning and cleared by parseYAMLFrontMatter after aliasing.
	KeepRecentMessages *int `yaml:"keep_recent_messages"`
	// CommandTimeoutSecs is the pre-rename spelling of
	// command_idle_timeout_secs, accepted for back-compat with a
	// deprecation warning and cleared by parseYAMLFrontMatter after aliasing.
	CommandTimeoutSecs *int `yaml:"command_timeout_secs"`
}

// ProjectFile is a loaded combined config + guidelines file.
type ProjectFile struct {
	Path       string
	HasConfig  bool
	Guidelines string
	Config     FileConfig
}

// FlagOverrides are CLI flag values applied after env/file merge.
type FlagOverrides struct {
	WorkingDir string
	OpenAIURL  string
	CLIVerbose *bool
	WebBind    string
}

// WriteOptions controls SaveConfig output.
type WriteOptions struct {
	IncludeSecrets bool
}

const defaultGuidelinesPlaceholder = "# Project guidelines\n\nAdd agent instructions for this repository here.\n"
