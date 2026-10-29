package config

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"gogen/internal/onoff"
)

// Exported defaults for configurable settings. Other packages reference these
// constants instead of duplicating literals, so the values have a single
// source of truth.
const (
	// DefaultCompactThreshold is the fraction of the context limit that
	// triggers auto-compaction. 0.85 leaves less headroom before the window
	// fills than the old 0.75, but the continuation-summary request is a
	// cache-friendly prefix (see contextmgr.summarizeMiddle), so compacting
	// later is cheap and the headroom is covered by the reserve.
	DefaultCompactThreshold = 0.85
	// DefaultCompactKeepRecentMessages is the number of recent messages
	// preserved verbatim during compaction.
	DefaultCompactKeepRecentMessages = 12
	// DefaultMaxToolResultBytes is the cap for tool output before truncation
	// (256 KB — matches web_fetch's default body cap).
	DefaultMaxToolResultBytes = 262144
	// DefaultCompactReserveTokens is the token budget reserved for new
	// messages after compaction.
	DefaultCompactReserveTokens = 4000
	// DefaultCompactLastResort is the default compact_last_resort mode:
	// "condense" runs the last-resort condensation (Phase 0e) on a message
	// that cannot fit the context window; "error" returns a diagnostic
	// instead.
	DefaultCompactLastResort = "condense"
	// DefaultSmartCompact is the smart-compact switch. Off keeps compaction
	// at a user-message boundary. On also compacts inside a long tool loop,
	// before the next tool runs, and stops a thinking stream while there is
	// still room to summarize.
	DefaultSmartCompact = "off"
	// DefaultContextMode is the context_mode default: the system prompt's
	// context advice and the compaction layout as they always were.
	DefaultContextMode = "default"
	// ContextModeDeepDive is the context_mode for long-running tasks: the
	// prompt says compaction is automatic, and compaction keeps the current
	// request verbatim between two summaries.
	ContextModeDeepDive = "deep_dive"
	// DefaultCommandIdleTimeoutSecs is the maximum time a foreground
	// execute_command may run without producing any output before it is
	// killed; 0 in the config falls back to this default.
	DefaultCommandIdleTimeoutSecs = 300
	// DefaultSessionMaxCount is the maximum saved sessions per working dir.
	DefaultSessionMaxCount = 50
	// DefaultSessionMaxAgeDays is the retention window for saved sessions.
	DefaultSessionMaxAgeDays = 30
	// DefaultContextLimit is the fallback context window size (tokens) when
	// it cannot be resolved from the provider.
	DefaultContextLimit = 128000
	// DefaultWebMaxActiveSessions is the default cap on concurrently active
	// web sessions.
	DefaultWebMaxActiveSessions = 8
	// DefaultSubagentMaxDepth is the default maximum subagent nesting depth
	// (main agent = depth 0). 1 means subagents cannot spawn subagents.
	DefaultSubagentMaxDepth = 1
	// DefaultSubagentMaxConcurrent is the default cap on concurrently
	// running subagents per parent session (web host). Spawning beyond it
	// is refused; values <= 0 fall back to this default.
	DefaultSubagentMaxConcurrent = 4
	// DefaultStatusDisplayHost is the mDNS name the ttgo/status firmware
	// advertises. DefaultStatusDisplaySource is empty = AUTO: the reporter
	// derives "gogen-<project dir>" so several Gogen instances in different
	// projects land on different display rows without configuration (see
	// internal/statusdisplay.sourceForDir).
	DefaultStatusDisplayHost   = "statusscreen.local"
	DefaultStatusDisplaySource = ""
	// The status-display gate values: off never reports; auto (the default)
	// probes for the screen at startup and enables reporting when it
	// answers; on reports without probing; browser builds the rows
	// server-side but has the web UI POST them, for when the browser can
	// reach the display and the server cannot (web mode only).
	StatusDisplayOff     = "off"
	StatusDisplayAuto    = "auto"
	StatusDisplayOn      = "on"
	StatusDisplayBrowser = "browser"
)

// Effective returns v when v > 0, else def. It is the single "0 = unset, use
// the default" resolution rule for integer config values, so the policy is
// documented here once instead of drifting across call sites.
//
// NOTE on explicit zeros: this rule applies only where 0 is not a meaningful
// setting (session retention counts, ...). Some settings deliberately treat 0
// as meaningful and deliberately do NOT go through Effective:
//   - session retention (session.StoreOptions.MaxAgeDays): a NEGATIVE value
//     disables age-based pruning ("keep sessions forever"); 0 still means
//     the default via Effective;
//   - contextmgr.Settings: compact_threshold 0 disables auto-compaction,
//     max_tool_result_bytes 0 removes the truncation cap, compact_reserve_tokens
//     0 reserves nothing — only negative values fall back to defaults;
//   - projectfile.mergeIntOpt distinguishes an explicit file 0 from an absent
//     key via a non-nil pointer.
func Effective(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// MCP tool exposure modes (mcp_tool_mode).
const (
	// MCPToolModeNested exposes every MCP server tool through the single
	// `mcp` gateway tool (list/describe/call) instead of inlining one
	// definition per server tool. The model-facing tool set then has a fixed
	// size and fixed bytes no matter how many servers connect — which is
	// what keeps the request prefix stable for provider prompt caching — and
	// a large catalog (a reverse-engineering server publishes dozens of
	// tools) costs one definition instead of dozens.
	MCPToolModeNested = "nested"
	// MCPToolModeInline inlines every discovered MCP tool definition, the
	// historical behavior. Cheapest for a model with one or two extra tools
	// (native schemas, no discovery hop), at the cost of a prefix that grows
	// with the catalog and changes when the registry attaches.
	MCPToolModeInline = "inline"
	// DefaultMCPToolMode is nested: the fixed tool surface and stable prefix
	// win by default, and inline stays available as the opt-out.
	DefaultMCPToolMode = MCPToolModeNested
)

// MCPServerConfig describes one MCP stdio server entry. Tags cover both the
// JSON env-var form (GOGEN_MCP_SERVERS) and the YAML project-file form.
type MCPServerConfig struct {
	Name    string            `json:"name" yaml:"name"`
	Command string            `json:"command" yaml:"command"`
	Args    []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// InitTimeoutSecs bounds this server's initialize + tools/list handshake
	// (0 = the client default). Raise it when the launcher itself is slow —
	// `npx -y <package>` has to download the package on a cold cache.
	InitTimeoutSecs int `json:"init_timeout_secs,omitempty" yaml:"init_timeout_secs,omitempty"`
	// ToolTimeoutSecs is the default deadline for one tools/call on this
	// server when the caller supplies none (0 = the client default). Raise it
	// for servers whose tools legitimately run for minutes, e.g. a
	// reverse-engineering engine that starts Ghidra or IDA per analysis.
	ToolTimeoutSecs int `json:"tool_timeout_secs,omitempty" yaml:"tool_timeout_secs,omitempty"`
}

// OpenAIProviderConfig describes one registered OpenAI-compatible API
// endpoint: a base URL, an optional API key, and an optional default model.
// The legacy OpenAIKey/OpenAIModel/OpenAIURL fields form the implicit
// "default" profile; entries in Config.OpenAIProviders are additional
// registered providers whose models are aggregated into the web model
// picker and routed per model. Tags cover both the JSON env-var form
// (GOGEN_OPENAI_PROVIDERS) and the YAML project-file form.
type OpenAIProviderConfig struct {
	Name    string `json:"name" yaml:"name"`
	BaseURL string `json:"baseUrl" yaml:"base_url"`
	APIKey  string `json:"apiKey,omitempty" yaml:"api_key,omitempty"`
	Model   string `json:"model,omitempty" yaml:"model,omitempty"`
}

type Config struct {
	OpenAIKey   string
	OpenAIModel string
	OpenAIURL   string
	WorkingDir  string

	ContextLimit              int
	CompactThreshold          float64
	CompactKeepRecentMessages int
	MaxToolResultBytes        int
	CompactReserveTokens      int
	// CompactLastResort controls the last-resort condensation (Phase 0e)
	// for a message that cannot fit the context window even after all
	// compaction: "condense" (default) condenses the message in place via
	// the summarizer (the original is archived to the session's archive
	// sidecar and the condensation is announced in-band); "error" returns
	// a clear diagnostic instead of condensing.
	CompactLastResort string
	// SmartCompact compacts inside a tool loop when the conversation is
	// over the compact threshold, then runs the tool call that was already
	// formed. "off" (default) leaves compaction at a user-message boundary.
	// CompactThreshold 0 wins: the mid-think cut still happens, but nothing
	// is summarized and the cut ends the turn with a note.
	SmartCompact string
	// ContextMode is "default" or "deep_dive". deep_dive tells the model that
	// history is compacted automatically (only while compact_threshold > 0)
	// and makes compaction keep the latest user request verbatim between a
	// summary of what came before it and a summary of the work since.
	ContextMode string

	CommandSafetyMode string // blocklist, allowlist, off
	CommandAllowlist  string // comma-separated when allowlist mode

	DeleteApproval string // required, off

	// CommandApproval decides what a blocklisted command does when
	// command_safety is blocklist: "required" escalates it to the session's
	// approval prompt (and refuses it when nobody can be asked), "off" refuses
	// it outright with the block error — the pre-escalation behavior, for
	// setups that want no interactive prompt. It never makes a dangerous
	// command run: turning the guard itself off is command_safety: off.
	CommandApproval string // required, off

	TreeSitter      string
	TreeSitterLangs string
	CLIVerbose      bool
	DebugLog        string
	DebugSession    string
	MCP             string
	MCPServers      []MCPServerConfig
	// HostMCPServers are process-local servers a host appended with
	// GOGEN_MCP_EXTRA. They start with MCPServers when mcp is on, and a
	// saved config never includes them.
	HostMCPServers []MCPServerConfig
	// MCPToolMode selects how MCP server tools are exposed to the model
	// (nested = the single `mcp` gateway tool, inline = one definition per
	// server tool). Resolved once at session construction: the value is part
	// of the fixed request prefix and must not change mid-session.
	MCPToolMode string
	// OpenAIProviders lists additional registered OpenAI-compatible API
	// providers (see OpenAIProviderConfig). Empty means only the legacy
	// OpenAIKey/OpenAIModel/OpenAIURL default profile exists.
	OpenAIProviders []OpenAIProviderConfig

	// DebugCompareMessages enables view-fingerprint comparison across turns (GOGEN_DEBUG_COMPARE_MESSAGES).
	// Only effective in binaries built with `-tags debug`; ignored otherwise.
	DebugCompareMessages bool

	// ProjectGuidelines is loaded from the markdown body of the project file.
	ProjectGuidelines string
	ProjectFilePath   string
	TestCommand       string
	LintCommand       string

	WebBind           string // listen address for --web (default 127.0.0.1:8081)
	WebAllowedOrigins string // comma-separated host allowlist; empty uses localhost defaults
	WebAuthToken      string // required for non-loopback binds; also GOGEN_WEB_TOKEN
	WebTLSCertFile    string // PEM cert for TLS (required with key for non-loopback)
	WebTLSKeyFile     string // PEM key for TLS
	// WebMaxActiveSessions caps concurrently active web sessions (registry
	// eviction bound; 0 = DefaultWebMaxActiveSessions).
	WebMaxActiveSessions int
	// WebApprovalHoldSecs is how long a pending delete approval survives the
	// last attached client detaching before it is auto-denied (0 = deny
	// immediately on detach, the default).
	WebApprovalHoldSecs int

	SessionMaxCount   int // max saved sessions per working dir (0 = default 50)
	SessionMaxAgeDays int // delete sessions older than N days (0 = default 30, negative = keep forever)

	WebFetch  string // on, off
	WebSearch string // on, off
	// OutputSpill gates the spill feature: oversized tool output (command
	// output, show_diff, …) is persisted to the session's spill dir and the
	// inline result becomes a head/tail preview + locator instead of a
	// lossy head-only truncation ("on"/"off"; default on). When off, the
	// legacy plain-cap truncation applies.
	OutputSpill       string
	WebSearchBackend  string // brave or "" for ddg
	WebSearchAPIKey   string // Brave API key
	WebAllowedDomains string // comma-separated domain suffix allowlist
	WebFetchMode      string // https, all

	CommandSandbox         string // off, bwrap (bubblewrap when available)
	CommandIdleTimeoutSecs int    // execute_command idle (no-output) timeout; 0 = default 300s

	// PreserveReasoning controls chat_template_kwargs.preserve_reasoning for
	// self-hosted OpenAI-compatible servers: auto (probe /props), on, off.
	PreserveReasoning string

	// Board enables the project-wide kanban board feature ("on"/"off";
	// default off). When disabled the board tool is not registered and the
	// web board tab is hidden.
	Board string
	// Subagent enables the subagent tool that spawns nested sessions
	// ("on"/"off"; default off). When disabled the tool is not registered.
	Subagent string
	// SubagentMaxDepth is the maximum subagent nesting depth (main agent =
	// depth 0). The default 1 means subagents cannot spawn subagents; a
	// higher value re-enables nesting up to the configured depth. Values
	// <= 0 fall back to the default.
	SubagentMaxDepth int
	// SubagentMaxConcurrent is the maximum number of subagents that may run
	// concurrently for one parent session (web host; the TUI runs
	// foreground-only, so it is never bounded by this). The default 4
	// limits parallel fan-out; spawning beyond it is refused with an error
	// the model can act on (interrupt_agent / wait). Values <= 0 fall back
	// to the default.
	SubagentMaxConcurrent int
	// SubagentModel is the default model for spawned subagents. Empty
	// (the default) means subagents inherit the parent session's model;
	// an explicit tool-call model argument always wins over this value.
	SubagentModel string
	// SubagentThinkingLevel is the reasoning-effort level for spawned
	// subagents. Empty (the default) means subagents inherit the parent
	// session's live thinking level; "off" never sends reasoning_effort;
	// any other value is a literal reasoning_effort value, sent only when
	// the subagent's final model accepts it (omitted at spawn time
	// otherwise — the tool's model argument can override the configured
	// model, so validity is resolved against the child's model, never at
	// save time).
	SubagentThinkingLevel string

	// ReviewAgent enables the board auto-review agent ("on"/"off"; default
	// off; web mode only): when on, a ticket moved into in_review — by an
	// agent's board tool or by the user — automatically spawns a headless
	// review session that marks the ticket done or comments findings and
	// moves it back to in_progress.
	ReviewAgent string
	// ReviewAgentModel is the default model for auto review sessions.
	// Empty (the default) resolves the cascade per ticket: the ticket's
	// own review model override, then the workspace default model.
	ReviewAgentModel string
	// ReviewAgentThinkingLevel is the reasoning-effort level for auto
	// review sessions. Empty (the default) inherits the workspace
	// thinking level; "off" never sends reasoning_effort; any other value
	// is a literal reasoning_effort value, sent only when the reviewer's
	// final model accepts it (a per-ticket override wins over this
	// setting; validity is resolved against the final model at review
	// start, never at save time).
	ReviewAgentThinkingLevel string

	// BoardStartPrompt is the template for prompts given to agents started
	// from a board ticket ("" = the built-in default). Placeholders:
	// {id} {title} {description} {priority} {context}.
	BoardStartPrompt string
	// BoardReviewPrompt is the template for prompts given to auto review
	// agents ("" = the built-in default). Placeholders: {id} {title}
	// {description} {priority} {context} {assignee}.
	BoardReviewPrompt string
	// SystemPrompt is a custom system prompt template ("" = the built-in
	// default). The {working_dir} placeholder is substituted with the
	// working directory; the project profile, project rules, and plan-mode
	// suffixes always append after it.
	SystemPrompt string
	// SubagentPrompt is the template wrapping subagent jobs ("" = the
	// built-in default). The {job} placeholder is substituted with the
	// tool call's job text.
	SubagentPrompt string
	// AgentInstructions enables loading AGENTS.md / CLAUDE.md workspace
	// instruction files ("on"/"off"; default off). When disabled the files
	// are never read; when enabled they are appended BELOW the project
	// guidelines (.gogen/gogen.md stays authoritative) with byte caps.
	// Live: the web settings modal toggles it (the sweep re-renders every
	// session's instruction section).
	AgentInstructions string
	// Skills enables the skill tool ("on"/"off"; default off). When
	// disabled the tool is not registered. Live: the web settings modal
	// exposes it as a feature toggle (shared flag store, no restart).
	Skills string
	// Terminal enables the persistent-terminal feature tools (the terminal
	// tool's open/send/read/signal/list/close actions and bash_persistent)
	// ("on"/"off"; default off). When disabled the tools are not registered.
	// Live: the web settings modal exposes it as a feature toggle (shared
	// flag store, no restart).
	Terminal string
	// JobNotices enables background-job completion notices ("on"/"off";
	// default off): when a background shell job finishes naturally, a
	// summary is injected into the session as a user message and a turn
	// runs on it. Live: the web settings modal toggles it (the sweep
	// installs/removes each session's notice hook).
	JobNotices string
	// JoinQueued joins user messages queued during a busy turn into one
	// message when that turn ends ("on"/"off"; default off = each queued
	// message runs as its own turn). Live: the web settings modal toggles it.
	JoinQueued string

	// Automations enables the file-based cron scheduler ("on"/"off";
	// default off): when on, a running TUI or web host sweeps the global
	// automation store every 30 s and fires due automations as headless
	// `-p` runs (see internal/automation). The `gogen automation` CLI
	// manages the records; the flag only gates firing.
	Automations string

	// StatusDisplay gates the LAN status-screen reporter: "off", "auto"
	// (default: probe for the screen at startup, enable when it answers), or
	// "on" (report without probing). When active, each top-level turn
	// updates one display row over the firmware's HTTP API: progress is the
	// context-window usage, the state is running / done / error / idle, and
	// the message names the current activity. Updates are best-effort and
	// never affect a turn.
	StatusDisplay string
	// StatusDisplayHost is the display's mDNS name, IP, or base URL
	// (default statusscreen.local, the firmware's advertised name).
	StatusDisplayHost string
	// StatusDisplayToken is the optional shared secret the firmware
	// requires on mutating endpoints (sent as the X-Status-Token header).
	StatusDisplayToken string
	// StatusDisplaySource names the display row (feed id) the session
	// reports under. Empty (the default) derives "gogen-<project folder>"
	// so instances in different projects get their own display row; set it
	// explicitly when two instances share one project folder.
	StatusDisplaySource string
}

// Defaults returns built-in default configuration values.
func Defaults() Config {
	return Config{
		OpenAIKey:                 "",
		OpenAIModel:               "",
		OpenAIURL:                 "",
		WorkingDir:                ".",
		ContextLimit:              0,
		CompactThreshold:          DefaultCompactThreshold,
		CompactKeepRecentMessages: DefaultCompactKeepRecentMessages,
		MaxToolResultBytes:        DefaultMaxToolResultBytes,
		CompactReserveTokens:      DefaultCompactReserveTokens,
		CompactLastResort:         DefaultCompactLastResort,
		SmartCompact:              DefaultSmartCompact,
		ContextMode:               DefaultContextMode,
		CommandSafetyMode:         "blocklist",
		CommandAllowlist:          "",
		DeleteApproval:            "required",
		CommandApproval:           "required",
		TreeSitter:                "on",
		TreeSitterLangs:           "",
		CLIVerbose:                false,
		DebugLog:                  "",
		DebugSession:              "",
		MCP:                       "off",
		MCPToolMode:               DefaultMCPToolMode,
		DebugCompareMessages:      false,
		WebBind:                   "127.0.0.1:8081",
		WebAllowedOrigins:         "",
		WebTLSCertFile:            "",
		WebTLSKeyFile:             "",
		SessionMaxCount:           DefaultSessionMaxCount,
		SessionMaxAgeDays:         DefaultSessionMaxAgeDays,
		WebMaxActiveSessions:      DefaultWebMaxActiveSessions,
		WebApprovalHoldSecs:       0,
		WebFetch:                  "on",
		WebSearch:                 "on",
		OutputSpill:               "on",
		WebSearchBackend:          "",
		WebSearchAPIKey:           "",
		WebAllowedDomains:         "",
		WebFetchMode:              "https",
		CommandSandbox:            "off",
		CommandIdleTimeoutSecs:    DefaultCommandIdleTimeoutSecs,
		PreserveReasoning:         "auto",
		Board:                     "off",
		Subagent:                  "off",
		SubagentMaxDepth:          DefaultSubagentMaxDepth,
		SubagentMaxConcurrent:     DefaultSubagentMaxConcurrent,
		AgentInstructions:         "off",
		Skills:                    "off",
		Terminal:                  "off",
		JobNotices:                "off",
		JoinQueued:                "off",
		Automations:               "off",
		StatusDisplay:             StatusDisplayAuto,
		StatusDisplayHost:         DefaultStatusDisplayHost,
		StatusDisplaySource:       DefaultStatusDisplaySource,
	}
}

// ApprovalHold returns the configured approval-hold duration. Zero means
// "deny pending approvals immediately when the last client detaches".
func (c *Config) ApprovalHold() time.Duration {
	if c == nil || c.WebApprovalHoldSecs <= 0 {
		return 0
	}
	return time.Duration(c.WebApprovalHoldSecs) * time.Second
}

// configOn reports whether a config field is explicitly enabled.
func configOn(v string) bool {
	return onoff.Enabled(v)
}

// configOff reports whether a config field is explicitly disabled
// (set to "off", "0", "false", or "no"). An empty string is not "off" —
// absence of a value does not imply explicit disablement.
func configOff(v string) bool {
	on, ok := onoff.Parse(v)
	return ok && !on
}

// MCPEnabled reports whether MCP integration is active.
// Opt-in: servers in project config are not started unless mcp is explicitly enabled.
func (c *Config) MCPEnabled() bool {
	return c != nil && configOn(c.MCP)
}

// MCPNested reports whether MCP server tools are exposed through the single
// `mcp` gateway tool instead of one inlined definition per server tool.
// Empty or unknown values fall back to the default (nested); only an explicit
// "inline" selects the historical behavior.
func (c *Config) MCPNested() bool {
	if c == nil {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(c.MCPToolMode), MCPToolModeInline)
}

// MCPServersToStart is the server list the manager spawns. Host servers are
// included only when mcp is on, so the off switch skips them the same way it
// skips servers from the config file.
func (c *Config) MCPServersToStart() []MCPServerConfig {
	if !c.MCPEnabled() {
		return nil
	}
	return ValidMCPServers(append(append([]MCPServerConfig{}, c.MCPServers...), c.HostMCPServers...))
}

// MCPActive reports whether MCP is enabled with at least one server to start.
// This is the CONFIG answer, deliberately independent of whether a server has
// connected yet: the model-facing tool set and system prompt are decided from
// it at construction, so a slow server attaching later cannot change the
// request prefix mid-session (which would invalidate the provider's cache).
// A host server counts, so a sidebar that only adds an editor bridge still
// exposes the gateway.
func (c *Config) MCPActive() bool {
	return len(c.MCPServersToStart()) > 0
}

// ValidMCPServers keeps the server entries that can actually be spawned (name
// and command present). It is the single definition of "usable entry", shared
// with mcp.ValidServers (which cannot be imported here: internal/mcp depends
// on this package).
func ValidMCPServers(servers []MCPServerConfig) []MCPServerConfig {
	out := make([]MCPServerConfig, 0, len(servers))
	for _, s := range servers {
		if strings.TrimSpace(s.Name) != "" && strings.TrimSpace(s.Command) != "" {
			out = append(out, s)
		}
	}
	return out
}

// HostMCPServersEnv is the process-local MCP server list a host may set.
// It is a JSON array in the same shape as GOGEN_MCP_SERVERS. Unlike that
// variable it is appended, and it is never written to a config file.
const HostMCPServersEnv = "GOGEN_MCP_EXTRA"

// WebListenEnv is the listen address for this process only. A host may set
// it so the VS Code sidebar can bind a free port. It is not a config value:
// Merge ignores it, and a settings save keeps the user's web_bind.
const WebListenEnv = "GOGEN_WEB_LISTEN"

// LoadHostMCPServers reads HostMCPServersEnv. An unset or empty value is
// no servers. Invalid JSON is ignored so a bad host value cannot wipe the
// servers the user configured.
func LoadHostMCPServers() []MCPServerConfig {
	raw, ok := os.LookupEnv(HostMCPServersEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var servers []MCPServerConfig
	if err := json.Unmarshal([]byte(raw), &servers); err != nil {
		log.Printf("warning: %s is not a valid JSON array; ignoring it: %v", HostMCPServersEnv, err)
		return nil
	}
	return servers
}

// MergeHostMCPServers appends host servers after the configured list.
// A host entry whose name is already configured is skipped, so the user's
// server keeps that name.
func MergeHostMCPServers(configured, extra []MCPServerConfig) []MCPServerConfig {
	seen := make(map[string]bool, len(configured))
	for _, s := range configured {
		name := strings.TrimSpace(s.Name)
		if name != "" {
			seen[name] = true
		}
	}
	var out []MCPServerConfig
	for _, s := range extra {
		name := strings.TrimSpace(s.Name)
		if name != "" && seen[name] {
			log.Printf("host MCP server %q skipped; a configured server already uses that name", name)
			continue
		}
		if name != "" {
			seen[name] = true
		}
		out = append(out, s)
	}
	return out
}

// BoardEnabled reports whether the project kanban board feature is active.
// Opt-in: the board tool is not registered and the web board tab is hidden
// unless board is explicitly enabled.
func (c *Config) BoardEnabled() bool {
	return c != nil && configOn(c.Board)
}

// SubagentEnabled reports whether the subagent tool is active.
// Opt-in: the tool is not registered unless subagent is explicitly enabled.
func (c *Config) SubagentEnabled() bool {
	return c != nil && configOn(c.Subagent)
}

// ReviewAgentEnabled reports whether the board auto-review agent is active.
// Opt-in: a ticket moved into in_review spawns no review session unless
// review_agent is explicitly enabled (web mode only).
func (c *Config) ReviewAgentEnabled() bool {
	return c != nil && configOn(c.ReviewAgent)
}

// AgentInstructionsEnabled reports whether AGENTS.md/CLAUDE.md workspace
// instruction files are loaded. Opt-in: the files are never read unless
// agent_instructions is explicitly enabled.
func (c *Config) AgentInstructionsEnabled() bool {
	return c != nil && configOn(c.AgentInstructions)
}

// SkillsEnabled reports whether the skill tool is active. Opt-in: the tool
// is not registered unless skills is explicitly enabled.
func (c *Config) SkillsEnabled() bool {
	return c != nil && configOn(c.Skills)
}

// TerminalEnabled reports whether the persistent-terminal feature tools are
// active. Opt-in: the tools are not registered unless terminal is explicitly
// enabled.
func (c *Config) TerminalEnabled() bool {
	return c != nil && configOn(c.Terminal)
}

// JobNoticesEnabled reports whether background-job completion notices are
// active. Opt-in: no notice hook is installed unless job_notices is
// explicitly enabled.
func (c *Config) JobNoticesEnabled() bool {
	return c != nil && configOn(c.JobNotices)
}

// JoinQueuedEnabled reports whether user messages queued during one turn
// leave together as one message. Opt-in: each runs as its own turn unless
// join_queued is explicitly enabled.
func (c *Config) JoinQueuedEnabled() bool {
	return c != nil && configOn(c.JoinQueued)
}

// DeepDiveContext reports whether context_mode is deep_dive, in any case.
// Any other value, including an empty one, is the default mode.
func (c *Config) DeepDiveContext() bool {
	return c != nil && strings.EqualFold(strings.TrimSpace(c.ContextMode), ContextModeDeepDive)
}

// AutomationsEnabled reports whether the automation scheduler may fire.
// Opt-in: the store can be inspected and edited via the `gogen automation`
// CLI regardless, but no host sweeps/fires unless automations is explicitly
// enabled.
func (c *Config) AutomationsEnabled() bool {
	return c != nil && configOn(c.Automations)
}

// StatusDisplayMode returns the normalized status-display gate: "on",
// "off", or "auto" (the default). Anything else — including an absent value
// — resolves to "auto", so a typo degrades to discovery rather than
// silently reporting to nowhere or staying off.
func (c *Config) StatusDisplayMode() string {
	if c == nil {
		return StatusDisplayAuto
	}
	switch strings.ToLower(strings.TrimSpace(c.StatusDisplay)) {
	case StatusDisplayOn:
		return StatusDisplayOn
	case StatusDisplayOff:
		return StatusDisplayOff
	case StatusDisplayBrowser:
		return StatusDisplayBrowser
	default:
		return StatusDisplayAuto
	}
}

// SubagentDepth returns the effective maximum subagent nesting depth
// (main agent = depth 0). Values <= 0 fall back to DefaultSubagentMaxDepth.
func (c *Config) SubagentDepth() int {
	if c == nil || c.SubagentMaxDepth <= 0 {
		return DefaultSubagentMaxDepth
	}
	return c.SubagentMaxDepth
}

// SubagentLimit returns the effective maximum number of concurrently
// running subagents per parent session. Values <= 0 fall back to
// DefaultSubagentMaxConcurrent.
func (c *Config) SubagentLimit() int {
	if c == nil || c.SubagentMaxConcurrent <= 0 {
		return DefaultSubagentMaxConcurrent
	}
	return c.SubagentMaxConcurrent
}

// TreeSitterEnabled reports whether tree-sitter checks are active.
// Enabled by default (on unless explicitly set to "off", "0", or "false").
func (c *Config) TreeSitterEnabled() bool {
	if c == nil {
		return true
	}
	return !configOff(c.TreeSitter)
}

// WebFetchEnabled reports whether the web_fetch tool is active.
func (c *Config) WebFetchEnabled() bool {
	if c == nil {
		return false
	}
	return configOn(c.WebFetch)
}

// WebSearchEnabled reports whether the web_search tool is active.
func (c *Config) WebSearchEnabled() bool {
	if c == nil {
		return false
	}
	return configOn(c.WebSearch)
}

// OutputSpillEnabled reports whether the spill feature is active (oversized
// tool output persisted to the session's spill dir with a head/tail
// preview + locator). Enabled by default: on unless explicitly set to an
// "off" spelling (absence of a value does not disable it).
func (c *Config) OutputSpillEnabled() bool {
	if c == nil {
		return true
	}
	return !configOff(c.OutputSpill)
}
