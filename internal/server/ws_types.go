package server

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/automation"
	"gogen/internal/llm"
	"gogen/internal/steerq"
	"gogen/internal/streambuf"
)

type ModelEntry struct {
	ID           string `json:"id"`
	ContextLimit int    `json:"contextLimit,omitempty"`
	Current      bool   `json:"current,omitempty"`
	// Provider is the registered provider profile name serving this model
	// ("default" for the legacy single endpoint); the picker groups models
	// by it.
	Provider         string  `json:"provider,omitempty"`
	InputPricePer1M  float64 `json:"inputPricePer1M,omitempty"`
	OutputPricePer1M float64 `json:"outputPricePer1M,omitempty"`
	CachedPricePer1M float64 `json:"cachedPricePer1M,omitempty"`
	// ReasoningEfforts are the reasoning_effort values this model accepts
	// (models.dev); empty for unknown or toggle/budget-only models.
	ReasoningEfforts []string `json:"reasoningEfforts,omitempty"`
	// Description is the models.dev model description; empty for unknown
	// models. Shown as a hover tooltip in the client.
	Description string `json:"description,omitempty"`
}

type SessionEntry struct {
	ID           string `json:"id"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
	MessageCount int    `json:"messageCount,omitempty"`
	Label        string `json:"label,omitempty"`
	Oneshot      bool   `json:"oneshot,omitempty"`
	// Active marks sessions that are genuinely live: open as a pane in
	// some tab (an attached viewer) or with a turn running server-side.
	// The client renders "resume to continue" for them. A merely
	// registered runtime does NOT set the flag — the restored default
	// session after a restart and passive (approval-only) attachments
	// must not pin the indicator for a session nobody is viewing.
	Active bool `json:"active,omitempty"`
	// TurnActive marks active sessions whose runtime currently has a
	// running turn. The client uses it to distinguish a genuinely running
	// session ("responding") from one that is merely registered-but-idle
	// (open as a pane, or resumed from the store after a restart) — e.g.
	// when rendering nested (subagent) rows after a reload, when the live
	// subagent_started/finished events are gone.
	TurnActive bool `json:"turnActive,omitempty"`
	// Registered marks sessions whose runtime is live in THIS process,
	// viewed or not. Active and TurnActive are subsets of it (a runtime
	// with a viewer or a running turn is necessarily registered). The
	// client uses it to tell a live child that nobody is viewing — a
	// background subagent between turns, held for its retention window —
	// from one whose runtime is gone: the first is idle but continuable,
	// the second never finished. Without it the label depended on whether
	// somebody happened to have the child open.
	Registered bool `json:"registered,omitempty"`
	// ParentID is non-empty for nested (subagent) sessions; the client
	// renders them as indented rows under their parent.
	ParentID string `json:"parentId,omitempty"`
	// SubagentStatus is the persisted outcome of a nested (subagent)
	// session: "running" (admitted, no terminal state recorded), "success",
	// "failed", or "" (not a subagent / legacy record). The sidebar uses it
	// when the live subagent_started/finished events are gone (reload /
	// restart), so a failed child stays failed and a child that never
	// finished reads as interrupted rather than done.
	SubagentStatus string `json:"subagentStatus,omitempty"`
	// SubagentSummary is the report/error summary written alongside
	// SubagentStatus.
	SubagentSummary string `json:"subagentSummary,omitempty"`
	// Label is now the full first user message — CSS text-overflow: ellipsis
	// handles dynamic truncation on the client side.
}

type HistoryToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// BoardOpRequest is one kanban-tab operation sent client→server as a
// "board_op" message: list (no mutation) or add/update/claim/move/block/
// comment/done/start/remove. After a successful mutation the server
// broadcasts a fresh board_state to every client.
type BoardOpRequest struct {
	Action      string `json:"action,omitempty"`
	ID          string `json:"id,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Priority    string `json:"priority,omitempty"`
	// Context is the free-text context merged into a started agent's prompt
	// (update only).
	Context string `json:"context,omitempty"`
	Column  string `json:"column,omitempty"`
	Text    string `json:"text,omitempty"`
	// Reason is the block reason (block only).
	Reason string `json:"reason,omitempty"`
	// Model is the per-ticket model chosen in the "Start agent" popover
	// ("" = workspace default model). start only.
	Model string `json:"model,omitempty"`
	// Prompt is the per-ticket prompt template for the started agent
	// ("" = the configured board_start_prompt template; the stored ticket
	// override is cleared). start only.
	Prompt string `json:"prompt,omitempty"`
	// ThinkingLevel is the per-ticket reasoning-effort level chosen in the
	// "Start agent" popover ("" = inherit the active pane's live level;
	// the stored ticket override is cleared). Validated against the
	// ticket's final model at start time. start only.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	// ReviewModel / ReviewThinkingLevel are the per-ticket review-agent
	// options chosen in the card popover's review section. review_options
	// only ("" clears back to the inherited defaults); the review trigger
	// resolves the cascade and validates the level against the final
	// model at review-start time.
	ReviewModel         string `json:"reviewModel,omitempty"`
	ReviewThinkingLevel string `json:"reviewThinkingLevel,omitempty"`
}

// AutomationsOpRequest is one automations-tab operation sent client→server
// as an "automations_op" message: list/runs (reads) or create/enable/
// disable/delete (mutations — each broadcast a fresh automations_state).
// The create fields mirror the `gogen automation create` flags; exactly one
// schedule kind is required and the server validates via the same
// Automation.Validate as the CLI.
type AutomationsOpRequest struct {
	Action string `json:"action,omitempty"`
	ID     string `json:"id,omitempty"`
	// Create fields.
	Title      string `json:"title,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	WorkingDir string `json:"workingDir,omitempty"`
	Kind       string `json:"kind,omitempty"`
	At         string `json:"at,omitempty"`
	Time       string `json:"time,omitempty"`
	Minute     int    `json:"minute,omitempty"`
	Weekdays   []int  `json:"weekdays,omitempty"`
	Timezone   string `json:"timezone,omitempty"`
	// StartDisabled creates the automation paused.
	StartDisabled bool `json:"startDisabled,omitempty"`
}

// AutomationsState is the automations-tab snapshot (server→client): the
// feature-flag state, the store health, and all saved automations.
type AutomationsState struct {
	Enabled     bool                    `json:"enabled"`
	StoreOK     bool                    `json:"storeOk"`
	StoreError  string                  `json:"storeError,omitempty"`
	Automations []automation.Automation `json:"automations"`
}

// AutomationsRunsPayload is the run history of one automation
// (server→client reply to the "runs" op).
type AutomationsRunsPayload struct {
	AutomationID string           `json:"automationId"`
	Runs         []automation.Run `json:"runs"`
}

// ProviderEntry is one registered OpenAI-compatible provider in the config
// push: name, base URL, optional default model, and whether a key is
// stored — the key itself is NEVER pushed to the client.
type ProviderEntry struct {
	Name      string `json:"name"`
	BaseURL   string `json:"baseUrl"`
	Model     string `json:"model,omitempty"`
	APIKeySet bool   `json:"apiKeySet"`
	// Deletable is false for the implicit default profile (built from the
	// legacy config fields), which cannot be deleted.
	Deletable bool `json:"deletable"`
}

// ProviderOpRequest is one provider-list operation sent client→server via
// provider_save / provider_delete / test_provider.
type ProviderOpRequest struct {
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
	Model   string `json:"model,omitempty"`
}

// ProviderTestResult is the test_provider reply: connectivity + model
// catalog check against a throwaway provider that is never registered or
// wired to a session.
type ProviderTestResult struct {
	OK        bool         `json:"ok"`
	LatencyMs int64        `json:"latencyMs,omitempty"`
	Models    []ModelEntry `json:"models,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// MCPOpRequest is one MCP server-list mutation (mcp_save / mcp_delete).
//
// Save upserts by name: a blank Command on an EXISTING entry keeps the
// stored command, and an empty Env keeps the stored env (so re-saving a row
// without retyping secrets cannot wipe them). The per-server timeout
// overrides are kept unless explicitly provided (0 = keep). Delete removes
// by name. Both are restart-scoped (the MCP manager is built at startup):
// the list is persisted for the next start and pushed back immediately, but
// no server is started or stopped live.
type MCPOpRequest struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// InitTimeoutSecs / ToolTimeoutSecs: 0 keeps the stored value on an
	// existing entry (and means "client default" on a new one).
	InitTimeoutSecs int `json:"initTimeoutSecs,omitempty"`
	ToolTimeoutSecs int `json:"toolTimeoutSecs,omitempty"`
}

// MCPTestRequest carries one MCP server test (test_mcp, client→server):
// either a registered server name (stored command/args/env resolved
// server-side) or the raw command/args/env to probe (add form).
type MCPTestRequest struct {
	Name    string            `json:"name,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// MCPTestResult is the test_mcp reply: connectivity + tools/list check
// against a throwaway stdio process that is never registered.
type MCPTestResult struct {
	OK        bool          `json:"ok"`
	LatencyMs int64         `json:"latencyMs,omitempty"`
	Tools     []MCPTestTool `json:"tools,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// MCPTestTool is one tool exposed by a probed MCP server.
type MCPTestTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// MCPEntry is one MCP server in the config push: name, command, args, and
// whether env values are set. The env values themselves are never pushed.
// Host is a process-local server (GOGEN_MCP_EXTRA) that is running in this
// process. It is not in the config file.
type MCPEntry struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	EnvSet  bool     `json:"envSet,omitempty"`
	Host    bool     `json:"host,omitempty"`
}

type HistoryEntry struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	Images    []llm.ImageInput `json:"images,omitempty"`
	Reasoning string           `json:"reasoning,omitempty"`
	Refusal   string           `json:"refusal,omitempty"`
	// Model is the model ID that produced the reply, as reported by the
	// provider (may differ from the requested alias on router endpoints
	// such as OpenCode Zen). Empty when not reported.
	Model      string            `json:"model,omitempty"`
	ToolCalls  []HistoryToolCall `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
	Index      int               `json:"index"`               // index in agent.Messages (0 is valid; do not omitempty)
	CreatedAt  string            `json:"createdAt,omitempty"` // RFC3339Nano UTC when the message was created
}

// QueueItem is one queued user steering message on the wire (server→client,
// on queue_update frames and session_state): the client correlates its
// queued bubble by ID and cancels the item with queue_remove{queueId}.
type QueueItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type WSMessage struct {
	Type            string         `json:"type"`
	Content         string         `json:"content,omitempty"`
	Tool            string         `json:"tool,omitempty"`
	TermID          string         `json:"termId,omitempty"`
	Cols            int            `json:"cols,omitempty"`
	Rows            int            `json:"rows,omitempty"`
	Code            int            `json:"code,omitempty"`
	ToolCallID      string         `json:"toolCallId,omitempty"`
	Index           int            `json:"index,omitempty"`
	ArgsDelta       string         `json:"argsDelta,omitempty"`
	Args            map[string]any `json:"args,omitempty"`
	Result          string         `json:"result,omitempty"`
	Success         bool           `json:"success,omitempty"`
	ResultTruncated bool           `json:"resultTruncated,omitempty"`
	WorkingDir      string         `json:"workingDir,omitempty"`
	Model           string         `json:"model,omitempty"`
	// ModelProvider is the registered provider profile serving the current
	// model ("default" for the legacy single endpoint). Model IDs are not
	// unique across registered providers, so the client's current-model
	// check must match (provider, id) — the bare Model alone would mark
	// every catalog row that lists the same ID.
	ModelProvider string `json:"modelProvider,omitempty"`
	// TokensPerSec is the shared streamutil.SpeedMeter's smoothed token
	// rate on stream_stats frames (content, thinking, and tool-args
	// deltas alike).
	TokensPerSec float64 `json:"tokensPerSec,omitempty"`
	// Pricing for the current model (USD per 1M tokens), populated from models.dev cache.
	InputPricePer1M  float64 `json:"inputPricePer1M,omitempty"`
	OutputPricePer1M float64 `json:"outputPricePer1M,omitempty"`
	CachedPricePer1M float64 `json:"cachedPricePer1M,omitempty"`
	// ReasoningEfforts are the reasoning_effort values the current model
	// accepts (models.dev, or a llama.cpp /props-derived set); empty means
	// unknown or no effort control. Clients fall back to the default set for
	// chips when empty and ReasoningEffortsUnsupported is absent.
	ReasoningEfforts []string `json:"reasoningEfforts,omitempty"`
	// ReasoningEffortsUnsupported is true when the current model definitively
	// has NO reasoning-effort control (a known models.dev entry with no
	// effort options, or a llama.cpp capability probe that reported no
	// support): clients hide the thinking chips entirely. Omitted when false
	// (JSON omitempty); an absent flag with empty ReasoningEfforts means
	// "unknown", not "no support" — clients keep the default chip set.
	ReasoningEffortsUnsupported bool `json:"reasoningEffortsUnsupported,omitempty"`
	// ModelDescription is the models.dev description of the current model;
	// empty means unknown. Shown as a hover tooltip in the client.
	ModelDescription string `json:"modelDescription,omitempty"`
	ContextLimit     int    `json:"contextLimit,omitempty"`
	// UsedTokens is always emitted (no omitempty): 0 is meaningful
	// (fresh session, nothing counted yet) and lets clients distinguish
	// "0 used" from "no context data at all".
	UsedTokens       int    `json:"usedTokens"`
	UsedSource       string `json:"usedSource,omitempty"`
	PromptTokens     int    `json:"promptTokens,omitempty"`
	CompletionTokens int    `json:"completionTokens,omitempty"`
	CachedTokens     int    `json:"cachedTokens,omitempty"`
	CompactAt        int    `json:"compactAt,omitempty"`
	MessageCount     int    `json:"messageCount,omitempty"`
	// Images carries user-attached images (data URLs) on inbound "message"
	// frames; the server validates and forwards them to the agent.
	Images []llm.ImageInput `json:"images,omitempty"`
	// NearCompact and WarnNearCompact intentionally have NO omitempty: a
	// false value must reach the client or a previously shown near-compact
	// banner never hides after a compaction.
	NearCompact     bool    `json:"nearCompact"`
	WarnNearCompact bool    `json:"warnNearCompact"`
	UsedPercent     float64 `json:"usedPercent,omitempty"`
	ToolTruncated   bool    `json:"toolTruncated,omitempty"`
	// Accumulated session usage
	TotalPromptTokens     int          `json:"totalPromptTokens,omitempty"`
	TotalCompletionTokens int          `json:"totalCompletionTokens,omitempty"`
	TotalCachedTokens     int          `json:"totalCachedTokens,omitempty"`
	TotalTurns            int          `json:"totalTurns,omitempty"`
	Models                []ModelEntry `json:"models,omitempty"`
	ApprovalID            string       `json:"approvalId,omitempty"`
	Approved              bool         `json:"approved,omitempty"`
	Paths                 []string     `json:"paths,omitempty"`
	Command               string       `json:"command,omitempty"`
	Reason                string       `json:"reason,omitempty"`
	Mode                  string       `json:"mode,omitempty"`
	ThinkingLevel         string       `json:"thinkingLevel,omitempty"`
	GlobalMode            bool         `json:"globalMode,omitempty"`
	SessionID             string       `json:"sessionId,omitempty"`
	SessionAction         string       `json:"sessionAction,omitempty"`
	SessionLabel          string       `json:"sessionLabel,omitempty"`
	// Board and Subagent are the live feature-flag states ("on"/"off").
	// Server→client config pushes carry the current state (the settings
	// modal initializes and stays in sync from them); client→server config
	// messages carry the requested values from the modal toggles. Empty
	// means "not provided" in either direction.
	Board    string `json:"board,omitempty"`
	Subagent string `json:"subagent,omitempty"`
	// Skills and Terminal are the live optional-tool feature flags ("on"/
	// "off"): the skill tool and the persistent-terminal feature tools.
	// Same contract as Board/Subagent.
	Skills   string `json:"skills,omitempty"`
	Terminal string `json:"terminal,omitempty"`
	// SubagentMaxDepth is the live subagent nesting-depth limit (0 = not
	// provided in client→server messages; the server normalizes <= 0 to the
	// config default when seeding agents).
	SubagentMaxDepth int `json:"subagentMaxDepth,omitempty"`
	// SubagentMaxConcurrent is the live per-parent concurrent-subagent
	// limit (0 = not provided in client→server messages; the server
	// normalizes <= 0 to the config default when enforcing).
	SubagentMaxConcurrent int `json:"subagentMaxConcurrent,omitempty"`
	// SubagentModel is the live default model for spawned subagents
	// (client→server value inside ConfigFields; server→client current value
	// in config pushes). A POINTER so config pushes always carry it —
	// including the empty "inherit" state a clear by one tab must
	// broadcast to every other tab — while non-config messages omit it
	// entirely (no per-token overhead).
	SubagentModel *string `json:"subagentModel,omitempty"`
	// SubagentThinkingLevel is the live reasoning-effort level for spawned
	// subagents (client→server value inside ConfigFields; server→client
	// current value in config pushes). A POINTER like SubagentModel so
	// config pushes always carry it — including the empty "inherit the
	// parent's level" state a clear by one tab must broadcast to every
	// other tab — while non-config messages omit it entirely.
	SubagentThinkingLevel *string `json:"subagentThinkingLevel,omitempty"`
	// BoardStartPrompt / SystemPrompt / SubagentPrompt are the configurable
	// prompt templates (settings modal "Agent" group). Pushed server→client
	// as the RESOLVED effective templates (empty config → built-in default),
	// so the fields are pre-populated with what will actually be used;
	// client→server carries the user's edits ("" = reset to the built-in
	// default).
	BoardStartPrompt  string `json:"boardStartPrompt,omitempty"`
	BoardReviewPrompt string `json:"boardReviewPrompt,omitempty"`
	SystemPrompt      string `json:"systemPrompt,omitempty"`
	SubagentPrompt    string `json:"subagentPrompt,omitempty"`
	// ReviewAgent is the live board auto-review flag state ("on"/"off"),
	// same contract as Board/Subagent.
	ReviewAgent string `json:"reviewAgent,omitempty"`
	// Automations is the live automation-scheduler flag state ("on"/"off"),
	// same contract as Board/Subagent. Toggling it starts/stops the
	// host-side automation scheduler live (see internal/automation).
	Automations string `json:"automations,omitempty"`
	// AutomationsOp carries an automations-tab operation (client→server
	// "automations_op").
	AutomationsOp *AutomationsOpRequest `json:"automationsOp,omitempty"`
	// AutomationsState carries the full automations-tab snapshot
	// (server→client, after every mutation and on request).
	AutomationsState *AutomationsState `json:"automationsState,omitempty"`
	// AutomationsRuns carries the run history of one automation
	// (server→client reply to the "runs" op).
	AutomationsRuns *AutomationsRunsPayload `json:"automationsRuns,omitempty"`
	// ReviewAgentModel is the live default model for auto review sessions
	// (client→server value inside ConfigFields; server→client current
	// value in config pushes). A POINTER like SubagentModel so config
	// pushes always carry it — including the empty "inherit" state —
	// while non-config messages omit it entirely.
	ReviewAgentModel *string `json:"reviewAgentModel,omitempty"`
	// ReviewAgentThinkingLevel is the live reasoning-effort level for auto
	// review sessions (pointer, same contract as SubagentThinkingLevel;
	// empty = inherit the workspace level).
	ReviewAgentThinkingLevel *string `json:"reviewAgentThinkingLevel,omitempty"`
	// BoardOp carries a kanban-tab operation (client→server "board_op").
	BoardOp *BoardOpRequest `json:"boardOp,omitempty"`
	// BoardState carries the full board snapshot (server→client
	// "board_state" broadcasts; the kanban tab renders from it).
	BoardState *agent.BoardSnapshot `json:"boardState,omitempty"`
	// Providers is the registered OpenAI-compatible provider list pushed in
	// config messages (name, baseUrl, model, apiKeySet — never the keys).
	Providers []ProviderEntry `json:"providers,omitempty"`
	// ConfigFilePath is where the effective config is persisted (project
	// .gogen/gogen.conf vs global config) — drives the provider-key storage
	// warning in the settings modal.
	ConfigFilePath string `json:"configFilePath,omitempty"`
	// ProviderOp carries a provider-list operation (provider_save /
	// provider_delete / test_provider, client→server).
	ProviderOp *ProviderOpRequest `json:"providerOp,omitempty"`
	// ProviderTest carries the test_provider reply (server→client).
	ProviderTest *ProviderTestResult `json:"providerTest,omitempty"`
	// AgentInstructions / JobNotices are two more live agent feature flags
	// ("on"/"off"), same contract as Board/Subagent: agent_instructions
	// loads AGENTS.md/CLAUDE.md workspace instructions into the prompt,
	// job_notices posts a follow-up turn when a background job finishes.
	AgentInstructions string `json:"agentInstructions,omitempty"`
	JobNotices        string `json:"jobNotices,omitempty"`
	// JoinQueued ("on"/"off") is join_queued: user messages queued during
	// one turn leave together as one message.
	JoinQueued string `json:"joinQueued,omitempty"`
	// TestCommand / LintCommand override the auto-detected project commands
	// (client→server inside ConfigFields; server→client current value in
	// config pushes). "" = auto-detect.
	TestCommand string `json:"testCommand,omitempty"`
	LintCommand string `json:"lintCommand,omitempty"`
	// MCPToolMode is the MCP tool-exposure mode ("nested" = the single
	// `mcp` gateway tool, "inline" = one definition per server tool).
	// Client→server inside ConfigFields; server→client carries the
	// effective mode in config pushes. It is part of the fixed request
	// prefix, so a change applies to NEW sessions only.
	MCPToolMode string `json:"mcpToolMode,omitempty"`
	// MCPOp carries an MCP server-list mutation (mcp_save / mcp_delete,
	// client→server). The saved list is pushed back as MCPServers.
	MCPOp *MCPOpRequest `json:"mcpOp,omitempty"`
	// MCPTest carries a test_mcp request (client→server).
	MCPTest *MCPTestRequest `json:"mcpTest,omitempty"`
	// MCPTestResult carries the test_mcp reply (server→client).
	MCPTestResult *MCPTestResult `json:"mcpTestResult,omitempty"`
	// MCPServers is the configured MCP server list pushed in config
	// messages (name, command, args, envSet — never env values).
	MCPServers []MCPEntry `json:"mcpServers,omitempty"`
	// ConfigFields names the config options being set in a client→server
	// "config" message (runtime-config branch). Values are applied ONLY for
	// the listed fields, so explicit empty/zero values are legal (e.g.
	// clearing the command allowlist, resetting context_limit to 0 = auto).
	ConfigFields []string `json:"configFields,omitempty"`

	// Runtime-config values (settings modal). Sent client→server inside a
	// "config" message whose ConfigFields names them; pushed server→client
	// in every config message (current values, except the two *Set flags
	// and RestartRequired below).
	CommandSafetyMode         string  `json:"commandSafety,omitempty"`
	CommandAllowlist          string  `json:"commandAllowlist,omitempty"`
	DeleteApproval            string  `json:"deleteApproval,omitempty"`
	CommandApproval           string  `json:"commandApproval,omitempty"`
	CommandSandbox            string  `json:"commandSandbox,omitempty"`
	CommandIdleTimeoutSecs    int     `json:"commandIdleTimeoutSecs,omitempty"`
	ContextLimitConfig        int     `json:"contextLimitConfig,omitempty"` // 0 = auto (provider resolution)
	CompactThreshold          float64 `json:"compactThreshold,omitempty"`
	CompactKeepRecentMessages int     `json:"compactKeepRecentMessages,omitempty"`
	MaxToolResultBytes        int     `json:"maxToolResultBytes,omitempty"`
	CompactReserveTokens      int     `json:"compactReserveTokens,omitempty"`
	CompactLastResort         string  `json:"compactLastResort,omitempty"` // condense (default) | error
	SmartCompact              string  `json:"smartCompact,omitempty"`      // off (default) | on
	ContextMode               string  `json:"contextMode,omitempty"`       // default (default) | deep_dive
	WebFetch                  string  `json:"webFetch,omitempty"`
	WebSearch                 string  `json:"webSearch,omitempty"`
	OutputSpill               string  `json:"outputSpill,omitempty"` // on (default) | off — spill oversized tool output to the session spill dir
	WebSearchBackend          string  `json:"webSearchBackend,omitempty"`
	WebSearchAPIKey           string  `json:"webSearchApiKey,omitempty"` // client→server only
	WebSearchAPIKeySet        bool    `json:"webSearchApiKeySet,omitempty"`
	WebAllowedDomains         string  `json:"webAllowedDomains,omitempty"`
	WebFetchMode              string  `json:"webFetchMode,omitempty"`
	TreeSitter                string  `json:"treesitter,omitempty"`
	TreeSitterLangs           string  `json:"treesitterLangs,omitempty"`
	StatusDisplay             string  `json:"statusDisplay,omitempty"` // off | auto (probe) | on | browser — LAN status-screen reporter
	StatusDisplayHost         string  `json:"statusDisplayHost,omitempty"`
	StatusDisplayToken        string  `json:"statusDisplayToken,omitempty"` // client→server only
	StatusDisplayTokenSet     bool    `json:"statusDisplayTokenSet,omitempty"`
	StatusDisplaySource       string  `json:"statusDisplaySource,omitempty"`
	// StatusDisplayTokenValue is the display token pushed to web clients
	// ONLY in browser mode (status_display: browser), where the browser —
	// not the server — is the sender and therefore needs the value. Never
	// pushed in the server-side modes (the settings modal shows the set flag
	// instead). The chat socket is authenticated, and the settings UI
	// already accepts the token, so this is not a new capability for a
	// connected client.
	StatusDisplayTokenValue string `json:"statusDisplayTokenValue,omitempty"`
	// StatusDisplayRow / StatusDisplayClear / StatusDisplaySource relay one
	// display operation to web clients in browser mode: the server builds
	// the row (it owns the turn state) and the browser POSTs it, because the
	// browser may be on the display's network when the server is not.
	StatusDisplayRow    string `json:"statusDisplayRow,omitempty"`
	StatusDisplayClear  bool   `json:"statusDisplayClear,omitempty"`
	PreserveReasoning   string `json:"preserveReasoning,omitempty"`
	SessionMaxCount     int    `json:"sessionMaxCount,omitempty"`
	SessionMaxAgeDays   int    `json:"sessionMaxAgeDays,omitempty"`
	WebApprovalHoldSecs int    `json:"webApprovalHoldSecs,omitempty"`
	// Restart-staged options (A0b): persisted, applied on the next start.
	WebBind              string `json:"webBind,omitempty"`
	WebAllowedOrigins    string `json:"webAllowedOrigins,omitempty"`
	WebAuthToken         string `json:"webAuthToken,omitempty"` // client→server only
	WebAuthTokenSet      bool   `json:"webAuthTokenSet,omitempty"`
	WebTLSCertFile       string `json:"webTLSCertFile,omitempty"`
	WebTLSKeyFile        string `json:"webTLSKeyFile,omitempty"`
	WebMaxActiveSessions int    `json:"webMaxActiveSessions,omitempty"`
	MCP                  string `json:"mcp,omitempty"`
	// RestartRequired lists restart-staged settings whose staged value
	// differs from the running process (server→client); the settings modal
	// renders the "restart to take effect" banner from it.
	RestartRequired []string `json:"restartRequired,omitempty"`
	// Subagent event fields (subagent_started / subagent_finished): the
	// sidebar renders nested rows from these.
	SubagentID      string `json:"subagentId,omitempty"`
	SubagentLabel   string `json:"subagentLabel,omitempty"`
	SubagentJob     string `json:"subagentJob,omitempty"`
	SubagentParent  string `json:"subagentParent,omitempty"`
	SubagentSuccess bool   `json:"subagentSuccess,omitempty"`
	SubagentSummary string `json:"subagentSummary,omitempty"`
	// Kind scopes a "notice" message to the UI surface that produced it
	// ("board", "settings", "workspace", "model", "sessions", "models", …).
	// The client uses it for kind-based follow-ups (e.g. resync the board
	// after a failed board op) without per-feature message types.
	//
	// MESSAGE-TYPE CONTRACT: "response" is the CONVERSATION channel — it
	// renders into the chat transcript and finalizes in-flight stream state
	// (typed commands, session commands, turn outcomes). "notice" is the
	// UI-FEEDBACK channel — it only toasts and NEVER touches chat/stream
	// state (board ops, settings toggles, working-dir input, model picker,
	// sidebar refreshes). Handlers must pick by that rule: UI-channel
	// errors must go out as notice, never as response.
	Kind string `json:"kind,omitempty"`
	// TurnActive describes a session's in-flight turn; sent in session_state
	// replies so a reconnecting client can render "resuming…".
	TurnActive bool `json:"turnActive,omitempty"`
	// QueueID is the client-provided correlation id for a user steering
	// message sent while a turn is running (client→server, on "message"
	// frames): the server stores it as the queue item's ID and echoes it on
	// queue_update, so the sending tab can resolve its optimistic queued
	// bubble without a matching round trip. A "response" that refuses a
	// message while a turn is running (queue full, busy) echoes its QueueID,
	// because during a turn the reply order alone does not say which send it
	// answers. Client→server it also names the queued item a
	// "queue_remove" drops and the one a "queue_send_now" was shown on (""
	// = any queued user message). A refused queue_send_now is answered with
	// a "queue" notice.
	QueueID string `json:"queueId,omitempty"`
	// Queue carries the session's pending user steering messages
	// (server→client, on queue_update frames and session_state) so every
	// attached client renders the same queued state.
	Queue []QueueItem `json:"queue,omitempty"`
	// QueueLeft lists queue-item ids that left the queue because their turn
	// STARTED (server→client, on the queue_update frame emitted at the
	// drain's pop, which reaches every attached client before the started
	// turn's user_acked): the client clears those items' queued chips and
	// resolves the paired user_acked normally. Ids absent from both Queue
	// and QueueLeft left the queue by REMOVAL (per-item ✕, clear-all,
	// interrupt) and render as cancelled-before-running. session_state
	// snapshots never carry it (an attach has no "left" to report). On the
	// same frame, Content is the one message the started turn runs: the
	// item's text, or with join_queued the joined texts of every id listed,
	// so a client draws that text instead of rebuilding the join.
	QueueLeft    []string       `json:"queueLeft,omitempty"`
	MessageIndex int            `json:"messageIndex,omitempty"`
	Sessions     []SessionEntry `json:"sessions,omitempty"`
	History      []HistoryEntry `json:"history,omitempty"`
	// NoHistory requests a lightweight session_attach: the server skips the
	// full history snapshot (and rewind) and sends only session_state +
	// config + context. The client uses it to re-register BACKGROUND panes
	// on reconnect — their transcript re-derives from a full attach when
	// focused, so the (potentially multi-MB) history payload would be
	// discarded client-side. Absent/false = full attach (unchanged).
	NoHistory bool `json:"noHistory,omitempty"`
	// ContentPos / ThinkingPos / ArgsPos are cumulative character offsets
	// within the current round's content / thinking / tool-args streams,
	// stamped on stream / thinking_token / tool_call_delta frames. A client
	// uses them to merge an attach rewind with live content exactly — never
	// duplicating a chunk already inside the snapshot nor dropping one
	// beyond it.
	ContentPos  int `json:"contentPos,omitempty"`
	ThinkingPos int `json:"thinkingPos,omitempty"`
	ArgsPos     int `json:"argsPos,omitempty"`
	// HistoryEpoch is a per-session counter bumped whenever the conversation
	// is replaced wholesale (compaction/restore/rollback/fork), letting
	// clients distinguish a stale snapshot from a reshaped history.
	HistoryEpoch uint64 `json:"historyEpoch,omitempty"`
	// KnownHistoryEpoch / KnownHistoryIndex carry a client's rendered
	// transcript state on a session_attach (request direction): the history
	// epoch of the last history payload it applied and the newest message
	// index rendered in the pane. When both still match the session's
	// HistoryFingerprint (same epoch, and the client has rendered through
	// the newest shipped entry) and no turn is in flight, the attach skips
	// building and sending the history snapshot entirely — the pane keeps
	// its cached transcript; when only an in-flight round's partial output
	// is missing, a rewind-only frame (RewindOnly) ships just that rewind.
	// Pointers so "provided epoch 0" is distinguishable from "absent"
	// (absent = full attach, unchanged).
	KnownHistoryEpoch *uint64 `json:"knownHistoryEpoch,omitempty"`
	KnownHistoryIndex *int    `json:"knownHistoryIndex,omitempty"`
	// RewindOnly marks a history frame whose transcript portion was
	// suppressed by a conditional attach: the client's knownHistoryEpoch /
	// knownHistoryIndex proved its transcript is already current, so it
	// must keep the rendered transcript and merge ONLY the carried rewind
	// (the in-flight round's partial output) instead of clearing and
	// replaying. Never set on a full history frame.
	RewindOnly bool `json:"rewindOnly,omitempty"`
	// Rewind carries the in-flight turn's partial output on a mid-turn
	// attach/resume history payload (nil when nothing has streamed yet).
	// It is the shared streambuf.Snapshot — the same struct the TUI renders
	// on a mid-turn join — so the wire shape and the join render cannot
	// drift (the JSON tags are pinned by rewind_wire_test.go).
	Rewind *streambuf.Snapshot `json:"rewind,omitempty"`
	// Filesystem / git editor APIs
	Path       string           `json:"path,omitempty"`
	Pattern    string           `json:"pattern,omitempty"`
	Glob       string           `json:"glob,omitempty"`
	IgnoreCase bool             `json:"ignoreCase,omitempty"`
	Language   string           `json:"language,omitempty"`
	Error      string           `json:"error,omitempty"`
	Entries    []FSEntry        `json:"entries,omitempty"`
	GitEntries []GitStatusEntry `json:"gitEntries,omitempty"`
	// GitStatus is the pre-bucketed v2 payload (git_status_result).
	// Pointer so omitempty can drop it from non-git messages.
	GitStatus *GitStatus `json:"gitStatus,omitempty"`
	// Branch is the target branch for git_push (empty = bare `git push`,
	// letting git resolve the configured upstream).
	Branch      string              `json:"branch,omitempty"`
	Matches     []agent.SearchMatch `json:"matches,omitempty"`
	Truncated   bool                `json:"truncated,omitempty"`
	Original    string              `json:"original,omitempty"`
	Modified    string              `json:"modified,omitempty"`
	Diff        string              `json:"diff,omitempty"`
	RequestID   string              `json:"requestId,omitempty"`
	Replacement string              `json:"replacement,omitempty"`
	Replaced    int                 `json:"replaced,omitempty"`
	FileCount   int                 `json:"fileCount,omitempty"`
	// Files lists the workspace-relative paths modified by fs_replace, so the
	// editor can refresh the open buffers for exactly those files.
	Files []string `json:"files,omitempty"`
}

func applyContextStats(msg *WSMessage, stats agent.TurnContext, accum *agent.UsageAccumulator) {
	snap := stats.Snapshot
	// An unresolved limit is the snapshot's 128k display fallback, not the
	// model's window: omit contextLimit to keep the wire contract
	// (absent = unknown), so clients can show their own best estimate
	// (the model list's per-model limit) instead of a wrong-but-plausible
	// number. The first resolved message (turn start, model switch,
	// session restore) carries the real value and overwrites it.
	if snap.Limit > 0 && snap.LimitResolved {
		msg.ContextLimit = snap.Limit
	}
	// Always report the used count (0 for a fresh session) so clients can
	// distinguish "0 used" from "no context data at all" — the web badge
	// renders "—/limit" for the former instead of "unknown".
	msg.UsedTokens = snap.Used
	msg.PromptTokens = stats.PromptTokens
	msg.CompletionTokens = stats.CompletionTokens
	msg.CachedTokens = stats.CachedTokens
	msg.CompactAt = snap.CompactAt
	msg.MessageCount = snap.MessageCount
	msg.NearCompact = snap.NearCompact
	msg.WarnNearCompact = snap.WarnNearCompact
	msg.ToolTruncated = snap.ToolTruncated
	// When the API returned exact usage, the Snapshot.Used incorporates that
	// authoritative baseline (plus estimates for any messages added after the
	// last request). Otherwise it's purely a local estimate.
	msg.UsedSource = "estimated"
	if stats.LastUsage != nil && stats.LastUsage.PromptTokens > 0 {
		msg.UsedSource = "api"
	}
	if msg.ContextLimit > 0 {
		msg.UsedPercent = snap.Percent
	}
	if accum != nil {
		msg.TotalPromptTokens = accum.TotalPromptTokens
		msg.TotalCompletionTokens = accum.TotalCompletionTokens
		msg.TotalCachedTokens = accum.TotalCachedTokens
		msg.TotalTurns = accum.TotalTurns
	}
}

// Limits for user-attached images (vision input). These cap WebSocket frame
// size and per-message model cost without being overly restrictive. The
// image count is steerq's, because a joined queue run must stay under it too.
const (
	maxImagesPerMessage   = steerq.MaxImagesPerMessage
	maxImageDataURLBytes  = 5 * 1024 * 1024
	imageDataURLPrefix    = "data:image/"
	imageDataURLBase64Sep = ";base64,"
)

// validateImageInputs checks user-attached images and returns a clean copy
// (nil when the message carried none). Empty inputs are rejected: a message
// must not claim an image that carries no bytes, and the data URL must look
// like a base64 image to keep the wire format honest.
func validateImageInputs(images []llm.ImageInput) ([]llm.ImageInput, error) {
	if len(images) == 0 {
		return nil, nil
	}
	if len(images) > maxImagesPerMessage {
		return nil, fmt.Errorf("too many images: %d (max %d per message)", len(images), maxImagesPerMessage)
	}
	out := make([]llm.ImageInput, 0, len(images))
	for i, img := range images {
		url := strings.TrimSpace(img.DataURL)
		if url == "" {
			return nil, fmt.Errorf("image %d has no data URL", i+1)
		}
		if len(url) > maxImageDataURLBytes {
			return nil, fmt.Errorf("image %d exceeds %d bytes (data URL too large)", i+1, maxImageDataURLBytes)
		}
		if !strings.HasPrefix(url, imageDataURLPrefix) || !strings.Contains(url, imageDataURLBase64Sep) {
			return nil, fmt.Errorf("image %d must be a base64 data URL (data:image/...;base64,...)", i+1)
		}
		detail := strings.ToLower(strings.TrimSpace(img.Detail))
		switch detail {
		case "", "auto", "low", "high":
		default:
			return nil, fmt.Errorf("image %d has invalid detail %q (use auto, low, or high)", i+1, img.Detail)
		}
		out = append(out, llm.ImageInput{DataURL: url, Detail: detail})
	}
	return out, nil
}

// contextMsg builds a context stats message. Safe to call while holding the
// session's turnMu (ContextStats snapshots under its own statsMu; the turn
// goroutine calls it during a turn), but it can be slow — ContextStats
// tokenizes the full history view — so callers on the WS read loop that are
// NOT holding the lock should keep it that way.
func contextMsg(ctx context.Context, a *agent.Agent) WSMessage {
	msg := WSMessage{Type: "context", SessionID: a.SessionID}
	accum := a.SnapshotUsageAccum()
	applyContextStats(&msg, a.ContextStats(ctx), &accum)
	msg.SessionLabel = a.SessionLabelSnapshot()
	return msg
}
