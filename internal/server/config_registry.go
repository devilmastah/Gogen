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
//
// Each side is ONE field accessor returning a pointer, not a get/set pair:
// four independent closures made it possible to wire a getter to a different
// field's setter and never notice (every accessor is
// `func(r *config.Config) string { return r.X }` / `{ r.X = v }`, so a
// copy-paste slip compiles). With pointer accessors the read and the write
// are the same expression by construction.
func strSpec(name string,
	cfg func(*config.Config) *string,
	msg func(*WSMessage) *string) fieldSpec {
	return fieldSpec{
		name:    name,
		get:     func(r *config.Config) any { return *cfg(r) },
		set:     func(r *config.Config, v any) { *cfg(r) = v.(string) },
		fromMsg: func(m *WSMessage) any { return *msg(m) },
		project: func(r *config.Config, m *WSMessage) { *msg(m) = *cfg(r) },
		msgGet:  func(m *WSMessage) any { return *msg(m) },
	}
}

// intSpec builds an int field whose client push mirrors the overlay value
// verbatim (see strSpec for why the accessors are pointers).
func intSpec(name string,
	cfg func(*config.Config) *int,
	msg func(*WSMessage) *int) fieldSpec {
	return fieldSpec{
		name:    name,
		get:     func(r *config.Config) any { return *cfg(r) },
		set:     func(r *config.Config, v any) { *cfg(r) = v.(int) },
		fromMsg: func(m *WSMessage) any { return *msg(m) },
		project: func(r *config.Config, m *WSMessage) { *msg(m) = *cfg(r) },
		msgGet:  func(m *WSMessage) any { return *msg(m) },
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

// enumNormalize validates a case-insensitive enum option, canonicalizes it to
// lower case, and renders the allowed values in the settings-modal error.
//
// It replaces the closure that was copy-pasted for every enum field
// (commandSafety, deleteApproval, commandApproval, commandSandbox,
// compactLastResort, webSearchBackend, statusDisplay): "lowercase, trim,
// membership test, Error: invalid <name> %q (want …)". The wording is
// byte-for-byte what those hand-rolled versions produced — allowed is joined
// Oxford-style ("a, b, or c", "a or b"), and an empty entry renders as
// "empty" (webSearchBackend's "" = unset) — because
// TestRuntimeConfigFieldErrors pins those strings verbatim.
func enumNormalize(name string, allowed ...string) func(any) (any, string) {
	valid := make(map[string]bool, len(allowed))
	words := make([]string, len(allowed))
	for i, a := range allowed {
		valid[a] = true
		if a == "" {
			words[i] = "empty"
		} else {
			words[i] = a
		}
	}
	var want string
	switch len(words) {
	case 0:
		want = "nothing"
	case 1:
		want = words[0]
	case 2:
		want = words[0] + " or " + words[1]
	default:
		want = strings.Join(words[:len(words)-1], ", ") + ", or " + words[len(words)-1]
	}
	return func(v any) (any, string) {
		raw := v.(string)
		mode := strings.ToLower(strings.TrimSpace(raw))
		if !valid[mode] {
			return nil, fmt.Sprintf("Error: invalid %s %q (want %s)", name, raw, want)
		}
		return mode, ""
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
	{
		f := strSpec("commandSafety",
			func(r *config.Config) *string { return &r.CommandSafetyMode },
			func(m *WSMessage) *string { return &m.CommandSafetyMode })
		f.normalize = enumNormalize("commandSafety", "blocklist", "allowlist", "off")
		f.applyLive = func(s *Server, r *config.Config) {
			s.ws.Exec.SetCommandGuard(r.CommandSafetyMode, agent.ParseAllowlist(r.CommandAllowlist))
		}
		f.sample = "allowlist"
		add(f)
	}

	{
		f := strSpec("commandAllowlist",
			func(r *config.Config) *string { return &r.CommandAllowlist },
			func(m *WSMessage) *string { return &m.CommandAllowlist })
		f.applyLive = func(s *Server, r *config.Config) {
			s.ws.Exec.SetCommandGuard(r.CommandSafetyMode, agent.ParseAllowlist(r.CommandAllowlist))
		}
		f.sample = "ls, cat"
		add(f)
	}

	{
		f := strSpec("deleteApproval",
			func(r *config.Config) *string { return &r.DeleteApproval },
			func(m *WSMessage) *string { return &m.DeleteApproval })
		f.normalize = enumNormalize("deleteApproval", "required", "off")
		f.applyLive = func(s *Server, r *config.Config) {
			s.ws.Exec.SetDeleteApproval(!strings.EqualFold(r.DeleteApproval, "off"))
		}
		f.sample = "off"
		add(f)
	}

	// command_approval: what a blocklisted command does when command_safety
	// is blocklist. required = ask the user (and refuse when nobody can be
	// asked); off = refuse outright, no prompt. It never allows the command —
	// that is command_safety: off.
	{
		f := strSpec("commandApproval",
			func(r *config.Config) *string { return &r.CommandApproval },
			func(m *WSMessage) *string { return &m.CommandApproval })
		f.normalize = enumNormalize("commandApproval", "required", "off")
		f.applyLive = func(s *Server, r *config.Config) {
			s.ws.Exec.SetCommandApproval(!strings.EqualFold(r.CommandApproval, "off"))
		}
		f.sample = "required"
		add(f)
	}

	{
		f := strSpec("commandSandbox",
			func(r *config.Config) *string { return &r.CommandSandbox },
			func(m *WSMessage) *string { return &m.CommandSandbox })
		f.normalize = enumNormalize("commandSandbox", "off", "bwrap")
		f.applyLive = func(s *Server, r *config.Config) { s.ws.Exec.SetSandbox(r.CommandSandbox) }
		f.sample = "bwrap"
		add(f)
	}

	{
		f := intSpec("commandIdleTimeoutSecs",
			func(r *config.Config) *int { return &r.CommandIdleTimeoutSecs },
			func(m *WSMessage) *int { return &m.CommandIdleTimeoutSecs })
		f.normalize = nonNegInt("commandIdleTimeoutSecs")
		f.applyLive = func(s *Server, r *config.Config) {
			s.ws.Exec.SetIdleTimeout(time.Duration(r.CommandIdleTimeoutSecs) * time.Second)
		}
		f.sample = 45
		add(f)
	}

	// --- context management ---------------------------------------------
	{
		f := intSpec("contextLimit",
			func(r *config.Config) *int { return &r.ContextLimit },
			func(m *WSMessage) *int { return &m.ContextLimitConfig })
		f.normalize = nonNegInt("contextLimit")
		f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.ContextLimit = r.ContextLimit }
		f.applyLive = contextApplyLive(f.setContext)
		f.sample = 20000
		add(f)
	}

	{
		f := fieldSpec{
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
	}

	{
		f := intSpec("compactKeepRecentMessages",
			func(r *config.Config) *int { return &r.CompactKeepRecentMessages },
			func(m *WSMessage) *int { return &m.CompactKeepRecentMessages })
		f.normalize = nonNegInt("compactKeepRecentMessages")
		f.setContext = func(next *contextmgr.Settings, r *config.Config) {
			next.CompactKeepRecentMessages = r.CompactKeepRecentMessages
		}
		f.applyLive = contextApplyLive(f.setContext)
		f.sample = 3
		add(f)
	}

	{
		f := intSpec("maxToolResultBytes",
			func(r *config.Config) *int { return &r.MaxToolResultBytes },
			func(m *WSMessage) *int { return &m.MaxToolResultBytes })
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
	}

	// Gates the spill feature: oversized tool output is persisted to the
	// session's spill dir and the inline result becomes a head/tail
	// preview + locator (on, default) instead of the legacy plain-cap
	// head-only truncation (off).
	{
		f := strSpec("outputSpill",
			func(r *config.Config) *string { return &r.OutputSpill },
			func(m *WSMessage) *string { return &m.OutputSpill })
		f.normalize = onOffNormalize("outputSpill")
		f.applyLive = func(s *Server, r *config.Config) {
			agent.ConfigureOutputSpill(onoff.Enabled(r.OutputSpill))
		}
		f.sample = "off"
		add(f)
	}

	{
		f := intSpec("compactReserveTokens",
			func(r *config.Config) *int { return &r.CompactReserveTokens },
			func(m *WSMessage) *int { return &m.CompactReserveTokens })
		f.normalize = nonNegInt("compactReserveTokens")
		f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.CompactReserveTokens = r.CompactReserveTokens }
		f.applyLive = contextApplyLive(f.setContext)
		f.sample = 1000
		add(f)
	}

	{
		f := strSpec("compactLastResort",
			func(r *config.Config) *string { return &r.CompactLastResort },
			func(m *WSMessage) *string { return &m.CompactLastResort })
		f.normalize = enumNormalize("compactLastResort", "condense", "error")
		f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.CompactLastResort = r.CompactLastResort }
		f.applyLive = contextApplyLive(f.setContext)
		f.sample = "error"
		add(f)
	}

	{
		f := strSpec("smartCompact",
			func(r *config.Config) *string { return &r.SmartCompact },
			func(m *WSMessage) *string { return &m.SmartCompact })
		f.normalize = onOffNormalize("smartCompact")
		f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.SmartCompact = onoff.Enabled(r.SmartCompact) }
		f.applyLive = contextApplyLive(f.setContext)
		f.sample = "on"
		add(f)
	}

	// The system prompt reads the mode from each session's context manager,
	// so the live update reaches the prompt and the compaction layout together.
	{
		f := strSpec("contextMode",
			func(r *config.Config) *string { return &r.ContextMode },
			func(m *WSMessage) *string { return &m.ContextMode })
		f.normalize = enumNormalize("contextMode", config.DefaultContextMode, config.ContextModeDeepDive)
		f.setContext = func(next *contextmgr.Settings, r *config.Config) { next.DeepDive = r.DeepDiveContext() }
		f.applyLive = contextApplyLive(f.setContext)
		f.sample = config.ContextModeDeepDive
		add(f)
	}

	// --- web features -----------------------------------------------------
	{
		f := strSpec("webFetch",
			func(r *config.Config) *string { return &r.WebFetch },
			func(m *WSMessage) *string { return &m.WebFetch })
		f.normalize = onOffNormalize("webFetch")
		f.applyLive = func(s *Server, r *config.Config) {
			agent.ConfigureWebFetch(onoff.Enabled(r.WebFetch), r.WebFetchMode, r.WebAllowedDomains)
		}
		f.sample = "off"
		add(f)
	}

	{
		f := strSpec("webSearch",
			func(r *config.Config) *string { return &r.WebSearch },
			func(m *WSMessage) *string { return &m.WebSearch })
		f.normalize = onOffNormalize("webSearch")
		f.applyLive = func(s *Server, r *config.Config) {
			agent.ConfigureWebSearchEnabled(onoff.Enabled(r.WebSearch))
		}
		f.sample = "off"
		add(f)
	}

	{
		f := strSpec("webSearchBackend",
			func(r *config.Config) *string { return &r.WebSearchBackend },
			func(m *WSMessage) *string { return &m.WebSearchBackend })
		f.normalize = enumNormalize("webSearchBackend", "brave", "")
		f.applyLive = func(s *Server, r *config.Config) {
			agent.ConfigureWebSearch(r.WebSearchBackend, r.WebSearchAPIKey)
		}
		f.sample = "brave"
		add(f)
	}

	{
		f := strSpec("webSearchApiKey",
			func(r *config.Config) *string { return &r.WebSearchAPIKey },
			func(m *WSMessage) *string { return &m.WebSearchAPIKey })
		// The key itself is never pushed — only the set flag.
		f.project = func(r *config.Config, m *WSMessage) { m.WebSearchAPIKeySet = r.WebSearchAPIKey != "" }
		f.msgGet = func(m *WSMessage) any { return m.WebSearchAPIKeySet }
		f.applyLive = func(s *Server, r *config.Config) {
			agent.ConfigureWebSearch(r.WebSearchBackend, r.WebSearchAPIKey)
		}
		f.sample = "key"
		add(f)
	}

	{
		f := strSpec("webAllowedDomains",
			func(r *config.Config) *string { return &r.WebAllowedDomains },
			func(m *WSMessage) *string { return &m.WebAllowedDomains })
		f.applyLive = func(s *Server, r *config.Config) {
			agent.ConfigureWebFetch(onoff.Enabled(r.WebFetch), r.WebFetchMode, r.WebAllowedDomains)
		}
		f.sample = "example.com"
		add(f)
	}

	{
		f := strSpec("webFetchMode",
			func(r *config.Config) *string { return &r.WebFetchMode },
			func(m *WSMessage) *string { return &m.WebFetchMode })
		f.applyLive = func(s *Server, r *config.Config) {
			agent.ConfigureWebFetch(onoff.Enabled(r.WebFetch), r.WebFetchMode, r.WebAllowedDomains)
		}
		f.sample = "https"
		add(f)
	}

	{
		f := strSpec("treesitter",
			func(r *config.Config) *string { return &r.TreeSitter },
			func(m *WSMessage) *string { return &m.TreeSitter })
		f.normalize = onOffNormalize("treesitter")
		f.applyLive = func(s *Server, r *config.Config) {
			treesitter.Configure(onoff.Enabled(r.TreeSitter), r.TreeSitterLangs)
		}
		f.sample = "off"
		add(f)
	}

	{
		f := strSpec("treesitterLangs",
			func(r *config.Config) *string { return &r.TreeSitterLangs },
			func(m *WSMessage) *string { return &m.TreeSitterLangs })
		f.applyLive = func(s *Server, r *config.Config) {
			treesitter.Configure(onoff.Enabled(r.TreeSitter), r.TreeSitterLangs)
		}
		f.sample = "go"
		add(f)
	}

	{
		f := strSpec("preserveReasoning",
			func(r *config.Config) *string { return &r.PreserveReasoning },
			func(m *WSMessage) *string { return &m.PreserveReasoning })
		f.applyLive = func(s *Server, r *config.Config) { s.applyPreserveReasoningToAll(r.PreserveReasoning) }
		f.sample = "on"
		add(f)
	}

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

	{
		f := strSpec("statusDisplay",
			func(r *config.Config) *string { return &r.StatusDisplay },
			func(m *WSMessage) *string { return &m.StatusDisplay })
		// off / auto (probe at startup) / on / browser (the web UI sends).
		f.normalize = enumNormalize("statusDisplay",
			config.StatusDisplayOff, config.StatusDisplayAuto, config.StatusDisplayOn, config.StatusDisplayBrowser)
		f.applyLive = applyStatusDisplay
		f.sample = "on"
		add(f)
	}

	{
		f := strSpec("statusDisplayHost",
			func(r *config.Config) *string { return &r.StatusDisplayHost },
			func(m *WSMessage) *string { return &m.StatusDisplayHost })
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
	}

	{
		f := strSpec("statusDisplayToken",
			func(r *config.Config) *string { return &r.StatusDisplayToken },
			func(m *WSMessage) *string { return &m.StatusDisplayToken })
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
	}

	{
		f := strSpec("statusDisplaySource",
			func(r *config.Config) *string { return &r.StatusDisplaySource },
			func(m *WSMessage) *string { return &m.StatusDisplaySource })
		// Empty = AUTO ("gogen-<project dir>"); keep it empty so the reporter
		// derives the name, and an explicit value always wins.
		f.normalize = func(v any) (any, string) {
			return strings.TrimSpace(v.(string)), ""
		}
		f.applyLive = applyStatusDisplay
		f.sample = "gogen-alt"
		add(f)
	}

	// --- sessions ---------------------------------------------------------
	{
		f := intSpec("sessionMaxCount",
			func(r *config.Config) *int { return &r.SessionMaxCount },
			func(m *WSMessage) *int { return &m.SessionMaxCount })
		f.normalize = nonNegInt("sessionMaxCount")
		f.applyLive = func(s *Server, r *config.Config) {
			if s.ws.Store != nil {
				s.ws.Store.SetRetention(r.SessionMaxCount, r.SessionMaxAgeDays)
				s.pruneSessions()
			}
		}
		f.sample = 4
		add(f)
	}

	{
		f := intSpec("sessionMaxAgeDays",
			func(r *config.Config) *int { return &r.SessionMaxAgeDays },
			func(m *WSMessage) *int { return &m.SessionMaxAgeDays })
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
	}

	{
		f := intSpec("webApprovalHoldSecs",
			func(r *config.Config) *int { return &r.WebApprovalHoldSecs },
			func(m *WSMessage) *int { return &m.WebApprovalHoldSecs })
		f.normalize = nonNegInt("webApprovalHoldSecs")
		f.sample = 7
		add(f)
	}

	// --- web server (restart-staged) ---------------------------------------
	{
		f := strSpec("webBind",
			func(r *config.Config) *string { return &r.WebBind },
			func(m *WSMessage) *string { return &m.WebBind })
		f.restartDisplay = "web_bind"
		f.sample = "0.0.0.0:9090"
		add(f)
	}

	{
		f := strSpec("webAllowedOrigins",
			func(r *config.Config) *string { return &r.WebAllowedOrigins },
			func(m *WSMessage) *string { return &m.WebAllowedOrigins })
		f.restartDisplay = "web_allowed_origins"
		f.sample = "example.com"
		add(f)
	}

	{
		f := strSpec("webAuthToken",
			func(r *config.Config) *string { return &r.WebAuthToken },
			func(m *WSMessage) *string { return &m.WebAuthToken })
		// The token itself is never pushed — only the set flag.
		f.project = func(r *config.Config, m *WSMessage) { m.WebAuthTokenSet = r.WebAuthToken != "" }
		f.msgGet = func(m *WSMessage) any { return m.WebAuthTokenSet }
		f.restartDisplay = "web_auth_token"
		f.forceSecrets = true
		f.sample = "token"
		add(f)
	}

	{
		f := strSpec("webTLSCertFile",
			func(r *config.Config) *string { return &r.WebTLSCertFile },
			func(m *WSMessage) *string { return &m.WebTLSCertFile })
		f.restartDisplay = "web_tls_cert_file"
		f.sample = "/tmp/cert.pem"
		add(f)
	}

	{
		f := strSpec("webTLSKeyFile",
			func(r *config.Config) *string { return &r.WebTLSKeyFile },
			func(m *WSMessage) *string { return &m.WebTLSKeyFile })
		f.restartDisplay = "web_tls_key_file"
		f.sample = "/tmp/key.pem"
		add(f)
	}

	{
		f := intSpec("webMaxActiveSessions",
			func(r *config.Config) *int { return &r.WebMaxActiveSessions },
			func(m *WSMessage) *int { return &m.WebMaxActiveSessions })
		f.normalize = nonNegInt("webMaxActiveSessions")
		f.restartDisplay = "web_max_active_sessions"
		f.sample = 2
		add(f)
	}

	{
		f := strSpec("mcpToolMode",
			func(r *config.Config) *string { return &r.MCPToolMode },
			func(m *WSMessage) *string { return &m.MCPToolMode })
		f.normalize = enumNormalize("mcpToolMode", config.MCPToolModeNested, config.MCPToolModeInline)
		// Pushed as the EFFECTIVE mode: an unset file value means the
		// default (nested), so the select is never stuck on a blank option.
		// No applyLive: the mode is part of the fixed request prefix, so a
		// change applies to new sessions only (never mid-session).
		f.project = func(r *config.Config, m *WSMessage) {
			m.MCPToolMode = r.MCPToolMode
			if strings.TrimSpace(m.MCPToolMode) == "" {
				m.MCPToolMode = config.DefaultMCPToolMode
			}
		}
		f.sample = config.MCPToolModeInline
		add(f)
	}

	{
		f := strSpec("mcp",
			func(r *config.Config) *string { return &r.MCP },
			func(m *WSMessage) *string { return &m.MCP })
		// Deliberately NOT enumNormalize: this one accepts the full onoff
		// vocabulary (on/off/true/false/yes/no/…) and stores the normalized
		// INPUT spelling rather than a canonical form, because the config-file
		// loader accepts the same vocabulary here.
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
	}

	// --- subagents ----------------------------------------------------------
	{
		f := fieldSpec{
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
	}

	// --- board auto-review agent --------------------------------------------
	{
		f := fieldSpec{
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
	}

	{
		f := fieldSpec{
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
	}

	{
		f := fieldSpec{
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
	}

	// --- prompt templates ----------------------------------------------------
	{
		f := strSpec("boardStartPrompt",
			func(r *config.Config) *string { return &r.BoardStartPrompt },
			func(m *WSMessage) *string { return &m.BoardStartPrompt })
		f.normalize = promptNormalize("boardStartPrompt", func() string { return agent.DefaultBoardStartPrompt })
		// Pushed as the RESOLVED effective template (empty config → built-in
		// default), so the modal is pre-populated with what will be used.
		f.project = func(r *config.Config, m *WSMessage) {
			m.BoardStartPrompt = agent.ResolvePromptTemplate(r.BoardStartPrompt, agent.DefaultBoardStartPrompt)
		}
		f.sample = "board tmpl {title}"
		add(f)
	}

	{
		f := strSpec("boardReviewPrompt",
			func(r *config.Config) *string { return &r.BoardReviewPrompt },
			func(m *WSMessage) *string { return &m.BoardReviewPrompt })
		f.normalize = promptNormalize("boardReviewPrompt", func() string { return agent.DefaultReviewAgentPrompt })
		// Pushed as the RESOLVED effective template (empty config → built-in
		// default), so the modal is pre-populated with what will be used.
		f.project = func(r *config.Config, m *WSMessage) {
			m.BoardReviewPrompt = agent.ResolvePromptTemplate(r.BoardReviewPrompt, agent.DefaultReviewAgentPrompt)
		}
		f.sample = "review tmpl {title}"
		add(f)
	}

	{
		f := strSpec("systemPrompt",
			func(r *config.Config) *string { return &r.SystemPrompt },
			func(m *WSMessage) *string { return &m.SystemPrompt })
		f.normalize = promptNormalize("systemPrompt", func() string { return agent.DefaultSystemPromptTemplate() })
		f.project = func(r *config.Config, m *WSMessage) {
			m.SystemPrompt = agent.ResolvePromptTemplate(r.SystemPrompt, agent.DefaultSystemPromptTemplate())
		}
		// Live for every agent (subagents included): the next turn's system
		// message resolves the configured template.
		f.applyLive = func(s *Server, r *config.Config) { agent.ConfigureSystemPrompt(r.SystemPrompt) }
		f.sample = "sys tmpl {working_dir}"
		add(f)
	}

	{
		f := strSpec("subagentPrompt",
			func(r *config.Config) *string { return &r.SubagentPrompt },
			func(m *WSMessage) *string { return &m.SubagentPrompt })
		f.normalize = promptNormalize("subagentPrompt", func() string { return agent.DefaultSubagentPrompt })
		f.project = func(r *config.Config, m *WSMessage) {
			m.SubagentPrompt = agent.ResolvePromptTemplate(r.SubagentPrompt, agent.DefaultSubagentPrompt)
		}
		// Live for every spawner (web + TUI): jobs are wrapped at spawn time.
		f.applyLive = func(s *Server, r *config.Config) { agent.ConfigureSubagentPrompt(r.SubagentPrompt) }
		f.sample = "sub tmpl {job}"
		add(f)
	}

	// --- workspace instructions / job notices --------------------------------
	{
		f := strSpec("agentInstructions",
			func(r *config.Config) *string { return &r.AgentInstructions },
			func(m *WSMessage) *string { return &m.AgentInstructions })
		f.normalize = onOffNormalize("agentInstructions")
		// Live: each session agent flips the flag and re-renders its
		// AGENTS.md/CLAUDE.md section from its CURRENT working dir (enabling
		// without the refresh would leave the section empty until the next
		// working-dir change).
		f.applyLive = func(s *Server, r *config.Config) { s.applyAgentInstructionsToAll(r) }
		f.sample = "on"
		add(f)
	}

	{
		f := strSpec("jobNotices",
			func(r *config.Config) *string { return &r.JobNotices },
			func(m *WSMessage) *string { return &m.JobNotices })
		f.normalize = onOffNormalize("jobNotices")
		// Live: install or remove every session's notice hook (the
		// deliverer is process-wide and always installed).
		f.applyLive = func(s *Server, r *config.Config) { s.applyJobNoticesToAll(r) }
		f.sample = "on"
		add(f)
	}

	{
		f := strSpec("joinQueued",
			func(r *config.Config) *string { return &r.JoinQueued },
			func(m *WSMessage) *string { return &m.JoinQueued })
		f.normalize = onOffNormalize("joinQueued")
		// Live: every runtime's drain reads the workspace flag at its next pop.
		f.applyLive = func(s *Server, r *config.Config) { s.ws.SetJoinQueued(r.JoinQueuedEnabled()) }
		f.sample = "on"
		add(f)
	}

	// --- project profile (test / lint commands) ------------------------------
	// Both override the auto-detection (DetectProjectProfile) for the
	// project; they are per-session agent state, so applyLive sweeps every
	// live session and the runtime overlay seeds sessions created later.
	{
		f := strSpec("testCommand",
			func(r *config.Config) *string { return &r.TestCommand },
			func(m *WSMessage) *string { return &m.TestCommand })
		// Trimmed only: the command line itself is free text (flags, paths),
		// and "" means "auto-detect".
		f.normalize = func(v any) (any, string) { return strings.TrimSpace(v.(string)), "" }
		f.applyLive = func(s *Server, r *config.Config) { s.applyTestLintCommandsToAll(r) }
		f.sample = "make test"
		add(f)
	}

	{
		f := strSpec("lintCommand",
			func(r *config.Config) *string { return &r.LintCommand },
			func(m *WSMessage) *string { return &m.LintCommand })
		f.normalize = func(v any) (any, string) { return strings.TrimSpace(v.(string)), "" }
		f.applyLive = func(s *Server, r *config.Config) { s.applyTestLintCommandsToAll(r) }
		f.sample = "make vet"
		add(f)
	}

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
