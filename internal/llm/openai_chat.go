package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/debuglog"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// clientForModel returns the openai.Client that should serve the currently
// selected model (its ModelRef routing key: owning profile + bare wire ID).
//
//   - A non-OpenCode profile serves every model picked under it: the
//     profile's stream client directly, no discovery.
//   - An OpenCode profile fronts two catalogs (zen + go) behind one profile:
//     the modelClient twin routing (built by applyRouting from the last
//     successful catalog fetch — Go takes precedence on overlap) decides
//     which twin serves the model. A miss triggers a bounded catalog fetch
//     (single-flight, backoff-gated), then the models.dev inference
//     (inferOpenCodeEndpoint), then the profile's configured endpoint.
//
// OpenCode note: the gateways do not implement GET /models/{model}, so a
// per-model probe can never tell the Zen endpoint from the Go endpoint —
// discovery must build routing from the full /models lists instead.
func (p *OpenAIProvider) clientForModel(ctx context.Context) *openai.Client {
	p.modelsMu.RLock()
	ref := ModelRef{Profile: p.currentModelProfile, ID: p.model}.Normalized()
	var prof *providerProfile
	for _, pr := range p.profiles {
		if pr.name == ref.ProfileName() {
			prof = pr
			break
		}
	}
	if prof == nil {
		// The owning profile is not registered (direct-constructed provider,
		// or a routing key SetProfiles has not yet reset): the default
		// profile serves.
		if len(p.profiles) > 0 {
			prof = p.profiles[0]
		}
	}
	if prof == nil {
		p.modelsMu.RUnlock()
		return p.fallbackClient()
	}
	if prof.zenStream == nil || prof.goStream == nil {
		c := prof.stream
		p.modelsMu.RUnlock()
		return c
	}
	if c, ok := p.modelClient[ref]; ok {
		p.modelsMu.RUnlock()
		return c
	}
	p.modelsMu.RUnlock()

	// Catalog-based discovery (OpenCode twins): a bounded listModels fetch
	// populates modelClient with the exact model → twin mapping. The
	// modelsCache TTL and the modelsFetch single-flight make repeat misses
	// cheap, and the failure backoff (modelsFetchFailedAt) stops a dead
	// endpoint from being re-probed on every request. Do not hold modelsMu
	// across network I/O; bound the fetch so a hung endpoint cannot stall
	// the first chat request indefinitely. The caller's context is threaded
	// through so Ctrl+C aborts an in-flight catalog fetch instead of
	// waiting out the timeout.
	if !p.catalogFetchOnBackoff() {
		fetchCtx, cancel := context.WithTimeout(ctx, modelsCatalogTimeout)
		_, _ = p.listModels(fetchCtx)
		cancel()
	}
	p.modelsMu.RLock()
	if c, ok := p.modelClient[ref]; ok {
		p.modelsMu.RUnlock()
		return c
	}
	p.modelsMu.RUnlock()
	// The model is not in the catalog — models.dev inference (Go over Zen),
	// then the profile's configured endpoint.
	if c := p.inferOpenCodeEndpoint(prof, ref.ID); c != nil {
		return c
	}
	return prof.stream
}

// inferOpenCodeEndpoint picks the twin client that should serve model on the
// OpenCode profile prof without a catalog fetch, using the models.dev
// registry (in-memory/disk cached; never blocks on the network). Go takes
// precedence: a model listed on both the Go and Zen catalogs routes to the
// Go endpoint. Returns nil when nothing can be determined (cold registry,
// model unknown), in which case the caller falls back to the profile's
// configured endpoint.
func (p *OpenAIProvider) inferOpenCodeEndpoint(prof *providerProfile, model string) *openai.Client {
	if p.modelInfo == nil || model == "" || prof == nil {
		return nil
	}
	if p.registryListsModel(openCodeGoRegistryURLs(), model) {
		return prof.goStream
	}
	if p.registryListsModel(openCodeZenRegistryURLs(), model) {
		return prof.zenStream
	}
	return nil
}

// registryListsModel reports whether any of urls resolves model in the
// models.dev registry. The plural URL list covers the registry's spelling
// drift for one endpoint (see openCodeGoRegistryURLs).
func (p *OpenAIProvider) registryListsModel(urls []string, model string) bool {
	for _, u := range urls {
		if _, _, _, _, err := p.modelInfo.Resolve(u, model); err == nil {
			return true
		}
	}
	return false
}

func toolsToOpenAI(tools []Tool, allowed map[string]struct{}) []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		if allowed != nil {
			if _, ok := allowed[t.Name]; !ok {
				continue
			}
		}
		out = append(out, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        t.Name,
					Description: param.NewOpt(t.Description),
					Parameters:  shared.FunctionParameters(t.Parameters),
				},
			},
		})
	}
	return out
}

func (p *OpenAIProvider) messagesToChat(messages []Message) []openai.ChatCompletionMessageParamUnion {
	chatMessages := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	// Resolved once per conversion rather than per message; off drops the
	// historical reasoning echo (see reasoningEchoEnabled).
	echoReasoning := reasoningEchoEnabled()
	for _, m := range messages {
		switch m.Role {
		case "system":
			chatMessages = append(chatMessages, openai.SystemMessage(m.Content))
		case "user":
			if !m.HasImages() {
				chatMessages = append(chatMessages, openai.UserMessage(m.Content))
				continue
			}
			// Vision input: build a multi-part content array (text + one
			// image_url part per image). The text part is omitted entirely
			// when the message has no text, mirroring how the provider
			// expects a pure-image prompt.
			parts := make([]openai.ChatCompletionContentPartUnionParam, 0, 1+len(m.Images))
			if m.Content != "" {
				parts = append(parts, openai.TextContentPart(m.Content))
			}
			for _, img := range m.Images {
				if img.DataURL == "" {
					continue
				}
				detail := img.Detail
				if detail == "" {
					detail = "auto"
				}
				parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL:    img.DataURL,
					Detail: detail,
				}))
			}
			if len(parts) == 0 {
				// All images were empty; fall back to plain text so the
				// request still sends something coherent.
				chatMessages = append(chatMessages, openai.UserMessage(m.Content))
				continue
			}
			chatMessages = append(chatMessages, openai.UserMessage(parts))
		case "assistant":
			// Always build an explicit assistant param so reasoning_content /
			// refusal round-trip on the wire. Folding them into Content would
			// diverge from the original completion bytes and bust prompt-cache
			// prefixes on providers that emit those fields.
			asst := openai.ChatCompletionAssistantMessageParam{}
			if m.Content != "" {
				asst.Content.OfString = param.NewOpt(m.Content)
			}
			if m.Refusal != "" {
				asst.Refusal = param.NewOpt(m.Refusal)
			}
			if m.Reasoning != "" && echoReasoning {
				asst.SetExtraFields(map[string]any{
					"reasoning_content": m.Reasoning,
				})
			}
			for i := range m.ToolCalls {
				asst.ToolCalls = append(asst.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: m.ToolCalls[i].ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      m.ToolCalls[i].Name,
							Arguments: toolCallArgumentsJSON(&m.ToolCalls[i]),
						},
					},
				})
			}
			chatMessages = append(chatMessages, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
		case "tool":
			toolCallID := m.ToolCallID
			if toolCallID == "" {
				toolCallID = "unknown"
			}
			chatMessages = append(chatMessages, openai.ToolMessage(m.Content, toolCallID))
		}
	}
	return chatMessages
}

// toolCallArgumentsJSON returns provider-stable tool argument JSON.
// Prefer the raw ArgsStr from the model so re-sends match the bytes that
// established the prompt-cache prefix. Accepts a pointer so the exact wire
// bytes can be pinned in tc.ArgsStr for all future turns.
//
// encoding/json already sorts map keys, so a remarsal fallback is
// deterministic — but it still usually differs from the provider's original
// ArgsStr (spacing/key order), which is why pinning matters.
func toolCallArgumentsJSON(tc *ToolCall) string {
	// Fast path: ArgsStr was already validated (and trimmed) by
	// StabilizeToolCallArgs or session restore, so re-running json.Valid —
	// once per historical tool call on every API request — is pure overhead.
	// The flag is only ever set before the message is published, never by
	// this serializer (which runs outside the stats lock on shared
	// stabilized ToolCalls), so a valid flag is always trustworthy.
	if tc.ArgsJSONValid {
		return tc.ArgsStr
	}
	if s := strings.TrimSpace(tc.ArgsStr); s != "" && json.Valid([]byte(s)) {
		// Pin the exact bytes we send so history stays aligned with the wire.
		if tc.ArgsStr != s {
			tc.ArgsStr = s
		}
		return tc.ArgsStr
	}

	// Remarsal for the wire only. Never overwrite a non-empty ArgsStr — even
	// when invalid/truncated — so later turns keep the original history bytes
	// and the drift detector can still see the provider fragment.
	hadArgsStr := tc.ArgsStr != ""
	if hadArgsStr {
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr invalid, remarsaling without overwrite",
			"", map[string]any{
				"name":       tc.Name,
				"id":         tc.ID,
				"argsStr":    tc.ArgsStr,
				"argsStrLen": len(tc.ArgsStr),
			})
	} else if len(tc.Args) > 0 {
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr empty, re-marshaling from map",
			"", map[string]any{
				"name": tc.Name,
				"id":   tc.ID,
			})
	}

	marshaled := marshalToolArgsJSON(tc.Args)
	if !hadArgsStr {
		tc.ArgsStr = marshaled
	}
	return marshaled
}

// marshalToolArgsJSON encodes tool args without HTML escaping so remarsaled
// bytes stay closer to typical provider JSON (`<` not `\u003c`).
func marshalToolArgsJSON(args map[string]any) string {
	if args == nil {
		return "{}"
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return "{}"
	}
	// Encoder always appends a trailing newline; strip it for stable ArgsStr.
	return strings.TrimSuffix(buf.String(), "\n")
}

// StabilizeToolCallArgs pins ArgsStr to the bytes that will be sent on the
// wire when ArgsStr is empty. Non-empty ArgsStr (valid or not) is left alone
// so provider/history bytes are not rewritten mid-session. Records whether
// the resulting ArgsStr is the exact trimmed valid wire bytes in
// ArgsJSONValid, so the wire serializer can skip re-validating stabilized
// tool calls on every request.
func StabilizeToolCallArgs(tc *ToolCall) {
	_ = toolCallArgumentsJSON(tc)
	s := strings.TrimSpace(tc.ArgsStr)
	tc.ArgsJSONValid = s != "" && json.Valid([]byte(s))
}

// roundRequest is one agent round's wire request: the history and tool
// definitions converted once, and each wire variant's JSON body serialized at
// most once.
//
// The stream recovery ladder (a muted streaming retry, then a non-streaming
// fallback) re-requests the IDENTICAL turn. Building the request inside the
// attempt functions meant the whole conversation was converted and
// re-serialized on every attempt — for a large session that is megabytes of
// garbage per attempt, multiplied by the ladder, which is exactly the load
// that compounds when several agents share one process. Serialization goes
// through ChatCompletionNewParams.MarshalJSON, the same call the SDK's request
// builder makes (requestconfig.NewRequestConfig serializes a json.Marshaler
// body directly), so the bytes on the wire are unchanged.
type roundRequest struct {
	// client is the streaming client resolved for the round's model.
	client *openai.Client
	// base carries everything the wire body needs except the per-variant
	// StreamOptions: the converted messages, the tool definitions, the model,
	// the prompt cache key, and the extras/thinking-level fields.
	base openai.ChatCompletionNewParams
	// extras records whether the llama.cpp-style chat_template_kwargs went
	// into base: only a round that actually carried them can be re-issued
	// without them (see chatTemplateKwargsRejected).
	extras bool

	// The two variants are memoized: the "stream" flag is added by the SDK
	// (NewStreaming sets it on the serialized body), so the cached streaming
	// body does not carry it. sync.Once because the first attempt and the
	// fallback may each be the first to need their variant.
	streamOnce sync.Once
	streamBody []byte
	streamErr  error

	fallbackOnce sync.Once
	fallbackBody []byte
	fallbackErr  error

	// bytes is the round's upload breakdown (zero unless debug logging is
	// enabled — see wireBytes); the encoded size completes it at serialization
	// time. logBody is installed with it so each serialized variant reports
	// once, without holding the provider through the request path.
	bytes   wireBytes
	logBody func(w wireBytes, variant string)
}

// newRoundRequest converts the turn's messages and tools once and applies the
// per-request extras (llama.cpp kwargs, reasoning effort), resolving the
// client the attempts will use. Everything that does not depend on the wire
// variant is shared by both serialized bodies.
func (p *OpenAIProvider) newRoundRequest(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool) *roundRequest {
	params := openai.ChatCompletionNewParams{
		Messages: p.messagesToChat(messages),
		Tools:    toolsToOpenAI(tools, allowedTools),
		Model:    p.currentModel(),
	}
	if p.promptCacheKey.Valid() {
		params.PromptCacheKey = p.promptCacheKey
	}
	extras := p.applyChatCompletionExtras(ctx, &params)
	p.applyThinkingLevel(ctx, &params)
	req := &roundRequest{client: p.clientForModel(ctx), base: params, extras: extras}
	// Wire-byte accounting is diagnostic only: measured once per round, and
	// only when someone is actually reading the debug log.
	if debuglog.Enabled() {
		req.bytes = measureWireBytes(messages, tools)
		req.logBody = p.logWireBytes
	}
	return req
}

// reportWireBytes completes the round's breakdown with the exact encoded body
// size and logs it. No-op unless debug logging is enabled (logBody is nil).
func (r *roundRequest) reportWireBytes(size int, variant string) {
	if r.logBody == nil {
		return
	}
	r.bytes.Framing = size - (r.bytes.System + r.bytes.History + r.bytes.Reasoning + r.bytes.Images + r.bytes.Tools)
	r.bytes.Total = size
	r.logBody(r.bytes, variant)
}

// streaming returns the serialized body for a streaming attempt (the SDK adds
// "stream": true itself). Serialized once per round: the muted retry reuses
// these bytes instead of re-encoding the whole conversation.
func (r *roundRequest) streaming() ([]byte, error) {
	r.streamOnce.Do(func() {
		params := r.base
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}
		r.streamBody, r.streamErr = params.MarshalJSON()
		if r.streamErr == nil {
			r.reportWireBytes(len(r.streamBody), "stream")
		}
	})
	return r.streamBody, r.streamErr
}

// nonStreaming returns the serialized body for the non-streaming fallback: the
// same params without StreamOptions, matching what GenerateResponse sends.
func (r *roundRequest) nonStreaming() ([]byte, error) {
	r.fallbackOnce.Do(func() {
		r.fallbackBody, r.fallbackErr = r.base.MarshalJSON()
		if r.fallbackErr == nil {
			r.reportWireBytes(len(r.fallbackBody), "non-stream")
		}
	})
	return r.fallbackBody, r.fallbackErr
}

// newChatRequest builds the request options for one call from an
// already-serialized body. The placeholder params exist only so the SDK's
// request builder has a body to serialize before option.WithRequestBody
// replaces it with the round's bytes — passing the real params would
// re-serialize the whole conversation, which is what roundRequest avoids. The
// placeholder carries the model so a dropped option fails loudly at the
// endpoint (empty messages) instead of silently sending the wrong request.
func newChatRequest(body []byte, model string) (openai.ChatCompletionNewParams, []option.RequestOption) {
	return openai.ChatCompletionNewParams{Model: model},
		[]option.RequestOption{option.WithRequestBody("application/json", body)}
}

func (p *OpenAIProvider) GenerateResponse(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool) (Response, error) {
	req := p.newRoundRequest(ctx, messages, allowedTools, tools)
	resp, err := p.nonStreamingTurn(ctx, req)
	if err == nil || !req.extras || !chatTemplateKwargsRejected(err) {
		return resp, err
	}
	// The endpoint refuses the optional chat_template_kwargs: record the
	// refusal as its capability answer and re-issue the identical turn
	// without the extras (see chatTemplateKwargsRejected).
	debuglog.Write("llm/stream_recovery", "endpoint rejected chat_template_kwargs; retrying without extras", "", map[string]any{
		"streamError": clipErrForLog(err),
		"variant":     "non-stream",
	})
	p.rejectPreserveReasoning()
	return p.nonStreamingTurn(ctx, p.newRoundRequest(ctx, messages, allowedTools, tools))
}

// nonStreamingTurn performs the round's non-streaming request (the fallback
// body variant), translating a serialization failure into the same error shape
// as a failed request.
func (p *OpenAIProvider) nonStreamingTurn(ctx context.Context, req *roundRequest) (Response, error) {
	body, err := req.nonStreaming()
	if err != nil {
		return Response{}, fmt.Errorf("serialize chat request: %w", err)
	}
	return p.generateResponseBody(ctx, req.client, req.base.Model, body)
}

// generateResponseBody performs one non-streaming chat request for an
// already-serialized body and parses the reply. Shared by GenerateResponse and
// the stream fallback, which reuses the round's prebuilt body instead of
// converting and re-encoding the conversation a third time.
func (p *OpenAIProvider) generateResponseBody(ctx context.Context, client *openai.Client, model string, body []byte) (Response, error) {
	params, opts := newChatRequest(body, model)
	resp, err := client.Chat.Completions.New(ctx, params, opts...)

	if err != nil {
		// wrapContextWindowError classifies a context-window refusal so the
		// agent run loop can recover in-loop (forced compaction + retry)
		// instead of aborting the turn; surfaceAPIError keeps the
		// provider's message in the text (openai-go >= v3.81's Error()
		// prints only a status summary).
		return Response{}, wrapContextWindowError(fmt.Errorf("openai api error: %w", surfaceAPIError(err)))
	}

	if len(resp.Choices) == 0 {
		return Response{}, fmt.Errorf("no choices returned")
	}

	var toolCalls []ToolCall
	for _, tc := range resp.Choices[0].Message.ToolCalls {
		// A malformed arguments blob must not kill the whole round: record it
		// per call (ArgsError) and keep the other tool calls, matching the
		// streaming accumulator (buildResult) so recovery does not depend on
		// which transport served the turn. parseToolCallArgs also carries the
		// duplicated-object recovery the stream path relies on.
		args, argsErr := parseToolCallArgs(tc.Function.Arguments)
		errStr := ""
		if argsErr != nil {
			args = map[string]any{}
			errStr = argsErr.Error()
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Args:      args,
			ArgsStr:   tc.Function.Arguments,
			ArgsError: errStr,
		})
	}

	content := resp.Choices[0].Message.Content
	msg := resp.Choices[0].Message
	extras := extraFieldsFromMessage(msg)
	reasoning := primaryDisplayFromExtrasMap(extras)
	// Keep content/reasoning/refusal separate. Providers that emit
	// reasoning_content or refusal expect those fields echoed back; stuffing
	// them into Content changes the wire bytes and busts prompt-cache prefixes.
	display := content
	if display == "" {
		display = reasoning
	}
	if msg.Refusal != "" && display == "" {
		display = msg.Refusal
	}
	logNonStreamResponse(model, "non-stream", content, msg.Refusal, display, extras, toolCalls, usageFromOpenAI(resp.Usage))
	return Response{
		Content:   content,
		Reasoning: reasoning,
		Refusal:   msg.Refusal,
		ToolCalls: toolCalls,
		Usage:     usageFromOpenAI(resp.Usage),
		Model:     resp.Model,
	}, nil
}

// streamRetryLabel builds the reason surfaced to hosts when a failed stream
// attempt is retried. Hosts render it verbatim in progress labels, so it
// stays one bounded line: how far the attempt got (an interruption before
// any rendered output means the stream died during the silent
// pre-first-token wait — long prompt processing, backend queueing, or a
// middlebox idle timeout) plus a short single-line cause. The error can
// embed the provider's message (SDK error text, via apiErrorSurface) or
// wrap several lines.
func streamRetryLabel(stage string, acc *streamAccumulator, err error) string {
	cause := ""
	if err != nil {
		cause = err.Error()
		if i := strings.IndexByte(cause, '\n'); i >= 0 {
			cause = cause[:i]
		}
		const max = 120
		if len(cause) > max {
			cause = cause[:max]
		}
	}
	label := stage
	if acc != nil && !acc.renderedOutput() {
		label += " before any output"
	}
	if cause == "" {
		return label
	}
	return label + ": " + cause
}

// GenerateResponseStream streams one chat completion with a two-stage
// recovery ladder for failed streams: one muted streaming retry first, then
// the non-streaming fallback (handleStreamFallback). The retry stays on the
// streaming path, so arriving chunks keep resetting the per-read idle
// deadline (sse_http.go) — a long regeneration cannot hit the hard read cap
// the non-streaming path dies on (llama.cpp sends nothing until the whole
// completion is done). Round-end callbacks (OnStreamEnd /
// OnRecoverPartialStream) stay owned by the agent loop on every path.
func (p *OpenAIProvider) GenerateResponseStream(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool, h *StreamHandlers) (*StreamResult, error) {
	h = ensureStreamCallbacks(h)

	// Convert and serialize the turn ONCE. Every attempt below — the first
	// stream, the muted retry, and the non-streaming fallback — re-requests
	// the identical conversation, so re-building it per attempt was pure
	// duplicated work (see roundRequest).
	req := p.newRoundRequest(ctx, messages, allowedTools, tools)

	at := p.streamChatAttempt(ctx, req, h)
	if at.err != nil && req.extras && !at.acc.renderedOutput() && chatTemplateKwargsRejected(at.err) {
		// The endpoint refuses the optional chat_template_kwargs (a strict
		// gateway fails the whole request for the unrecognized field). Only
		// an attempt that rendered NOTHING can be re-issued: a partial
		// generation must not be restarted into the same bubble. The refusal
		// is recorded as the endpoint's capability answer, so the rebuilt
		// round (and every later round, in this session and its siblings)
		// omits the parameter.
		debuglog.Write("llm/stream_recovery", "endpoint rejected chat_template_kwargs; retrying without extras", "", map[string]any{
			"streamError": clipErrForLog(at.err),
			"variant":     "stream",
		})
		p.rejectPreserveReasoning()
		req = p.newRoundRequest(ctx, messages, allowedTools, tools)
		at = p.streamChatAttempt(ctx, req, h)
	}
	if at.err == nil {
		return at.result, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !at.fallbackEligible {
		// Terminal failure (a reasoning-stop grace expiration or a clean
		// close that built no result): returned as-is, exactly as before the
		// retry existed — re-requesting the turn is what those paths avoid.
		return at.result, at.err
	}
	streamErr := at.err
	debuglog.Write("llm/stream_recovery", "stream attempt failed; recovery ladder entered", "", map[string]any{
		"streamError": clipErrForLog(streamErr),
		"retryable":   retryableStreamError(streamErr),
		"hadPartial": at.acc.fullContent.Len() > 0 || at.acc.fullReasoning.Len() > 0 ||
			at.acc.fullRefusal.Len() > 0 || len(at.acc.tcAccums) > 0,
	})
	// trimAcc is the rendered text the final recovery must trim against
	// (see trimRecoveredText): attempt 1's partial output normally, or —
	// when the retry delivered live — the retry's own partial output.
	trimAcc := at.acc
	// rateLimited tracks whether ANY attempt in the ladder hit a provider
	// rate limit: that changes the recovery policy below (pace to Retry-After,
	// and do not spend a non-streaming regeneration on a limit).
	rateLimited := isRateLimited(streamErr)
	if retryableStreamError(streamErr) {
		// Hosts must know so the UI can leave the bare-spinner state; the
		// reason carries how far the attempt got and the underlying cause,
		// so hosts (and users) can see WHY the stream broke without
		// enabling the debug log.
		h.OnStreamRetry(streamRetryLabel("stream interrupted", at.acc, streamErr))
		// Let a briefly-persistent failure condition (router re-dialing a
		// dead upstream, a LAN blip) clear before re-requesting: an
		// immediate retry lands in the same dead window and burns the
		// ladder's only streaming attempt. A provider that told us how long
		// to wait (Retry-After) is obeyed instead of guessing.
		if waitErr := waitStreamBackoff(ctx, recoveryDelay(streamErr, 0)); waitErr != nil {
			return nil, waitErr
		}
		if at.acc.renderedOutput() {
			// Muted retry: a fresh generation matches the already-rendered
			// prefix only until it diverges, and the trim contract forbids
			// splicing two divergent generations into one bubble — so
			// delivery is buffered and suffix-only.
			if res, retryErr := p.retryStreamChat(ctx, req, h, at.acc); retryErr == nil {
				debuglog.Write("llm/stream_recovery", "muted streaming retry recovered the round", "", nil)
				return res, nil
			} else {
				rateLimited = rateLimited || isRateLimited(retryErr)
				debuglog.Write("llm/stream_retry", "streaming retry failed; using non-streaming fallback", "", map[string]any{
					"streamError": clipErrForLog(streamErr),
					"retryError":  clipErrForLog(retryErr),
				})
				streamErr = fmt.Errorf("%w; streaming retry also failed: %v", streamErr, retryErr)
			}
		} else {
			// Attempt 1 rendered NOTHING (the break hit before any output):
			// there is no rendered prefix to diverge from, so muting serves
			// no purpose — the retry delivers live, exactly like a normal
			// stream, instead of a silent end-of-retry batch.
			live := p.streamChatAttempt(ctx, req, h)
			if live.err == nil {
				if live.result.Model == "" {
					live.result.Model = at.acc.model
				}
				debuglog.Write("llm/stream_recovery", "live streaming retry recovered the round", "", nil)
				return live.result, nil
			}
			rateLimited = rateLimited || isRateLimited(live.err)
			debuglog.Write("llm/stream_retry", "streaming retry failed; using non-streaming fallback", "", map[string]any{
				"streamError": clipErrForLog(streamErr),
				"retryError":  clipErrForLog(live.err),
			})
			streamErr = fmt.Errorf("%w; streaming retry also failed: %v", streamErr, live.err)
			trimAcc = live.acc
		}
	}
	// A rate-limited round must NOT spend a non-streaming regeneration. The
	// fallback uploads the entire conversation a third time, and a rate limit
	// is not a transport failure that a re-request can fix — re-requesting
	// only deepens the queue the request is already waiting on. The provider's
	// own transport retry (with Retry-After honored) and the streaming retry
	// above are the recovery budget for this condition.
	if rateLimited {
		debuglog.Write("llm/stream_recovery", "rate limited: skipping the non-streaming fallback", "", map[string]any{
			"streamError": clipErrForLog(streamErr),
		})
		return nil, fmt.Errorf("stream error: %w", wrapContextWindowError(surfaceAPIError(streamErr)))
	}
	// The fallback re-requests the turn non-streaming (llama.cpp delivers
	// nothing until the whole completion is done); hosts must know the
	// spinner now covers a full silent regeneration.
	h.OnStreamRetry(streamRetryLabel("non-streaming recovery", trimAcc, streamErr))
	// Longer wait than the streaming retry: the fallback re-requests the
	// whole turn, so it is worth giving the upstream a bigger window to
	// recover before spending a full silent regeneration on it.
	if waitErr := waitStreamBackoff(ctx, recoveryDelay(streamErr, 1)); waitErr != nil {
		return nil, waitErr
	}
	return p.handleStreamFallback(ctx, req, h, streamErr, trimAcc)
}

// streamAttempt is the outcome of one streaming request. acc is always
// non-nil; result is non-nil only when err is nil.
type streamAttempt struct {
	result *StreamResult
	acc    *streamAccumulator
	err    error
	// fallbackEligible marks recoverable stream failures — everything that
	// surfaces as stream.Err() on a non-grace close: transport drops and
	// read timeouts, malformed SSE payloads, mid-stream SSE error frames,
	// and HTTP statuses on the streaming POST (which arrive only after the
	// SDK's own transport-level retries are exhausted). Clean closes that
	// built no result and grace expirations are terminal.
	fallbackEligible bool
}

// retryableStreamError reports whether a failed streaming attempt is worth
// one streaming retry. Transport-level failures (truncated SSE frames,
// dropped connections, read timeouts, malformed SSE payloads) are transient
// and worth the retry; so are transient HTTP statuses (429 rate limits, 5xx
// gateway/upstream hiccups, 408) — the SDK already retries each request
// twice at the transport level, so such a status only reaches this ladder
// after a sustained burst of failures, and skipping the streaming retry
// landed every one of those directly on the non-streaming fallback. A
// successful retry also keeps the round's live rendering, which the
// fallback trades away entirely. Context-window refusals are deterministic
// (the agent loop recovers them via forced compaction) and definitive 4xx
// errors (auth, bad request, unknown model) cannot succeed on a
// regeneration, so neither is retried.
func retryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if IsContextWindowError(err) {
		return false
	}
	var oerr *openai.Error
	if errors.As(err, &oerr) {
		return retryableHTTPStatus(oerr.StatusCode)
	}
	return true
}

// retryAfterMax caps how long a provider-requested Retry-After may park the
// recovery ladder. A hostile or mistaken value must not stall a turn for
// minutes; the SDK's own transport retry already honors the header within its
// own bounds, so this cap only bounds gogen's extra layer.
const retryAfterMax = 60 * time.Second

// retryAfterDelay returns the wait a provider explicitly asked for in its
// Retry-After header (delta-seconds or HTTP-date), or 0 when the failure
// carries none. The header is a provider telling a client how to pace itself;
// ignoring it while re-uploading the whole conversation is what turns a rate
// limit into congestion.
func retryAfterDelay(err error) time.Duration {
	var oerr *openai.Error
	if !errors.As(err, &oerr) || oerr.Response == nil {
		return 0
	}
	raw := strings.TrimSpace(oerr.Response.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, convErr := strconv.Atoi(raw); convErr == nil {
		if secs <= 0 {
			return 0
		}
		return min(time.Duration(secs)*time.Second, retryAfterMax)
	}
	if when, parseErr := http.ParseTime(raw); parseErr == nil {
		if d := time.Until(when); d > 0 {
			return min(d, retryAfterMax)
		}
	}
	return 0
}

// recoveryDelay is the wait before a recovery re-request: the configured ladder
// backoff, extended to the provider's own Retry-After when it asks for longer.
func recoveryDelay(err error, stage int) time.Duration {
	d := streamRetryBackoff(stage)
	if ra := retryAfterDelay(err); ra > d {
		return ra
	}
	return d
}

// isRateLimited reports whether err is a provider rate limit (HTTP 429).
func isRateLimited(err error) bool {
	var oerr *openai.Error
	return errors.As(err, &oerr) && oerr.StatusCode == http.StatusTooManyRequests
}

// retryableHTTPStatus reports whether an HTTP status from the streaming
// endpoint is worth one streaming retry: rate limits and server-side
// failures are usually transient; client errors are not.
func retryableHTTPStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return code >= 500
}

// retryStreamChat runs ONE additional streaming attempt with live-delivery
// callbacks muted, and on success delivers only the text the client has not
// already rendered: the suffix beyond the first attempt's partial output,
// via trimRecoveredText — the same contract as the non-streaming fallback.
// Tokens cannot be delivered live during the retry: a fresh generation
// matches the already-rendered prefix only until it diverges, and the trim
// contract forbids splicing two divergent generations into one bubble.
// Tool-call UI callbacks stay muted too: partial chips from the dead attempt
// are left as rendered, like the non-streaming fallback, and the round-end
// callbacks remain with the agent loop.
func (p *OpenAIProvider) retryStreamChat(ctx context.Context, req *roundRequest, h *StreamHandlers, first *streamAccumulator) (*StreamResult, error) {
	quiet := ensureStreamCallbacks(&StreamHandlers{})
	at := p.streamChatAttempt(ctx, req, quiet)
	if at.err != nil {
		return nil, at.err
	}
	result := at.result
	reasoning := trimRecoveredText(first.fullReasoning.String(), result.Reasoning)
	content := trimRecoveredText(first.fullContent.String(), result.Content)
	refusal := trimRecoveredText(first.fullRefusal.String(), result.Refusal)
	if reasoning != "" {
		h.OnThinkingToken(reasoning)
	}
	if content != "" {
		h.OnToken(content)
	} else if refusal != "" {
		h.OnToken(refusal)
	}
	if result.Model == "" {
		result.Model = first.model
	}
	result.PartialStream = len(first.tcAccums) > 0 || first.fullContent.Len() > 0 || first.fullRefusal.Len() > 0 || first.extras.textLen() > 0
	return result, nil
}

// budgetStopAttempt returns the text accumulated so far as a successful
// partial round. A nil or empty build (the stream stopped before any
// finish reason) is still a budget stop, not an error the retry ladder
// should regenerate.
func budgetStopAttempt(acc *streamAccumulator) streamAttempt {
	res, err := acc.buildResult()
	if err != nil || res == nil {
		return streamAttempt{result: &StreamResult{FinishReason: "budget", BudgetStop: true}, acc: acc}
	}
	res.BudgetStop = true
	if res.FinishReason == "" {
		res.FinishReason = "budget"
	}
	return streamAttempt{result: res, acc: acc}
}

// streamChatAttempt performs ONE streaming chat-completion request,
// forwarding live delivery callbacks as chunks arrive. It never recovers:
// failures come back in streamAttempt for GenerateResponseStream to run
// the retry ladder. A budget stop is a successful partial result, so it
// does not enter that ladder.
func (p *OpenAIProvider) streamChatAttempt(ctx context.Context, req *roundRequest, h *StreamHandlers) streamAttempt {
	onToken := h.OnToken
	onThinking := h.OnThinkingToken

	body, err := req.streaming()
	if err != nil {
		return streamAttempt{acc: newStreamAccumulator(), err: fmt.Errorf("serialize chat request: %w", err)}
	}
	params, opts := newChatRequest(body, req.base.Model)
	stream := req.client.Chat.Completions.NewStreaming(ctx, params, opts...)
	defer stream.Close()

	// stream.Next() can block on the response body even after ctx cancel if
	// the transport is slow to abort. Closing the stream from a watcher
	// unblocks Next promptly (important for --web Ctrl+C).

	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = stream.Close()
		case <-stopWatch:
		}
	}()

	acc := newStreamAccumulator()

	// reasoningStopGrace bounds the wait after a reasoning-only stop: a
	// two-phase stream (reasoning → content) legitimately continues with
	// content, but a provider that sends the stop and then holds the
	// connection open without [DONE] would otherwise block the read for the
	// full idle timeout. The timer is armed once the stop is seen and
	// re-armed on every subsequent chunk while still pending (so an active
	// but slow thinking stream is never cut short); it is disarmed the
	// moment content/refusal/tool-calls resume. Closing the stream from the
	// timer unblocks Next() promptly (same mechanism as the ctx watcher).
	grace := reasoningStopGrace()
	var stopGrace *time.Timer

	// Stall watchdog: a stretch with no surfaced SSE chunk (long prefill,
	// server-side queueing, a stalled upstream) fires OnStreamStall so hosts
	// can show a "still waiting" state instead of a bare spinner.
	// Informational only: chunk arrivals reset the clock, and the signal
	// never interrupts the stream — the per-read idle deadline
	// (streamReadIdleTimeout) remains the hard bound.
	stallAfter := streamStallAfter()
	var lastChunk atomic.Int64
	lastChunk.Store(time.Now().UnixNano())
	if stallAfter > 0 {
		onStall := h.OnStreamStall
		if onStall == nil {
			onStall = func() {}
		}
		go func() {
			ticker := time.NewTicker(stallAfter)
			defer ticker.Stop()
			for {
				select {
				case <-stopWatch:
					return
				case <-ticker.C:
					if time.Since(time.Unix(0, lastChunk.Load())) >= stallAfter {
						onStall()
					}
				}
			}
		}()
	}

	// Usage-drain grace: once the round is finished (finish_reason seen) but
	// the usage chunk hasn't arrived, bound the wait — a silent endpoint that
	// holds the connection open would otherwise park the turn on the read
	// idle timeout with the reply already complete on screen. Same
	// close-from-timer mechanism as the reasoning-stop grace above.
	drainGrace := streamDrainGrace()
	var drainTimer *time.Timer

	// Disarm the grace timers on every exit path, including the in-loop
	// ctx-cancel return below: an armed AfterFunc would otherwise stay
	// scheduled for up to a full grace window, retaining the accumulator and
	// re-closing an already-closed stream.
	defer func() {
		if stopGrace != nil {
			stopGrace.Stop()
		}
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()

	for stream.Next() {
		// Every surfaced chunk (even a no-op delta) proves the pipe is
		// moving: reset the stall clock.
		lastChunk.Store(time.Now().UnixNano())
		if ctx.Err() != nil {
			return streamAttempt{acc: acc, err: ctx.Err()}
		}
		if stop := acc.processChunk(stream.Current(), onToken, onThinking, h); stop {
			break
		}
		if ctx.Err() != nil {
			return streamAttempt{acc: acc, err: ctx.Err()}
		}
		// A budget stop is a finished partial round, not a transport
		// failure: return what was generated and do not enter the retry
		// ladder (that ladder would start the thinking over).
		if h.StopStream != nil && h.StopStream() {
			return budgetStopAttempt(acc)
		}
		if acc.stopPending && grace > 0 {
			if stopGrace == nil {
				stopGrace = time.AfterFunc(grace, func() {
					acc.graceExpired.Store(true)
					_ = stream.Close()
				})
			} else {
				stopGrace.Reset(grace)
			}
		} else if stopGrace != nil {
			stopGrace.Stop()
			stopGrace = nil
		}
		// streamDone without usage: only the usage chunk (or body EOF) can
		// end the drain — arm the bound once. processChunk returns true the
		// moment usage arrives, which breaks the loop below; the timer never
		// fires on a compliant endpoint.
		if acc.streamDone && acc.streamUsage == nil && drainGrace > 0 && drainTimer == nil {
			drainTimer = time.AfterFunc(drainGrace, func() {
				acc.drainExpired.Store(true)
				_ = stream.Close()
			})
		}
	}

	if err := ctx.Err(); err != nil {
		return streamAttempt{acc: acc, err: err}
	}

	if err := stream.Err(); err != nil {
		if acc.graceExpired.Load() {
			// The stream was closed by the reasoning-stop grace timer, not
			// by a real failure: return the accumulated reasoning as a
			// complete response (the provider held the connection open
			// instead of sending [DONE]) rather than triggering the
			// non-streaming fallback, which would re-request the turn.
			res, berr := acc.buildResult()
			return streamAttempt{result: res, acc: acc, err: berr}
		}
		if acc.drainExpired.Load() {
			// The stream was closed by the usage-drain grace timer (see
			// streamDrainGrace): the round is finished — finish_reason was
			// seen, only the promised usage chunk never arrived on an
			// endpoint that holds the connection open. A completion, not a
			// transport failure: no retry, no non-streaming fallback (both
			// would re-request a turn whose answer is already complete).
			res, berr := acc.buildResult()
			return streamAttempt{result: res, acc: acc, err: berr}
		}
		return streamAttempt{acc: acc, err: err, fallbackEligible: true}
	}

	res, berr := acc.buildResult()
	return streamAttempt{result: res, acc: acc, err: berr}
}

// handleStreamFallback is the last stage of the stream recovery ladder: the
// streaming attempt AND the muted streaming retry both failed, so it attempts
// a non-streaming request to recover partial results.
func (p *OpenAIProvider) handleStreamFallback(ctx context.Context, req *roundRequest, h *StreamHandlers, streamErr error, acc *streamAccumulator) (*StreamResult, error) {
	// Deliberately NOT firing OnStreamEnd / OnRecoverPartialStream here: the
	// agent loop owns those and fires them exactly once when it finalizes the
	// round (finishStreamUI on the content path, the tool-call branch + the
	// PartialStream check on the tool path). Firing them here too would
	// double-deliver stream_end frames / streamRoundEndMsg on every stream
	// failure; the error path that runs when the fallback also fails already
	// finalizes the round itself. h came from GenerateResponseStream, which
	// ran ensureStreamCallbacks, so the callbacks used below are non-nil.
	//
	// The body is the round's own non-streaming variant: the conversation was
	// converted once by GenerateResponseStream, so the ladder's third attempt
	// (after the first stream and the muted retry) does not re-encode it.
	resp, fbErr := p.nonStreamingTurn(ctx, req)
	if fbErr != nil {
		debuglog.Write("llm/stream_recovery", "non-streaming fallback failed", "", map[string]any{
			"streamError":   clipErrForLog(streamErr),
			"fallbackError": clipErrForLog(fbErr),
		})
		// Classify the STREAM error (the primary cause): a context-window
		// refusal that also fails the fallback must reach the agent run
		// loop still classified, so it recovers in-loop instead of
		// aborting the turn.
		return nil, fmt.Errorf("stream error: %w (non-streaming fallback also failed: %v)", wrapContextWindowError(surfaceAPIError(streamErr)), surfaceAPIError(fbErr))
	}
	// Re-render only the suffix beyond what the failed stream already
	// emitted, so the live bubble does not show the recovered text twice (a
	// retry of the same request typically re-generates the same opening, and
	// the client already rendered the partial stream). The StreamResult below
	// still carries the complete recovery for persistence — only the live
	// re-render is trimmed.
	reasoning := trimRecoveredText(acc.fullReasoning.String(), resp.Reasoning)
	content := trimRecoveredText(acc.fullContent.String(), resp.Content)
	refusal := trimRecoveredText(acc.fullRefusal.String(), resp.Refusal)
	if reasoning != "" {
		h.OnThinkingToken(reasoning)
	}
	if content != "" {
		h.OnToken(content)
	} else if refusal != "" {
		h.OnToken(refusal)
	}
	// The non-streaming fallback may omit the model field; the failed stream
	// already reported one (router endpoints resolve aliases server-side).
	model := resp.Model
	if model == "" {
		model = acc.model
	}
	logFallbackResponse(model, streamErr, resp)
	debuglog.Write("llm/stream_recovery", "non-streaming fallback recovered the round", "", map[string]any{
		"streamError": clipErrForLog(streamErr),
		"model":       model,
	})
	return &StreamResult{
		Content:       resp.Content,
		Reasoning:     resp.Reasoning,
		Refusal:       resp.Refusal,
		ToolCalls:     resp.ToolCalls,
		Usage:         resp.Usage,
		PartialStream: len(acc.tcAccums) > 0 || acc.fullContent.Len() > 0 || acc.fullRefusal.Len() > 0 || acc.extras.textLen() > 0,
		Model:         model,
	}, nil
}

// trimRecoveredText drops the portion of a recovered response the client
// already saw streamed. It trims only when the streamed text is an exact
// byte prefix of the recovered text; a divergent re-generation is emitted in
// full so two different answers are never spliced into one bubble. Byte-wise
// comparison is safe: both strings reached this point via JSON decoding, so
// they are valid UTF-8 and a byte prefix of one is also a rune-aligned
// prefix.
func trimRecoveredText(streamed, recovered string) string {
	if streamed == "" || recovered == "" {
		return recovered
	}
	n := len(streamed)
	if n > len(recovered) {
		n = len(recovered)
	}
	if recovered[:n] != streamed {
		return recovered // no exact-prefix overlap: status quo
	}
	return recovered[n:]
}
