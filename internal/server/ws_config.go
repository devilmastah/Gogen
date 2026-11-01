package server

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/onoff"
	"gogen/internal/projectfile"
)

// agentConfigMsgBasic returns config fields that are cheap to read for the
// given session agent. Every field is internally synchronized (executor,
// provider, statsMu for mode/thinking/label), so it is safe WITHOUT the
// session's turnMu — the attach handshake must never block on a running
// turn, which holds turnMu for its entire duration. Do not call ContextStats
// while holding turnMu — tokenize after unlocking via applyContextStats.
func agentConfigMsgBasic(a *agent.Agent) WSMessage {
	mode, thinking := a.ModeAndThinkingLevel()
	msg := WSMessage{
		Type:       "config",
		WorkingDir: a.Executor.GetWorkingDir(),
		Model:      a.CurrentModel(),
		// The owning profile travels with the model: model IDs are not
		// unique across registered providers, so the client's current-model
		// check must match (provider, id), not the bare id.
		ModelProvider: a.CurrentModelRef().ProfileName(),
		Mode:          mode.String(),
		ThinkingLevel: string(thinking),
		GlobalMode:    a.GlobalMode,
		SessionID:     a.SessionID,
		SessionLabel:  a.SessionLabelSnapshot(),
	}
	// Reasoning-effort options and description for the current model
	// (in-memory lookups, never block), so the client can render the
	// per-model chips and a hover tooltip.
	msg.ReasoningEfforts = a.CurrentModelEfforts()
	msg.ReasoningEffortsUnsupported = reasoningEffortsUnsupported(a)
	msg.ModelDescription = a.CurrentModelDescription()
	// Live feature flags: the settings modal renders and toggles these.
	msg.Board = onOff(a.BoardEnabled())
	msg.Subagent = onOff(a.SubagentsEnabled())
	msg.Skills = onOff(a.SkillsEnabled())
	msg.Terminal = onOff(a.TerminalEnabled())
	msg.CodeIndex = onOff(a.CodeIndexEnabled())
	if ix := a.CodeIndex(); ix != nil {
		snap := ix.Snapshot()
		msg.CodeIndexLine = snap.Line()
		msg.CodeIndexPhase = string(snap.Phase)
		msg.CodeIndexSeq = snap.Seq
	}
	msg.ReviewAgent = onOff(a.ReviewAgentEnabled())
	msg.Automations = onOff(a.AutomationsEnabled())
	msg.SubagentMaxDepth = a.SubagentMaxDepth()
	return msg
}

// onOff renders a boolean as the config-WS "on"/"off" spelling (the shared
// canonical form, see onoff.Format).
func onOff(v bool) string {
	return onoff.Format(v)
}

// reasoningEffortsUnsupported reports whether the session's current model
// definitively has no reasoning-effort control (a known models.dev entry with
// no effort options, or a llama.cpp capability probe that reported no
// support) — the client hides the thinking chips in that case. In-memory
// lookup, never blocks.
func reasoningEffortsUnsupported(a *agent.Agent) bool {
	if a == nil || a.Provider == nil {
		return false
	}
	if p, ok := a.Provider.(*llm.OpenAIProvider); ok {
		return p.ReasoningEffortUnsupported(a.CurrentModelRef())
	}
	return false
}

// pushConfigForAgent broadcasts a fresh config snapshot for a session agent
// to its attached clients. Used after background model validation so a
// client that attached before validation completed does not keep showing a
// stale model (one that was cleared or auto-selected by the validation).
// Safe without turnMu: agentConfigMsgBasic is internally synchronized (see
// its contract above).
func (s *Server) pushConfigForAgent(a *agent.Agent) {
	if a == nil {
		return
	}
	if rt, ok := s.registry.get(a.SessionID); ok {
		msg := agentConfigMsgBasic(a)
		s.decorateConfig(&msg)
		rt.broadcast(msg)
	}
}

// maybeProbeReasoningEfforts derives the session model's accepted
// reasoning-effort values from a llama.cpp /props (+ /apply-template)
// capability probe and pushes a fresh config when the derived set differs
// from what clients are showing (the initial config echo carries the
// fallback set). Runs in its own goroutine — bounded network I/O; the
// provider caches per model, so repeat triggers are no-ops.
func (s *Server) maybeProbeReasoningEfforts(ctx context.Context, a *agent.Agent) {
	if a == nil || a.Provider == nil {
		return
	}
	p, ok := a.Provider.(*llm.OpenAIProvider)
	if !ok {
		return
	}
	go func() {
		// The session's routing key, not the bare ID: the probe must target
		// the endpoint that actually serves the model (a model another
		// provider also lists must not be probed on the default endpoint).
		changed, err := p.ProbeReasoningEfforts(ctx, a.CurrentModelRef())
		if err != nil {
			return // keep the fallback set; a later trigger retries
		}
		if changed {
			s.pushConfigForAgent(a)
		}
	}()
}

// agentConfigMsg is an internally-synchronized basic snapshot plus
// ContextStats applied outside any lock (ContextStats snapshots under its own
// statsMu). No turnMu is taken — callers may hold it (session command echoes)
// or not (the attach handshake, which must never block on a running turn).
func agentConfigMsg(ctx context.Context, rt *sessionRuntime) WSMessage {
	a := rt.agent
	msg := agentConfigMsgBasic(a)
	fillModelPricing(a, &msg)
	accum := a.SnapshotUsageAccum()
	applyContextStats(&msg, a.ContextStats(ctx), &accum)
	return msg
}

// echoConfigOffLoop applies context stats to cfg and writes the config echo
// off the read loop: ContextStats tokenization can take seconds on a large
// uncached session, and the read loop serializes every message on the
// connection (including cancel).
//
// The echo goes to the acting connection AND to every other client attached to
// the same session: mode / model / thinking level are per-SESSION state that
// every attached client renders (toolbar mirrors), so leaving the others on
// the previous value shows a tab a state the session does not have. The mode
// case is not cosmetic — the model's tool list follows the SERVER's mode, and
// a stale picker makes clicking the mode the user believes is active a no-op
// (see wsHandleSetMode).
func echoConfigOffLoop(rt *sessionRuntime, ws *wsConn, ctx context.Context, a *agent.Agent, cfg *WSMessage) {
	echoConfigReply(rt, ws, ctx, a, cfg, nil, false)
}

// echoConfigReply is the one "finish a config-changing operation" path: off
// the read loop it applies the context stats to cfg, writes it to the acting
// connection, broadcasts it to the session's other clients, and finally writes
// reply (when non-nil) on the acting connection.
//
// cfg may be nil, which skips the config frame (a failed model selection
// changes nothing and must not pay for a tokenization pass). reply carries the
// command's textual answer, so it is written from the SAME goroutine after the
// config frame — the send queue is FIFO, so the client sees state before text.
// stampReply copies the post-stats model identity and usage counters into
// reply, for a reply that reports them (the /models command's answer); the
// fields only exist once the stats are applied, so it cannot happen earlier.
//
// Every handler that mutates per-session state and echoes config goes through
// here: the blocks it replaces each hand-rolled "goroutine, apply stats, write,
// maybe write response, maybe broadcast", and they disagreed about the
// broadcast — the /mode, /think and /models command paths updated only the
// acting connection while the picker handlers updated every attached client,
// leaving a second tab on a stale picker (see echoConfigOffLoop for why that
// matters for mode).
func echoConfigReply(rt *sessionRuntime, ws *wsConn, ctx context.Context, a *agent.Agent, cfg *WSMessage, reply *WSMessage, stampReply bool) {
	go func() {
		if cfg != nil {
			accum := a.SnapshotUsageAccum()
			applyContextStats(cfg, a.ContextStats(ctx), &accum)
			_ = ws.writeJSON(*cfg)
			if rt != nil {
				rt.broadcastExcept(ws, *cfg)
			}
		}
		if reply != nil {
			if stampReply && cfg != nil {
				reply.Model = cfg.Model
				reply.ContextLimit = cfg.ContextLimit
				reply.UsedTokens = cfg.UsedTokens
				reply.UsedSource = cfg.UsedSource
				reply.UsedPercent = cfg.UsedPercent
			}
			_ = ws.writeJSON(*reply)
		}
	}()
}

// withTurnConfig runs the shared "take the turn, mutate, echo" choreography
// behind the picker handlers (set model / mode / thinking level): acquire the
// session's turn, let mutate change per-session state, build the config frame
// from the post-mutation agent, release, then echo it off the read loop.
//
// mutate reports whether it changed anything: an invalid value (an
// unrecognized mode) is a no-op, and projecting costs an off-loop
// tokenization pass, so nothing is echoed then. A mutation error also skips
// the echo — the model did not change, and the caller reports the error.
//
// acquired=false means another turn holds the lock; the caller decides how to
// report the busy rejection (every current caller sends a settings notice).
// The frame order the three handlers used to have inline — model filled
// pricing before release, thinking level after, mode never — is now fixed:
// pricing and decoration happen for all of them, before the release.
func (s *Server) withTurnConfig(rt *sessionRuntime, ws *wsConn, ctx context.Context,
	mutate func(a *agent.Agent) (changed bool, err error)) (acquired, changed bool, err error) {
	if !rt.acquireTurnForHandler(ws) {
		return false, false, nil // acquired=false is the busy signal
	}
	a := rt.agent
	changed, err = mutate(a)
	var cfg WSMessage
	if err == nil && changed {
		cfg = agentConfigMsgBasic(a)
		fillModelPricing(a, &cfg)
		s.decorateConfig(&cfg)
	}
	rt.releaseTurn()
	if err == nil && changed {
		echoConfigOffLoop(rt, ws, ctx, a, &cfg)
	}
	return true, changed, err
}

// fillModelPricing looks up pricing for the current model from the models.dev
// registry cache (never blocks — pure map lookup).
func fillModelPricing(a *agent.Agent, msg *WSMessage) {
	if p, ok := a.Provider.(*llm.OpenAIProvider); ok && msg.Model != "" {
		if in, out, cached, ok := p.ModelPricing(a.CurrentModelRef()); ok {
			msg.InputPricePer1M = in
			msg.OutputPricePer1M = out
			msg.CachedPricePer1M = cached
		}
	}
}

// isValidThinkingLevel reports whether v is a valid reasoning-effort selection
// for the session's current model: ""/"off" are always valid (omit), and any
// other value is valid only when it is in the model's effective accepted set
// (models.dev when known, DefaultReasoningEfforts otherwise). Providers
// without effort reporting (test stubs) accept any non-blank value.
func (s *Server) isValidThinkingLevel(a *agent.Agent, v string) bool {
	level := agent.NormalizeThinkingLevel(v)
	if level == "" || level == agent.ThinkingOff {
		return true // omit
	}
	if p, ok := a.Provider.(llm.ReasoningEffortsProvider); ok {
		// Membership check against the normalized value ("Max" → "max").
		return slices.Contains(p.ModelReasoningEfforts(a.CurrentModelRef()), string(level))
	}
	return true
}

func (s *Server) handleWSConfig(ws *wsConn, ctx context.Context, pane **sessionRuntime, msg WSMessage) {
	// The config message carries independent settings. The working-dir
	// branch keeps its historical global-mode gate; the feature-flag
	// branches (Board / Subagent / SubagentMaxDepth) are project settings
	// and work in ANY mode — the whole point of the settings modal; the
	// runtime-config branch (ConfigFields) applies the settings-modal
	// options (live or restart-staged, see handleWSRuntimeConfig).
	if msg.WorkingDir != "" {
		s.handleWSWorkingDir(ws, ctx, pane, msg)
	}
	if msg.Board != "" || msg.Subagent != "" || msg.Skills != "" || msg.Terminal != "" || msg.CodeIndex != "" || msg.ReviewAgent != "" || msg.Automations != "" || msg.SubagentMaxDepth != 0 || msg.SubagentMaxConcurrent != 0 {
		s.handleWSFeatureFlags(ws, msg)
	}
	if len(msg.ConfigFields) > 0 {
		s.handleWSRuntimeConfig(ws, msg)
	}
}

// handleWSFeatureFlags handles the Board / Subagent / Skills / Terminal /
// ReviewAgent / Automations / SubagentMaxDepth / SubagentMaxConcurrent
// branches of the config message (the live settings-modal toggles). Any
// invalid value rejects the whole request with an error reply; on success
// the workspace flags are written to the shared store every session agent
// reads (visible to all sessions immediately), the board manager is pushed
// to every live session agent, the effective config is persisted
// (durability), and a fresh config push is broadcast to all clients so
// every tab updates instantly.
func (s *Server) handleWSFeatureFlags(ws *wsConn, msg WSMessage) {
	var board, boardSet bool
	var subagent, subagentSet bool
	if msg.Board != "" {
		var ok bool
		board, ok = onoff.Parse(msg.Board)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid board value %q (want on or off)", msg.Board))
			return
		}
		boardSet = true
	}
	if msg.Subagent != "" {
		var ok bool
		subagent, ok = onoff.Parse(msg.Subagent)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid subagent value %q (want on or off)", msg.Subagent))
			return
		}
		subagentSet = true
	}
	var skills, skillsSet bool
	if msg.Skills != "" {
		var ok bool
		skills, ok = onoff.Parse(msg.Skills)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid skills value %q (want on or off)", msg.Skills))
			return
		}
		skillsSet = true
	}
	var codeIndex, codeIndexSet bool
	if msg.CodeIndex != "" {
		var ok bool
		codeIndex, ok = onoff.Parse(msg.CodeIndex)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid codeIndex value %q (want on or off)", msg.CodeIndex))
			return
		}
		codeIndexSet = true
	}
	var terminal, terminalSet bool
	if msg.Terminal != "" {
		var ok bool
		terminal, ok = onoff.Parse(msg.Terminal)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid terminal value %q (want on or off)", msg.Terminal))
			return
		}
		terminalSet = true
	}
	var reviewAgent bool
	var reviewAgentSet bool
	if msg.ReviewAgent != "" {
		var ok bool
		reviewAgent, ok = onoff.Parse(msg.ReviewAgent)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid reviewAgent value %q (want on or off)", msg.ReviewAgent))
			return
		}
		reviewAgentSet = true
	}
	var automations bool
	var automationsSet bool
	if msg.Automations != "" {
		var ok bool
		automations, ok = onoff.Parse(msg.Automations)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid automations value %q (want on or off)", msg.Automations))
			return
		}
		automationsSet = true
	}
	if msg.SubagentMaxDepth < 0 {
		writeNoticeError(ws, "settings", "Error: subagentMaxDepth must be >= 0")
		return
	}
	if msg.SubagentMaxConcurrent < 0 {
		writeNoticeError(ws, "settings", "Error: subagentMaxConcurrent must be >= 0")
		return
	}
	if boardSet {
		s.ws.SetBoardEnabled(board)
	}
	if subagentSet {
		s.ws.SetSubagentEnabled(subagent)
	}
	if skillsSet {
		s.ws.SetSkillsEnabled(skills)
	}
	if terminalSet {
		s.ws.SetTerminalEnabled(terminal)
	}
	if codeIndexSet {
		s.ws.SetCodeIndexEnabled(codeIndex)
		s.ws.SyncCodeIndexWatch()
	}
	if reviewAgentSet {
		s.ws.SetReviewAgentEnabled(reviewAgent)
	}
	if automationsSet {
		s.ws.SetAutomationsEnabled(automations)
		// The scheduler follows the flag live: on = sweep the global store
		// (or restart it), off = stop sweeping. The in-flight headless
		// children are unaffected (they are independent processes).
		if automations {
			s.startAutomations()
		} else {
			s.stopAutomations()
		}
	}
	if msg.SubagentMaxDepth > 0 {
		s.ws.SetSubagentMaxDepth(msg.SubagentMaxDepth)
	}
	if msg.SubagentMaxConcurrent > 0 {
		s.ws.SetSubagentMaxConcurrent(msg.SubagentMaxConcurrent)
	}
	// Board-manager push + persist + broadcast OFF the read loop: the
	// persistence is a file write and the broadcast fans out to every
	// attached socket. The flag writes above already reached every session
	// (shared store); only the board manager pointer needs pushing.
	s.persistAndBroadcast(s.applyBoardManagerToAll, false)
}

// persistAndBroadcast writes the effective config and pushes a fresh one to
// every tab, OFF the read loop: the persistence is a file write and the
// broadcast fans out to every attached socket. pre (optional) runs inside the
// goroutine before the write, for handlers that must also sweep live state.
//
// The goroutine is tracked by persistWg so teardown can join it: it outlives
// the handler that launched it, and a write still in flight when a caller
// removes the config directory (tests) fails the rename or, worse, races
// t.TempDir's RemoveAll ("directory not empty").
func (s *Server) persistAndBroadcast(pre func(), forceSecrets bool) {
	s.persistWg.Add(1)
	go func() {
		defer s.persistWg.Done()
		if pre != nil {
			pre()
		}
		if s.config != nil {
			if forceSecrets {
				s.persistConfigForced(s.effectiveConfig())
			} else {
				s.persistConfig(s.effectiveConfig())
			}
		}
		s.broadcastConfigAll()
	}()
}

// waitPersistsWithin waits up to d for the in-flight persist+broadcast
// goroutines to finish, reporting whether they all did. Teardown paths (the
// test drain) use it so a half-finished config write cannot outlive the
// directory it writes into.
func (s *Server) waitPersistsWithin(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.persistWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// effectiveConfig returns the config snapshot used for persistence: the
// startup config with every live-mutable value overlaid (feature flags,
// registered provider list, runtime overlay incl. restart-staged settings).
// A later single-field persist must never revert an earlier live change.
// Returns nil when the server has no startup config (tests).
func (s *Server) effectiveConfig() *config.Config {
	if s == nil || s.config == nil {
		return nil
	}
	out := *s.config
	r := s.ws.GetRuntimeConfig()
	// Live-adjustable fields: the runtime overlay wins (it is seeded from
	// the startup config, so unchanged fields persist their original
	// values). Every overlay field is a registry entry — a new field
	// cannot be forgotten here.
	for _, f := range configFields {
		f.set(&out, f.get(&r))
	}
	// Feature flags + provider list live in their own workspace stores.
	out.Board = onOff(s.ws.GetBoardEnabled())
	out.Subagent = onOff(s.ws.GetSubagentEnabled())
	out.Skills = onOff(s.ws.GetSkillsEnabled())
	out.Terminal = onOff(s.ws.GetTerminalEnabled())
	out.CodeIndex = onOff(s.ws.GetCodeIndexEnabled())
	out.ReviewAgent = onOff(s.ws.GetReviewAgentEnabled())
	out.Automations = onOff(s.ws.GetAutomationsEnabled())
	out.SubagentMaxDepth = s.ws.GetSubagentMaxDepth()
	out.SubagentMaxConcurrent = s.ws.GetSubagentMaxConcurrent()
	out.OpenAIProviders = s.ws.GetOpenAIProviders()
	return &out
}

// applyBoardManagerToAll pushes the workspace's shared board manager to
// every live session agent so claims/moves/NextID serialize in-process.
// The feature flags themselves need no sweep: every session agent reads
// the workspace's single FeatureFlags store (SetFeatureFlags at spawn
// time), so a settings toggle is visible to all sessions immediately.
// Enabling the board creates the shared manager (data from a previous
// enable persists; disabling keeps it so re-enabling restores the board).
func (s *Server) applyBoardManagerToAll() {
	var bm *agent.BoardManager
	if s.ws.GetBoardEnabled() {
		bm = s.ws.ensureBoardManager()
	}
	// The review hook rides on the (possibly just-created) manager.
	s.installReviewAgent()
	for _, rt := range s.registry.runtimes() {
		rt.agent.SetBoardManager(bm)
	}
}

// broadcastConfigAll pushes a fresh config snapshot to every attached client
// of every live session (the settings modal syncs across tabs from these).
func (s *Server) broadcastConfigAll() {
	for _, rt := range s.registry.runtimes() {
		msg := agentConfigMsgBasic(rt.agent)
		s.decorateConfig(&msg)
		rt.broadcast(msg)
	}
}

// decorateConfig fills the workspace-level config fields on a config
// message: the registered provider list (never the keys), the config file
// path the storage warning renders, the live runtime-config values the
// settings modal displays, and the restart-pending list for the banner.
// Cheap accessor reads; safe without the session turn lock.
func (s *Server) decorateConfig(msg *WSMessage) {
	if s == nil || msg == nil {
		return
	}
	msg.Providers = s.providerEntries()
	msg.ConfigFilePath = s.configFilePath()
	r := s.ws.GetRuntimeConfig()
	// Every runtime field's client projection is a registry entry (secrets
	// as set-flags, prompts resolved to the effective template, subagent
	// model/level as always-present pointers) — a new field cannot drift
	// out of the push.
	for _, f := range configFields {
		if f.project != nil {
			f.project(&r, msg)
		}
	}
	// Feature flags live in their own workspace store, not the overlay.
	msg.SubagentMaxConcurrent = s.ws.GetSubagentMaxConcurrent()
	msg.MCPServers = s.mcpEntries()
	msg.RestartRequired = s.restartPendingFields()
}

// providerEntries projects the registered provider list for the client: the
// implicit default profile (built from the legacy config fields, live-
// editable but not deletable) followed by the live additional providers.
// Keys are never pushed — only the apiKeySet flag.
func (s *Server) providerEntries() []ProviderEntry {
	out := make([]ProviderEntry, 0, 1+len(s.ws.GetOpenAIProviders()))
	r := s.ws.GetRuntimeConfig()
	def := ProviderEntry{
		Name:      "default",
		BaseURL:   r.OpenAIURL,
		Model:     r.OpenAIModel,
		APIKeySet: r.OpenAIKey != "",
	}
	out = append(out, def)
	for _, p := range s.ws.GetOpenAIProviders() {
		out = append(out, ProviderEntry{
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			Model:     p.Model,
			APIKeySet: p.APIKey != "",
			Deletable: true,
		})
	}
	return out
}

// configFilePath returns where the effective config is persisted: the
// project .gogen/gogen.conf in project mode, the global config file in
// global mode. Drives the provider-key storage warning in the settings UI.
func (s *Server) configFilePath() string {
	if s.ws.GlobalMode {
		return projectfile.GlobalConfigPath()
	}
	return projectfile.DefaultSavePath(s.ws.GetWorkingDir())
}

// persistConfig writes the effective config so a live toggle survives a
// restart. Project mode writes .gogen/gogen.conf; global mode writes the
// global config file. The write is best-effort (log on failure) — the live
// toggle is already applied to the running process.
//
// Secrets (openai_api_key, MCP server env) are preserved when the EXISTING
// file already contains them: the toggle rewrite must never drop a key the
// user stored in the file (IncludeSecrets=false would rewrite the file
// without it). Keys that only ever came from the environment stay out of
// the file, exactly as before.
func (s *Server) persistConfig(cfg *config.Config) {
	// One writer at a time: each write is temp-file + rename, so two
	// concurrent persists race on the target (a benign "no such file" on
	// POSIX, ACCESS_DENIED on Windows).
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	var err error
	if s.ws.GlobalMode {
		err = projectfile.SaveGlobalConfig(cfg, projectfile.WriteOptions{
			IncludeSecrets: projectfile.ConfigFileHasSecrets(projectfile.GlobalConfigPath()),
		})
	} else {
		path := projectfile.DefaultSavePath(s.ws.GetWorkingDir())
		includeSecrets := projectfile.ConfigFileHasSecrets(path)
		if !includeSecrets {
			// The user's config may live in a .md front matter with no
			// .gogen/gogen.conf yet: creating a key-less .conf here would
			// shadow the .md's key (a .conf takes precedence on load).
			if cfgPath, ok := projectfile.DiscoverConfigPath(s.ws.GetWorkingDir()); ok {
				includeSecrets = projectfile.ConfigFileHasSecrets(cfgPath)
			}
		}
		err = projectfile.SaveConfig(path, "", cfg, "", projectfile.WriteOptions{
			IncludeSecrets: includeSecrets,
		})
	}
	if err != nil {
		log.Printf("config save failed: %v", err)
	}
}

// persistConfigForced writes the effective config with secrets forced on.
// Provider saves through the UI always persist their API keys (the user
// entered them explicitly and expects them stored); projectfile writes the
// file 0600 in that case. Side effect: any legacy openai_api_key that only
// came from the environment is also persisted on this write — accepted,
// since the user just opted into storing provider keys.
func (s *Server) persistConfigForced(cfg *config.Config) {
	if s == nil || s.ws == nil || cfg == nil {
		return
	}
	// Serialized with persistConfig (see the persistMu comment).
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	var err error
	if s.ws.GlobalMode {
		err = projectfile.SaveGlobalConfig(cfg, projectfile.WriteOptions{IncludeSecrets: true})
	} else {
		path := projectfile.DefaultSavePath(s.ws.GetWorkingDir())
		err = projectfile.SaveConfig(path, "", cfg, "", projectfile.WriteOptions{IncludeSecrets: true})
	}
	if err != nil {
		log.Printf("config save failed: %v", err)
	}
}
