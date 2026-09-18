package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"gogen/internal/buildinfo"
)

// errEffortsUnavailable is returned by ProbeReasoningEfforts when the
// endpoint does not expose a derivable reasoning-effort vocabulary (not a
// llama.cpp server, missing capability keys, probe failure). Callers keep
// the DefaultReasoningEfforts fallback; nothing is cached, so a later
// trigger retries.
var errEffortsUnavailable = fmt.Errorf("reasoning-effort options unavailable from endpoint")

// derivedEfforts is one cached llama.cpp capability-probe result: the
// reasoning_effort values the model's chat template accepts, plus the
// endpoint it was derived from. probed=false (or an absent entry) means "not
// probed yet". A probed entry with a nil efforts slice means the endpoint
// definitively reported NO reasoning-effort control (empty accepted set, like
// a toggle-only models.dev model).
type derivedEfforts struct {
	probed  bool
	efforts []string
	baseURL string
}

// effortMembershipRe matches an (in|not in) membership check whose members
// are quoted string literals: not in ('xhigh', 'medium', 'low') or
// in ["low", "medium", "high"].
var effortMembershipRe = regexp.MustCompile(`(?:not\s+)?in\s*[\(\[]\s*((?:'[^']*'|"[^"]*")\s*(?:,\s*(?:'[^']*'|"[^"]*")\s*)*)[\)\]]`)

// effortLiteralRe extracts the quoted literals from a membership clause.
var effortLiteralRe = regexp.MustCompile(`'([^']*)'|"([^"]*)"`)

// supportedTypesRe matches the prose enumeration llama.cpp templates embed in
// raise_exception messages: "Supported types are xhigh (default), medium, and
// low." (an optional colon after the verb is tolerated).
var supportedTypesRe = regexp.MustCompile(`(?i)supported\s+types?\s+(?:are|include[s]?|is)\s*:?\s+([^.!]+)[.!]`)

// validEffortTokenRe gates prose-parsed values: a plausible effort token must
// be a bare word (letters, digits, underscore, hyphen). Anything else (quotes
// left in, sentence fragments) invalidates the parse.
var validEffortTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// parseReasoningEffortsTemplate extracts the accepted reasoning_effort values
// declared by a llama.cpp chat template's own source. ok=false means the
// template does not declare a parseable vocabulary (the caller falls back to
// the /apply-template sentinel probe).
//
// Recognized constructs, in priority order:
//
//  1. A membership check (in / not in over a quoted literal list) within a
//     window of a reasoning_effort / reasoning_strength reference — the
//     canonical validation construct:
//     {%- if resolved_reasoning_effort not in ('xhigh', 'medium', 'low') %}
//  2. A raise_exception message enumerating the values ("Supported types are
//     xhigh (default), medium, and low.") — the same vocabulary in prose.
//
// Aliases (e.g. 'high' remapped to 'xhigh') are NOT included: the declared
// set is the template's canonical vocabulary, which is what the UI should
// offer. The default level (|default('xhigh')) is likewise not part of the
// accepted set.
func parseReasoningEffortsTemplate(src string) ([]string, bool) {
	for _, ref := range []string{"reasoning_effort", "reasoning_strength"} {
		idx := 0
		for {
			i := strings.Index(src[idx:], ref)
			if i < 0 {
				break
			}
			i += idx
			start := i - 200
			if start < 0 {
				start = 0
			}
			end := i + 300
			if end > len(src) {
				end = len(src)
			}
			if m := effortMembershipRe.FindStringSubmatch(src[start:end]); m != nil {
				if out := literalsFromClause(m[1]); len(out) > 0 {
					return out, true
				}
			}
			idx = i + len(ref)
		}
	}
	if out, ok := parseSupportedTypesMessage(src); ok {
		return out, true
	}
	return nil, false
}

// parseSupportedTypesMessage parses the prose value enumeration from a
// template's raise_exception text (also present verbatim in llama.cpp's error
// responses): "Supported types are xhigh (default), medium, and low."
func parseSupportedTypesMessage(s string) ([]string, bool) {
	m := supportedTypesRe.FindStringSubmatch(s)
	if m == nil {
		return nil, false
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "and ")
		part = strings.TrimSpace(part)
		part = strings.TrimSuffix(part, " (default)")
		part = strings.Trim(part, `"'`)
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !validEffortTokenRe.MatchString(part) {
			return nil, false // not a clean enumeration — refuse to guess
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// literalsFromClause extracts the quoted string literals from a membership
// clause, preserving order and dropping duplicates.
func literalsFromClause(clause string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, m := range effortLiteralRe.FindAllStringSubmatch(clause, -1) {
		v := m[1]
		if v == "" {
			v = m[2]
		}
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// ProbeReasoningEfforts derives the accepted reasoning_effort values for
// modelID from a llama.cpp capability probe (GET /props, plus a single
// /apply-template sentinel render when the template declares no parseable
// vocabulary) and caches the result per model. Returns whether the derived
// set differs from the previously effective one — the caller uses that to
// refresh UI state. Never called on the inference or turn-lock path; the
// probe performs bounded network I/O (propsProbeTimeout per call).
//
// Cache misses and failures are NOT cached: a later trigger retries. The
// cache is cleared by SetProfiles (the endpoint set changed) and keyed per
// model so switching back to a previously probed model is a no-op.
func (p *OpenAIProvider) ProbeReasoningEfforts(ctx context.Context, modelID string) (bool, error) {
	if p == nil || modelID == "" {
		return false, nil
	}
	baseURL := p.propsBaseURLForModel(modelID)
	if strings.TrimSpace(baseURL) == "" {
		return false, nil // official API: no llama.cpp capability discovery
	}
	ref := p.refForModelID(modelID).Normalized()
	p.effortsMu.Lock()
	if p.effortsDerived == nil {
		p.effortsDerived = make(map[ModelRef]derivedEfforts)
	}
	prev, ok := p.effortsDerived[ref]
	if ok && prev.probed && prev.baseURL == baseURL {
		p.effortsMu.Unlock()
		return false, nil // already derived for this endpoint
	}
	p.effortsMu.Unlock()

	efforts, err := p.probeEffortsOnce(ctx, baseURL, modelID)
	if err != nil {
		return false, err
	}
	// The model may have been switched while the probe was in flight; only
	// cache when the probe still describes the requested model (router hosts
	// can change the loaded template mid-probe).
	if p.currentModel() != modelID {
		return false, nil
	}
	effective := DefaultReasoningEfforts
	if ok && prev.probed {
		effective = prev.efforts
	}
	p.effortsMu.Lock()
	p.effortsDerived[ref] = derivedEfforts{probed: true, efforts: efforts, baseURL: baseURL}
	p.effortsMu.Unlock()
	return !slices.Equal(effective, efforts), nil
}

// probeEffortsOnce performs the actual capability probe for one endpoint:
// GET /props for chat_template_caps + the template source, then a static
// parse of the declared vocabulary, then (only when the template references
// reasoning_effort but declares no parseable vocabulary) one /apply-template
// sentinel render whose error message enumerates the accepted values.
//
// Returns a nil efforts slice (with nil error) when the endpoint definitively
// reports no reasoning-effort control. Returns errEffortsUnavailable when the
// vocabulary cannot be derived (non-llama.cpp endpoint, missing caps,
// probe failure) — callers must not cache that state.
func (p *OpenAIProvider) probeEffortsOnce(ctx context.Context, baseURL, modelID string) ([]string, error) {
	propsURL := llamaPropsURL(baseURL, modelID)
	if propsURL == "" {
		return nil, errEffortsUnavailable
	}
	status, body, err := p.propsRequest(ctx, http.MethodGet, propsURL, p.apiKeyForBaseURL(baseURL), "")
	if err != nil || status != http.StatusOK {
		return nil, errEffortsUnavailable
	}
	var props struct {
		ChatTemplateCaps map[string]bool `json:"chat_template_caps"`
		ChatTemplate     string          `json:"chat_template"`
	}
	if err := json.Unmarshal(body, &props); err != nil {
		return nil, errEffortsUnavailable
	}
	supported, ok := props.ChatTemplateCaps["supports_reasoning_effort"]
	if !ok {
		// Not a llama.cpp server (or a build that predates the cap):
		// nothing authoritative to derive — keep the default fallback.
		return nil, errEffortsUnavailable
	}
	if !supported {
		// Definitive: the template never references reasoning_effort —
		// no effort control (the toggle-only semantics of an empty set).
		return []string{}, nil
	}
	if strings.TrimSpace(props.ChatTemplate) == "" {
		return nil, errEffortsUnavailable
	}
	if efforts, ok := parseReasoningEffortsTemplate(props.ChatTemplate); ok {
		return efforts, nil
	}
	// The template uses reasoning_effort but declares no parseable
	// vocabulary: ask the server to render the template with an invalid
	// value. Validating templates reject it and enumerate the accepted
	// values in the error message; non-validating templates render it
	// (accepting any value → the default set is the pragmatic choice).
	return p.probeEffortValuesViaApplyTemplate(ctx, baseURL, modelID)
}

// probeEffortValuesViaApplyTemplate POSTs one sentinel render to llama.cpp's
// /apply-template endpoint (template rendering only — no inference, no
// tokens) and derives the accepted values from the outcome.
func (p *OpenAIProvider) probeEffortValuesViaApplyTemplate(ctx context.Context, baseURL, modelID string) ([]string, error) {
	applyURL := llamaApplyTemplateURL(baseURL, modelID)
	if applyURL == "" {
		return nil, errEffortsUnavailable
	}
	// "model" is a standard field that direct llama.cpp servers accept;
	// router-style hosts (llama-route) require it to resolve the upstream.
	probeBody, err := json.Marshal(map[string]any{
		"model":                 modelID,
		"messages":              []map[string]string{{"role": "user", "content": "hi"}},
		"chat_template_kwargs":  map[string]string{"reasoning_effort": "__gogen_probe__"},
		"add_generation_prompt": false,
	})
	if err != nil {
		return nil, errEffortsUnavailable
	}
	status, respBody, err := p.propsRequest(ctx, http.MethodPost, applyURL, p.apiKeyForBaseURL(baseURL), string(probeBody))
	if err != nil {
		return nil, errEffortsUnavailable
	}
	if status >= 200 && status < 300 {
		// The sentinel rendered: the template does not validate its
		// vocabulary, so any effort string is accepted. The default set is
		// the pragmatic UI choice.
		return append([]string(nil), DefaultReasoningEfforts...), nil
	}
	var apiErr struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Error.Message != "" {
		if efforts, ok := parseSupportedTypesMessage(apiErr.Error.Message); ok {
			return efforts, nil
		}
	}
	return nil, errEffortsUnavailable
}

// propsRequest performs one bounded HTTP call to a llama.cpp capability
// endpoint (/props, /apply-template) with the endpoint's owning-profile API
// key. The response body is capped at 1 MiB.
func (p *OpenAIProvider) propsRequest(ctx context.Context, method, url, apiKey, body string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, propsProbeTimeout)
	defer cancel()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// Probes can target an OpenCode base URL (router hosts): tag it like the
	// client pair does so the request carries the session header too.
	if p.sessionHdr != nil {
		if uuid := p.sessionHdr.uuid(); uuid != "" {
			req.Header.Set(SessionHeaderName, uuid)
		}
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := propsHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}

// llamaEndpointBase strips the /v1 suffix from an OpenAI-compatible base URL,
// returning the llama.cpp endpoint root ("http://host:port"). Empty when
// baseURL is not a usable URL.
func llamaEndpointBase(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/v1")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// llamaApplyTemplateURL maps an OpenAI-compatible base URL (.../v1) to the
// llama.cpp POST /apply-template endpoint (template render without
// inference). A non-empty modelID selects the model on router-style hosts;
// single-model servers ignore the parameter.
func llamaApplyTemplateURL(baseURL, modelID string) string {
	base := llamaEndpointBase(baseURL)
	if base == "" {
		return ""
	}
	if modelID != "" {
		return base + "/apply-template?model=" + url.QueryEscape(modelID)
	}
	return base + "/apply-template"
}

// invalidateReasoningEfforts drops every cached llama.cpp effort derivation.
// Called by SetProfiles: the endpoint set changed, so per-model derivations
// may no longer describe the serving endpoint.
func (p *OpenAIProvider) invalidateReasoningEfforts() {
	p.effortsMu.Lock()
	p.effortsDerived = make(map[ModelRef]derivedEfforts)
	p.effortsMu.Unlock()
}
