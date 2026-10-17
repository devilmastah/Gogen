package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"

	"gogen/internal/onoff"
)

// SetPreserveReasoningMode controls whether chat_template_kwargs.preserve_reasoning
// is sent for self-hosted endpoints: auto (probe /props), on, or off.
func (p *OpenAIProvider) SetPreserveReasoningMode(mode string) {
	if p == nil {
		return
	}
	mode = normalizePreserveReasoningMode(mode)
	p.modelsMu.Lock()
	p.preserveReasoningMode = mode
	p.modelsMu.Unlock()
}

// chatTemplateKwargsRejected reports whether err is an endpoint refusing the
// optional chat_template_kwargs parameter: a 400/422 whose body names it.
// Strict OpenAI-compatible gateways reject the WHOLE request for an
// unrecognized field instead of ignoring it, which turns an optional
// prompt-cache optimization into a hard failure of every round.
func chatTemplateKwargsRejected(err error) bool {
	if err == nil {
		return false
	}
	var oerr *openai.Error
	if !errors.As(err, &oerr) {
		return false
	}
	if oerr.StatusCode != http.StatusBadRequest && oerr.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	// Read the parsed payload, not err.Error(): the SDK's text dereferences
	// Request/Response, which a wrapped error may not carry. The raw body is
	// the provider's own words (the transient rewrite in api_error_body.go
	// keeps them), and Param is set when the gateway attributes the failure to
	// the field explicitly.
	text := strings.ToLower(oerr.RawJSON() + "\n" + oerr.Message + "\n" + oerr.Param)
	return strings.Contains(text, "chat_template_kwargs")
}

func normalizePreserveReasoningMode(mode string) string {
	if on, ok := onoff.Parse(mode); ok {
		if on {
			return "on"
		}
		return "off"
	}
	return "auto"
}

// applyChatCompletionExtras sets llama.cpp-style kwargs that keep prompt-cache
// prefixes stable across multi-turn agent loops. It reports whether the kwargs
// were applied, so the caller can re-issue the turn without them when the
// endpoint refuses the parameter.
//
//   - mode "off": never send
//   - mode "on": send when a custom base URL is set (skip /props probe), unless
//     the endpoint has already answered that it does not accept the parameter
//   - mode "auto" (default): send only if GET /props reports
//     chat_template_caps.supports_preserve_reasoning
//
// Empty base URL is the default OpenAI API and never gets kwargs.
func (p *OpenAIProvider) applyChatCompletionExtras(ctx context.Context, params *openai.ChatCompletionNewParams) (applied bool) {
	if p == nil || params == nil {
		return false
	}
	p.modelsMu.RLock()
	mode := p.preserveReasoningMode
	p.modelsMu.RUnlock()
	switch normalizePreserveReasoningMode(mode) {
	case "off":
		return false
	case "on":
		// The live default endpoint (SetProfiles may have replaced it): the
		// kwargs must reach the endpoint the request actually goes to.
		if strings.TrimSpace(p.defaultBaseURL()) == "" {
			return false
		}
		// A known "no" wins even over the forced mode: "on" is the user's
		// statement about their own (self-hosted) server, not evidence about
		// every registered endpoint. A strict hosted gateway rejects the WHOLE
		// request for this unrecognized field, and rejectPreserveReasoning has
		// already recorded that answer for this endpoint+model.
		if supported, known := p.cachedPreserveReasoningSupport(); known && !supported {
			return false
		}
	default: // auto
		if !p.templateSupportsPreserveReasoning(ctx) {
			return false
		}
	}
	params.SetExtraFields(map[string]any{
		"chat_template_kwargs": map[string]any{
			"preserve_reasoning": true,
		},
	})
	return true
}

// cachedPreserveReasoningSupport returns the already-known capability answer
// for the current endpoint+model (a completed /props probe, or an observed
// parameter rejection) and whether an answer is known. Never performs I/O: no
// dial, no retry window, no network.
func (p *OpenAIProvider) cachedPreserveReasoningSupport() (supported, known bool) {
	baseURL := p.propsBaseURLForCurrentModel()
	if v, ok := p.propsCapsCache().Known(propsCapsKey(baseURL, p.currentModel())); ok {
		return v, true
	}
	p.propsMu.Lock()
	defer p.propsMu.Unlock()
	if p.propsChecked && p.propsBaseURL == baseURL {
		return p.propsPreserveReasoning, true
	}
	return false, false
}

// rejectPreserveReasoning records that the current endpoint refused
// chat_template_kwargs (see chatTemplateKwargsRejected). The refusal is a
// COMPLETED capability answer — the endpoint told us the field is not part of
// its API — so it is stored as a negative exactly like a /props probe result:
// locally for this provider and in the process-shared cache, so sibling
// sessions on the same endpoint stop sending the parameter too.
func (p *OpenAIProvider) rejectPreserveReasoning() {
	baseURL := p.propsBaseURLForCurrentModel()
	model := p.currentModel()
	p.propsMu.Lock()
	p.propsChecked = true
	p.propsBaseURL = baseURL
	p.propsPreserveReasoning = false
	p.propsMu.Unlock()
	p.propsCapsCache().RecordSuccess(propsCapsKey(baseURL, model), false)
}

// DefaultReasoningEfforts is the reasoning_effort value set used when the
// current model has no models.dev entry and no llama.cpp capability probe has
// derived a set (unknown/self-hosted endpoints). It is the most common
// accepted set in the registry: the low/medium/high triple is the mode of
// models.dev's per-model effort values (accepted by ~80%+ of the
// effort-enabled models; high alone by 99%).
var DefaultReasoningEfforts = []string{"low", "medium", "high"}

// applyThinkingLevel sets the reasoning_effort field on chat completion params
// when a non-empty thinking level is configured. Empty or "off" means omit the
// parameter entirely (no reasoning/effort requested from the API).
//
// The configured value is sent verbatim — never translated — but only when the
// current model accepts it. The accepted set comes from the models.dev
// registry when the model is known (empty for toggle/budget-only models), from
// a llama.cpp capability probe when one has completed, else
// DefaultReasoningEfforts. A stored value the model does not accept is omitted
// rather than rewritten (policy B: it stays stored, inactive, and re-activates
// if a later model accepts it).
func (p *OpenAIProvider) applyThinkingLevel(_ context.Context, params *openai.ChatCompletionNewParams) {
	if p == nil || params == nil {
		return
	}
	p.modelsMu.RLock()
	level := p.thinkingLevel
	p.modelsMu.RUnlock()
	if level == "" || level == "off" {
		return
	}
	if !slices.Contains(p.acceptedReasoningEfforts(), level) {
		return // stored value not accepted by this model → omit (policy B)
	}
	params.ReasoningEffort = shared.ReasoningEffort(level)
}

// acceptedReasoningEfforts returns the effective reasoning-effort options for
// the current model: its models.dev accepted set when known (empty for
// toggle-only/budget-only models), else the llama.cpp /props-derived set when
// one has been probed, else DefaultReasoningEfforts. Never blocks — registry
// lookups and the derived cache are in-memory map reads.
func (p *OpenAIProvider) acceptedReasoningEfforts() []string {
	return p.ModelReasoningEfforts(p.CurrentModelRef())
}

// templateSupportsPreserveReasoning probes llama.cpp GET /props once per
// endpoint and caches chat_template_caps.supports_preserve_reasoning. The
// probe hits the CURRENT model's owning profile (with multiple registered
// providers, /props must be asked of the endpoint that actually serves the
// request). Failures / missing caps → false.
//
// A completed exchange (200 with parseable JSON) is cached for the endpoint's
// lifetime as a value. A failed probe (transport error, timeout, non-200, or
// unparseable body) is NOT cached as a value — a cold-start probe fired
// before the router has loaded the model must not pin "off" for the process
// lifetime — but it IS remembered for propsProbeRetryTTL, so an endpoint
// without /props costs one bounded attempt per window instead of one full
// handshake per agent-loop round (F1).
func (p *OpenAIProvider) templateSupportsPreserveReasoning(ctx context.Context) bool {
	baseURL := p.propsBaseURLForCurrentModel()
	// Process-shared memo first (see PropsCapsCache): the capability belongs
	// to the endpoint (+model), so a sibling session's probe answers for this
	// one — no re-dial, no second fresh connection.
	shared := p.propsCapsCache()
	sharedKey := propsCapsKey(baseURL, p.currentModel())
	if v, known := shared.Lookup(sharedKey); known {
		return v
	}
	p.propsMu.Lock()
	if p.propsChecked && p.propsBaseURL == baseURL {
		v := p.propsPreserveReasoning
		p.propsMu.Unlock()
		return v
	}
	if p.propsNegBaseURL == baseURL && time.Since(p.propsNegAt) < propsProbeRetryTTL {
		// A recent attempt for this endpoint failed: reuse the negative
		// result instead of re-dialing it every round.
		p.propsMu.Unlock()
		return false
	}
	p.propsMu.Unlock()

	supported, ok := p.probePreserveReasoning(ctx, baseURL, p.currentModel())
	if !ok {
		// Incomplete probe: don't pin the negative as a value, but remember
		// the attempt so rounds within propsProbeRetryTTL skip re-probing —
		// locally and process-wide (the shared record is what stops the
		// other sessions from re-dialing the same endpoint).
		p.propsMu.Lock()
		p.propsNegBaseURL = baseURL
		p.propsNegAt = time.Now()
		p.propsMu.Unlock()
		shared.RecordFailure(sharedKey)
		return false
	}

	p.propsMu.Lock()
	// Another goroutine may have filled the cache while we probed.
	if !p.propsChecked || p.propsBaseURL != baseURL {
		p.propsChecked = true
		p.propsBaseURL = baseURL
		p.propsPreserveReasoning = supported
	}
	// A completed probe supersedes any recorded failure for this endpoint.
	if p.propsNegBaseURL == baseURL {
		p.propsNegBaseURL = ""
		p.propsNegAt = time.Time{}
	}
	v := p.propsPreserveReasoning
	p.propsMu.Unlock()
	// Publish the completed answer: every sibling session's provider serves it
	// from here without dialing the endpoint.
	shared.RecordSuccess(sharedKey, v)
	return v
}

// propsCapsCache returns the process-shared capability cache (nil when none was
// installed — the provider then relies on its own memo).
func (p *OpenAIProvider) propsCapsCache() *PropsCapsCache {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return p.propsCaps
}

// propsBaseURLForRef returns the base URL of the endpoint that serves the
// model reference: the model's owning profile when the catalog has been
// fetched, else the ref's own registered profile (see
// ownedBaseURLLocked), then the default profile's base URL. A ref with no
// profile (a bare-ID caller) resolves to the first cached entry that lists
// the ID — model IDs are not unique across registered providers, so callers
// that know the owner pass the full ref.
func (p *OpenAIProvider) propsBaseURLForRef(ref ModelRef) string {
	p.modelsMu.RLock()
	baseURL := p.ownedBaseURLLocked(ref)
	p.modelsMu.RUnlock()
	if baseURL != "" {
		return baseURL
	}
	return p.defaultBaseURL()
}

// propsBaseURLForCurrentModel returns the base URL of the endpoint that
// serves the currently selected model (see propsBaseURLForRef). The current
// ROUTING KEY is passed, not the bare ID: a model the session runs on a
// non-default provider must have /props asked of that provider's endpoint
// even when another registered provider lists the same ID.
func (p *OpenAIProvider) propsBaseURLForCurrentModel() string {
	return p.propsBaseURLForRef(p.CurrentModelRef())
}

func (p *OpenAIProvider) invalidatePropsCaps() {
	p.propsMu.Lock()
	p.propsChecked = false
	p.propsBaseURL = ""
	p.propsPreserveReasoning = false
	p.propsNegBaseURL = ""
	p.propsNegAt = time.Time{}
	p.propsMu.Unlock()
	// Same invalidation in the shared record: a model switch can change the
	// template a router host reports for the model now selected, and an
	// endpoint-set change can re-point the base URL.
	p.propsCapsCache().Invalidate(propsCapsKey(p.propsBaseURLForCurrentModel(), p.currentModel()))
}

// probePreserveReasoning GETs /props on baseURL and reads
// supports_preserve_reasoning. ok=false (any error, non-200, or unparseable
// body) means the probe did not complete — the caller must not cache the
// negative result. A 200 response lacking the caps key is a completed probe
// reporting "unsupported" (ok=true, supported=false).
func (p *OpenAIProvider) probePreserveReasoning(ctx context.Context, baseURL, modelID string) (supported, ok bool) {
	propsURL := llamaPropsURL(baseURL, modelID)
	if propsURL == "" {
		return false, false
	}
	status, body, err := p.propsRequest(ctx, http.MethodGet, propsURL, p.apiKeyForBaseURL(baseURL), "")
	if err != nil || status != http.StatusOK {
		return false, false
	}
	return parsePreserveReasoningCap(body)
}

func parsePreserveReasoningCap(body []byte) (supported, ok bool) {
	var props struct {
		ChatTemplateCaps map[string]bool `json:"chat_template_caps"`
	}
	if err := json.Unmarshal(body, &props); err != nil {
		return false, false
	}
	return props.ChatTemplateCaps["supports_preserve_reasoning"], true
}

// llamaPropsURL maps an OpenAI-compatible base URL (.../v1) to llama.cpp GET
// /props. A non-empty modelID selects the model on router-style hosts
// (llama.cpp router mode, llama-route); single-model servers ignore the
// parameter.
func llamaPropsURL(baseURL, modelID string) string {
	base := llamaEndpointBase(baseURL)
	if base == "" {
		return ""
	}
	if modelID != "" {
		return base + "/props?model=" + url.QueryEscape(modelID)
	}
	return base + "/props"
}
