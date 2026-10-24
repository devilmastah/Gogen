package server

import (
	"fmt"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/onoff"
	"gogen/internal/statusdisplay"
	"gogen/internal/treesitter"
)

// fieldSpec describes ONE runtime-config option: how its value is extracted
// from a client "config" message, validated, stored in the config.Config
// overlay, pushed to the live runtime targets, and projected into the two
// server→client directions (the persisted effective config via get/set, the
// settings-modal push via project/msgGet). Adding a setting means adding
// one entry here — the allowlist, validation, storage, live application,
// persistence overlay, and client push all derive from it.
type fieldSpec struct {
	name string
	// get/set move the value in/out of the config.Config overlay. Used by
	// the persistence projection (effectiveConfig) and the tests.
	get func(r *config.Config) any
	set func(r *config.Config, v any)
	// fromMsg/normalize handle the client→server direction. fromMsg nil
	// means the option is overlay-only (not client-settable via the
	// settings modal); normalize nil means no validation (passthrough).
	// normalize returns the stored value and a non-empty error string that
	// rejects the WHOLE request when invalid.
	fromMsg   func(m *WSMessage) any
	normalize func(v any) (any, string)
	// applyLive pushes the value to the live runtime target (executor,
	// process globals, per-session context managers, session store) after
	// the overlay swap. Nil = restart-staged or overlay-only.
	applyLive func(s *Server, r *config.Config)
	// project renders the value into a server→client config push
	// (decorateConfig); msgGet reads it back (tests). Nil = not pushed
	// directly (e.g. provider-profile fields, projected via providerEntries).
	project func(r *config.Config, m *WSMessage)
	msgGet  func(m *WSMessage) any
	// setContext copies the value into a per-session context-manager
	// Settings (applyContextSettingsToAll). Nil = not a context setting.
	setContext func(next *contextmgr.Settings, r *config.Config)
	// restartDisplay is the config-key name shown in the "restart to take
	// effect" notice/banner (empty = applies live or overlay-only).
	restartDisplay string
	// forceSecrets forces a 0600 secret write on the persist that follows
	// a save (webAuthToken: user-entered tokens must survive the restart).
	forceSecrets bool
	// sample is a representative non-zero value the projection tests use.
	sample any
}

// strSpec builds a string field whose client push mirrors the overlay value
// verbatim (project and msgGet are the same accessor pair).
func strSpec(name string,
	cfgGet func(*config.Config) string, cfgSet func(*config.Config, string),
	msgGet func(*WSMessage) string, msgSet func(*WSMessage, string)) fieldSpec {
	return fieldSpec{
		name:    name,
		get:     func(r *config.Config) any { return cfgGet(r) },
		set:     func(r *config.Config, v any) { cfgSet(r, v.(string)) },
		fromMsg: func(m *WSMessage) any { return msgGet(m) },
		project: func(r *config.Config, m *WSMessage) { msgSet(m, cfgGet(r)) },
		msgGet:  func(m *WSMessage) any { return msgGet(m) },
	}
}

// intSpec builds an int field whose client push mirrors the overlay value
// verbatim.
func intSpec(name string,
	cfgGet func(*config.Config) int, cfgSet func(*config.Config, int),
	msgGet func(*WSMessage) int, msgSet func(*WSMessage, int)) fieldSpec {
	return fieldSpec{
		name:    name,
		get:     func(r *config.Config) any { return cfgGet(r) },
		set:     func(r *config.Config, v any) { cfgSet(r, v.(int)) },
		fromMsg: func(m *WSMessage) any { return msgGet(m) },
		project: func(r *config.Config, m *WSMessage) { msgSet(m, cfgGet(r)) },
		msgGet:  func(m *WSMessage) any { return msgGet(m) },
	}
}

// nonNegInt validates a "must be >= 0" int option.
func nonNegInt(name string) func(any) (any, string) {
	return func(v any) (any, string) {
		n := v.(int)
		if n < 0 {
			return nil, fmt.Sprintf("Error: %s must be >= 0", name)
		}
		return n, ""
	}
}

// onOffNormalize validates an on/off option and canonicalizes to "on"/"off".
func onOffNormalize(name string) func(any) (any, string) {
	return func(v any) (any, string) {
		raw := v.(string)
		on, ok := onoff.Parse(raw)
		if !ok {
			return nil, fmt.Sprintf("Error: invalid %s %q (want on or off)", name, raw)
		}
		return onOff(on), ""
	}
}

// promptNormalize validates a prompt-template option: trims, caps the
// length, and stores nothing when the value is the built-in default
// (no bake-in).
func promptNormalize(name string, def func() string) func(any) (any, string) {
	return func(v any) (any, string) {
		raw := v.(string)
		val := strings.TrimSpace(raw)
		if len(val) > maxPromptTemplateLen {
			return nil, fmt.Sprintf("Error: %s exceeds %d characters", name, maxPromptTemplateLen)
		}
		return agent.NormalizePromptTemplate(val, def()), ""
	}
}

// contextApplyLive returns the applyLive closure for a context-manager
// field: it sweeps every live session's context manager, merging ONLY this
// field (see applyContextField for why the copy closure is passed in
// instead of looked up in the registry).
func contextApplyLive(copy func(next *contextmgr.Settings, r *config.Config)) func(s *Server, r *config.Config) {
	return func(s *Server, r *config.Config) {
		s.applyContextField(r, copy)
	}
}

// configFields is the runtime-config field registry: every option the
// settings modal can set (client-settable: fromMsg non-nil) plus
// the overlay-only provider-profile fields the persistence projection must
// carry. The settings-modal allowlist (runtimeConfigFields) derives from
// it — do not maintain a separate literal list.
var configFields = buildConfigFields()

func buildConfigFields() map[string]fieldSpec {
	m := map[string]fieldSpec{}
	add := func(f fieldSpec) { m[f.name] = f }

	// --- command safety -------------------------------------------------
	f := strSpec("commandSafety",
		func(r *config.Config) string { return r.CommandSafetyMode },
		func(r *config.Config, v string) { r.CommandSafetyMode = v },
		func(m *WSMessage) string { return m.CommandSafetyMode },
		func(m *WSMessage, v string) { m.CommandSafetyMode = v })
	f.normalize = func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		if mode != "blocklist" && mode != "allowlist" && mode != "off" {
			return nil, fmt.Sprintf("Error: invalid commandSafety %q (want blocklist, allowlist, or off)", raw)
		}
		return mode, ""
	}
	f.applyLive = func(s *Server, r *config.Config) {
		s.ws.Exec.SetCommandGuard(r.CommandSafetyMode, agent.ParseAllowlist(r.CommandAllowlist))
	}
	f.sample = "allowlist"
	add(f)

	f = strSpec("commandAllowlist",
		func(r *config.Config) string { return r.CommandAllowlist },
		func(r *config.Config, v string) { r.CommandAllowlist = v },
		func(m *WSMessage) string { return m.CommandAllowlist },
		func(m *WSMessage, v string) { m.CommandAllowlist = v })
	f.applyLive = func(s *Server, r *config.Config) {
		s.ws.Exec.SetCommandGuard(r.CommandSafetyMode, agent.ParseAllowlist(r.CommandAllowlist))
	}
	f.sample = "ls, cat"
	add(f)

	f = strSpec("deleteApproval",
		func(r *config.Config) string { return r.DeleteApproval },
		func(r *config.Config, v string) { r.DeleteApproval = v },
		func(m *WSMessage) string { return m.DeleteApproval },
		func(m *WSMessage, v string) { m.DeleteApproval = v })
	f.normalize = func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		if mode != "required" && mode != "off" {
			return nil, fmt.Sprintf("Error: invalid deleteApproval %q (want required or off)", raw)
		}
		return mode, ""
	}
	f.applyLive = func(s *Server, r *config.Config) {
		s.ws.Exec.SetDeleteApproval(!strings.EqualFold(r.DeleteApproval, "off"))
	}
	f.sample = "off"
	add(f)

	// command_approval: what a blocklisted command does when command_safety
	// is blocklist. required = ask the user (and refuse when nobody can be
	// asked); off = refuse outright, no prompt. It never allows the command —
	// that is command_safety: off.
	f = strSpec("commandApproval",
		func(r *config.Config) string { return r.CommandApproval },
		func(r *config.Config, v string) { r.CommandApproval = v },
		func(m *WSMessage) string { return m.CommandApproval },
		func(m *WSMessage, v string) { m.CommandApproval = v })
	f.normalize = func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		if mode != "required" && mode != "off" {
			return nil, fmt.Sprintf("Error: invalid commandApproval %q (want required or off)", raw)
		}
		return mode, ""
	}
	f.applyLive = func(s *Server, r *config.Config) {
		s.ws.Exec.SetCommandApproval(!strings.EqualFold(r.CommandApproval, "off"))
	}
	f.sample = "required"
	add(f)

	f = strSpec("commandSandbox",
		func(r *config.Config) string { return r.CommandSandbox },
		func(r *config.Config, v string) { r.CommandSandbox = v },
		func(m *WSMessage) string { return m.CommandSandbox },
		func(m *WSMessage, v string) { m.CommandSandbox = v })
	f.normalize = func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		if mode != "off" && mode != "bwrap" {
			return nil, fmt.Sprintf("Error: invalid commandSandbox %q (want off or bwrap)", raw)
		}
		return mode, ""
	}
	f.applyLive = func(s *Server, r *config.Config) { s.ws.Exec.SetSandbox(r.CommandSandbox) }
	f.sample = "bwrap"
	add(f)

	f = intSpec("commandIdleTimeoutSecs",
		func(r *config.Config) int { return r.CommandIdleTimeoutSecs },
		func(r *config.Config, v int) { r.CommandIdleTimeoutSecs = v },
		func(m *WSMessage) int { return m.CommandIdleTimeoutSecs },
		func(m *WSMessage, v int) { m.CommandIdleTimeoutSecs = v })
	f.normalize = nonNegInt("commandIdleTimeoutSecs")
	f.applyLive = func(s *Server, r *config.Config) {
		s.ws.Exec.SetIdleTimeout(time.Duration(r.CommandIdleTimeoutSecs) * time.Second)
	}
	f.sample = 45
	add(f)

	// --- context management ---------------------------------------------
	f = intSpec("contextLimit",
		func(r *config.Config) int { return r.ContextLimit },
		func(r *config.Config, v int) { r.ContextLimit = v },
		func(m *WSMessage) int { return m.ContextLimitConfig },
		func(m *WSMessage, v int) { m.ContextLimitConfig = v })
	f.normalize = nonNegInt("contextLimit")
	f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.ContextLimit = r.ContextLimit }
	f.applyLive = contextApplyLive(f.setContext)
	f.sample = 20000
	add(f)

	f = fieldSpec{
		name:    "compactThreshold",
		get:     func(r *config.Config) any { return r.CompactThreshold },
		set:     func(r *config.Config, v any) { r.CompactThreshold = v.(float64) },
		fromMsg: func(m *WSMessage) any { return m.CompactThreshold },
		project: func(r *config.Config, m *WSMessage) { m.CompactThreshold = r.CompactThreshold },
		msgGet:  func(m *WSMessage) any { return m.CompactThreshold },
		normalize: func(v any) (any, string) {
			x := v.(float64)
			if x < 0 || x > 1 {
				return nil, "Error: compactThreshold must be between 0 and 1"
			}
			return x, ""
		},
		setContext: func(next *contextmgr.Settings, r *config.Config) { next.CompactThreshold = r.CompactThreshold },
		sample:     0.5,
	}
	f.applyLive = contextApplyLive(f.setContext)
	add(f)

	f = intSpec("compactKeepRecentMessages",
		func(r *config.Config) int { return r.CompactKeepRecentMessages },
		func(r *config.Config, v int) { r.CompactKeepRecentMessages = v },
		func(m *WSMessage) int { return m.CompactKeepRecentMessages },
		func(m *WSMessage, v int) { m.CompactKeepRecentMessages = v })
	f.normalize = nonNegInt("compactKeepRecentMessages")
	f.setContext = func(next *contextmgr.Settings, r *config.Config) {
		next.CompactKeepRecentMessages = r.CompactKeepRecentMessages
	}
	f.applyLive = contextApplyLive(f.setContext)
	f.sample = 3
	add(f)

	f = intSpec("maxToolResultBytes",
		func(r *config.Config) int { return r.MaxToolResultBytes },
		func(r *config.Config, v int) { r.MaxToolResultBytes = v },
		func(m *WSMessage) int { return m.MaxToolResultBytes },
		func(m *WSMessage, v int) { m.MaxToolResultBytes = v })
	f.normalize = nonNegInt("maxToolResultBytes")
	f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.MaxToolResultBytes = r.MaxToolResultBytes }
	copyContext := f.setContext // capture the value: f is reused for later fields
	f.applyLive = func(s *Server, r *config.Config) {
		s.applyContextField(r, copyContext)
		// The executor bounds in-memory command output at the same cap the
		// context managers apply to tool results (0 = no cap, pass-through).
		s.ws.Exec.SetMaxToolOutputBytes(r.MaxToolResultBytes)
	}
	f.sample = 65536
	add(f)

	// Gates the spill feature: oversized tool output is persisted to the
	// session's spill dir and the inline result becomes a head/tail
	// preview + locator (on, default) instead of the legacy plain-cap
	// head-only truncation (off).
	f = strSpec("outputSpill",
		func(r *config.Config) string { return r.OutputSpill },
		func(r *config.Config, v string) { r.OutputSpill = v },
		func(m *WSMessage) string { return m.OutputSpill },
		func(m *WSMessage, v string) { m.OutputSpill = v })
	f.normalize = onOffNormalize("outputSpill")
	f.applyLive = func(s *Server, r *config.Config) {
		agent.ConfigureOutputSpill(onoff.Enabled(r.OutputSpill))
	}
	f.sample = "off"
	add(f)

	f = intSpec("compactReserveTokens",
		func(r *config.Config) int { return r.CompactReserveTokens },
		func(r *config.Config, v int) { r.CompactReserveTokens = v },
		func(m *WSMessage) int { return m.CompactReserveTokens },
		func(m *WSMessage, v int) { m.CompactReserveTokens = v })
	f.normalize = nonNegInt("compactReserveTokens")
	f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.CompactReserveTokens = r.CompactReserveTokens }
	f.applyLive = contextApplyLive(f.setContext)
	f.sample = 1000
	add(f)

	f = strSpec("compactLastResort",
		func(r *config.Config) string { return r.CompactLastResort },
		func(r *config.Config, v string) { r.CompactLastResort = v },
		func(m *WSMessage) string { return m.CompactLastResort },
		func(m *WSMessage, v string) { m.CompactLastResort = v })
	f.normalize = func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		if mode != "condense" && mode != "error" {
			return nil, fmt.Sprintf("Error: invalid compactLastResort %q (want condense or error)", raw)
		}
		return mode, ""
	}
	f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.CompactLastResort = r.CompactLastResort }
	f.applyLive = contextApplyLive(f.setContext)
	f.sample = "error"
	add(f)

	// --- web features -----------------------------------------------------
	f = strSpec("webFetch",
		func(r *config.Config) string { return r.WebFetch },
		func(r *config.Config, v string) { r.WebFetch = v },
		func(m *WSMessage) string { return m.WebFetch },
		func(m *WSMessage, v string) { m.WebFetch = v })
	f.normalize = onOffNormalize("webFetch")
	f.applyLive = func(s *Server, r *config.Config) {
		agent.ConfigureWebFetch(onoff.Enabled(r.WebFetch), r.WebFetchMode, r.WebAllowedDomains)
	}
	f.sample = "off"
	add(f)

	f = strSpec("webSearch",
		func(r *config.Config) string { return r.WebSearch },
		func(r *config.Config, v string) { r.WebSearch = v },
		func(m *WSMessage) string { return m.WebSearch },
		func(m *WSMessage, v string) { m.WebSearch = v })
	f.normalize = onOffNormalize("webSearch")
	f.applyLive = func(s *Server, r *config.Config) {
		agent.ConfigureWebSearchEnabled(onoff.Enabled(r.WebSearch))
	}
	f.sample = "off"
	add(f)

	f = strSpec("webSearchBackend",
		func(r *config.Config) string { return r.WebSearchBackend },
		func(r *config.Config, v string) { r.WebSearchBackend = v },
		func(m *WSMessage) string { return m.WebSearchBackend },
		func(m *WSMessage, v string) { m.WebSearchBackend = v })
	f.normalize = func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		if mode != "" && mode != "brave" {
			return nil, fmt.Sprintf("Error: invalid webSearchBackend %q (want brave or empty)", raw)
		}
		return mode, ""
	}
	f.applyLive = func(s *Server, r *config.Config) {
		agent.ConfigureWebSearch(r.WebSearchBackend, r.WebSearchAPIKey)
	}
	f.sample = "brave"
	add(f)

	f = strSpec("webSearchApiKey",
		func(r *config.Config) string { return r.WebSearchAPIKey },
		func(r *config.Config, v string) { r.WebSearchAPIKey = v },
		func(m *WSMessage) string { return m.WebSearchAPIKey },
		func(m *WSMessage, v string) { m.WebSearchAPIKey = v })
	// The key itself is never pushed — only the set flag.
	f.project = func(r *config.Config, m *WSMessage) { m.WebSearchAPIKeySet = r.WebSearchAPIKey != "" }
	f.msgGet = func(m *WSMessage) any { return m.WebSearchAPIKeySet }
	f.applyLive = func(s *Server, r *config.Config) {
		agent.ConfigureWebSearch(r.WebSearchBackend, r.WebSearchAPIKey)
	}
	f.sample = "key"
	add(f)

	f = strSpec("webAllowedDomains",
		func(r *config.Config) string { return r.WebAllowedDomains },
		func(r *config.Config, v string) { r.WebAllowedDomains = v },
		func(m *WSMessage) string { return m.WebAllowedDomains },
		func(m *WSMessage, v string) { m.WebAllowedDomains = v })
	f.applyLive = func(s *Server, r *config.Config) {
		agent.ConfigureWebFetch(onoff.Enabled(r.WebFetch), r.WebFetchMode, r.WebAllowedDomains)
	}
	f.sample = "example.com"
	add(f)

	f = strSpec("webFetchMode",
		func(r *config.Config) string { return r.WebFetchMode },
		func(r *config.Config, v string) { r.WebFetchMode = v },
		func(m *WSMessage) string { return m.WebFetchMode },
		func(m *WSMessage, v string) { m.WebFetchMode = v })
	f.applyLive = func(s *Server, r *config.Config) {
		agent.ConfigureWebFetch(onoff.Enabled(r.WebFetch), r.WebFetchMode, r.WebAllowedDomains)
	}
	f.sample = "https"
	add(f)

	f = strSpec("treesitter",
		func(r *config.Config) string { return r.TreeSitter },
		func(r *config.Config, v string) { r.TreeSitter = v },
		func(m *WSMessage) string { return m.TreeSitter },
		func(m *WSMessage, v string) { m.TreeSitter = v })
	f.normalize = onOffNormalize("treesitter")
	f.applyLive = func(s *Server, r *config.Config) {
		treesitter.Configure(onoff.Enabled(r.TreeSitter), r.TreeSitterLangs)
	}
	f.sample = "off"
	add(f)

	f = strSpec("treesitterLangs",
		func(r *config.Config) string { return r.TreeSitterLangs },
		func(r *config.Config, v string) { r.TreeSitterLangs = v },
		func(m *WSMessage) string { return m.TreeSitterLangs },
		func(m *WSMessage, v string) { m.TreeSitterLangs = v })
	f.applyLive = func(s *Server, r *config.Config) {
		treesitter.Configure(onoff.Enabled(r.TreeSitter), r.TreeSitterLangs)
	}
	f.sample = "go"
	add(f)

	f = strSpec("preserveReasoning",
		func(r *config.Config) string { return r.PreserveReasoning },
		func(r *config.Config, v string) { r.PreserveReasoning = v },
		func(m *WSMessage) string { return m.PreserveReasoning },
		func(m *WSMessage, v string) { m.PreserveReasoning = v })
	f.applyLive = func(s *Server, r *config.Config) { s.applyPreserveReasoningToAll(r.PreserveReasoning) }
	f.sample = "on"
	add(f)

	// --- status display (LAN status screen) -------------------------------
	// One row per top-level turn on a ttgo/status display: progress is the
	// context-window usage, the state is running / done / error / idle, and
	// the message names the current activity. Off by default; applied live
	// like the other host integrations, and every sub-field's applyLive
	// re-applies the whole tuple so a single changed field can never
	// desync the others.
	applyStatusDisplay := func(s *Server, r *config.Config) {
		host, token := r.StatusDisplayHost, r.StatusDisplayToken
		source, workdir := r.StatusDisplaySource, s.ws.GetWorkingDir()
		switch r.StatusDisplayMode() {
		case config.StatusDisplayBrowser:
			// Server transport wins when the server can reach the display;
			// the browser relay is the fallback for when it cannot.
			statusdisplay.ProbeWithRelay(host, token, source, workdir, s.broadcastStatusDisplay)
		case config.StatusDisplayOn:
			statusdisplay.SetSink(nil)
			statusdisplay.Configure(true, host, token, source, workdir)
		case config.StatusDisplayOff:
			statusdisplay.SetSink(nil)
			statusdisplay.Configure(false, host, token, source, workdir)
		default: // auto: probe for the screen, enable reporting when it answers
			statusdisplay.SetSink(nil)
			statusdisplay.Probe(host, token, source, workdir)
		}
	}

	f = strSpec("statusDisplay",
		func(r *config.Config) string { return r.StatusDisplay },
		func(r *config.Config, v string) { r.StatusDisplay = v },
		func(m *WSMessage) string { return m.StatusDisplay },
		func(m *WSMessage, v string) { m.StatusDisplay = v })
	// off / auto (probe at startup) / on / browser (the web UI sends).
	f.normalize = func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		switch mode {
		case config.StatusDisplayOff, config.StatusDisplayAuto, config.StatusDisplayOn, config.StatusDisplayBrowser:
			return mode, ""
		}
		return nil, fmt.Sprintf("Error: invalid statusDisplay %q (want off, auto, on, or browser)", raw)
	}
	f.applyLive = applyStatusDisplay
	f.sample = "on"
	add(f)

	f = strSpec("statusDisplayHost",
		func(r *config.Config) string { return r.StatusDisplayHost },
		func(r *config.Config, v string) { r.StatusDisplayHost = v },
		func(m *WSMessage) string { return m.StatusDisplayHost },
		func(m *WSMessage, v string) { m.StatusDisplayHost = v })
	// An empty host falls back to the firmware's mDNS default rather than
	// disabling the row silently.
	f.normalize = func(v any) (any, string) {
		host := strings.TrimSpace(v.(string))
		if host == "" {
			host = config.DefaultStatusDisplayHost
		}
		return host, ""
	}
	f.applyLive = applyStatusDisplay
	f.sample = "192.168.1.50"
	add(f)

	f = strSpec("statusDisplayToken",
		func(r *config.Config) string { return r.StatusDisplayToken },
		func(r *config.Config, v string) { r.StatusDisplayToken = v },
		func(m *WSMessage) string { return m.StatusDisplayToken },
		func(m *WSMessage, v string) { m.StatusDisplayToken = v })
	// The token itself is pushed only in browser mode, where the client is
	// the sender and therefore needs the value; the server-side modes see
	// just the set flag.
	f.project = func(r *config.Config, m *WSMessage) {
		m.StatusDisplayTokenSet = r.StatusDisplayToken != ""
		if r.StatusDisplayMode() == config.StatusDisplayBrowser {
			m.StatusDisplayTokenValue = r.StatusDisplayToken
		}
	}
	f.msgGet = func(m *WSMessage) any { return m.StatusDisplayTokenSet }
	f.applyLive = applyStatusDisplay
	// Like web_auth_token: user-entered, so the save forces the 0600
	// secret write and it survives the restart it is staged for.
	f.forceSecrets = true
	f.sample = "token"
	add(f)

	f = strSpec("statusDisplaySource",
		func(r *config.Config) string { return r.StatusDisplaySource },
		func(r *config.Config, v string) { r.StatusDisplaySource = v },
		func(m *WSMessage) string { return m.StatusDisplaySource },
		func(m *WSMessage, v string) { m.StatusDisplaySource = v })
	// Empty = AUTO ("gogen-<project dir>"); keep it empty so the reporter
	// derives the name, and an explicit value always wins.
	f.normalize = func(v any) (any, string) {
		return strings.TrimSpace(v.(string)), ""
	}
	f.applyLive = applyStatusDisplay
	f.sample = "gogen-alt"
	add(f)

	// --- sessions ---------------------------------------------------------
	f = intSpec("sessionMaxCount",
		func(r *config.Config) int { return r.SessionMaxCount },
		func(r *config.Config, v int) { r.SessionMaxCount = v },
		func(m *WSMessage) int { return m.SessionMaxCount },
		func(m *WSMessage, v int) { m.SessionMaxCount = v })
	f.normalize = nonNegInt("sessionMaxCount")
	f.applyLive = func(s *Server, r *config.Config) {
		if s.ws.Store != nil {
			s.ws.Store.SetRetention(r.SessionMaxCount, r.SessionMaxAgeDays)
			s.pruneSessions()
		}
	}
	f.sample = 4
	add(f)

	f = intSpec("sessionMaxAgeDays",
		func(r *config.Config) int { return r.SessionMaxAgeDays },
		func(r *config.Config, v int) { r.SessionMaxAgeDays = v },
		func(m *WSMessage) int { return m.SessionMaxAgeDays },
		func(m *WSMessage, v int) { m.SessionMaxAgeDays = v })
	f.normalize = func(v any) (any, string) {
		// -1 = "keep sessions forever" (the store's retention sentinel);
		// the merge path preserves it so it survives a restart.
		n := v.(int)
		if n < -1 {
			return nil, "Error: sessionMaxAgeDays must be >= -1 (-1 = keep sessions forever)"
		}
		return n, ""
	}
	f.applyLive = func(s *Server, r *config.Config) {
		if s.ws.Store != nil {
			s.ws.Store.SetRetention(r.SessionMaxCount, r.SessionMaxAgeDays)
			s.pruneSessions()
		}
	}
	f.sample = 9
	add(f)

	f = intSpec("webApprovalHoldSecs",
		func(r *config.Config) int { return r.WebApprovalHoldSecs },
		func(r *config.Config, v int) { r.WebApprovalHoldSecs = v },
		func(m *WSMessage) int { return m.WebApprovalHoldSecs },
		func(m *WSMessage, v int) { m.WebApprovalHoldSecs = v })
	f.normalize = nonNegInt("webApprovalHoldSecs")
	f.sample = 7
	add(f)

	// --- web server (restart-staged) ---------------------------------------
	f = strSpec("webBind",
		func(r *config.Config) string { return r.WebBind },
		func(r *config.Config, v string) { r.WebBind = v },
		func(m *WSMessage) string { return m.WebBind },
		func(m *WSMessage, v string) { m.WebBind = v })
	f.restartDisplay = "web_bind"
	f.sample = "0.0.0.0:9090"
	add(f)

	f = strSpec("webAllowedOrigins",
		func(r *config.Config) string { return r.WebAllowedOrigins },
		func(r *config.Config, v string) { r.WebAllowedOrigins = v },
		func(m *WSMessage) string { return m.WebAllowedOrigins },
		func(m *WSMessage, v string) { m.WebAllowedOrigins = v })
	f.restartDisplay = "web_allowed_origins"
	f.sample = "example.com"
	add(f)

	f = strSpec("webAuthToken",
		func(r *config.Config) string { return r.WebAuthToken },
		func(r *config.Config, v string) { r.WebAuthToken = v },
		func(m *WSMessage) string { return m.WebAuthToken },
		func(m *WSMessage, v string) { m.WebAuthToken = v })
	// The token itself is never pushed — only the set flag.
	f.project = func(r *config.Config, m *WSMessage) { m.WebAuthTokenSet = r.WebAuthToken != "" }
	f.msgGet = func(m *WSMessage) any { return m.WebAuthTokenSet }
	f.restartDisplay = "web_auth_token"
	f.forceSecrets = true
	f.sample = "token"
	add(f)

	f = strSpec("webTLSCertFile",
		func(r *config.Config) string { return r.WebTLSCertFile },
		func(r *config.Config, v string) { r.WebTLSCertFile = v },
		func(m *WSMessage) string { return m.WebTLSCertFile },
		func(m *WSMessage, v string) { m.WebTLSCertFile = v })
	f.restartDisplay = "web_tls_cert_file"
	f.sample = "/tmp/cert.pem"
	add(f)

	f = strSpec("webTLSKeyFile",
		func(r *config.Config) string { return r.WebTLSKeyFile },
		func(r *config.Config, v string) { r.WebTLSKeyFile = v },
		func(m *WSMessage) string { return m.WebTLSKeyFile },
		func(m *WSMessage, v string) { m.WebTLSKeyFile = v })
	f.restartDisplay = "web_tls_key_file"
	f.sample = "/tmp/key.pem"
	add(f)

	f = intSpec("webMaxActiveSessions",
		func(r *config.Config) int { return r.WebMaxActiveSessions },
		func(r *config.Config, v int) { r.WebMaxActiveSessions = v },
		func(m *WSMessage) int { return m.WebMaxActiveSessions },
		func(m *WSMessage, v int) { m.WebMaxActiveSessions = v })
	f.normalize = nonNegInt("webMaxActiveSessions")
	f.restartDisplay = "web_max_active_sessions"
	f.sample = 2
	add(f)

	f = strSpec("mcp",
		func(r *config.Config) string { return r.MCP },
		func(r *config.Config, v string) { r.MCP = v },
		func(m *WSMessage) string { return m.MCP },
		func(m *WSMessage, v string) { m.MCP = v })
	f.normalize = func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := onoff.Parse(mode); !ok {
			return nil, fmt.Sprintf("Error: invalid mcp %q (want on or off)", raw)
		}
		// Store the normalized spelling (unlike the on/off fields above,
		// the config-file loader accepts the full onoff spellings here).
		return mode, ""
	}
	f.restartDisplay = "mcp"
	f.sample = "on"
	add(f)

	// --- subagents ----------------------------------------------------------
	f = fieldSpec{
		name: "subagentModel",
		get:  func(r *config.Config) any { return r.SubagentModel },
		set:  func(r *config.Config, v any) { r.SubagentModel = v.(string) },
		fromMsg: func(m *WSMessage) any {
			if m.SubagentModel == nil {
				return ""
			}
			return *m.SubagentModel
		},
		// The default model for spawned subagents; empty clears back to
		// "inherit the parent's model". Any model id string is accepted —
		// catalog validation is fail-open at spawn time (the spawner
		// falls back to the workspace default when unselectable).
		normalize: func(v any) (any, string) {
			raw := v.(string)
			val := strings.TrimSpace(raw)
			if raw != "" && val == "" {
				return nil, "Error: subagentModel must be a model id or empty (inherit)"
			}
			return val, ""
		},
		// Pointer so config pushes always carry it — including the empty
		// "inherit" state a clear by one tab must broadcast to every tab.
		project: func(r *config.Config, m *WSMessage) { m.SubagentModel = &r.SubagentModel },
		msgGet: func(m *WSMessage) any {
			if m.SubagentModel == nil {
				return nil
			}
			return *m.SubagentModel
		},
		sample: "gpt-4o-mini",
	}
	add(f)

	// --- board auto-review agent --------------------------------------------
	f = fieldSpec{
		name: "reviewAgentModel",
		get:  func(r *config.Config) any { return r.ReviewAgentModel },
		set:  func(r *config.Config, v any) { r.ReviewAgentModel = v.(string) },
		fromMsg: func(m *WSMessage) any {
			if m.ReviewAgentModel == nil {
				return ""
			}
			return *m.ReviewAgentModel
		},
		// The default model for auto review sessions; empty clears back
		// to the cascade (the ticket's own override, then the workspace
		// default). Any model id string is accepted — catalog validation
		// is fail-open at review-start time (the cascade falls through to
		// the workspace default when unselectable).
		normalize: func(v any) (any, string) {
			raw := v.(string)
			val := strings.TrimSpace(raw)
			if raw != "" && val == "" {
				return nil, "Error: reviewAgentModel must be a model id or empty (cascade)"
			}
			return val, ""
		},
		// Pointer so config pushes always carry it — including the empty
		// "cascade" state a clear by one tab must broadcast to every tab.
		project: func(r *config.Config, m *WSMessage) { m.ReviewAgentModel = &r.ReviewAgentModel },
		msgGet: func(m *WSMessage) any {
			if m.ReviewAgentModel == nil {
				return nil
			}
			return *m.ReviewAgentModel
		},
		sample: "gpt-4o-mini",
	}
	add(f)

	f = fieldSpec{
		name: "reviewAgentThinkingLevel",
		get:  func(r *config.Config) any { return r.ReviewAgentThinkingLevel },
		set:  func(r *config.Config, v any) { r.ReviewAgentThinkingLevel = v.(string) },
		fromMsg: func(m *WSMessage) any {
			if m.ReviewAgentThinkingLevel == nil {
				return ""
			}
			return *m.ReviewAgentThinkingLevel
		},
		// The reasoning-effort level for auto review sessions; empty
		// clears back to "inherit the workspace level". Normalized to a
		// literal reasoning_effort value; validity against the reviewer's
		// final model is resolved at review start (the per-ticket
		// override wins, so the server cannot validate it here).
		normalize: func(v any) (any, string) {
			raw := v.(string)
			val := string(agent.NormalizeThinkingLevel(raw))
			if raw != "" && val == "" {
				return nil, "Error: reviewAgentThinkingLevel must be a reasoning-effort value or empty (inherit)"
			}
			return val, ""
		},
		project: func(r *config.Config, m *WSMessage) { m.ReviewAgentThinkingLevel = &r.ReviewAgentThinkingLevel },
		msgGet: func(m *WSMessage) any {
			if m.ReviewAgentThinkingLevel == nil {
				return nil
			}
			return *m.ReviewAgentThinkingLevel
		},
		sample: "high",
	}
	add(f)

	f = fieldSpec{
		name: "subagentThinkingLevel",
		get:  func(r *config.Config) any { return r.SubagentThinkingLevel },
		set:  func(r *config.Config, v any) { r.SubagentThinkingLevel = v.(string) },
		fromMsg: func(m *WSMessage) any {
			if m.SubagentThinkingLevel == nil {
				return ""
			}
			return *m.SubagentThinkingLevel
		},
		// The reasoning-effort level for spawned subagents; empty clears
		// back to "inherit the parent's level". Normalized to a literal
		// reasoning_effort value; validity against the subagent's final
		// model is fail-open at spawn time (a value the model does not
		// accept is omitted — the tool's model argument can override the
		// configured model, so the server cannot validate it here).
		normalize: func(v any) (any, string) {
			raw := v.(string)
			val := string(agent.NormalizeThinkingLevel(raw))
			if raw != "" && val == "" {
				return nil, "Error: subagentThinkingLevel must be a reasoning-effort value or empty (inherit)"
			}
			return val, ""
		},
		project: func(r *config.Config, m *WSMessage) { m.SubagentThinkingLevel = &r.SubagentThinkingLevel },
		msgGet: func(m *WSMessage) any {
			if m.SubagentThinkingLevel == nil {
				return nil
			}
			return *m.SubagentThinkingLevel
		},
		sample: "high",
	}
	add(f)

	// --- prompt templates ----------------------------------------------------
	f = strSpec("boardStartPrompt",
		func(r *config.Config) string { return r.BoardStartPrompt },
		func(r *config.Config, v string) { r.BoardStartPrompt = v },
		func(m *WSMessage) string { return m.BoardStartPrompt },
		func(m *WSMessage, v string) { m.BoardStartPrompt = v })
	f.normalize = promptNormalize("boardStartPrompt", func() string { return agent.DefaultBoardStartPrompt })
	// Pushed as the RESOLVED effective template (empty config → built-in
	// default), so the modal is pre-populated with what will be used.
	f.project = func(r *config.Config, m *WSMessage) {
		m.BoardStartPrompt = agent.ResolvePromptTemplate(r.BoardStartPrompt, agent.DefaultBoardStartPrompt)
	}
	f.sample = "board tmpl {title}"
	add(f)

	f = strSpec("boardReviewPrompt",
		func(r *config.Config) string { return r.BoardReviewPrompt },
		func(r *config.Config, v string) { r.BoardReviewPrompt = v },
		func(m *WSMessage) string { return m.BoardReviewPrompt },
		func(m *WSMessage, v string) { m.BoardReviewPrompt = v })
	f.normalize = promptNormalize("boardReviewPrompt", func() string { return agent.DefaultReviewAgentPrompt })
	// Pushed as the RESOLVED effective template (empty config → built-in
	// default), so the modal is pre-populated with what will be used.
	f.project = func(r *config.Config, m *WSMessage) {
		m.BoardReviewPrompt = agent.ResolvePromptTemplate(r.BoardReviewPrompt, agent.DefaultReviewAgentPrompt)
	}
	f.sample = "review tmpl {title}"
	add(f)

	f = strSpec("systemPrompt",
		func(r *config.Config) string { return r.SystemPrompt },
		func(r *config.Config, v string) { r.SystemPrompt = v },
		func(m *WSMessage) string { return m.SystemPrompt },
		func(m *WSMessage, v string) { m.SystemPrompt = v })
	f.normalize = promptNormalize("systemPrompt", func() string { return agent.DefaultSystemPromptTemplate() })
	f.project = func(r *config.Config, m *WSMessage) {
		m.SystemPrompt = agent.ResolvePromptTemplate(r.SystemPrompt, agent.DefaultSystemPromptTemplate())
	}
	// Live for every agent (subagents included): the next turn's system
	// message resolves the configured template.
	f.applyLive = func(s *Server, r *config.Config) { agent.ConfigureSystemPrompt(r.SystemPrompt) }
	f.sample = "sys tmpl {working_dir}"
	add(f)

	f = strSpec("subagentPrompt",
		func(r *config.Config) string { return r.SubagentPrompt },
		func(r *config.Config, v string) { r.SubagentPrompt = v },
		func(m *WSMessage) string { return m.SubagentPrompt },
		func(m *WSMessage, v string) { m.SubagentPrompt = v })
	f.normalize = promptNormalize("subagentPrompt", func() string { return agent.DefaultSubagentPrompt })
	f.project = func(r *config.Config, m *WSMessage) {
		m.SubagentPrompt = agent.ResolvePromptTemplate(r.SubagentPrompt, agent.DefaultSubagentPrompt)
	}
	// Live for every spawner (web + TUI): jobs are wrapped at spawn time.
	f.applyLive = func(s *Server, r *config.Config) { agent.ConfigureSubagentPrompt(r.SubagentPrompt) }
	f.sample = "sub tmpl {job}"
	add(f)

	// --- overlay-only provider-profile fields (provider_save API, not the
	// --- settings modal): carried by the persistence projection only.
	add(fieldSpec{
		name:   "openaiKey",
		get:    func(r *config.Config) any { return r.OpenAIKey },
		set:    func(r *config.Config, v any) { r.OpenAIKey = v.(string) },
		sample: "key",
	})
	add(fieldSpec{
		name:   "openaiModel",
		get:    func(r *config.Config) any { return r.OpenAIModel },
		set:    func(r *config.Config, v any) { r.OpenAIModel = v.(string) },
		sample: "gpt-5",
	})
	add(fieldSpec{
		name:   "openaiURL",
		get:    func(r *config.Config) any { return r.OpenAIURL },
		set:    func(r *config.Config, v any) { r.OpenAIURL = v.(string) },
		sample: "https://example.com/v1",
	})
	add(fieldSpec{
		name:   "mcpServers",
		get:    func(r *config.Config) any { return r.MCPServers },
		set:    func(r *config.Config, v any) { r.MCPServers = v.([]config.MCPServerConfig) },
		sample: []config.MCPServerConfig{{Name: "drift"}},
	})

	return m
}

// clientSettableFields returns the config option names the runtime-config
// branch accepts (the settings-modal allowlist), derived from the registry:
// every field with a client→server extraction (fromMsg). Overlay-only
// fields (provider profile) have no fromMsg and are excluded.
func clientSettableFields() map[string]bool {
	m := make(map[string]bool)
	for name, f := range configFields {
		if f.fromMsg != nil {
			m[name] = true
		}
	}
	return m
}
