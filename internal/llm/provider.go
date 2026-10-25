package llm

import (
	"context"
	"time"
)

// Tool represents a tool the AI agent can call.
type Tool struct {
	Type        string
	Name        string
	Description string
	Parameters  map[string]any
}

// Usage reports token counts from the provider when available.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens is the subset of PromptTokens served from the provider prompt cache.
	CachedTokens int
}

// Response represents the AI agent's response.
type Response struct {
	Content   string
	Reasoning string
	Refusal   string // model refusal text when content is empty (kept separate for cache stability)
	ToolCalls []ToolCall
	Usage     *Usage
	// Model is the model ID reported by the provider for this response (may
	// differ from the requested alias on router/proxy endpoints such as
	// OpenCode Zen). Empty when the provider did not report one.
	Model string
}

// ToolCall represents a request from the AI to execute a tool.
type ToolCall struct {
	Index int // stream index from the provider, used to correlate streaming UI
	ID    string
	Name  string
	Args  map[string]any
	// ArgsStr is the exact JSON arguments string from the provider.
	// Prefer this when re-sending history so prompt-cache prefixes stay byte-stable.
	ArgsStr string
	// ArgsError is set when streamed tool arguments could not be parsed.
	// Callers must not execute the tool; return this error as the tool result.
	ArgsError string
	// ArgsJSONValid is true when ArgsStr is non-empty, already trimmed, and
	// valid JSON — i.e. the exact bytes the wire serializer will send.
	// Set by StabilizeToolCallArgs (stabilization, stream finalization) and
	// by session restore, always before the message is published; the wire
	// serializer only READS it to skip the per-request json.Valid scan of
	// every historical tool call. Never set by the serializer itself (it
	// runs outside the stats lock on shared stabilized ToolCalls). Not
	// serialized — restored messages recompute it once at load.
	ArgsJSONValid bool `json:"-"`
}

// StreamCallback receives partial tokens as they arrive from a streamed response.
type StreamCallback func(token string)

// StreamHandlers provides optional callbacks for streaming and progress events.
type StreamHandlers struct {
	OnStart        func() // called once when processing begins
	OnRoundStart   func() // called at the start of each LLM round after the first
	OnStreamOpened func() // called when the SSE connection is established
	OnCompacting   func() // called when context compaction starts (before the summarization call)
	// OnCondensed is called when the last-resort condensation (Phase 0e)
	// replaced a message that could not fit the context window: note is
	// the human-readable announcement (message size vs window, original
	// archived) for an in-band system line / banner.
	OnCondensed func(note string)
	// OnStreamStall is called when no SSE chunk has arrived for several
	// seconds while the round is still streaming (see streamStallAfter).
	// Hosts use it to show a "still waiting" state: a long prefill or a
	// server-side stall is otherwise indistinguishable from a dead UI.
	// Informational only — it does not affect the stream.
	OnStreamStall    func()
	OnStreamRetry    func(reason string) // called when a failed stream attempt is retried (muted retry / non-stream fallback)
	OnStreamActivity func()              // called on the first visible content/refusal token
	OnThinkingToken  StreamCallback      // called for each reasoning/thinking token (display separately)
	OnToken          StreamCallback      // called for each content token
	OnStreamEnd      func()              // called when a streamed LLM turn completes with pending tool calls
	// OnReplyModel is called with the provider-reported model ID for every
	// completed round, right before OnStreamEnd finalizes it. Receives ""
	// when the provider did not report a model.
	OnReplyModel           func(model string)
	OnToolCallStart        func(index int, id, name string)            // called when a tool call name first appears in the stream
	OnToolCallArgsDelta    func(index int, id, name, argsDelta string) // called for each streamed args fragment
	OnToolCall             func(tc ToolCall)                           // called before a tool executes (args fully parsed)
	OnRecoverPartialStream func()                                      // reset UI after stream error mid-tool-call
	OnToolExecute          func(name string)                           // called immediately before a tool runs (may block)
	OnToolOutput           func(id, name, command, chunk string)       // live combined stdout/stderr chunks from shell tools while they run
	OnToolOutputEnd        func(id string, success bool)               // the tool call's live output stream terminated (foreground: when the command finishes; background job: when the job exits, possibly long after the turn ended)
	OnToolResult           func(id, name, result string, success bool) // called after a tool executes
	// OnMessageAppended reports the index in the agent's message list of the
	// assistant message each ROUND just appended. Hosts use it to track how
	// far their rendered transcript has advanced, so a later conditional
	// attach can prove the transcript is current instead of re-fetching the
	// history. Messages the host does not render (tool results, the synthetic
	// image user message) are deliberately not reported: a trailing mark
	// stays conservative and only costs the unoptimized fallback.
	OnMessageAppended func(index int, role string)
	// StopStream is checked after each streamed chunk. When it returns true
	// the provider returns the text accumulated so far with BudgetStop set
	// and a nil error. It does not cancel the turn context: a cancelled
	// stream drops the partial text and the retry ladder would start the
	// thinking over.
	StopStream func() bool
}

// StreamResult holds the final accumulated response from a streamed call.
type StreamResult struct {
	Content       string
	Reasoning     string // reasoning/thinking content accumulated during streaming
	Refusal       string // refusal text accumulated during streaming (kept separate from Content)
	ToolCalls     []ToolCall
	Usage         *Usage
	PartialStream bool // true when streaming failed after partial output before fallback
	// Model is the model ID reported by the provider for this stream (may
	// differ from the requested alias on router/proxy endpoints such as
	// OpenCode Zen). Empty when the provider did not report one.
	Model string
	// FinishReason is the provider-reported stop cause from the stream's
	// final choice ("stop", "length", "tool_calls", ...); the streaming
	// accumulator infers it when a provider omits the final chunk. Empty
	// for results built outside the streaming accumulator (test stubs, the
	// non-streaming fallback).
	FinishReason string
	// BudgetStop is set when StopStream ended the stream early so the
	// caller can compact before continuing. The result is still the
	// partial generation; the error is nil so the stream is not retried.
	BudgetStop bool
}

// ModelInfo describes a model available from the provider endpoint.
type ModelInfo struct {
	ID           string
	ContextLimit int
	Current      bool
	// Provider is the name of the registered provider profile that serves
	// this model ("default" for the legacy single-endpoint configuration).
	// The web model picker groups models by it.
	Provider string
	// Description is the models.dev model description; empty when the model is
	// unknown or the registry has no entry.
	Description string
	// ReasoningEfforts are the reasoning_effort values this model accepts
	// (from the models.dev registry). Empty when the model is unknown or has
	// no effort control (toggle/budget-only models).
	ReasoningEfforts []string
	// Pricing from models.dev (USD per 1M tokens). Zero means no pricing data.
	InputPricePer1M  float64
	OutputPricePer1M float64
	CachedPricePer1M float64
}

// LLMProvider defines the interface for different LLM providers.
type LLMProvider interface {
	GenerateResponse(ctx context.Context, messages []Message, allowedTools map[string]struct{}, extraTools []Tool) (Response, error)
	GenerateResponseStream(ctx context.Context, messages []Message, allowedTools map[string]struct{}, extraTools []Tool, h *StreamHandlers) (*StreamResult, error)
	// ModelContextLimit resolves the current model's context window (tokens)
	// from provider metadata and the models.dev registry. A positive value
	// is the model's window; 0 means the window could not be resolved (the
	// registry may still be warming) and the caller must apply its own
	// fallback — 0 must never be reported to the UI as a real window.
	ModelContextLimit(ctx context.Context) (int, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
	SetModel(id string) error
	ModelName() string
	// SetThinkingLevel controls reasoning/thinking effort. Pass empty string to
	// omit the parameter (equivalent to "off").
	SetThinkingLevel(level string)
}

// ModelRefProvider is implemented by providers that route models by the
// (provider profile, model ID) pair instead of the bare model ID alone
// (OpenAIProvider: model IDs are not unique across registered providers).
// It is optional: providers without it (test stubs) treat every model as
// belonging to the default profile.
type ModelRefProvider interface {
	// SetModelRef selects the model AND its owning profile in one step.
	SetModelRef(ref ModelRef) error
	// CurrentModelRef returns the current model's routing key (owning
	// profile + bare wire ID; zero ID = no model selected).
	CurrentModelRef() ModelRef
}

// ReasoningEffortsProvider is implemented by providers that can report the
// reasoning-effort values the current model accepts (from the models.dev
// registry, or derived from a llama.cpp /props capability probe for unknown
// self-hosted models). It is optional: providers without it (test stubs,
// providers with no registry data) fall back to DefaultReasoningEfforts.
type ReasoningEffortsProvider interface {
	// ModelReasoningEfforts returns the effective reasoning-effort options for
	// ref: the models.dev accepted set when the model is known (empty for
	// toggle/budget-only models, meaning no effort control), else
	// DefaultReasoningEfforts. Must never block on the network.
	ModelReasoningEfforts(ref ModelRef) []string
}

// ModelDescriptionProvider is implemented by providers that can report the
// models.dev description of a model. It is optional: providers without it
// yield an empty description (no tooltip in the UI).
type ModelDescriptionProvider interface {
	// ModelDescription returns the models.dev description for ref, or ""
	// when the model is unknown. Must never block on the network.
	ModelDescription(ref ModelRef) string
}

// Message represents a chat message.
type Message struct {
	Role       string
	Content    string
	Reasoning  string     // reasoning/thinking content from the model (sent as reasoning_content)
	Refusal    string     // refusal text from the model (sent as refusal; not folded into Content)
	ToolCalls  []ToolCall // set on assistant messages that invoke tools
	ToolCallID string     // set on tool result messages
	// Images holds user-supplied images (data URLs) attached to a user
	// message, sent to vision-capable models as image_url content parts.
	// Only user messages carry images.
	Images    []ImageInput `json:"images,omitempty"`
	CreatedAt time.Time    `json:"createdAt,omitempty"` // when the message was created (UTC), zero when not set
	// Model is the model ID that produced this message, as reported by the
	// provider (may differ from the requested alias on router/proxy
	// endpoints such as OpenCode Zen). Empty when not reported.
	Model string `json:"model,omitempty"`

	// ArgsStabilized is true when ToolCalls[j].Args has been marshalled
	// into ArgsStr. Used to avoid re-stabilizing every message on every
	// turn. Not serialized — restored messages start as stabilized.
	ArgsStabilized bool `json:"-"`
}

// ImageInput is one user-supplied image attached to a user message,
// transported as a data URL (e.g. "data:image/png;base64,..."). Detail
// controls the vision detail level the provider receives: "auto", "low", or
// "high"; empty means "auto".
type ImageInput struct {
	DataURL string `json:"dataUrl"`
	Detail  string `json:"detail,omitempty"`
}

// HasImages reports whether the message carries one or more images.
func (m Message) HasImages() bool {
	return len(m.Images) > 0
}
