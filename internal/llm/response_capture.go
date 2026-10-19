package llm

import (
	"encoding/json"
	"strings"

	"gogen/internal/debuglog"

	"github.com/openai/openai-go/v3"
)

type extraFieldAccums map[string]*strings.Builder

func newExtraFieldAccums() extraFieldAccums {
	return make(extraFieldAccums)
}

var streamDisplayExtraFields = map[string]bool{
	"reasoning_content": true,
	"reasoning":         true,
	"thinking":          true,
	"thought":           true,
	"analysis":          true,
}

func extraFieldShouldDisplay(key string) bool {
	if streamDisplayExtraFields[key] {
		return true
	}
	lower := strings.ToLower(key)
	return strings.Contains(lower, "reason") ||
		strings.Contains(lower, "think") ||
		strings.Contains(lower, "thought")
}

func (a extraFieldAccums) addFromDelta(delta openai.ChatCompletionChunkChoiceDelta, onThinking func(string), fullReasoning *strings.Builder) {
	emit := a.thinkingEmitter(onThinking, fullReasoning)
	// Every non-standard field a provider emits is surfaced in
	// delta.JSON.ExtraFields — including null and type-mismatched values
	// (apijson records those as !Valid() with the raw JSON still available).
	// Nulls are skipped by ingestPiece; type-mismatched values (e.g. a
	// provider sending reasoning as a JSON object) are still decodable by
	// decodeJSONFieldText. Re-parsing delta.RawJSON() as a fallback is
	// therefore redundant — it was a full json.Unmarshal of every chunk on
	// standard OpenAI streams, where ExtraFields is empty and the parse only
	// rediscovered the known fields it was written to skip.
	for key, field := range delta.JSON.ExtraFields {
		a.ingestPiece(key, field.Raw(), emit)
	}
}

// thinkingEmitter deduplicates reasoning/thinking pieces within a single delta.
// Some providers emit the same text under multiple keys (e.g. reasoning_content
// and reasoning) in one chunk; emitting each copy produces the interleaved
// "Now I have aNow I have a" duplication seen in the TUI.
func (a extraFieldAccums) thinkingEmitter(onThinking func(string), fullReasoning *strings.Builder) func(key, piece string) {
	var seen map[string]struct{}
	return func(key, piece string) {
		if !extraFieldShouldDisplay(key) || piece == "" {
			return
		}
		if seen == nil {
			seen = make(map[string]struct{})
		}
		if _, dup := seen[piece]; dup {
			return
		}
		seen[piece] = struct{}{}
		if fullReasoning != nil {
			fullReasoning.WriteString(piece)
		}
		if onThinking != nil {
			onThinking(piece)
		}
	}
}

func (a extraFieldAccums) ingestPiece(key, raw string, emit func(key, piece string)) {
	if raw == "" || raw == "null" {
		return
	}
	piece := decodeJSONFieldText(raw)
	if piece == "" {
		return
	}
	if a[key] == nil {
		a[key] = &strings.Builder{}
	}
	a[key].WriteString(piece)
	if emit != nil {
		emit(key, piece)
	}
}

func decodeJSONFieldText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		for _, nestedKey := range []string{"text", "content", "value", "data", "reasoning", "thinking"} {
			if v, ok := obj[nestedKey]; ok {
				if text := decodeJSONFieldText(string(v)); text != "" {
					return text
				}
			}
		}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		var parts []string
		for _, item := range arr {
			if text := decodeJSONFieldText(string(item)); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	}
	return raw
}

func (a extraFieldAccums) primaryDisplayText() string {
	for _, key := range []string{"reasoning_content", "reasoning", "thinking", "thought", "analysis"} {
		if b := a[key]; b != nil {
			if s := strings.TrimSpace(b.String()); s != "" {
				return s
			}
		}
	}
	for key, b := range a {
		if extraFieldShouldDisplay(key) && b != nil {
			if s := strings.TrimSpace(b.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

func (a extraFieldAccums) textLen() int {
	total := 0
	for _, b := range a {
		total += b.Len()
	}
	return total
}

func (a extraFieldAccums) snapshot() map[string]string {
	if len(a) == 0 {
		return nil
	}
	out := make(map[string]string, len(a))
	for k, b := range a {
		if s := strings.TrimSpace(b.String()); s != "" {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func primaryDisplayFromExtrasMap(extras map[string]string) string {
	if len(extras) == 0 {
		return ""
	}
	acc := newExtraFieldAccums()
	for k, v := range extras {
		if acc[k] == nil {
			acc[k] = &strings.Builder{}
		}
		acc[k].WriteString(v)
	}
	return acc.primaryDisplayText()
}

func extraFieldsFromMessage(msg openai.ChatCompletionMessage) map[string]string {
	out := make(map[string]string)
	for key, field := range msg.JSON.ExtraFields {
		if !field.Valid() {
			continue
		}
		if text := decodeJSONFieldText(field.Raw()); text != "" {
			out[key] = text
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func logNonStreamResponse(model, source string, content, refusal, displayContent string, extras map[string]string, toolCalls []ToolCall, usage *Usage) {
	debuglog.WriteLLMResponse(debuglog.LLMResponseRecord{
		Model:          model,
		Source:         source,
		Content:        content,
		Refusal:        refusal,
		DisplayContent: displayContent,
		Reasoning:      primaryDisplayFromExtrasMap(extras),
		ExtraFields:    extras,
		ToolCalls:      toolCallRecords(toolCalls),
		Usage:          usageRecord(usage),
	})
}

// toolCallRecords shapes tool calls for the response log (index/id/name,
// parsed args when present, and the exact wire bytes as argsJson).
func toolCallRecords(toolCalls []ToolCall) []debuglog.LLMToolCallRecord {
	tools := make([]debuglog.LLMToolCallRecord, 0, len(toolCalls))
	for _, tc := range toolCalls {
		argsJSON, _ := json.Marshal(tc.Args)
		tools = append(tools, debuglog.LLMToolCallRecord{
			Index:    tc.Index,
			ID:       tc.ID,
			Name:     tc.Name,
			Args:     tc.Args,
			ArgsJSON: string(argsJSON),
		})
	}
	return tools
}

// usageRecord shapes usage for the response log; nil becomes an absent map.
func usageRecord(usage *Usage) map[string]int {
	if usage == nil {
		return nil
	}
	return map[string]int{
		"promptTokens":     usage.PromptTokens,
		"completionTokens": usage.CompletionTokens,
		"totalTokens":      usage.TotalTokens,
		"cachedTokens":     usage.CachedTokens,
	}
}

// logFallbackResponse records a successful non-streaming fallback in the
// response log: Source "stream-fallback" with UsedFallback set and the
// stream error that forced the recovery, so offline review can see both
// the recovered round and why it was not delivered as a stream. The record
// vocabulary matches LLMResponseRecord.Source ("stream-fallback"); plain
// stream and non-stream successes stay unlogged (logNonStreamResponse is
// only wired on the non-stream path).
func logFallbackResponse(model string, streamErr error, resp Response) {
	debuglog.WriteLLMResponse(debuglog.LLMResponseRecord{
		Model:        model,
		Source:       "stream-fallback",
		UsedFallback: true,
		Error:        clipErrForLog(streamErr),
		Content:      resp.Content,
		Refusal:      resp.Refusal,
		Reasoning:    resp.Reasoning,
		ToolCalls:    toolCallRecords(resp.ToolCalls),
		Usage:        usageRecord(resp.Usage),
	})
}

// clipErrForLog bounds an error string for the JSON logs: stream errors can
// embed the provider's error message (the SDK error text carries it via
// apiErrorSurface), which would otherwise bloat every record with the same
// oversized blob.
func clipErrForLog(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const max = 1024
	if len(s) > max {
		s = s[:max]
	}
	return s
}
