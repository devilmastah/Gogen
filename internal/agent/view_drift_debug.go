//go:build debug

package agent

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"strings"

	"gogen/internal/debuglog"
	"gogen/internal/llm"
	"gogen/internal/textutil"
)

// ViewDriftCompiledIn reports whether this binary includes the view-drift
// detector. Production builds (!debug) always return false.
func ViewDriftCompiledIn() bool { return true }

// recordViewForDrift compares the current wire view against the previous
// snapshot (when enabled) and stores a deep copy for the next turn.
func (a *Agent) recordViewForDrift(view []llm.Message) {
	if !a.DebugCompareMessages {
		return
	}
	if len(a.lastViewMessages) > 0 {
		a.reportViewDrift(view, viewDriftTurn, "", "")
	}
	a.lastViewMessages = cloneViewMessages(view)
}

// compareViewOnRestore compares the previous session's wire view against the
// restored session and snapshots the restored view for later turn drift.
func (a *Agent) compareViewOnRestore(prevSessionID, newSessionID string) {
	if !a.DebugCompareMessages {
		a.lastViewMessages = nil
		return
	}
	prevView := a.lastViewMessages
	view := a.wireViewForDebug()
	if len(prevView) > 0 {
		a.reportViewDrift(view, viewDriftSessionRestore, prevSessionID, newSessionID)
	}
	a.lastViewMessages = cloneViewMessages(view)
}

// clearViewDriftSnapshot drops any stored wire-view snapshot.
func (a *Agent) clearViewDriftSnapshot() {
	a.lastViewMessages = nil
}

// wireViewForDebug builds the LLM wire view from current agent state without
// compaction or updating the drift snapshot.
func (a *Agent) wireViewForDebug() []llm.Message {
	if a.Context != nil {
		// In-place rewrite of oversized tool bodies — exclude concurrent
		// ContextStats/SnapshotMessages clones (see prepareMessages).
		a.statsMu.Lock()
		a.Context.EnsureToolResultsCapped(a.Messages)
		a.statsMu.Unlock()
	}
	a.stabilizeToolArgs()
	view := buildSystemView(a.Messages, a.systemPromptBase(a.WorkingDir), a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode, a.mcpToolHint())
	return view
}

type viewDriftKind string

const (
	viewDriftTurn           viewDriftKind = "turn"
	viewDriftSessionRestore viewDriftKind = "session-restore"
)

// reportViewDrift detects prefix drift that would bust provider prompt-cache
// hits. Append-only growth is fine (shared prefix stays valid). Turn drift is
// logged only on mismatch; session-restore always logs a summary so switching
// sessions is visible even when the shared prefix is empty or coincidental.
func (a *Agent) reportViewDrift(current []llm.Message, kind viewDriftKind, prevSessionID, newSessionID string) {
	overlap := len(current)
	if len(a.lastViewMessages) < overlap {
		overlap = len(a.lastViewMessages)
	}
	mismatchIdx := -1
	for i := 0; i < overlap; i++ {
		if !messagesEqual(&current[i], &a.lastViewMessages[i]) {
			mismatchIdx = i
			break
		}
	}
	matchedPrefix := overlap
	if mismatchIdx >= 0 {
		matchedPrefix = mismatchIdx
	}
	if mismatchIdx < 0 && kind != viewDriftSessionRestore {
		return // Shared prefix unchanged — cache-safe for in-session turns.
	}

	fields := map[string]any{
		"kind":               string(kind),
		"turnNewCount":       len(current),
		"turnPrevCount":      len(a.lastViewMessages),
		"matchedPrefixLen":   matchedPrefix,
		"firstMismatchIndex": mismatchIdx,
	}
	if prevSessionID != "" {
		fields["prevSessionID"] = prevSessionID
	}
	if newSessionID != "" {
		fields["newSessionID"] = newSessionID
	}
	if len(a.lastViewMessages) > 0 && len(current) > 0 {
		fields["systemHashPrev"] = messageWireHash(&a.lastViewMessages[0])
		fields["systemHashNew"] = messageWireHash(&current[0])
	}
	if mismatchIdx >= 0 {
		prevMsg := a.lastViewMessages[mismatchIdx]
		newMsg := current[mismatchIdx]
		fields["mismatchHashPrev"] = messageWireHash(&prevMsg)
		fields["mismatchHashNew"] = messageWireHash(&newMsg)
		fields["prevRole"] = prevMsg.Role
		fields["newRole"] = newMsg.Role
		fields["prevContentLen"] = len(prevMsg.Content)
		fields["newContentLen"] = len(newMsg.Content)
		fields["prevReasoningLen"] = len(prevMsg.Reasoning)
		fields["newReasoningLen"] = len(newMsg.Reasoning)
		fields["prevRefusalLen"] = len(prevMsg.Refusal)
		fields["newRefusalLen"] = len(newMsg.Refusal)
		fields["prevToolCalls"] = len(prevMsg.ToolCalls)
		fields["newToolCalls"] = len(newMsg.ToolCalls)
		fields["prevArgsStrLen"] = toolCallsArgsStrLen(prevMsg.ToolCalls)
		fields["newArgsStrLen"] = toolCallsArgsStrLen(newMsg.ToolCalls)
		fields["prevToolCallID"] = prevMsg.ToolCallID
		fields["newToolCallID"] = newMsg.ToolCallID
		// Previews + field-level diff so a mid-prefix llama.cpp break can be
		// mapped back to the exact message/bytes that changed.
		fields["changedFields"] = messageChangedFields(&prevMsg, &newMsg)
		fields["prevContentPreview"] = driftPreview(prevMsg.Content)
		fields["newContentPreview"] = driftPreview(newMsg.Content)
		if prevMsg.Reasoning != "" || newMsg.Reasoning != "" {
			fields["prevReasoningPreview"] = driftPreview(prevMsg.Reasoning)
			fields["newReasoningPreview"] = driftPreview(newMsg.Reasoning)
		}
		if prevMsg.Refusal != "" || newMsg.Refusal != "" {
			fields["prevRefusalPreview"] = driftPreview(prevMsg.Refusal)
			fields["newRefusalPreview"] = driftPreview(newMsg.Refusal)
		}
		if len(prevMsg.ToolCalls) > 0 || len(newMsg.ToolCalls) > 0 {
			fields["prevToolCallsPreview"] = toolCallsDriftPreview(prevMsg.ToolCalls)
			fields["newToolCallsPreview"] = toolCallsDriftPreview(newMsg.ToolCalls)
		}
		if mismatchIdx > 0 {
			fields["beforeHashPrev"] = messageWireHash(&a.lastViewMessages[mismatchIdx-1])
			fields["beforeHashNew"] = messageWireHash(&current[mismatchIdx-1])
		}
		if mismatchIdx+1 < len(a.lastViewMessages) && mismatchIdx+1 < len(current) {
			fields["afterHashPrev"] = messageWireHash(&a.lastViewMessages[mismatchIdx+1])
			fields["afterHashNew"] = messageWireHash(&current[mismatchIdx+1])
		}
	}

	msg := "message view prefix changed between turns"
	event := "view-mismatch"
	if kind == viewDriftSessionRestore {
		msg = "message view compared across session restore"
		event = "session-restore-view"
		if mismatchIdx < 0 {
			msg = "session restore shares full overlapping prefix with previous view"
		}
	}
	debuglog.Write("agent/view-drift", msg, event, fields)
}

// compareViewFingerprints is the in-session turn entry point (tests).
func (a *Agent) compareViewFingerprints(current []llm.Message) {
	a.reportViewDrift(current, viewDriftTurn, "", "")
}

func toolCallsArgsStrLen(tcs []llm.ToolCall) int {
	n := 0
	for i := range tcs {
		n += len(tcs[i].ArgsStr)
	}
	return n
}

const driftPreviewMaxRunes = 160

func driftPreview(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\t", "\\t")
	// The marker sits beyond the budget (like clipRunes): this is a log/diff
	// preview, not a fixed-width field.
	if t := textutil.TruncateRunes(s, driftPreviewMaxRunes); t != s {
		return t + "…"
	}
	return s
}

func messageChangedFields(prev, cur *llm.Message) []string {
	var out []string
	if prev.Role != cur.Role {
		out = append(out, "role")
	}
	if prev.Content != cur.Content {
		out = append(out, "content")
	}
	if prev.Reasoning != cur.Reasoning {
		out = append(out, "reasoning")
	}
	if prev.Refusal != cur.Refusal {
		out = append(out, "refusal")
	}
	if prev.ToolCallID != cur.ToolCallID {
		out = append(out, "toolCallID")
	}
	if !toolCallsWireEqual(prev.ToolCalls, cur.ToolCalls) {
		out = append(out, "toolCalls")
	}
	return out
}

func toolCallsWireEqual(a, b []llm.ToolCall) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Name != b[i].Name || a[i].ArgsStr != b[i].ArgsStr {
			return false
		}
	}
	return true
}

func toolCallsDriftPreview(tcs []llm.ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(tcs))
	for i := range tcs {
		out = append(out, map[string]any{
			"id":             tcs[i].ID,
			"name":           tcs[i].Name,
			"argsStrLen":     len(tcs[i].ArgsStr),
			"argsStrValid":   jsonValid(tcs[i].ArgsStr),
			"argsStrPreview": driftPreview(tcs[i].ArgsStr),
		})
	}
	return out
}

func jsonValid(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && json.Valid([]byte(s))
}

// messageWireHash returns a short, stable hex digest of the fields that affect
// the serialized LLM request (the same set messagesEqual guards).
func messageWireHash(m *llm.Message) string {
	h := fnv.New64a()
	h.Write([]byte(m.Role))
	h.Write([]byte{0})
	h.Write([]byte(m.Content))
	h.Write([]byte{0})
	h.Write([]byte(m.Reasoning))
	h.Write([]byte{0})
	h.Write([]byte(m.Refusal))
	h.Write([]byte{0})
	h.Write([]byte(m.ToolCallID))
	for i := range m.ToolCalls {
		h.Write([]byte{0})
		h.Write([]byte(m.ToolCalls[i].ID))
		h.Write([]byte{0})
		h.Write([]byte(m.ToolCalls[i].Name))
		h.Write([]byte{0})
		h.Write([]byte(m.ToolCalls[i].ArgsStr))
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], h.Sum64())
	return hex.EncodeToString(b[:])
}

// cloneViewMessages deep-copies messages for drift detection. ToolCalls must
// be cloned so later ArgsStr pinning on a.Messages cannot silently update the
// previous-turn snapshot (shared slice backing).
func cloneViewMessages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i := range msgs {
		out[i] = msgs[i]
		if len(msgs[i].ToolCalls) == 0 {
			continue
		}
		out[i].ToolCalls = make([]llm.ToolCall, len(msgs[i].ToolCalls))
		copy(out[i].ToolCalls, msgs[i].ToolCalls)
	}
	return out
}

// messagesEqual compares fields that affect the serialized LLM request.
func messagesEqual(a, b *llm.Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.Reasoning != b.Reasoning || a.Refusal != b.Refusal || a.ToolCallID != b.ToolCallID {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID ||
			a.ToolCalls[i].Name != b.ToolCalls[i].Name ||
			a.ToolCalls[i].ArgsStr != b.ToolCalls[i].ArgsStr {
			return false
		}
	}
	return true
}
