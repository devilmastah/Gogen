package server

import (
	"fmt"

	"gogen/internal/agent"
	"gogen/internal/steerq"
	"gogen/internal/subagents"
)

// wsHandlers maps inbound message types to their handlers. Unknown types are
// dropped silently, exactly like the switch's implicit default. FS entries
// carry a kind tag so TestWSEditorHandlerParity can assert they stay in
// lockstep with the /ws/editor socket's editorReadTypes/editorWriteTypes.
var wsHandlers = map[string]wsHandlerEntry{
	"session_fork":       {handle: wsHandleFork},
	"fs_list":            {handle: wsHandleFSRead, kind: wsKindFSRead},
	"fs_read":            {handle: wsHandleFSRead, kind: wsKindFSRead},
	"fs_search":          {handle: wsHandleFSRead, kind: wsKindFSRead},
	"git_status":         {handle: wsHandleFSRead, kind: wsKindFSRead},
	"git_file_diff":      {handle: wsHandleFSRead, kind: wsKindFSRead},
	"git_commit_message": {handle: wsHandleFSRead, kind: wsKindFSRead},
	"fs_write":           {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"fs_replace":         {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"fs_apply_patch":     {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"git_commit":         {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"git_stage":          {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"git_unstage":        {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"git_push":           {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"list_sessions":      {handle: wsHandleListSessions},
	"session_new":        {handle: wsHandleSessionAction},
	"session_resume":     {handle: wsHandleSessionAction},
	"session_delete":     {handle: wsHandleSessionAction},
	"list_models":        {handle: wsHandleListModels},
	"set_model":          {handle: wsHandleSetModel},
	"set_mode":           {handle: wsHandleSetMode},
	"set_thinking_level": {handle: wsHandleSetThinkingLevel},
	"config":             {handle: wsHandleConfig},
	"reindex_code":       {handle: wsHandleReindexCode},
	"cancel":             {handle: wsHandleCancel},
	"board_op":           {handle: wsHandleBoardOp},
	"provider_save":      {handle: wsHandleProviderSave},
	"provider_delete":    {handle: wsHandleProviderDelete},
	"test_provider":      {handle: wsHandleTestProvider},
	"test_mcp":           {handle: wsHandleTestMCP},
	"mcp_save":           {handle: wsHandleMCPSave},
	"mcp_delete":         {handle: wsHandleMCPDelete},
	"session_attach":     {handle: wsHandleAttach},
	"session_detach":     {handle: wsHandleDetach},
	"session_close":      {handle: wsHandleClose},
	"user_term_input":    {handle: wsHandleUserTermInput},
	"user_term_resize":   {handle: wsHandleUserTermResize},
	"user_term_request":  {handle: wsHandleUserTermRequest},
	"compact":            {handle: wsHandleCompact},
	"queue_remove":       {handle: wsHandleQueueRemove},
	"queue_clear":        {handle: wsHandleQueueClear},
	"queue_send_now":     {handle: wsHandleQueueSendNow},
	"message":            {handle: wsHandleMessage},
}

// wsHandleQueueRemove drops one queued user steering message (the queued
// bubble's ✕). The broadcast inside enqueue/clear keeps every tab's queue
// state converged; here the removal reports the new state on success.
func wsHandleQueueRemove(req *wsRequest) {
	target := req.target
	if target == nil {
		return
	}
	if target.removeQueuedItem(req.msg.QueueID) {
		target.broadcastQueueState()
	}
}

// wsHandleQueueSendNow asks the running turn to stop at the next round
// boundary so the queued messages can run (see sendNow). It does not cancel
// the turn and it does not clear the queue: a hard cancel means "stop what
// you're doing" and drops the user's queued text, and Send now exists to
// deliver that text. A refusal answers the sender on the UI channel, so the
// client can tell it from an accepted request.
func wsHandleQueueSendNow(req *wsRequest) {
	target := req.target
	if target == nil || target.agent == nil {
		return
	}
	if res := target.sendNow(req.msg.QueueID); res != steerq.SendNowRecorded {
		// Session-tagged: the client clears the pending Send now of THAT pane,
		// and the user may have switched panes while the answer was in flight.
		writeNoticeErrorFor(req.conn, target.agent.SessionID, "queue", res.Message())
	}
}

// wsHandleQueueClear drops every queued user steering message ("clear all"
// in the queued UI). System deliveries are kept (they are machine
// bookkeeping — see clearUserQueue).
func wsHandleQueueClear(req *wsRequest) {
	target := req.target
	if target == nil {
		return
	}
	if target.clearUserQueue() > 0 {
		target.broadcastQueueState()
	}
}

// wsHandleFork handles session_fork: the fork source is the pane named by
// sessionId (edit-resend forks its own pane's session). Re-align the
// connection's pane with the explicit id so a reconnect-stale pointer cannot
// fork the wrong session, and answer with an error when the source session
// is gone entirely (never fork a different session — and never drop the
// request silently: the client's in-flight session-change/resend bookkeeping
// waits for this reply, and an Error response is what clears it).
func wsHandleFork(req *wsRequest) {
	s, ws, r, msg := req.server, req.conn, req.request, req.msg
	pane := &req.pane
	if msg.SessionID != "" {
		t := s.resolveRuntime(msg.SessionID)
		if t == nil {
			if *pane != nil && (*pane).agent.SessionID == msg.SessionID {
				t = *pane
			} else {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf(
					"Error: session %s is gone — fork not started", msg.SessionID)})
				return
			}
		}
		*pane = t
	}
	s.handleWSSessionAction(ws, r.Context(), pane, msg)
}

func wsHandleFSRead(req *wsRequest) {
	s, ws, r, msg := req.server, req.conn, req.request, req.msg
	s.handleFSReadMessage(ws, r.Context(), msg)
}

func wsHandleFSWrite(req *wsRequest) {
	s, ws, r, msg := req.server, req.conn, req.request, req.msg
	s.handleFSWriteMessage(ws, r.Context(), msg)
}

// wsHandleListSessions lists the saved sessions for the targeted session's
// working directory. An empty registry drops the request (the reply
// dereferences rt.agent in a goroutine and a nil runtime would panic the
// whole process); the client re-requests after its next session_new /
// session_attach.
func wsHandleListSessions(req *wsRequest) {
	s, ws, target := req.server, req.conn, req.target
	if target == nil {
		target = s.registry.first()
	}
	if target == nil {
		return
	}
	rt := target
	// Listing hits the session store on disk (metadata index read, label
	// migration file reads, legacy full-scan fallback). Run it off the WS
	// read loop like wsHandleListModels, so a slow store cannot block chat,
	// FS, or editor messages behind the sidebar.
	go func() {
		// The full list — INCLUDING nested (subagent) sessions — so the
		// sidebar renders persisted children under their parent row after a
		// page reload / late attach (subagent_started/finished events are
		// not replayed to connecting clients). The client skips nested
		// entries when building flat rows and groups them under parents.
		sessions, listErr := rt.agent.SessionListAll()
		if listErr != nil {
			writeNoticeError(ws, "sessions", fmt.Sprintf("Error: %v", listErr))
			return
		}
		if sessions == nil {
			sessions = []agent.SessionInfo{}
		}
		// The reply deliberately carries NO SessionID: the sessions payload
		// is connection-scoped sidebar state (the full saved list), not a
		// message for one session. Tagging it with the current session id
		// made the client route (and possibly drop) it when that id was not
		// the active pane — e.g. after another tab moved the global default.
		_ = ws.writeJSON(WSMessage{
			Type:     "sessions",
			Sessions: sessionEntries(sessions, s.registry.activeSet(), s.registry.turnActiveSet(), s.registry.registeredSet()),
		})
	}()
}

// wsHandleSessionAction handles session_new/session_resume/session_delete.
// session_new creates for the connection's CURRENT pane, but the client's
// edit-resend path scopes it with the acting pane's id (beginResend:
// histIdx == 0 sends session_new with sessionId). Re-align the pane pointer
// to that id (like session_fork does) so a reconnect-stale pointer can never
// replace the WRONG pane's session: when the id resolves to neither a
// registered session nor this pane's own (possibly evicted) session, the
// request is REJECTED with an error instead of falling through onto a
// different pane's session — a pane that still holds the id may proceed
// (replacing our own evicted session is exactly what was asked).
// session_resume/session_delete are NOT re-aligned: their sessionId names
// the TARGET session, not the acting pane.
func wsHandleSessionAction(req *wsRequest) {
	s, ws, r, msg := req.server, req.conn, req.request, req.msg
	pane := &req.pane
	if msg.Type == "session_new" && msg.SessionID != "" {
		if t := s.resolveRuntime(msg.SessionID); t != nil {
			*pane = t
		} else if *pane == nil || (*pane).agent.SessionID != msg.SessionID {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf(
				"Error: session %s is not this pane's session — new session not started", msg.SessionID)})
			return
		}
	}
	s.handleWSSessionAction(ws, r.Context(), pane, msg)
}

// wsHandleListModels lists the provider models for the targeted session. An
// empty registry drops the request (the reply dereferences rt.agent in a
// goroutine and a nil runtime would panic the process).
func wsHandleListModels(req *wsRequest) {
	s, ws, r, target := req.server, req.conn, req.request, req.target
	if target == nil {
		target = s.registry.first()
	}
	if target == nil {
		return
	}
	rt := target
	ctx := r.Context()
	go func() {
		models, err := rt.agent.ListModels(ctx)
		if err != nil {
			writeNoticeError(ws, "models", fmt.Sprintf("Error: %v", err))
			return
		}
		// CurrentModel reads under the provider's own modelsMu (the same
		// contract agentConfigMsgBasic relies on), so no turnMu is needed —
		// a running turn never delays the model catalog reply. The owning
		// profile travels with the bare ID: model IDs are not unique across
		// registered providers, so the client's current-model check must
		// match (provider, id).
		currentRef := rt.agent.CurrentModelRef()
		_ = ws.writeJSON(WSMessage{
			Type:          "models",
			Model:         currentRef.WireID(),
			ModelProvider: currentRef.ProfileName(),
			Models:        s.modelEntries(models),
		})
	}()
}

func wsHandleSetModel(req *wsRequest) {
	s, ws, r, msg, target := req.server, req.conn, req.request, req.msg, req.target
	if target == nil {
		return
	}
	rt := target
	ctx := r.Context()
	// SelectModel resolves the selector against the provider catalog, which
	// performs network I/O on first use. Pre-fetch the catalog OUTSIDE the
	// turn lock so a slow endpoint cannot stall the whole session (its
	// turns and every other turnMu-taking handler); SelectModel under the
	// lock then only touches the in-memory cache. If the pre-fetch fails,
	// SelectModel surfaces the same error under the lock — no regression.
	_, _ = rt.agent.ListModels(ctx)
	acquired, _, err := s.withTurnConfig(rt, ws, ctx, func(a *agent.Agent) (bool, error) {
		return true, a.SelectModel(ctx, msg.Model)
	})
	if !acquired {
		// UI-channel handler (model picker): the busy rejection must NOT
		// render into the chat transcript — it toasts as a model notice.
		writeNoticeError(ws, "model", errAgentBusy)
		return
	}
	if err != nil {
		writeNoticeError(ws, "model", fmt.Sprintf("Error: %v", err))
		return
	}
	// Model is per-session: SelectModel above applied to this pane's
	// provider only. The workspace default (ws.Model) is only mutated by the
	// settings modal's default-profile save (provider_save), never here, and
	// no other session's provider is touched, so two panes can run different
	// models concurrently. The config echo carries this session's own
	// Mode/ThinkingLevel/Model to every client attached to IT (echoConfigReply),
	// never to another session's clients.
	// llama.cpp endpoints: derive the new model's true reasoning-effort
	// options from /props (+ /apply-template) and push a config update when
	// they differ from the fallback the echo above carried (the provider
	// caches per model, so repeat switches are no-ops).
	s.maybeProbeReasoningEfforts(ctx, rt.agent)
}

func wsHandleSetMode(req *wsRequest) {
	s, ws, r, msg, target := req.server, req.conn, req.request, req.msg, req.target
	if target == nil {
		return
	}
	rt := target
	ctx := r.Context()
	// Mode is per-SESSION state that every attached client renders in its
	// picker, and the model's tool list follows the SERVER's mode: a second
	// tab/window (or a second pane on the same session) left on the previous
	// value would show "act" for a session whose tools are the read-only plan
	// set — and its picker's "already active" short-circuit makes clicking Act
	// a silent no-op, so the user could not recover from that tab. The echo
	// therefore fans out to every attached client (echoConfigReply).
	acquired, _, _ := s.withTurnConfig(rt, ws, ctx, func(a *agent.Agent) (bool, error) {
		m, ok := agent.ParseMode(msg.Mode)
		if !ok {
			return false, nil // unrecognized value: nothing to echo
		}
		a.SetMode(m)
		return true, nil
	})
	if !acquired {
		// UI-channel handler (mode picker): busy rejection as a notice.
		writeNoticeError(ws, "mode", errAgentBusy)
	}
}

func wsHandleSetThinkingLevel(req *wsRequest) {
	s, ws, r, msg, target := req.server, req.conn, req.request, req.msg, req.target
	if target == nil {
		return
	}
	rt := target
	ctx := r.Context()
	acquired, _, _ := s.withTurnConfig(rt, ws, ctx, func(a *agent.Agent) (bool, error) {
		// An out-of-range level for the current model is ignored, not
		// rejected — but the echo still runs, so the client's picker
		// re-syncs to the level the session actually has.
		if s.isValidThinkingLevel(a, msg.ThinkingLevel) {
			a.SetThinkingLevel(agent.ThinkingLevel(msg.ThinkingLevel))
		}
		return true, nil
	})
	if !acquired {
		// UI-channel handler (thinking-level picker): busy rejection as a
		// notice.
		writeNoticeError(ws, "thinking", errAgentBusy)
	}
}

// wsHandleConfig routes config scoped by the pane's sessionId. After a
// reconnect the re-attach loop leaves the pane pointer on the LAST attached
// pane, which can differ from the client's active pane — re-align it with
// the explicit id so the working-dir change interrupts the right session's
// turn. session_delete/session_detach are NOT re-aligned here: their
// sessionId names the TARGET session, not the acting pane.
func wsHandleConfig(req *wsRequest) {
	s, ws, r, msg, target := req.server, req.conn, req.request, req.msg, req.target
	pane := &req.pane
	if msg.SessionID != "" && target != nil {
		*pane = target
	}
	s.handleWSConfig(ws, r.Context(), pane, msg)
}

// wsHandleCancel cancels the targeted session's in-flight turn. Cancel is
// the ONLY way to stop a turn, and it works cross-connection (scoped to the
// targeted session). Interrupt semantics: the user's own queued steering
// messages are cleared with the turn ("stop what you're doing") and the
// cleared queue is broadcast; system deliveries are kept (machine
// bookkeeping — job notices, subagent reports, reply capture).
//
// When NOTHING is cancelled — the session is gone, the turn already ended,
// or another connection's cancel won the race — no cancelled/turn_end pair
// will follow, so the sender is answered with a `cancel_ack` instead: the
// web client holds a per-cancel bookkeeping unit (suppressTurnEnds) from
// every cancel it sends, and without an answer that unit could never drain —
// the next real turn_end would be swallowed and the client's busy state
// would stick. cancel_ack consumes exactly one unit and has no other
// effects; a real pair that was already broadcast precedes this frame on the
// socket, so the pair drains the unit first and the ack arrives as a no-op.
// Clients without a cancel_ack handler ignore the frame (unknown types fall
// through both dispatcher switches).
func wsHandleCancel(req *wsRequest) {
	target, ws := req.target, req.conn
	wasInFlight := false
	if target != nil {
		wasInFlight = target.stream.cancelInFlight()
		if !wasInFlight {
			// Another connection's cancel may hold the handle already (just
			// released, pair not yet broadcast): while the turn still reads
			// active its cancelled/turn_end pair is coming — withhold the
			// ack so the real pair does the draining.
			if active, _ := target.turnState(); active {
				wasInFlight = true
			}
		}
		if n := target.clearUserQueue(); n > 0 {
			target.broadcastQueueState()
		}
	}
	if !wasInFlight {
		_ = ws.writeJSON(WSMessage{Type: "cancel_ack"})
	}
}

// wsHandleAttach makes the session the connection's current pane and resends
// session_state + history + config + context. Sessions that are not currently
// active are loaded from the store, so the sidebar's "open session" works for
// saved sessions too.
func wsHandleAttach(req *wsRequest) {
	s, ws, r, msg := req.server, req.conn, req.request, req.msg
	pane := &req.pane
	rt2, err := s.ensureSessionRuntime(msg.SessionID)
	if err != nil {
		// The session no longer exists (deleted elsewhere / server
		// restarted with pruning): tell the client to drop the pane.
		_ = ws.writeJSON(WSMessage{Type: "session_removed", SessionID: msg.SessionID, Content: err.Error()})
	} else {
		if !s.switchPane(ws, pane, rt2) {
			// The attach raced an eviction: the resolved runtime is
			// closing (a claimed session_close) or already evicted —
			// report the detach so the client drops the pane instead of
			// sending messages to a runtime that is leaving memory.
			s.notifySessionDetached(ws, rt2)
			return
		}
		// Conditional attach: a pane re-focusing with an unchanged
		// transcript (session id + history epoch + newest rendered index)
		// lets the server skip the history snapshot entirely.
		var known *attachKnown
		if msg.KnownHistoryEpoch != nil && msg.KnownHistoryIndex != nil {
			known = &attachKnown{epoch: *msg.KnownHistoryEpoch, index: *msg.KnownHistoryIndex}
		}
		if !s.attachSession(ws, r, rt2, !msg.NoHistory, known) {
			// The same race, landing between the pane switch and the
			// payload (cap eviction ignores attachments). The eviction's
			// own session_detached broadcast may also arrive; duplicates
			// are idempotent client-side.
			s.notifySessionDetached(ws, rt2)
		}
	}
}

// wsHandleDetach handles session_detach: the client declared the pane
// closed; detach without cancelling any running turn.
func wsHandleDetach(req *wsRequest) {
	ws, target := req.conn, req.target
	if target == nil {
		return
	}
	target.detach(ws)
}

// wsHandleClose handles session_close: the client pressed ✕ on an open pane;
// the session is explicitly closed. Detach, then — when no other socket is
// still attached (another tab may be watching the same session and must not
// have its turn cancelled or its runtime evicted) — cancel the in-flight
// turn, flush, and unregister. The session stays saved on disk and reopens
// from the store like any other saved session (ensureSessionRuntime). If the
// detach already orphan-evicted an idle runtime, closeRuntime is a no-op
// (evicted flag).
func wsHandleClose(req *wsRequest) {
	s, ws, target := req.server, req.conn, req.target
	if target == nil {
		return
	}
	target.detach(ws)
	// The clientless decision must be atomic against attach: a plain
	// clientCount()==0 check here raced a concurrent session_attach, which
	// could resolve this (still-registered) runtime in the window before
	// closeRuntime and attach — the close then evicted the racing viewer's
	// runtime from under it (and cancelled a turn started in that window),
	// violating the multi-tab guarantee in the comment above. claimClose
	// latches the runtime closing under clientsMu only when it is
	// clientless, so the racing attach is refused instead and its handler
	// reports session_detached (the session stays saved; the client reopens
	// it from the saved list).
	if !target.claimClose() {
		return
	}
	// Closing a nested (subagent) child reports back to its parent
	// session: the main agent must learn the child was stopped by the
	// user (delivered as a system message once the parent is idle /
	// its turn ends). Skipped when the runtime was already evicted —
	// closeRuntime would be a no-op, so there is nothing to report.
	if parentID := target.agent.ParentID(); parentID != "" && !target.evicted.Load() {
		label := target.agent.SessionLabelSnapshot()
		if label == "" {
			label = target.agent.SessionID
		}
		s.registry.deliverToParent(parentID, steerq.Notice, subagents.Notice(label, "closed by the user — its session stays saved and can be reopened."))
	}
	s.registry.closeRuntime(target)
}

func wsHandleUserTermInput(req *wsRequest) {
	holder, msg := req.holder, req.msg
	if ut := holder.get(); ut != nil {
		_ = ut.Write([]byte(msg.Content))
	}
}

func wsHandleUserTermResize(req *wsRequest) {
	holder, msg := req.holder, req.msg
	if ut := holder.get(); ut != nil && msg.Cols > 0 && msg.Rows > 0 {
		_ = ut.Resize(uint16(msg.Cols), uint16(msg.Rows))
	}
}

func wsHandleUserTermRequest(req *wsRequest) {
	s, ws, holder := req.server, req.conn, req.holder
	s.spawnUserTerminal(ws, holder)
}

func wsHandleCompact(req *wsRequest) {
	s, ws, r, target := req.server, req.conn, req.request, req.target
	if target == nil {
		return
	}
	s.handleWSCompact(ws, r, target, "")
}

// wsHandleMessage routes a user message. The client scopes every message
// with the sessionId of the pane it was typed in, but the server routes
// messages via the connection's current pane. After a reconnect the
// re-attach loop leaves that pointer on the LAST attached pane, which may
// not be the pane the client is using — so route by the explicit id (the
// pane pointer remains the fallback for legacy empty ids). An id that no
// longer resolves is either an in-flight eviction of THIS very session
// (continue on the same runtime) or a stale pane — in the latter case drop
// the message rather than deliver it to a different session, and SAY SO:
// the client has already rendered an optimistic bubble and armed its
// pending-ack correlation, so a silent drop would strand both (the stale
// entry would then stamp the NEXT ack with the wrong history index —
// edit/resend/fork at a wrong point). An "Error:" response is what the
// client's response handler needs to revoke the bubble and drop the entry.
func wsHandleMessage(req *wsRequest) {
	s, ws, r, msg, target := req.server, req.conn, req.request, req.msg, req.target
	pane := &req.pane
	if msg.SessionID != "" {
		if target == nil {
			if *pane != nil && (*pane).agent.SessionID == msg.SessionID {
				target = *pane
			} else {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf(
					"Error: session %s is not available — message not sent", msg.SessionID)})
				return
			}
		}
		*pane = target
	}
	s.handleWSUserMessage(ws, r, pane, msg)
}
