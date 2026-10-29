package projectfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gogen/internal/config"
	"gogen/internal/ioutil"
	"gopkg.in/yaml.v3"
)

// SaveConfig writes effective configuration to cfgPath as pure YAML
// and guidelines to guidelinesPath as markdown. When guidelinesPath is empty,
// only the config file is written (used for global config saves).
func SaveConfig(cfgPath, guidelinesPath string, cfg *config.Config, guidelines string, opts WriteOptions) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	yamlBody, err := buildConfigYAML(cfg, opts)
	if err != nil {
		return err
	}
	if err := writeConfigFile(cfgPath, []byte(yamlBody), opts); err != nil {
		return err
	}

	if guidelinesPath == "" {
		return nil
	}

	if strings.TrimSpace(guidelines) == "" {
		guidelines = defaultGuidelinesPlaceholder
	}
	mdBody := strings.TrimRight(strings.TrimLeft(guidelines, "\n"), "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(guidelinesPath), 0o755); err != nil {
		return err
	}
	if err := ioutil.WriteFileAtomic(guidelinesPath, []byte(mdBody), 0o644); err != nil {
		return err
	}
	return nil
}

// SaveGlobalConfig writes the effective configuration to the global config
// location (~/.config/gogen/config.yaml). No guidelines file is written.
func SaveGlobalConfig(cfg *config.Config, opts WriteOptions) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	yamlBody, err := buildConfigYAML(cfg, opts)
	if err != nil {
		return err
	}
	return writeConfigFile(GlobalConfigPath(), []byte(yamlBody), opts)
}

// writeConfigFile writes a config body. A file that embeds a secret must
// not be world-readable: mode 0600. WriteFileAtomic preserves a
// pre-existing mode, so an earlier 0644 save would survive the rewrite;
// chmod forces 0600 whenever a secret is persisted.
func writeConfigFile(path string, body []byte, opts WriteOptions) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if opts.IncludeSecrets {
		perm = 0o600
	}
	if err := ioutil.WriteFileAtomic(path, body, perm); err != nil {
		return err
	}
	if opts.IncludeSecrets {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("config: restrict permissions on %s: %w", path, err)
		}
	}
	return nil
}

// configYAML is the YAML projection of the effective config written by
// --save-config. Field order defines output order.
//
// The four context settings (CompactThreshold, CompactKeepRecentMessages,
// MaxToolResultBytes, CompactReserveTokens) deliberately have no omitempty:
// their zero value is a real setting (auto-compaction off, no recent messages
// kept, no truncation cap, no reserved tokens), so an explicit 0 must survive
// regeneration. All other fields follow the file convention that an empty or
// zero value means "use the default", so they may be omitted when empty.
// Options added by the runtime-config / feature-flag work additionally omit
// their key when the value equals the built-in default (buildConfigYAML
// normalizes them): a config file never bakes in a default.
type configYAML struct {
	OpenAIAPIKey              string  `yaml:"openai_api_key,omitempty"`
	OpenAIModel               string  `yaml:"openai_model"`
	OpenAIBaseURL             string  `yaml:"openai_base_url"`
	WorkingDir                string  `yaml:"working_dir"`
	ContextLimit              int     `yaml:"context_limit"`
	CompactThreshold          float64 `yaml:"compact_threshold"`
	CompactKeepRecentMessages int     `yaml:"compact_keep_recent_messages"`
	MaxToolResultBytes        int     `yaml:"max_tool_result_bytes"`
	CompactReserveTokens      int     `yaml:"compact_reserve_tokens"`
	CompactLastResort         string  `yaml:"compact_last_resort,omitempty"`
	SmartCompact              string  `yaml:"smart_compact,omitempty"`
	ContextMode               string  `yaml:"context_mode,omitempty"`
	CommandSafety             string  `yaml:"command_safety"`
	CommandAllowlist          string  `yaml:"command_allowlist,omitempty"`
	DeleteApproval            string  `yaml:"delete_approval"`
	CommandApproval           string  `yaml:"command_approval,omitempty"`
	CommandSandbox            string  `yaml:"command_sandbox,omitempty"`
	CommandIdleTimeoutSecs    int     `yaml:"command_idle_timeout_secs,omitempty"`
	TreeSitter                string  `yaml:"treesitter"`
	TreeSitterLangs           string  `yaml:"treesitter_langs,omitempty"`
	TestCommand               string  `yaml:"test_command,omitempty"`
	LintCommand               string  `yaml:"lint_command,omitempty"`
	CLIVerbose                bool    `yaml:"cli_verbose"`
	DebugLog                  string  `yaml:"debug_log,omitempty"`
	DebugSession              string  `yaml:"debug_session,omitempty"`
	MCP                       string  `yaml:"mcp"`
	PreserveReasoning         string  `yaml:"preserve_reasoning,omitempty"`
	Board                     string  `yaml:"board,omitempty"`
	Subagent                  string  `yaml:"subagent,omitempty"`
	SubagentMaxDepth          int     `yaml:"subagent_max_depth,omitempty"`
	SubagentMaxConcurrent     int     `yaml:"subagent_max_concurrent,omitempty"`
	SubagentModel             string  `yaml:"subagent_model,omitempty"`
	SubagentThinkingLevel     string  `yaml:"subagent_thinking_level,omitempty"`
	ReviewAgent               string  `yaml:"review_agent,omitempty"`
	ReviewAgentModel          string  `yaml:"review_agent_model,omitempty"`
	ReviewAgentThinkingLevel  string  `yaml:"review_agent_thinking_level,omitempty"`
	BoardStartPrompt          string  `yaml:"board_start_prompt,omitempty"`
	BoardReviewPrompt         string  `yaml:"board_review_prompt,omitempty"`
	SystemPrompt              string  `yaml:"system_prompt,omitempty"`
	SubagentPrompt            string  `yaml:"subagent_prompt,omitempty"`
	AgentInstructions         string  `yaml:"agent_instructions,omitempty"`
	Skills                    string  `yaml:"skills,omitempty"`
	Terminal                  string  `yaml:"terminal,omitempty"`
	JobNotices                string  `yaml:"job_notices,omitempty"`
	JoinQueued                string  `yaml:"join_queued,omitempty"`
	Automations               string  `yaml:"automations,omitempty"`
	StatusDisplay             string  `yaml:"status_display,omitempty"`
	StatusDisplayHost         string  `yaml:"status_display_host,omitempty"`
	StatusDisplayToken        string  `yaml:"status_display_token,omitempty"`
	StatusDisplaySource       string  `yaml:"status_display_source,omitempty"`
	OutputSpill               string  `yaml:"output_spill,omitempty"`
	SessionMaxCount           int     `yaml:"session_max_count,omitempty"`
	SessionMaxAgeDays         int     `yaml:"session_max_age_days,omitempty"`
	WebMaxActiveSessions      int     `yaml:"web_max_active_sessions,omitempty"`
	WebApprovalHoldSecs       int     `yaml:"web_approval_hold_secs,omitempty"`
	WebBind                   string  `yaml:"web_bind,omitempty"`
	WebAllowedOrigins         string  `yaml:"web_allowed_origins,omitempty"`
	WebAuthToken              string  `yaml:"web_auth_token,omitempty"`
	WebTLSCertFile            string  `yaml:"web_tls_cert_file,omitempty"`
	WebTLSKeyFile             string  `yaml:"web_tls_key_file,omitempty"`
	// The web content group (fetch/search) is read from the file and settable
	// from the settings modal, and used to be MISSING here: a save rewrote the
	// config without those keys, silently erasing the user's choices (and the
	// search API key, which --save-config-secrets must persist).
	WebFetch             string                        `yaml:"web_fetch,omitempty"`
	WebSearch            string                        `yaml:"web_search,omitempty"`
	WebSearchBackend     string                        `yaml:"web_search_backend,omitempty"`
	WebSearchAPIKey      string                        `yaml:"web_search_api_key,omitempty"`
	WebAllowedDomains    string                        `yaml:"web_allowed_domains,omitempty"`
	WebFetchMode         string                        `yaml:"web_fetch_mode,omitempty"`
	MCPToolMode          string                        `yaml:"mcp_tool_mode,omitempty"`
	DebugCompareMessages bool                          `yaml:"debug_compare_messages,omitempty"`
	MCPServers           []config.MCPServerConfig      `yaml:"mcp_servers,omitempty"`
	OpenAIProviders      []config.OpenAIProviderConfig `yaml:"openai_providers,omitempty"`
}

// buildConfigYAML renders the effective config as a YAML document. Secrets
// (openai_api_key, web_auth_token, MCP server env, provider api_key) are
// included only when opts.IncludeSecrets is set; preserve_reasoning is
// omitted when it is the default "auto"; the runtime-config / feature-flag
// options are omitted when they equal their built-in default (the settings
// table's omitDefault flag, see settings.go).
func buildConfigYAML(cfg *config.Config, opts WriteOptions) (string, error) {
	def := config.Defaults()
	// One loop over the settings table (settings.go): every row that has a
	// configYAML field is written here, with its own omission rule and secret
	// gate. The saved key ORDER is the struct's field order — the loop fills
	// fields by name and never reorders — so existing config files keep their
	// layout.
	out := configYAML{}
	writeSettings(&out, cfg, &def, opts.IncludeSecrets)
	// openai_api_key, web_auth_token, status_display_token and
	// web_search_api_key are all written by the loop above via their
	// `secret` flag: persisted only with --save-config-secrets (a forced
	// provider/modal save sets it), never echoed by a plain save.
	// The two list settings (and preserve_reasoning below) are the rows the
	// settings table marks writeSpecial: their persistence needs more than a
	// value copy, so they are handled here rather than by the generic loop.
	out.OpenAIProviders = cfg.OpenAIProviders
	if !opts.IncludeSecrets && len(cfg.OpenAIProviders) > 0 {
		// Copy without api_key so provider keys are never persisted without
		// --save-config-secrets. The slice shares its backing array with
		// cfg, so a fresh copy is required.
		out.OpenAIProviders = make([]config.OpenAIProviderConfig, len(cfg.OpenAIProviders))
		for i, p := range cfg.OpenAIProviders {
			out.OpenAIProviders[i] = config.OpenAIProviderConfig{
				Name:    p.Name,
				BaseURL: p.BaseURL,
				Model:   p.Model,
				// APIKey intentionally empty: persisted only with --save-config-secrets.
			}
		}
	}
	if mode := strings.ToLower(strings.TrimSpace(cfg.PreserveReasoning)); mode != "" && mode != "auto" {
		out.PreserveReasoning = mode
	}
	out.MCPServers = cfg.MCPServers
	if !opts.IncludeSecrets && len(cfg.MCPServers) > 0 {
		// Copy without env so secrets are never persisted. The slice shares
		// its backing array with cfg, so a fresh copy is required.
		out.MCPServers = make([]config.MCPServerConfig, len(cfg.MCPServers))
		for i, s := range cfg.MCPServers {
			out.MCPServers[i] = config.MCPServerConfig{
				Name:            s.Name,
				Command:         s.Command,
				Args:            append([]string(nil), s.Args...),
				InitTimeoutSecs: s.InitTimeoutSecs,
				ToolTimeoutSecs: s.ToolTimeoutSecs,
				// Env intentionally nil: persisted only with --save-config-secrets.
			}
		}
	}

	body, err := yaml.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("marshal config YAML: %w", err)
	}
	return string(body), nil
}
