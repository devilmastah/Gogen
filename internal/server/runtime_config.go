package server

import (
	"fmt"
	"strings"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// runtimeConfigFields are the config option names the runtime-config branch
// accepts (client→server "config" with ConfigFields). A name in the list
// means the corresponding WSMessage value is applied — including explicit
// empty/zero values. Derived from the field registry (configFields): do not
// maintain a separate literal list.
var runtimeConfigFields = clientSettableFields()

// maxPromptTemplateLen caps the configurable prompt templates (settings
// modal). The RENDERED prompts can exceed this via ticket content; the cap
// bounds the templates themselves.
const maxPromptTemplateLen = 8192

// handleWSRuntimeConfig applies the settings-modal config options: each name
// in msg.ConfigFields names a value to apply (explicit empty/zero values are
// legal). Live-applied options update the runtime target immediately
// (executor, process globals, per-session context managers, session store)
// AND the workspace runtime overlay (source of truth for the push, new
// sessions, and persistence); restart-staged options only update the
// overlay. Any invalid value rejects the whole request; on success the
// effective config is persisted and every tab gets a fresh config push.
func (s *Server) handleWSRuntimeConfig(ws *wsConn, msg WSMessage) {
	if len(msg.ConfigFields) == 0 {
		return
	}
	seen := make(map[string]bool, len(msg.ConfigFields))
	for _, name := range msg.ConfigFields {
		if !runtimeConfigFields[name] {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: unknown config option %q", name))
			return
		}
		if seen[name] {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: config option %q listed twice", name))
			return
		}
		seen[name] = true
	}
	r := s.ws.GetRuntimeConfig()
	var restartChanged []string
	for name := range seen {
		f := configFields[name]
		raw := f.fromMsg(&msg)
		var err string
		if f.normalize != nil {
			raw, err = f.normalize(raw)
		}
		if err != "" {
			writeNoticeError(ws, "settings", err)
			return
		}
		f.set(&r, raw)
		if f.restartDisplay != "" {
			restartChanged = append(restartChanged, f.restartDisplay)
		}
	}
	s.ws.SetRuntimeConfig(r)

	// Apply to the runtime targets (live tier only). Each field's applyLive
	// closure names its target (executor, process globals, per-session
	// context managers, session store); the per-field context merge only
	// takes the fields this request named.
	for name := range seen {
		if f := configFields[name]; f.applyLive != nil {
			f.applyLive(s, &r)
		}
	}

	// Persist + broadcast off the read loop (file write + fan-out). A staged
	// web_auth_token is user-entered in the UI, so the write forces secrets
	// (0600) like provider saves — the token must survive the restart it is
	// staged for. Restart-staged changes get a success notice listing them.
	forceSecrets := false
	for name := range seen {
		if configFields[name].forceSecrets {
			forceSecrets = true
		}
	}
	s.persistAndBroadcast(nil, forceSecrets)
	if len(restartChanged) > 0 {
		writeNotice(ws, "settings", true, "Saved — restart gogen for these to take effect: "+strings.Join(restartChanged, ", "))
	}
}

// applyContextField pushes ONE live context setting (the copy closure from
// the field registry) to every live session's context manager, merging:
// only the field copy takes from r, everything else keeps the session's
// CURRENT values (each manager's SettingsSnapshot). A wholesale replace
// would clobber per-session state — most notably a restored session's
// resolved ContextLimit (set via SetContextLimit on restore, which is not a
// manual limit): changing only compact_threshold must not reset a restored
// session's limit back to provider resolution. All Manager methods are
// internally synchronized, so no turn locks are needed.
//
// The copy closure is passed in rather than looked up in configFields: a
// reference from the registry builder to a function that ranges over
// configFields would be a package-initialization cycle.
func (s *Server) applyContextField(r *config.Config, copy func(next *contextmgr.Settings, r *config.Config)) {
	for _, rt := range s.registry.runtimes() {
		if rt.agent.Context == nil {
			continue
		}
		// UpdateSettingsFunc merges against the CURRENT settings under one
		// lock, so two concurrent single-field pushes for the same session
		// cannot interleave and silently revert each other (the old
		// snapshot-then-update pair was not atomic).
		rt.agent.Context.UpdateSettingsFunc(func(cur contextmgr.Settings) contextmgr.Settings {
			copy(&cur, r)
			return cur
		})
	}
}

// applyAgentInstructionsToAll pushes the agent_instructions flag to every
// live session agent. Enabling also RE-RENDERS the section from the agent's
// current working dir (the render is skipped while the flag is off, so
// flipping the flag alone would leave the section empty), and disabling
// makes EffectiveGuidelines return the project guidelines alone.
func (s *Server) applyAgentInstructionsToAll(r *config.Config) {
	on := r.AgentInstructionsEnabled()
	for _, rt := range s.registry.runtimes() {
		rt.agent.SetInstructionsEnabled(on)
		if on {
			// Snapshot accessor: this runs off the turn goroutine while a
			// working-dir change may be publishing the field.
			rt.agent.RefreshWorkspaceInstructions(rt.agent.WorkingDirSnapshot())
		}
	}
}

// applyJobNoticesToAll installs or removes every live session's job-notice
// hook: with the feature on, a background job finishing naturally delivers
// its summary into that session (which starts a turn on it); with it off the
// hook is cleared, so a finished job stays silent. The deliverer itself is
// installed once by NewServer regardless of the flag.
func (s *Server) applyJobNoticesToAll(r *config.Config) {
	on := r.JobNoticesEnabled()
	s.ws.SetJobNotices(on)
	deliverer := s.ws.jobNoticeDeliverer
	for _, rt := range s.registry.runtimes() {
		if !on || deliverer == nil {
			rt.agent.SetJobNoticeHook(nil)
			continue
		}
		sid := rt.agent.SessionID
		rt.agent.SetJobNoticeHook(func(summary string) { deliverer(sid, summary) })
	}
}

// applyTestLintCommandsToAll pushes the configured test/lint command
// overrides to every live session agent. Each agent drops its cached project
// profile, so the next turn re-detects the project profile with the new
// overrides; sessions created afterwards seed them from the runtime overlay.
// No turn locks: the agent stores both under its stats lock.
func (s *Server) applyTestLintCommandsToAll(r *config.Config) {
	testCommand, lintCommand := r.TestCommand, r.LintCommand
	for _, rt := range s.registry.runtimes() {
		rt.agent.SetTestLintCommands(testCommand, lintCommand)
	}
}

// applyPreserveReasoningToAll pushes the preserve_reasoning mode to every
// live session's OpenAI provider.
func (s *Server) applyPreserveReasoningToAll(mode string) {
	for _, rt := range s.registry.runtimes() {
		if p, ok := rt.agent.Provider.(*llm.OpenAIProvider); ok {
			p.SetPreserveReasoningMode(mode)
		}
	}
}

// restartPendingFields lists restart-staged settings whose staged value
// differs from the running startup config (the modal's "restart to take
// effect" banner).
func (s *Server) restartPendingFields() []string {
	if s.config == nil {
		return nil
	}
	r := s.ws.GetRuntimeConfig()
	var out []string
	pick := func(name, staged, running string) {
		if staged != running {
			out = append(out, name)
		}
	}
	pick("web_bind", r.WebBind, s.config.WebBind)
	pick("web_allowed_origins", r.WebAllowedOrigins, s.config.WebAllowedOrigins)
	pick("web_auth_token", r.WebAuthToken, s.config.WebAuthToken)
	pick("web_tls_cert_file", r.WebTLSCertFile, s.config.WebTLSCertFile)
	pick("web_tls_key_file", r.WebTLSKeyFile, s.config.WebTLSKeyFile)
	pick("mcp", r.MCP, s.config.MCP)
	if r.WebMaxActiveSessions != s.config.WebMaxActiveSessions {
		out = append(out, "web_max_active_sessions")
	}
	return out
}
