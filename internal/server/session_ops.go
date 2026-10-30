package server

// Session lifecycle as registry operations.
// session_new/resume/fork/delete create, restore, clone, and evict live
// session runtimes instead of mutating one agent — the source session of a
// fork is untouched, resume dedupes against active sessions, and
// delete drains + evicts before removing the file.

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/session"
	"gogen/internal/steerq"
	"gogen/internal/subagents"
)

// runSessionCommand handles the session slash commands (/new, /sessions,
// /resume, /fork, resume del) typed in a pane, routing them through the
// registry instead of mutating one agent. pane is a pointer to the
// connection's current session runtime so lifecycle ops can switch the pane
// (D3: /new replaces that pane's session); ws is the requesting connection.
// The result mirrors agent.SessionCommandResult so writeSessionCommandResult
// keeps working unchanged. Returns handled=false for non-session commands.
func (s *Server) runSessionCommand(ctx context.Context, ws *wsConn, pane **sessionRuntime, input string) (agent.SessionCommandResult, bool, error) {
	cmd, args := agent.ParseSessionCommand(input)
	switch cmd {
	case "new":
		return s.sessionNew(ws, pane)
	case "sessions":
		a := (*pane).agent
		out, list, err := a.FormatSessionListForUI()
		if err != nil {
			return agent.SessionCommandResult{}, true, err
		}
		return agent.SessionCommandResult{Output: out, Sessions: list}, true, nil
	case "resume":
		if delID, ok, err := agent.ParseResumeDelArg(args); ok || err != nil {
			if err != nil {
				return agent.SessionCommandResult{}, true, err
			}
			return s.sessionDelete(ctx, ws, pane, delID)
		}
		return s.sessionResume(ctx, ws, pane, args)
	case "fork":
		return s.sessionFork(ctx, ws, pane, args)
	}
	return agent.SessionCommandResult{}, false, nil
}

// ensureSessionRuntime returns the live runtime for id, loading the session
// from the store (via the workspace factory) when it is not currently active.
// This is what session_attach uses so the sidebar can open saved sessions as
// panes. Returns an error when the session does not exist on disk either.
func (s *Server) ensureSessionRuntime(id string) (*sessionRuntime, error) {
	rt, err := s.loadOrCreateRuntime(id)
	if err != nil {
		return nil, err
	}
	// Attaching a session counts as activity even when it is already
	// registered — the sidebar session list re-attaches on every focus. Move
	// it to the front of the registration order so registry-cap eviction
	// targets the least-recently-attached idle session, never the
	// pane the client just opened, and messages without an explicit
	// sessionId (an id-less pane's toolbar action, legacy clients) route
	// to the session the user is actually on. Pre-fix, the early return
	// skipped setDefault and the default stayed on the last session_new/
	// resume/fork target — a stale default could cancel the WRONG pane's
	// running turn via the cancel-then-lock handlers.
	s.registry.setDefault(id)
	return rt, nil
}

// loadOrCreateRuntime returns the live runtime for id, loading the session
// from the store (via the workspace factory) when it is not currently
// active. The load/register/dedupe-recheck sequence is shared by
// ensureSessionRuntime, sessionResume, and createBootstrapSession so the
// three cannot drift.
func (s *Server) loadOrCreateRuntime(id string) (*sessionRuntime, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if rt, ok := s.registry.get(id); ok {
		return rt, nil
	}
	if s.ws.Store == nil {
		return nil, fmt.Errorf("session persistence disabled")
	}
	snap, err := s.ws.Store.LoadInWorkingDir(s.ws.GetWorkingDir(), id)
	if err != nil {
		return nil, err
	}
	a := s.ws.NewSessionAgent(&snap, id)
	rt := s.newSessionRuntimeFor(a)
	// A re-attached nested (subagent) child restores its runtime-level
	// parent link and privileges: the nested flag re-arms the
	// active-session cap exemption, and the D6 delete-approval override
	// routes to the parent's clients when the child has none attached.
	// The parent is resolved by id at approval time (it may not be live,
	// or may be a fresh runtime, when the child is reopened) and an
	// approval nobody can answer is denied instead of parked forever —
	// see nestedDeleteApprover. Without this, a child reopened after
	// eviction/restart could be cap-evicted mid-task and headless delete
	// approvals would hang instead of reaching the parent.
	if p := a.ParentID(); p != "" {
		rt.parentID = p
		rt.nested = true
		rt.approverOverride = s.registry.nestedDeleteApprover(rt, p)
		rt.commandApproverOverride = s.registry.nestedCommandApprover(rt, p)
	}
	s.registry.register(id, rt)
	// A concurrent attach (or resume) of the same session may have
	// registered a runtime between our get above and this register —
	// register dedupes and would then be a no-op. Re-read the registry
	// so the caller never receives an UNREGISTERED orphan runtime: two live
	// agents with the same SessionID would both persist to the same file
	// (last writer wins), and the orphan would be invisible to
	// delete/prune/shutdown (its in-memory turn state could be lost or its
	// file pruned out from under it).
	if registered, ok := s.registry.get(id); ok {
		rt = registered
	}
	// Model validation is spawned by the runtime owner (not by
	// NewSessionAgent): the async result must push a config refresh to the
	// session's clients, which requires the registered runtime. When a
	// concurrent attach won the registration race above, this agent is an
	// unregistered orphan — the winner validates its own agent.
	if rt.agent == a && snap.Model != "" {
		s.validateModelAsync(rt, snap.Model)
	}
	return rt, nil
}

// validateModelAsync confirms rt's model against the provider in the
// background and pushes the result to the session's clients. The install
// hook + spawn mechanics are Agent.ValidateRestoredModelAsync (the single
// wiring point for that contract across all hosts); this wrapper remains
// the web wiring point for loadOrCreateRuntime, createNewSession, and
// sessionFork: the hook must be installed on the REGISTERED runtime's agent
// (the config refresh targets that session's clients), and the caller
// decides the guard — whether the model came from a restored snapshot
// (snap.Model != ""), was inherited from the pane (modelInherited), or the
// agent is the registration winner (rt.agent == a).
func (s *Server) validateModelAsync(rt *sessionRuntime, model string) {
	a := rt.agent
	a.ValidateRestoredModelAsync(model, func() { s.pushConfigForAgent(a) })
}

// switchPane points the connection's current pane at rt and attaches the
// socket to rt's session. It deliberately does NOT detach the previous pane:
// attachment is client-managed via session_attach/session_detach, and a
// background pane must keep receiving its session's events. The client
// releases the old session's attachment with session_detach when a pane
// re-keys (typed /new, /resume, fork) and with session_close when the user
// closes a pane (✕ — the server cancels the turn and unregisters the
// runtime); a killed tab self-heals via lazy detach-on-write-failure
// (E26/E32).
//
// It returns false when the attach was refused — rt is closing (a claimed
// session_close) or already evicted — in which case the pane pointer is NOT
// moved to a runtime that is leaving memory; the caller reports
// session_detached (notifySessionDetached).
func (s *Server) switchPane(ws *wsConn, pane **sessionRuntime, rt *sessionRuntime) bool {
	if !rt.attach(ws) {
		return false
	}
	*pane = rt
	return true
}

// notifySessionDetached reports a refused attach: the runtime the client
// tried to attach to left the registry (a claimed session_close the attach
// raced, or a cap/orphan eviction). The session is saved, not deleted — the
// client closes the pane like a removal (handleSessionDetached in app.js)
// and can reopen it from the saved list, which reloads it from the store.
func (s *Server) notifySessionDetached(ws *wsConn, rt *sessionRuntime) {
	_ = ws.writeJSON(WSMessage{Type: "session_detached", SessionID: rt.agent.SessionID})
}

// pruneSessions invokes the store's explicit prune with the full active set
// the registry is the sole pruner in web mode (auto-prune is disabled
// in NewServer), so a Save from one session can never delete another live
// session's file. It is only called where the store actually grows (session
// create / fork persist new files) or shrinks (session delete): attach and
// resume add no files, and pruning there would delete the oldest saved
// sessions (retention cap) merely because a client opened a pane — including
// sessions the user is about to switch to.
//
// extraKeep are ids to protect IN ADDITION to the active set, for this prune
// only. They are the sessions evicted by the registry cap in the same
// operation that created a new session file: their files were just flushed
// and must survive this prune so the client's "resume to continue" pane can
// re-attach them (E2/E11). On the next prune they are ordinary saved sessions
// subject to normal retention.
func (s *Server) pruneSessions(extraKeep ...string) {
	if s.ws.Store != nil {
		keep := append(s.registry.activeIDs(), extraKeep...)
		s.ws.Store.Prune(s.ws.GetWorkingDir(), keep...)
	}
}

// sessionNew creates a fresh session agent, registers it, and makes it the
// pane's (and the default) session. The previous session stays alive and
// resume-able (its in-flight turn, if any, keeps running per §4).
func (s *Server) sessionNew(ws *wsConn, pane **sessionRuntime) (agent.SessionCommandResult, bool, error) {
	oldID := (*pane).agent.SessionID
	rt := s.createNewSession(ws, pane)
	out := fmt.Sprintf("New session %s.", rt.agent.SessionID)
	if s.ws.Store != nil {
		out = fmt.Sprintf("New session %s. Previous session %s saved — use `resume %s` to restore.",
			rt.agent.SessionID, oldID, oldID)
	}
	return agent.SessionCommandResult{Output: out, Action: agent.SessionActionClearChat}, true, nil
}

// registerSeededSession creates a fresh session agent from a snapshot
// (nil = empty), renames it, registers it, persists it, and prunes the
// store. Used by the board start path: the session file must exist even if
// the first turn never runs. Unlike createNewSession it does NOT touch the
// connection's pane — no setDefault, no switchPane, no attach — so the
// started session runs headless until the user opens it.
func (s *Server) registerSeededSession(snap *agent.SessionSnapshot, label string) (*sessionRuntime, []string) {
	newID := session.NewID()
	a := s.ws.NewSessionAgent(snap, newID)
	if label != "" {
		a.RenameSession(label)
	}
	rt := s.newSessionRuntimeFor(a)
	evicted := s.registry.register(newID, rt)
	// Persist now: the session file must exist even when the first turn
	// never runs (model missing, immediate failure).
	a.FlushSession()
	s.pruneSessions(evicted...)
	return rt, evicted
}

// createNewSession registers a fresh session and switches the pane to it.
// Returns the new runtime.
func (s *Server) createNewSession(ws *wsConn, pane **sessionRuntime) *sessionRuntime {
	newID := session.NewID()
	// Inherit the current pane's thinking level. The workspace default
	// (ws.ThinkingLevel) is captured once at server startup, so relying on
	// it here would silently reset the level for every /new issued after the
	// user changed it — the "thinking level resets" bug. The TUI's /new
	// already inherits (ResetSessionState leaves ThinkingLevel untouched);
	// mirror that behavior in the web UI.
	//
	// Inherit the pane's current model too. The workspace default (ws.Model)
	// is the startup-era seed and set_model never advances it (D1 — the
	// model is per-session), so without inheritance every /new would stay
	// locked on the first session's model. The TUI's /new inherits
	// implicitly (one shared provider); the web /new must do it explicitly.
	// The inheritance carries the FULL routing key (owning profile + ID),
	// never the bare ID: a bare ID resets the owner to the default profile
	// and silently re-homes a model the pane runs on a different provider
	// (a same-ID model on the OpenCode catalog would steal it).
	var inheritLevel agent.ThinkingLevel
	var inheritRef llm.ModelRef
	if old := *pane; old != nil && old.agent != nil {
		// Locked read: another connection can set the pane's thinking level
		// concurrently (wsHandleSetThinkingLevel under the session turnMu),
		// and /new must not tear the plain field read. CurrentModelRef is
		// provider-internal-locked.
		_, inheritLevel = old.agent.ModeAndThinkingLevel()
		inheritRef = old.agent.CurrentModelRef()
	}
	a := s.ws.NewSessionAgent(nil, newID)
	// Adopt the pane's model when it differs from the fresh provider's seed
	// (the workspace default — verified by construction). An equal routing
	// key needs no adoption and no validation; a different one is adopted
	// unverified and confirmed in the background below (same contract as
	// resume/fork), so the first turn never sends a model that no longer
	// exists at the endpoint. An empty pane model (e.g. cleared as stale by
	// its own validation) falls back to the workspace-default seed.
	modelInherited := inheritRef.Valid() && inheritRef != a.CurrentModelRef()
	if modelInherited {
		a.AdoptModelRef(inheritRef)
	}
	if inheritLevel != "" {
		// Seeds the fresh session's agent and provider (SetThinkingLevel
		// syncs the provider) and persists the level into the new session
		// file. When the pane never set a level this is a no-op — the agent
		// already carries the workspace default. Runs AFTER the model
		// adoption: the setter flushes, and the flush must persist the
		// inherited model, not the pre-adoption default seed.
		a.SetThinkingLevel(inheritLevel)
	}
	rt := s.newSessionRuntimeFor(a)
	evicted := s.registry.register(newID, rt)
	s.registry.setDefault(newID)
	s.switchPane(ws, pane, rt)
	s.pruneSessions(evicted...)
	if modelInherited {
		// Model validation runs after registration: the async result must
		// push a config refresh to the new session's clients, which requires
		// the registered runtime (same wiring as loadOrCreateRuntime and
		// sessionFork). The adopted model is confirmed against the
		// provider's own catalog — or cleared and replaced by sole-model
		// auto-select when it is gone — and the clients are corrected via
		// OnModelChanged.
		s.validateModelAsync(rt, inheritRef.WireID())
	}
	return rt
}

// sessionCommandFocusPane is the response action for a resume that resolved to
// a session already open as a background pane of the requesting connection:
// the server does not re-key the pane; the client focuses the pane that
// already shows the session (its re-attach re-syncs the connection's pane
// pointer). Server-local: the value has no meaning to the TUI/agent hosts.
const sessionCommandFocusPane agent.SessionCommandAction = "focus_pane"

// sessionResume handles "resume <id>" and "resume latest". A successful
// switch ships the ATTACH sequence — session_state and the history snapshot in
// this read-loop turn (see the ordering note below), then the config pair
// asynchronously — with no response/clear_chat placeholder frames; a resume of
// the connection's current session is a no-op acknowledgement, and a target
// already open as a pane of this connection resolves to a focus-pane reply
// without switching.
func (s *Server) sessionResume(ctx context.Context, ws *wsConn, pane **sessionRuntime, args string) (agent.SessionCommandResult, bool, error) {
	if s.ws.Store == nil {
		return agent.SessionCommandResult{}, true, fmt.Errorf("session persistence disabled")
	}
	id := strings.TrimSpace(args)
	if id == "latest" {
		// The same rule as the TUI/CLI and the README: the newest saved
		// top-level session OTHER than the current one (nested children are
		// excluded — they are reachable only through their parent). The
		// agent's resolver owns that rule, so the web cannot drift from its
		// hosts. Store.LatestID would pick the CURRENT session whenever it
		// is the newest, turning "resume latest" into a pointless
		// wipe-and-replay of the on-screen transcript (an explicit
		// "/resume <current-id>" is the no-op ack below).
		latest, err := (*pane).agent.ResolveResumeTarget("latest")
		if err != nil {
			return agent.SessionCommandResult{}, true, err
		}
		id = latest
	}
	if id == "" {
		return agent.SessionCommandResult{}, true, fmt.Errorf("session id is required")
	}
	rt, err := s.loadOrCreateRuntime(id)
	if err != nil {
		return agent.SessionCommandResult{}, true, err
	}
	// Resuming the connection's CURRENT session is a no-op acknowledgement:
	// the pane already shows this session, live — the generic reply's
	// clear_chat + full history snapshot would only wipe and replay the
	// very transcript on screen (losing the reading position). Clients
	// reach this for an explicit current-id resume ("/resume <current-id>"
	// typed into the composer, or a client sending session_resume for it);
	// "resume latest" never lands here — the resolver above excludes the
	// current session. The idempotent switchPane keeps the re-attach
	// bookkeeping and its refused-attach guard; when it refuses (the
	// runtime is leaving memory) the switch below reports the same error
	// as any other resume.
	if *pane != nil && *pane == rt && s.switchPane(ws, pane, rt) {
		s.registry.setDefault(id)
		return agent.SessionCommandResult{
			Output: fmt.Sprintf("Already on session %s.", id),
		}, true, nil
	}
	// The target is already open as a background pane of THIS connection:
	// re-keying the current pane onto it would put one session in two panes
	// (each desyncs the other's transcript and event routing). Leave the pane
	// pointer alone and point the client at the pane it already has — the
	// focus that follows re-attaches the session, which re-syncs the
	// connection's pane pointer server-side. Passive (approval-only)
	// attachments are not panes and fall through to the normal switch.
	if *pane != nil && *pane != rt && rt.viewedBy(ws) {
		_ = ws.writeJSON(WSMessage{
			Type:          "response",
			Content:       fmt.Sprintf("Session %s is already open in another pane.", id),
			SessionID:     id,
			SessionAction: string(sessionCommandFocusPane),
		})
		return agent.SessionCommandResult{}, true, nil
	}
	if !s.switchPane(ws, pane, rt) {
		// The pane pointer must not move to a runtime that is leaving memory
		// (a claimed session_close raced this resume). Report the failure so
		// the client stays on its current session and clears its pending
		// change; a session_detached would name an id the client's pane does
		// not hold and strand the pending flag.
		return agent.SessionCommandResult{}, true, fmt.Errorf("session %s is closing", id)
	}
	s.registry.setDefault(id)
	// The resume reply IS the attach sequence: session_state (the client's
	// re-key frame and, for a RUNNING headless turn, the convergence latch —
	// the history snapshot cannot contain the in-flight reply, so the client
	// re-attaches on turn_end), then the history snapshot, then the config
	// pair. Byte-identical to session_attach, so a pane re-derives its
	// transcript through exactly one code path. The empty result tells
	// writeSessionCommandResult the reply was already sent (no
	// response/clear_chat placeholder frames).
	//
	// The history is built and enqueued in THIS read-loop turn rather than in
	// the payload goroutine (which still ships the configs): the client
	// reacts to session_state IMMEDIATELY — it releases the left-behind
	// session (session_detach, whose orphan eviction broadcasts
	// session_detached) and refreshes the sidebar (list_sessions) — and those
	// reactions and their replies would otherwise be written AHEAD of a
	// goroutine-built history on the same socket, leaving the pane on its
	// empty state for the whole snapshot build. It is the frame that swaps
	// the transcript, so it must be back-to-back with the re-key frame. The
	// clone here is the same read-loop work the pre-unification reply did
	// (minus its second clone and context brief).
	s.sendSessionState(ws, rt)
	s.sendAttachHistory(ws, rt, nil)
	s.sendAttachPayload(ctx, ws, rt, false, nil)
	return agent.SessionCommandResult{}, true, nil
}

// sessionFork forks the pane's session into a NEW agent (the source session
// is untouched, E13) and switches the pane to it.
func (s *Server) sessionFork(ctx context.Context, ws *wsConn, pane **sessionRuntime, args string) (agent.SessionCommandResult, bool, error) {
	src := *pane
	forkedMsgs, err := agent.ForkMessages(src.agent.SnapshotMessages(), args)
	if err != nil {
		return agent.SessionCommandResult{}, true, err
	}
	newID := session.NewID()
	// Re-point the forked transcript's spill locators at the child's spill
	// dir (hardlinks) so retrieval keeps working after the source session
	// is deleted (delete removes the source's spill files).
	src.agent.RepointSpillLocators(forkedMsgs, newID)
	// Carry the source's session state into the fork. The TUI fork
	// (ForkSession) replaces messages on the SAME agent, so it keeps the
	// source's mode, thinking level, model, and todos; the web fork must
	// mirror that instead of silently producing a fresh act-mode session
	// with the workspace default thinking/model and empty todos.
	// Mode/thinking/model are internally synchronized (statsMu / provider);
	// the todo list is guarded by TodoManager's own mutex (Snapshot is
	// internally synchronized), so NO source turnMu is needed here. In
	// particular the composer path (handleWSUserMessage) routes a typed
	// "/fork" through runSessionCommand while ALREADY holding the pane's
	// turnMu (write lock) — a turnMu.RLock here would self-deadlock, since
	// sync.RWMutex is not reentrant.
	mode, thinking := src.agent.ModeAndThinkingLevel()
	ref := src.agent.CurrentModelRef()
	todos := src.agent.TodoManager.Snapshot()
	snap := agent.SessionSnapshot{
		WorkingDir:    s.ws.GetWorkingDir(),
		Model:         ref.WireID(),
		ModelProvider: ref.ProfileName(),
		Mode:          mode.String(),
		ThinkingLevel: string(thinking),
		Todos:         todos,
		Messages:      forkedMsgs,
	}
	a := s.ws.NewSessionAgent(&snap, newID)
	rt := s.newSessionRuntimeFor(a)
	evicted := s.registry.register(newID, rt)
	s.registry.setDefault(newID)
	s.switchPane(ws, pane, rt)
	s.pruneSessions(evicted...)
	// Validate the forked model asynchronously and push the result to the
	// fork's clients: the model came from the source session, which may
	// itself have been restored with a model that no longer exists on the
	// current provider.
	if snap.Model != "" {
		s.validateModelAsync(rt, snap.Model)
	}
	// Persist the forked session. The source session is deliberately NOT
	// touched (no TouchSession): forking is an interaction with the FORK,
	// not the source, and bumping the source's Updated would push it to the
	// top of the saved-session list ("resume to continue" recency ordering
	// is interaction-based). The source keeps its own last-activity
	// timestamp and stays open as a pane.
	if s.ws.Store != nil {
		a.FlushSession()
	}
	out := agent.AppendContextBrief(ctx, rt.agent, fmt.Sprintf("Forked new session %s.", newID))
	return agent.SessionCommandResult{
		Output:  out,
		Action:  agent.SessionActionClearChat,
		History: rt.agent.SnapshotMessages(),
	}, true, nil
}

// sessionDelete removes a session: drains + evicts the live runtime (if
// active, E10), then deletes the file. Deleting the pane's current session
// starts a fresh one, mirroring the single-agent "was current" behavior.
func (s *Server) sessionDelete(ctx context.Context, ws *wsConn, pane **sessionRuntime, id string) (agent.SessionCommandResult, bool, error) {
	if s.ws.Store == nil {
		return agent.SessionCommandResult{}, true, fmt.Errorf("session persistence disabled")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return agent.SessionCommandResult{}, true, fmt.Errorf("session id is required")
	}
	// "Current" is the pane's session — compare by SessionID, NOT runtime
	// pointer. A delete of a session whose runtime was already evicted
	// (registry-cap eviction or a previous delete) but is still this
	// connection's pane must still count as "was current" and start a fresh
	// session; with the old pointer comparison the pane was left pointing at
	// a dead runtime whose messages were silently dropped. The default
	// (registry.first()) is a different concept: it is only the fallback
	// target for messages without a sessionId, and deleting it must NOT
	// hijack this pane into a brand-new session (clear_chat + fresh history)
	// — that is why we compare against the pane, not the default.
	wasCurrent := *pane != nil && (*pane).agent != nil && (*pane).agent.SessionID == id
	var rt *sessionRuntime
	var registered bool
	if rt, registered = s.registry.get(id); registered {
		// Drain the in-flight turn (≤ wsStreamDrainWait) and flush BEFORE
		// deleting the file, so a late doPersist from the turn cannot
		// resurrect the file after Store.Delete. The runtime is NOT
		// evicted yet: if the file delete fails, the session stays live and
		// registered — the pane keeps working and the error reaches the
		// client, instead of leaving the pane on an evicted runtime whose
		// next message would turn into an invisible headless turn.
		rt.stream.cancelInFlight()
		// Hold the session turn lock across the flush + delete + eviction so
		// a NEW turn cannot start in the window between the drained turn and
		// registry.remove() — its start-flush would re-create the file
		// Store.Delete just removed (delete resurrection). TryLock: if the
		// drain timed out (the cancelled turn's goroutine is still alive,
		// e.g. stuck in a tool), that turn itself holds the lock, so no new
		// turn can start anyway — proceed without it and accept that rare
		// stuck-turn resurrection (a running turn cannot be safely stopped
		// from the delete path).
		heldTurnMu := rt.turnMu.TryLock()
		if heldTurnMu {
			defer rt.releaseTurn()
		}
		rt.agent.FlushSession()
	}
	// Nested (subagent) descendants — children, grandchildren, etc. — are
	// drained with the same ordering as the root, whether or not the
	// deleted session's own runtime is registered (orphan/cap eviction):
	// the store's cascade delete removes their FILES, so their in-flight
	// turns must flush BEFORE that delete — a late doPersist from a
	// still-running child would recreate the file the cascade just removed
	// (delete resurrection, same shape as the root guard above). They are
	// evicted after the delete below.
	descendants := s.registry.nestedDescendants(id, func(sid string) string {
		if rt, ok := s.registry.get(sid); ok {
			return rt.agent.ParentID()
		}
		if info := s.ws.Store.Info(s.ws.GetWorkingDir(), sid); info != nil {
			return info.ParentID
		}
		return ""
	})
	for _, d := range descendants {
		d.stream.cancelInFlight()
		// Hold the child's turn lock across the flush + delete + eviction
		// (same rationale as the root): a NEW turn cannot start on a child
		// whose file is about to be cascade-deleted. TryLock: if the drain
		// timed out (the cancelled turn's goroutine is still alive), that
		// turn itself holds the lock, so no new turn can start anyway.
		if held := d.turnMu.TryLock(); held {
			defer d.releaseTurn()
		}
		d.agent.FlushSession()
	}
	// A nested (subagent) child whose outcome has NOT reached the parent
	// yet reports its deletion back to the parent session: a live child
	// (running, or interrupted and never delivered) may still be awaited,
	// so the parent must learn it (and its transcript) is gone. A
	// FINISHED child already delivered its report (completion notice) and
	// an UNREGISTERED child's runtime is long gone — both delete silently,
	// like top-level sessions: the notice is a paid parent turn, and for
	// a child whose outcome already landed it is pure noise.
	var notifyParentID, notifyLabel string
	notifyParent := false
	if registered {
		notifyParentID = rt.agent.ParentID()
		notifyLabel = rt.agent.SessionLabelSnapshot()
		notifyParent = true
		// A finished continuable child's completion notice already
		// delivered its outcome: its delete is housekeeping, no notice.
		if sp, ok := s.ws.SubagentSpawner.(*subagentSpawner); ok {
			if c := sp.Get(id); c != nil && c.IsFinished() {
				notifyParent = false
			}
		}
	}
	if err := s.ws.Store.Delete(s.ws.GetWorkingDir(), id); err != nil {
		return agent.SessionCommandResult{}, true, err
	}
	if notifyParent && notifyParentID != "" {
		if notifyLabel == "" {
			notifyLabel = id
		}
		s.registry.deliverToParent(notifyParentID, steerq.Notice, subagents.Notice(notifyLabel, "deleted by the user — its transcript is gone."))
	}
	// The file is gone, so the session can never be reopened: drop any
	// queued deliveries for it (subagent completions/reports) instead of
	// letting the queue linger in memory. Runs on the unregistered path
	// too — a parent orphan-evicted while its children kept running may
	// have a queued notice here.
	s.registry.clearParentDeliveries(id)
	if registered {
		// The file is gone: evict the runtime, then tell attached clients
		// the session no longer exists and detach every one of them.
		// Removal happens BEFORE the notification (same ordering as
		// evictRuntime): once the runtime is unregistered, a concurrent
		// session_attach/resume of the same id loads fresh from the store —
		// the file is already gone, so the load fails and the client gets
		// session_removed — instead of attaching to a dying-but-registered
		// runtime that this remove() then yanks out from under it. The
		// broadcast reaches every attached socket regardless (it writes to
		// the clients set, not the registry), so client-visible message
		// ordering is unchanged.
		s.teardownDeletedRuntime(rt)
	} else {
		// Explicit delete is a teardown even when the runtime is already
		// gone (orphan/cap eviction): the deleted session's background
		// subagent children are still tracked and must be cancelled and
		// released NOW, instead of lingering until their retention timers.
		s.registry.fireEvictHook(id)
	}
	// Evict every registered descendant the cascade just removed from
	// disk: their runtimes were drained (and flushed) BEFORE the store
	// delete, so the eviction itself writes nothing and cannot resurrect
	// the files. Same removal/notify ordering as the root. Runs on the
	// unregistered-parent path too (the parent was orphan/cap-evicted
	// while its background children kept running): without it, a child
	// whose file the cascade just removed would stay live and resurrect
	// the file on its next persist.
	s.evictNestedDescendants(descendants)
	if !registered && wasCurrent && *pane != nil {
		// The runtime was evicted before this delete (cap eviction or an
		// earlier delete) but is still the connection's pane: release the
		// stale attachment so the dead socket cannot linger in its clients
		// set (the pane is switched to a fresh session below).
		(*pane).detach(ws)
	}
	s.pruneSessions()
	if wasCurrent {
		rt := s.createNewSession(ws, pane)
		return agent.SessionCommandResult{
			Output:  fmt.Sprintf("Deleted session %s (was current — started new session %s).", id, rt.agent.SessionID),
			Action:  agent.SessionActionClearChat,
			History: rt.agent.SnapshotMessages(),
		}, true, nil
	}
	return agent.SessionCommandResult{Output: fmt.Sprintf("Deleted session %s.", id)}, true, nil
}

// teardownDeletedRuntime removes a deleted session's runtime from the registry
// and detaches every attached client, AFTER the session's file is gone:
// once the runtime is unregistered, a concurrent session_attach/resume of
// the same id loads fresh from the store — the file is already gone, so the
// load fails and the client gets session_removed — instead of attaching to
// a dying-but-registered runtime. The broadcast reaches every attached
// socket regardless (it writes to the clients set, not the registry), so
// client-visible message ordering is unchanged. The session's background
// jobs are killed too: with the session gone there is no owner left to
// poll them. Callers must hold (or have drained) the runtime's turnMu.
func (s *Server) teardownDeletedRuntime(rt *sessionRuntime) {
	id := rt.agent.SessionID
	s.registry.remove(id)
	rt.broadcast(WSMessage{Type: "session_removed", SessionID: id})
	// Session delete is an explicit teardown: the deleted session's
	// background subagent children are cancelled and released.
	s.registry.fireEvictHook(id)
	// Detach ALL attached clients, not just the requesting connection:
	// the deleted session may be a BACKGROUND pane of this connection or
	// a pane of another tab, and leaving any socket in the removed
	// runtime's clients set would leak it — teardown's detachAll only
	// sweeps REGISTERED sessions and would never reach this runtime.
	// detach is idempotent and never cancels a turn (the turn was drained
	// above); the requesting pane is re-homed by the wasCurrent /
	// createNewSession path in sessionDelete.
	rt.detachAllClients()
	// Kill background jobs (execute_command background=true): with the
	// session deleted there is no owner left to poll them.
	rt.agent.Close()
}

// evictNestedDescendants removes the registered runtimes whose files the
// store's cascade delete just removed: their turns were drained (and
// flushed) BEFORE the delete, so this eviction writes nothing and cannot
// resurrect the files. Same removal/notify ordering as teardownDeletedRuntime.
// Callers must hold (or have drained) each runtime's turnMu.
func (s *Server) evictNestedDescendants(descendants []*sessionRuntime) {
	for _, d := range descendants {
		s.teardownDeletedRuntime(d)
	}
	// Final sweep: the cascade removed these files, so any file that still
	// exists was recreated by a write that landed after the delete — the
	// release path's outcome flush, the eviction's FlushPending, or a late
	// turn flush racing the eviction. Remove the residue so no orphan
	// outlives the delete. Store.Delete is a no-op on a missing file.
	for _, d := range descendants {
		_ = s.ws.Store.Delete(s.ws.GetWorkingDir(), d.agent.SessionID)
	}
}

// createBootstrapSession registers a default session when the registry is
// empty — every runtime was evicted (the last pane was explicitly closed via
// session_close, or the last client detached from an idle session and the
// orphan eviction fired). Prefer the most recently saved session so a
// refreshed page lands the user back on their latest conversation, mirroring
// main.go's startup restore; fall back to a fresh session when the store is
// empty. Attach/load adds no files, so no prune runs here (same rationale as
// ensureSessionRuntime).
func (s *Server) createBootstrapSession() *sessionRuntime {
	if s.ws.Store != nil {
		if latest, err := s.ws.Store.LatestID(s.ws.GetWorkingDir()); err == nil && latest != "" {
			if rt, err := s.loadOrCreateRuntime(latest); err == nil {
				s.registry.setDefault(latest)
				return rt
			}
		}
	}
	a := s.ws.NewSessionAgent(nil, session.NewID())
	rt := s.newSessionRuntimeFor(a)
	s.registry.register(rt.agent.SessionID, rt)
	s.registry.setDefault(rt.agent.SessionID)
	return rt
}
