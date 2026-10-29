package projectfile

import (
	"reflect"

	"gogen/internal/config"
)

// This file is the SINGLE declaration of every configuration setting.
//
// A setting used to be declared in up to six places, each of which had to be
// edited by hand when a setting was added:
//
//	1. config.Config          the typed runtime field
//	2. config.Defaults()      its default (still hand-written, but every row's
//	                          default is read from it by name, and
//	                          TestSettingsCoverConfig asserts every field has
//	                          one)
//	3. FileConfig             the YAML key it is read from
//	4. merge.go               env > file > default precedence
//	5. write.go               the YAML key it is written back to, and whether
//	                          a default value is omitted (configYAML struct)
//	6. config_registry.go     the client-editable projection (server package)
//
// Places 3–5 are now derived from the table below: Merge and buildConfigYAML
// are loops over it, so a setting cannot be read but not written, or written
// with a different default rule than it is read with. That gap was not
// hypothetical — web_fetch, web_search, web_search_backend,
// web_search_api_key, web_allowed_domains, web_fetch_mode and
// debug_compare_messages were all merged but never written, so any
// --save-config or settings-modal save silently erased them from the user's
// config file (including the search API key that --save-config-secrets is
// supposed to persist).
//
// Place 6 (the server's fieldSpec registry) stays where it is: it owns
// validation and live application rather than persistence, and not every
// setting is client-editable (working_dir, test_command and the secrets are
// deliberately not exposed there). A row added above still reaches the client
// only if the registry gets its own entry, so that is the one manual step left.

// kind selects a setting's merge conversion and omission rule.
type kind int

const (
	// kString, kInt, kBool: plain FileConfig fields, where the zero value
	// means "not set" (see FileConfig's field convention).
	kString kind = iota
	kInt
	kBool
	// kIntPtr and kFloatPtr: pointer FileConfig fields, where an explicit 0
	// is a real setting ("compaction off", "no truncation cap", …), so a
	// non-nil pointer always wins over the default.
	kIntPtr
	kFloatPtr
	// kIntKeepNegative: like kInt, but a negative file value is preserved
	// (session_max_age_days -1 = keep sessions forever).
	kIntKeepNegative
	// The four settings with bespoke merge semantics; the merge function is
	// named after the kind's setting.
	kKeepRecent
	kCommandIdleTimeout
	kMCPServers
	kOpenAIProviders
	// kLegacy: a pre-rename spelling that parseYAMLFrontMatter aliases onto a
	// renamed field. Read for back-compat, never written.
	kLegacy
)

// setting is one configuration setting.
type setting struct {
	// key is the YAML key in the config file (and in the saved file).
	key string
	// env is the environment variable that overrides the file value. Empty
	// means the setting has no env override (test_command, lint_command).
	// Most are "GOGEN_" + the key in upper case, but the historical names are
	// kept verbatim (OPENAI_*, GOGEN_WEB_TOKEN, GOGEN_WEB_TLS_CERT, …): these
	// are a user-facing interface and renaming one would silently break every
	// script that sets it.
	env string
	// cfg is the config.Config field the merged value lands in ("" for
	// kLegacy rows, which are aliased before merge).
	cfg string
	// file is the FileConfig field the value is read from.
	file string
	// writer is the configYAML field the value is written to. Empty means the
	// setting is not persisted (legacy aliases only).
	writer string
	// kind is the merge conversion (see kind).
	kind kind
	// secret marks a value that is written only with IncludeSecrets.
	secret bool
	// omitDefault marks a setting whose key is omitted when the value equals
	// the built-in default, so a config file never bakes in a default and a
	// future default change reaches users who never customized it. Settings
	// without this keep the historical always-write convention… except that
	// the configYAML struct's own `omitempty` still drops empty values, which
	// is the file convention for "use the default".
	omitDefault bool
	// writeSpecial marks a setting whose persistence needs per-setting logic
	// beyond value/no-value (preserve_reasoning normalizes case and omits
	// "auto"; the provider and MCP lists need secret stripping). Those run in
	// buildConfigYAML after the generic loop.
	writeSpecial bool
}

// settings is the ordered declaration of every setting. The order is the order
// Merge applies them in and the order the tests report drift in; it does NOT
// control the saved YAML's key order, which stays the configYAML struct's
// field order (a test asserts the two agree, so neither can silently reorder).
var settings = []setting{
	// --- core / provider -------------------------------------------------
	{key: "openai_api_key", env: "OPENAI_API_KEY", cfg: "OpenAIKey", file: "OpenAIAPIKey", writer: "OpenAIAPIKey", kind: kString, secret: true},
	{key: "openai_model", env: "OPENAI_MODEL", cfg: "OpenAIModel", file: "OpenAIModel", writer: "OpenAIModel", kind: kString},
	{key: "openai_base_url", env: "OPENAI_BASE_URL", cfg: "OpenAIURL", file: "OpenAIBaseURL", writer: "OpenAIBaseURL", kind: kString},
	{key: "working_dir", env: "GOGEN_WORKING_DIR", cfg: "WorkingDir", file: "WorkingDir", writer: "WorkingDir", kind: kString},
	// The two JSON list settings carry their env override (GOGEN_OPENAI_PROVIDERS,
	// GOGEN_MCP_SERVERS) inside the bespoke merge, so `env` stays empty here.
	{key: "openai_providers", cfg: "OpenAIProviders", file: "OpenAIProviders", writer: "OpenAIProviders", kind: kOpenAIProviders, writeSpecial: true},

	// --- context management ----------------------------------------------
	{key: "context_limit", env: "GOGEN_CONTEXT_LIMIT", cfg: "ContextLimit", file: "ContextLimit", writer: "ContextLimit", kind: kInt},
	{key: "compact_threshold", env: "GOGEN_COMPACT_THRESHOLD", cfg: "CompactThreshold", file: "CompactThreshold", writer: "CompactThreshold", kind: kFloatPtr},
	{key: "compact_keep_recent_messages", cfg: "CompactKeepRecentMessages", file: "CompactKeepRecentMessages", writer: "CompactKeepRecentMessages", kind: kKeepRecent},
	{key: "max_tool_result_bytes", env: "GOGEN_MAX_TOOL_RESULT_BYTES", cfg: "MaxToolResultBytes", file: "MaxToolResultBytes", writer: "MaxToolResultBytes", kind: kIntPtr},
	{key: "compact_reserve_tokens", env: "GOGEN_COMPACT_RESERVE_TOKENS", cfg: "CompactReserveTokens", file: "CompactReserveTokens", writer: "CompactReserveTokens", kind: kIntPtr},
	{key: "compact_last_resort", env: "GOGEN_COMPACT_LAST_RESORT", cfg: "CompactLastResort", file: "CompactLastResort", writer: "CompactLastResort", kind: kString, omitDefault: true},
	{key: "smart_compact", env: "GOGEN_SMART_COMPACT", cfg: "SmartCompact", file: "SmartCompact", writer: "SmartCompact", kind: kString, omitDefault: true},
	{key: "context_mode", env: "GOGEN_CONTEXT_MODE", cfg: "ContextMode", file: "ContextMode", writer: "ContextMode", kind: kString, omitDefault: true},

	// --- command safety ---------------------------------------------------
	{key: "command_safety", env: "GOGEN_COMMAND_SAFETY", cfg: "CommandSafetyMode", file: "CommandSafety", writer: "CommandSafety", kind: kString},
	{key: "command_allowlist", env: "GOGEN_COMMAND_ALLOWLIST", cfg: "CommandAllowlist", file: "CommandAllowlist", writer: "CommandAllowlist", kind: kString},
	{key: "delete_approval", env: "GOGEN_DELETE_APPROVAL", cfg: "DeleteApproval", file: "DeleteApproval", writer: "DeleteApproval", kind: kString},
	{key: "command_approval", env: "GOGEN_COMMAND_APPROVAL", cfg: "CommandApproval", file: "CommandApproval", writer: "CommandApproval", kind: kString, omitDefault: true},
	{key: "command_sandbox", env: "GOGEN_COMMAND_SANDBOX", cfg: "CommandSandbox", file: "CommandSandbox", writer: "CommandSandbox", kind: kString, omitDefault: true},
	{key: "command_idle_timeout_secs", cfg: "CommandIdleTimeoutSecs", file: "CommandIdleTimeoutSecs", writer: "CommandIdleTimeoutSecs", kind: kCommandIdleTimeout, omitDefault: true},

	// --- code navigation / logging ---------------------------------------
	{key: "treesitter", env: "GOGEN_TREESITTER", cfg: "TreeSitter", file: "TreeSitter", writer: "TreeSitter", kind: kString},
	{key: "treesitter_langs", env: "GOGEN_TREESITTER_LANGS", cfg: "TreeSitterLangs", file: "TreeSitterLangs", writer: "TreeSitterLangs", kind: kString},
	{key: "test_command", cfg: "TestCommand", file: "TestCommand", writer: "TestCommand", kind: kString},
	{key: "lint_command", cfg: "LintCommand", file: "LintCommand", writer: "LintCommand", kind: kString},
	{key: "cli_verbose", env: "GOGEN_CLI_VERBOSE", cfg: "CLIVerbose", file: "CLIVerbose", writer: "CLIVerbose", kind: kBool},
	{key: "debug_log", env: "GOGEN_DEBUG_LOG", cfg: "DebugLog", file: "DebugLog", writer: "DebugLog", kind: kString},
	{key: "debug_session", env: "GOGEN_DEBUG_SESSION", cfg: "DebugSession", file: "DebugSession", writer: "DebugSession", kind: kString},
	{key: "debug_compare_messages", env: "GOGEN_DEBUG_COMPARE_MESSAGES", cfg: "DebugCompareMessages", file: "DebugCompareMessages", writer: "DebugCompareMessages", kind: kBool},

	// --- MCP --------------------------------------------------------------
	{key: "mcp", env: "GOGEN_MCP", cfg: "MCP", file: "MCP", writer: "MCP", kind: kString},
	{key: "mcp_tool_mode", env: "GOGEN_MCP_TOOL_MODE", cfg: "MCPToolMode", file: "MCPToolMode", writer: "MCPToolMode", kind: kString, omitDefault: true},
	{key: "mcp_servers", cfg: "MCPServers", file: "MCPServers", writer: "MCPServers", kind: kMCPServers, writeSpecial: true},

	// --- web host ---------------------------------------------------------
	{key: "web_bind", env: "GOGEN_WEB_BIND", cfg: "WebBind", file: "WebBind", writer: "WebBind", kind: kString, omitDefault: true},
	{key: "web_allowed_origins", env: "GOGEN_WEB_ALLOWED_ORIGINS", cfg: "WebAllowedOrigins", file: "WebAllowedOrigins", writer: "WebAllowedOrigins", kind: kString},
	{key: "web_auth_token", env: "GOGEN_WEB_TOKEN", cfg: "WebAuthToken", file: "WebAuthToken", writer: "WebAuthToken", kind: kString, secret: true},
	{key: "web_tls_cert_file", env: "GOGEN_WEB_TLS_CERT", cfg: "WebTLSCertFile", file: "WebTLSCertFile", writer: "WebTLSCertFile", kind: kString},
	{key: "web_tls_key_file", env: "GOGEN_WEB_TLS_KEY", cfg: "WebTLSKeyFile", file: "WebTLSKeyFile", writer: "WebTLSKeyFile", kind: kString},
	{key: "web_max_active_sessions", env: "GOGEN_WEB_MAX_ACTIVE_SESSIONS", cfg: "WebMaxActiveSessions", file: "WebMaxActiveSessions", writer: "WebMaxActiveSessions", kind: kInt, omitDefault: true},
	{key: "web_approval_hold_secs", env: "GOGEN_WEB_APPROVAL_HOLD_SECS", cfg: "WebApprovalHoldSecs", file: "WebApprovalHoldSecs", writer: "WebApprovalHoldSecs", kind: kInt, omitDefault: true},

	// --- web content (fetch / search) -------------------------------------
	{key: "web_fetch", env: "GOGEN_WEB_FETCH", cfg: "WebFetch", file: "WebFetch", writer: "WebFetch", kind: kString, omitDefault: true},
	{key: "web_search", env: "GOGEN_WEB_SEARCH", cfg: "WebSearch", file: "WebSearch", writer: "WebSearch", kind: kString, omitDefault: true},
	{key: "web_search_backend", env: "GOGEN_WEB_SEARCH_BACKEND", cfg: "WebSearchBackend", file: "WebSearchBackend", writer: "WebSearchBackend", kind: kString, omitDefault: true},
	{key: "web_search_api_key", env: "GOGEN_WEB_SEARCH_API_KEY", cfg: "WebSearchAPIKey", file: "WebSearchAPIKey", writer: "WebSearchAPIKey", kind: kString, secret: true},
	{key: "web_allowed_domains", env: "GOGEN_WEB_ALLOWED_DOMAINS", cfg: "WebAllowedDomains", file: "WebAllowedDomains", writer: "WebAllowedDomains", kind: kString, omitDefault: true},
	{key: "web_fetch_mode", env: "GOGEN_WEB_FETCH_MODE", cfg: "WebFetchMode", file: "WebFetchMode", writer: "WebFetchMode", kind: kString, omitDefault: true},

	// --- sessions ---------------------------------------------------------
	{key: "session_max_count", env: "GOGEN_SESSION_MAX_COUNT", cfg: "SessionMaxCount", file: "SessionMaxCount", writer: "SessionMaxCount", kind: kInt, omitDefault: true},
	{key: "session_max_age_days", env: "GOGEN_SESSION_MAX_AGE_DAYS", cfg: "SessionMaxAgeDays", file: "SessionMaxAgeDays", writer: "SessionMaxAgeDays", kind: kIntKeepNegative, omitDefault: true},

	// --- spill / output ---------------------------------------------------
	{key: "output_spill", env: "GOGEN_OUTPUT_SPILL", cfg: "OutputSpill", file: "OutputSpill", writer: "OutputSpill", kind: kString, omitDefault: true},
	{key: "preserve_reasoning", env: "GOGEN_PRESERVE_REASONING", cfg: "PreserveReasoning", file: "PreserveReasoning", writer: "PreserveReasoning", kind: kString, writeSpecial: true},

	// --- features ---------------------------------------------------------
	{key: "board", env: "GOGEN_BOARD", cfg: "Board", file: "Board", writer: "Board", kind: kString, omitDefault: true},
	{key: "subagent", env: "GOGEN_SUBAGENT", cfg: "Subagent", file: "Subagent", writer: "Subagent", kind: kString, omitDefault: true},
	{key: "subagent_max_depth", env: "GOGEN_SUBAGENT_MAX_DEPTH", cfg: "SubagentMaxDepth", file: "SubagentMaxDepth", writer: "SubagentMaxDepth", kind: kInt, omitDefault: true},
	{key: "subagent_max_concurrent", env: "GOGEN_SUBAGENT_MAX_CONCURRENT", cfg: "SubagentMaxConcurrent", file: "SubagentMaxConcurrent", writer: "SubagentMaxConcurrent", kind: kInt, omitDefault: true},
	{key: "subagent_model", env: "GOGEN_SUBAGENT_MODEL", cfg: "SubagentModel", file: "SubagentModel", writer: "SubagentModel", kind: kString},
	{key: "subagent_thinking_level", env: "GOGEN_SUBAGENT_THINKING_LEVEL", cfg: "SubagentThinkingLevel", file: "SubagentThinkingLevel", writer: "SubagentThinkingLevel", kind: kString},
	{key: "review_agent", env: "GOGEN_REVIEW_AGENT", cfg: "ReviewAgent", file: "ReviewAgent", writer: "ReviewAgent", kind: kString, omitDefault: true},
	{key: "review_agent_model", env: "GOGEN_REVIEW_AGENT_MODEL", cfg: "ReviewAgentModel", file: "ReviewAgentModel", writer: "ReviewAgentModel", kind: kString},
	{key: "review_agent_thinking_level", env: "GOGEN_REVIEW_AGENT_THINKING_LEVEL", cfg: "ReviewAgentThinkingLevel", file: "ReviewAgentThinkingLevel", writer: "ReviewAgentThinkingLevel", kind: kString},
	{key: "agent_instructions", env: "GOGEN_AGENT_INSTRUCTIONS", cfg: "AgentInstructions", file: "AgentInstructions", writer: "AgentInstructions", kind: kString, omitDefault: true},
	{key: "skills", env: "GOGEN_SKILLS", cfg: "Skills", file: "Skills", writer: "Skills", kind: kString, omitDefault: true},
	{key: "terminal", env: "GOGEN_TERMINAL", cfg: "Terminal", file: "Terminal", writer: "Terminal", kind: kString, omitDefault: true},
	{key: "job_notices", env: "GOGEN_JOB_NOTICES", cfg: "JobNotices", file: "JobNotices", writer: "JobNotices", kind: kString, omitDefault: true},
	{key: "join_queued", env: "GOGEN_JOIN_QUEUED", cfg: "JoinQueued", file: "JoinQueued", writer: "JoinQueued", kind: kString, omitDefault: true},
	{key: "automations", env: "GOGEN_AUTOMATIONS", cfg: "Automations", file: "Automations", writer: "Automations", kind: kString, omitDefault: true},

	// --- prompts ----------------------------------------------------------
	{key: "board_start_prompt", env: "GOGEN_BOARD_START_PROMPT", cfg: "BoardStartPrompt", file: "BoardStartPrompt", writer: "BoardStartPrompt", kind: kString},
	{key: "board_review_prompt", env: "GOGEN_BOARD_REVIEW_PROMPT", cfg: "BoardReviewPrompt", file: "BoardReviewPrompt", writer: "BoardReviewPrompt", kind: kString},
	{key: "system_prompt", env: "GOGEN_SYSTEM_PROMPT", cfg: "SystemPrompt", file: "SystemPrompt", writer: "SystemPrompt", kind: kString},
	{key: "subagent_prompt", env: "GOGEN_SUBAGENT_PROMPT", cfg: "SubagentPrompt", file: "SubagentPrompt", writer: "SubagentPrompt", kind: kString},

	// --- status display ---------------------------------------------------
	{key: "status_display", env: "GOGEN_STATUS_DISPLAY", cfg: "StatusDisplay", file: "StatusDisplay", writer: "StatusDisplay", kind: kString, omitDefault: true},
	{key: "status_display_host", env: "GOGEN_STATUS_DISPLAY_HOST", cfg: "StatusDisplayHost", file: "StatusDisplayHost", writer: "StatusDisplayHost", kind: kString, omitDefault: true},
	{key: "status_display_token", env: "GOGEN_STATUS_DISPLAY_TOKEN", cfg: "StatusDisplayToken", file: "StatusDisplayToken", writer: "StatusDisplayToken", kind: kString, secret: true},
	{key: "status_display_source", env: "GOGEN_STATUS_DISPLAY_SOURCE", cfg: "StatusDisplaySource", file: "StatusDisplaySource", writer: "StatusDisplaySource", kind: kString, omitDefault: true},

	// --- back-compat spellings --------------------------------------------
	// parseYAMLFrontMatter aliases them onto the renamed fields above, so they
	// are read (with a deprecation warning at merge time) but never written.
	{key: "keep_recent_messages", file: "KeepRecentMessages", kind: kLegacy},
	{key: "command_timeout_secs", file: "CommandTimeoutSecs", kind: kLegacy},
}

// Field offsets are resolved once. reflect's FieldByName scans the struct's
// fields on every call, and the loops below do that three times per row.
var (
	cfgFieldIndex  = structFieldIndexes(reflect.TypeOf(config.Config{}))
	fileFieldIndex = structFieldIndexes(reflect.TypeOf(FileConfig{}))
	yamlFieldIndex = structFieldIndexes(reflect.TypeOf(configYAML{}))
)

// structFieldIndexes maps an exported struct field name to its index.
func structFieldIndexes(t reflect.Type) map[string]int {
	m := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.PkgPath == "" {
			m[f.Name] = i
		}
	}
	return m
}

// lookupField returns the field by cached index, reporting false when the name
// does not exist (a mistyped row; the coverage tests name it).
func lookupField(v reflect.Value, indexes map[string]int, name string) (reflect.Value, bool) {
	i, ok := indexes[name]
	if !ok {
		return reflect.Value{}, false
	}
	return v.Field(i), true
}

// settingsNotInFiles are config.Config fields that are not configuration-file
// settings: they are derived per process (host-level MCP servers, the resolved
// project file and its guidelines) and assigned after Merge. The coverage test
// uses this list, so a genuinely new setting cannot hide here by accident.
var settingsNotInFiles = []string{"HostMCPServers", "ProjectFilePath", "ProjectGuidelines"}

// applySettings fills cfg from the file values and defaults, applying
// env > file > default precedence (CLI flags are applied by Merge afterwards,
// since they are not file settings).
func applySettings(cfg *config.Config, file *FileConfig, def *config.Config) {
	cv := reflect.ValueOf(cfg).Elem()
	fv := reflect.ValueOf(file).Elem()
	dv := reflect.ValueOf(def).Elem()
	for _, s := range settings {
		if s.kind == kLegacy {
			continue
		}
		switch s.kind {
		case kKeepRecent:
			cfg.CompactKeepRecentMessages = mergeCompactKeepRecentMessages(*file, def.CompactKeepRecentMessages)
			continue
		case kCommandIdleTimeout:
			cfg.CommandIdleTimeoutSecs = mergeCommandIdleTimeout(*file, def.CommandIdleTimeoutSecs)
			continue
		case kMCPServers:
			cfg.MCPServers = mergeMCPServers(*file)
			continue
		case kOpenAIProviders:
			cfg.OpenAIProviders = mergeOpenAIProviders(*file)
			continue
		}
		dst, okDst := lookupField(cv, cfgFieldIndex, s.cfg)
		src, okSrc := lookupField(fv, fileFieldIndex, s.file)
		ds, okDs := lookupField(dv, cfgFieldIndex, s.cfg)
		if !okDst || !okSrc || !okDs {
			// A row naming a field that does not exist is a programming error;
			// the coverage test reports it by name rather than panicking here.
			panic("projectfile: setting " + s.key + " names a missing field")
		}
		switch s.kind {
		case kString:
			dst.SetString(mergeString(s.env, src.String(), ds.String()))
		case kInt:
			dst.SetInt(int64(mergeInt(s.env, int(src.Int()), int(ds.Int()))))
		case kIntPtr:
			dst.SetInt(int64(mergeIntOpt(s.env, intPtrValue(src), int(ds.Int()))))
		case kFloatPtr:
			dst.SetFloat(mergeFloatOpt(s.env, floatPtrValue(src), ds.Float()))
		case kBool:
			dst.SetBool(mergeBool(s.env, src.Bool(), ds.Bool()))
		case kIntKeepNegative:
			dst.SetInt(int64(mergeIntKeepNegative(s.env, int(src.Int()), int(ds.Int()))))
		default:
			panic("projectfile: setting " + s.key + " has an unhandled kind")
		}
	}
}

// intPtrValue reads a *int FileConfig field as a nil-able value.
func intPtrValue(v reflect.Value) *int {
	if v.IsNil() {
		return nil
	}
	n := int(v.Elem().Int())
	return &n
}

// floatPtrValue reads a *float64 FileConfig field as a nil-able value.
func floatPtrValue(v reflect.Value) *float64 {
	if v.IsNil() {
		return nil
	}
	f := v.Elem().Float()
	return &f
}

// writeSettings fills out (the YAML projection) from cfg, omitting secrets
// unless includeSecrets and omitting values that equal the built-in default
// where the setting asks for it. Field order — the saved file's key order — is
// the configYAML struct's, which this loop never reorders.
func writeSettings(out *configYAML, cfg, def *config.Config, includeSecrets bool) {
	ov := reflect.ValueOf(out).Elem()
	cv := reflect.ValueOf(cfg).Elem()
	dv := reflect.ValueOf(def).Elem()
	for _, s := range settings {
		if s.kind == kLegacy || s.writer == "" || s.writeSpecial {
			continue
		}
		if s.secret && !includeSecrets {
			continue
		}
		dst, okDst := lookupField(ov, yamlFieldIndex, s.writer)
		src, okSrc := lookupField(cv, cfgFieldIndex, s.cfg)
		if !okDst || !okSrc {
			panic("projectfile: setting " + s.key + " names a missing writer field")
		}
		if s.omitDefault && reflect.DeepEqual(src.Interface(), dv.Field(cfgFieldIndex[s.cfg]).Interface()) {
			continue
		}
		dst.Set(src)
	}
}
