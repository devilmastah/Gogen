package server

import (
	"context"
	"fmt"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

func sessionEntries(list []agent.SessionInfo, active, turnActive, registered map[string]bool) []SessionEntry {
	out := make([]SessionEntry, 0, len(list))
	for _, s := range list {
		out = append(out, SessionEntry{
			ID:              s.ID,
			UpdatedAt:       s.UpdatedAt,
			MessageCount:    s.MessageCount,
			Label:           s.Label,
			Oneshot:         s.Oneshot,
			ParentID:        s.ParentID,
			Active:          active[s.ID],
			TurnActive:      turnActive[s.ID],
			Registered:      registered[s.ID],
			SubagentStatus:  s.SubagentStatus,
			SubagentSummary: s.SubagentSummary,
		})
	}
	return out
}

// activeSet returns the session ids that are genuinely live for the
// sessions payload's "resume to continue" indicator: a runtime with at
// least one attached VIEWER (open as a pane in some tab) or a running
// turn. Merely being registered is not enough — the restored default
// session and passive (approval-only) attachments must not pin the
// indicator for a session nobody is viewing (README: the indicator only
// appears for sessions open in another tab or with a turn running
// server-side). Ids are snapshotted under the registry lock and each
// runtime's state read without it (no lock ordering with clientsMu /
// stateMu), mirroring turnActiveSet.
func (r *sessionRegistry) activeSet() map[string]bool {
	ids := r.activeIDs()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		rt, ok := r.get(id)
		if !ok {
			continue
		}
		if rt.viewerCount() > 0 {
			out[id] = true
			continue
		}
		if active, _ := rt.turnState(); active {
			out[id] = true
		}
	}
	return out
}

// registeredSet returns the ids of every live runtime in this process,
// regardless of viewers or turn state. The sessions payload uses it to let a
// client tell a child whose runtime is live but nobody is watching (a
// background subagent between turns, held for its retention window) from one
// whose runtime is gone: the first is idle-but-continuable, the second never
// finished. activeSet deliberately excludes the unattended case, so it cannot
// answer that question. Ids are snapshotted under the registry lock.
func (r *sessionRegistry) registeredSet() map[string]bool {
	ids := r.activeIDs()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// turnActiveSet returns the ids of registered runtimes that currently have
// a running turn. The sessions payload uses it so the client can tell a
// genuinely running session ("responding") from one that is merely
// registered-but-idle (open as a pane, or resumed from the store) — the
// plain active set cannot. Ids are snapshotted under the registry lock and
// each runtime's turn state read without it (no lock ordering with stateMu).
func (r *sessionRegistry) turnActiveSet() map[string]bool {
	ids := r.activeIDs()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		rt, ok := r.get(id)
		if !ok {
			continue
		}
		if active, _ := rt.turnState(); active {
			out[id] = true
		}
	}
	return out
}

func historyEntries(msgs []llm.Message) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(msgs))
	for idx, m := range msgs {
		// Empty placeholder messages are not shipped (shared with
		// agent.HistoryFingerprint so the conditional attach's skip decision
		// always agrees with what this builder would have sent).
		if !agent.HistoryShips(m) {
			continue
		}
		createdAt := ""
		if !m.CreatedAt.IsZero() {
			createdAt = m.CreatedAt.UTC().Format(time.RFC3339Nano)
		}

		switch m.Role {
		case "user":
			out = append(out, HistoryEntry{Role: m.Role, Content: m.Content, Images: m.Images, Index: idx, CreatedAt: createdAt})
		case "assistant":
			entry := HistoryEntry{
				Role:      m.Role,
				Content:   m.Content,
				Reasoning: m.Reasoning,
				Refusal:   m.Refusal,
				Model:     m.Model,
				Index:     idx,
				CreatedAt: createdAt,
			}
			if len(m.ToolCalls) > 0 {
				entry.ToolCalls = make([]HistoryToolCall, len(m.ToolCalls))
				for i, tc := range m.ToolCalls {
					entry.ToolCalls[i] = HistoryToolCall{
						ID:   tc.ID,
						Name: tc.Name,
						Args: tc.Args,
					}
				}
			}
			out = append(out, entry)

		case "tool":
			out = append(out, HistoryEntry{
				Role:       m.Role,
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
				Index:      idx,
				CreatedAt:  createdAt,
			})
		}
	}
	return out
}

func (s *Server) writeSessionCommandResult(ws *wsConn, ctx context.Context, rt *sessionRuntime, result agent.SessionCommandResult, err error) {
	a := rt.agent
	resp := WSMessage{Type: "response"}
	clearChat := false
	if err != nil {
		resp.Content = fmt.Sprintf("Error: %v", err)
	} else if result.Output == "" && result.Action == "" && len(result.History) == 0 && len(result.Sessions) == 0 {
		// The command already shipped its own payload: a successful resume
		// writes the ATTACH sequence itself (session_state → history →
		// config; see sessionResume/sendAttachPayload), so there is nothing
		// left to reply here. An empty response frame is meaningless to
		// clients; errors never take this path (they must still be
		// reported). Every other session command (new/fork/sessions/delete)
		// returns a non-empty result.
		return
	} else {
		resp.Content = result.Output
		if result.Action == agent.SessionActionClearChat {
			resp.SessionAction = string(result.Action)
			clearChat = true
		}
	}

	var cfg WSMessage
	var history []llm.Message
	needHistory := clearChat && err == nil && len(result.History) == 0
	// No turnMu here: everything below is already-computed or internally
	// synchronized (sessionEntries reads the registry under its own lock;
	// the config snapshot is lock-free; result.History came from the
	// command). A resume of a session with a RUNNING turn must deliver its
	// reply immediately — the turn holds turnMu for its entire duration,
	// and blocking here is exactly the "can't switch to the responding
	// session until it's done" symptom.
	if err == nil && len(result.Sessions) > 0 {
		resp.Type = "sessions"
		resp.Sessions = sessionEntries(result.Sessions, s.registry.activeSet(), s.registry.turnActiveSet(), s.registry.registeredSet())
	}
	cfg = agentConfigMsgBasic(a)
	s.decorateConfig(&cfg)
	if len(result.History) > 0 {
		history = append([]llm.Message(nil), result.History...)
	}
	if needHistory {
		// /new (and any clear with empty History) — still emit history so
		// clients can reliably run post-session follow-ups (e.g. resend).
		history = a.SnapshotMessages()
	}
	// The sessions payload is connection-scoped sidebar state; leave its
	// SessionID empty so the client routes it to the active pane instead of
	// tying it to one session (which could drop it after a reconnect or a
	// cross-tab default change).
	if resp.Type != "sessions" {
		resp.SessionID = cfg.SessionID
	}
	resp.Mode = cfg.Mode
	// Paint sessions/history before ContextStats tokenization (can be slow on
	// large restored sessions / cold tiktoken init). clear_chat + history
	// carry the sessionId so the client routes them to the right pane.
	_ = ws.writeJSON(resp)
	if clearChat && err == nil {
		_ = ws.writeJSON(WSMessage{Type: "clear_chat", SessionID: cfg.SessionID})
	}
	// Always emit history on a clear (even empty) so clients can reliably
	// run post-session follow-ups (e.g. resend).
	if (clearChat && err == nil) || len(history) > 0 {
		_ = ws.writeJSON(WSMessage{
			Type:         "history",
			History:      historyEntries(history),
			HistoryEpoch: rt.agent.HistoryEpoch(),
			// Same as attach: a resumed session with a running turn carries
			// its in-flight reply here (nil between rounds).
			Rewind:    rt.live.Snapshot(),
			SessionID: cfg.SessionID,
		})
	}
	// Context stats (tokenization) can take seconds on a large uncached
	// session; compute them off the read loop like every other handler so a
	// slow probe cannot block this connection's messages (including cancel).
	// The response/history above were already enqueued, so the send-queue
	// FIFO keeps the ordering stable.
	go func() {
		stats := a.ContextStats(ctx)
		accum := a.SnapshotUsageAccum()
		applyContextStats(&cfg, stats, &accum)
		fillModelPricing(a, &cfg)
		ctxMsg := WSMessage{Type: "context"}
		applyContextStats(&ctxMsg, stats, &accum)
		_ = ws.writeJSON(ctxMsg)
		_ = ws.writeJSON(cfg)
	}()
}

func (s *Server) modelEntries(models []llm.ModelInfo) []ModelEntry {
	out := make([]ModelEntry, len(models))
	for i, m := range models {
		out[i] = ModelEntry{
			ID:               m.ID,
			ContextLimit:     m.ContextLimit,
			Current:          m.Current,
			Provider:         m.Provider,
			InputPricePer1M:  m.InputPricePer1M,
			OutputPricePer1M: m.OutputPricePer1M,
			CachedPricePer1M: m.CachedPricePer1M,
			ReasoningEfforts: m.ReasoningEfforts,
			Description:      m.Description,
		}
	}
	return out
}

// sendSessionState writes the session_state message describing the session's
// in-flight turn so a reconnecting client can render "resuming…". It also
// carries the queued steering messages so an attaching/reconnecting client
// seeds the queued UI from server truth (the same snapshot every attached
// client got via queue_update).
func (s *Server) sendSessionState(ws *wsConn, rt *sessionRuntime) {
	active, _ := rt.turnState()
	_ = ws.writeJSON(WSMessage{
		Type:       "session_state",
		SessionID:  rt.agent.SessionID,
		TurnActive: active,
		Queue:      rt.queueItems(),
	})
}
