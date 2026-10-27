package contextmgr

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"gogen/internal/llm"

	"github.com/tiktoken-go/tokenizer"
)

var (
	encOnce sync.Once
	codec   tokenizer.Codec
	encErr  error
)

func getCodec() (tokenizer.Codec, error) {
	encOnce.Do(func() {
		codec, encErr = tokenizer.Get(tokenizer.Cl100kBase)
	})
	return codec, encErr
}

// WarmTokenizer eagerly loads the cl100k_base tokenizer vocabulary so the
// first token-counting call does not pay the ~2.6 MB init cost inline.
func WarmTokenizer() {
	_, _ = getCodec()
}

// CountTokens returns per-message token counts for the given messages.
// Token counts are computed fresh each time; the cl100k_base tokenizer
// is fast enough that a global cache added more complexity than value.
//
// It resolves the counting strategy once and shares a single memo across the
// whole slice: repeated strings (system prompt, tool-call names and IDs,
// repeated argument values) encode once instead of once per message. This is
// the batched form of ComputeMessageTokens and is what cold paths (restore,
// first probe, compaction republish, session save) should use — looping
// ComputeMessageTokens per message would resolve the tokenizer and walk the
// structure once per message with zero cross-message dedup.
func CountTokens(messages []llm.Message) []int {
	if len(messages) == 0 {
		return nil
	}
	count := messageCounterFor()
	// Resolved once per pass (an env lookup, not a per-message cost).
	countReasoning := llm.ReasoningEchoEnabled()
	// One memo per pass: repeated strings encode once instead of once per
	// message.
	memo := make(map[string]int)
	counts := make([]int, len(messages))
	for i := range messages {
		counts[i] = computeMessageTokens(messages[i], count, memo, countReasoning) + imageTokenEstimate(messages[i])
	}
	return counts
}

// TokenCounts is the Manager-scoped alias of CountTokens, kept for callers
// that already hold a Manager. The counting itself does not use the Manager.
func (m *Manager) TokenCounts(messages []llm.Message) []int {
	return CountTokens(messages)
}

// ComputeMessageTokens returns the estimated token count for a single message
// using the cl100k_base tokenizer when available, falling back to a bytes/4 heuristic.
func ComputeMessageTokens(msg llm.Message) int {
	return computeMessageTokens(msg, messageCounterFor(), nil, llm.ReasoningEchoEnabled()) + imageTokenEstimate(msg)
}

func sortedToolArgKeys(args map[string]any) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// EstimateTokens approximates token count for a message list using cl100k_base
// (GPT-family). Falls back to a bytes/4 heuristic if the tokenizer is unavailable.
// No caching; the tokenizer is fast enough for on-demand use. Sums the
// per-message counts directly (no []int allocation, unlike TokenCounts).
func (m *Manager) EstimateTokens(messages []llm.Message) int {
	if len(messages) == 0 {
		return 0
	}
	count := messageCounterFor()
	countReasoning := llm.ReasoningEchoEnabled()
	memo := make(map[string]int)
	total := 0
	for i := range messages {
		total += computeMessageTokens(messages[i], count, memo, countReasoning) + imageTokenEstimate(messages[i])
	}
	return total
}

// ToolDefinitionString serializes a single tool definition in a stable,
// wire-like form: type, name, description, and JSON parameters
// (encoding/json sorts map keys, so the output is deterministic regardless
// of map construction order). EstimateToolTokens defines a tool definition's
// token cost through it, so this string is the canonical "what a definition
// is" for token accounting.
func ToolDefinitionString(t llm.Tool) string {
	var b strings.Builder
	b.WriteString(t.Type)
	b.WriteString(t.Name)
	b.WriteString(t.Description)
	if len(t.Parameters) > 0 {
		if raw, err := json.Marshal(t.Parameters); err == nil {
			b.Write(raw)
		} else {
			// Unmarshalable values (channels, funcs): fall back to a
			// deterministic key/value dump so the string is still stable.
			for _, k := range sortedToolArgKeys(t.Parameters) {
				b.WriteString(k)
				b.WriteString(fmt.Sprint(t.Parameters[k]))
			}
		}
	}
	return b.String()
}

// EstimateToolTokens estimates the wire token cost of a set of tool
// definitions: every definition serialized through ToolDefinitionString and
// counted through the same messageCounterFor path as messages, plus a small
// per-tool framing allowance. The per-pass memo is keyed by the full
// definition string, so it only deduplicates identical definitions.
// Overestimating is safe: it only makes compaction run slightly earlier.
func EstimateToolTokens(tools []llm.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	count := messageCounterFor()
	memo := make(map[string]int)
	// Per-pass memo: identical definitions encode once instead of once
	// per tool.
	c := func(s string) int {
		if v, ok := memo[s]; ok {
			return v
		}
		v := count(s)
		memo[s] = v
		return v
	}
	total := 0
	for _, t := range tools {
		total += 4 // per-tool framing overhead
		total += c(ToolDefinitionString(t))
	}
	return total
}

// imageTokenEstimate is a conservative flat per-image token estimate.
// OpenAI bills vision input at 85 base tokens + 170 per 512x512 tile (high
// detail); without decoding dimensions from the data URL, 1024 tokens per
// image comfortably covers typical screenshots. Overestimating only makes
// compaction run slightly earlier — underestimating would risk overflowing
// the context window.
func imageTokenEstimate(msg llm.Message) int {
	return 1024 * len(msg.Images)
}

// messageCounter counts the tokens in a single string. Both counting
// strategies (tokenizer-backed exact and bytes/4 heuristic) implement this so
// computeMessageTokens can walk the message structure exactly once.
type messageCounter func(string) int

// messageCounterFor resolves the counting strategy once per call: the
// cl100k_base tokenizer when available, else the bytes/4 heuristic.
func messageCounterFor() messageCounter {
	if count, ok := exactMessageCounter(); ok {
		return count
	}
	return HeuristicTokenCount
}

// exactMessageCounter returns a tokenizer-backed messageCounter, or ok=false
// when the tokenizer is unavailable. An encode failure for a single string
// counts that string as 0 tokens rather than failing the whole message.
func exactMessageCounter() (messageCounter, bool) {
	c, err := getCodec()
	if err != nil || c == nil {
		return nil, false
	}
	return func(s string) int {
		ids, _, err := c.Encode(s)
		if err != nil {
			return 0
		}
		return len(ids)
	}, true
}

// HeuristicTokenCount approximates tokens as bytes/4 for a single string.
// It is the tokenizer-free fallback of EstimateTokens and is also the
// shared cheap estimator for live UI indicators (the TUI streaming
// context estimate) that must not pay tokenizer cost mid-stream.
func HeuristicTokenCount(s string) int {
	return (len(s) + 3) / 4
}

// LiveEstimate is a cheap running context estimate: an authoritative
// baseline (the Used count of the last exact ContextStats snapshot) plus a
// tokenizer-free count of text streamed since the last Rebase. It backs the
// live "(est.)" context indicator in UIs that must not pay tokenizer cost
// mid-stream; the exact count is restored from ContextStats when streaming
// ends or at the next round boundary.
//
// Not synchronized: drive it from a single goroutine only (the TUI's Update
// thread, or the server's stream callback).
type LiveEstimate struct {
	base  int // last authoritative Used
	added int // HeuristicTokenCount of deltas since the last Rebase
}

// Rebase sets the baseline to the given authoritative Used count and
// discards any accumulated estimate.
func (e *LiveEstimate) Rebase(used int) {
	e.base = used
	e.added = 0
}

// Add accumulates a heuristic token count for a chunk of streamed text.
// Empty chunks are ignored.
func (e *LiveEstimate) Add(delta string) {
	if delta == "" {
		return
	}
	e.added += HeuristicTokenCount(delta)
}

// Used returns the current estimate: baseline plus accumulated deltas.
func (e *LiveEstimate) Used() int {
	return e.base + e.added
}

// Base returns the authoritative baseline set by the last Rebase.
func (e *LiveEstimate) Base() int {
	return e.base
}

// computeMessageTokens walks the message structure once and counts every
// string through count: content, reasoning, refusal, each tool call's
// name/id/arguments, and the tool-call id. Both the exact and heuristic
// strategies share this single walk, so what gets counted can never drift
// between them. When memo is non-nil, repeated strings are counted once per
// pass and the result reused — token counts are a pure function of the
// string, so this is exact.
func computeMessageTokens(msg llm.Message, count messageCounter, memo map[string]int, countReasoning bool) int {
	c := count
	if memo != nil {
		c = func(s string) int {
			if v, ok := memo[s]; ok {
				return v
			}
			v := count(s)
			memo[s] = v
			return v
		}
	}
	tokens := 4 // role/message framing overhead
	tokens += c(msg.Content)
	// Reasoning only counts when it will actually be sent: with
	// GOGEN_REASONING_ECHO=off the provider never sees historical reasoning, so
	// counting it made the estimate (and therefore compaction) fire early —
	// reasoning can be a third of a reasoning-heavy session.
	if countReasoning && msg.Reasoning != "" {
		tokens += c(msg.Reasoning)
	}
	tokens += c(msg.Refusal)
	for _, tc := range msg.ToolCalls {
		tokens += 4
		tokens += c(tc.Name)
		tokens += c(tc.ID)
		if tc.ArgsStr != "" {
			tokens += c(tc.ArgsStr)
		} else {
			for _, k := range sortedToolArgKeys(tc.Args) {
				v := tc.Args[k]
				tokens += c(k)
				tokens += c(fmt.Sprint(v))
				tokens += 2
			}
		}
	}
	if msg.ToolCallID != "" {
		tokens += c(msg.ToolCallID)
	}
	return tokens
}

func truncateForSummary(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}
	c, err := getCodec()
	if err == nil && c != nil {
		ids, _, err := c.Encode(text)
		if err == nil {
			if len(ids) <= maxTokens {
				return text
			}
			decoded, derr := c.Decode(ids[:maxTokens])
			if derr == nil {
				return decoded + fmt.Sprintf("\n… truncated for summarization (%d tokens total)", len(ids))
			}
		}
	}
	return truncateForSummaryHeuristic(text, maxTokens)
}

// truncateForSummaryHeuristic is the bytes/4 fallback for
// truncateForSummary, used when the tokenizer is unavailable. The cut is
// rune-safe so the result is always valid UTF-8.
func truncateForSummaryHeuristic(text string, maxTokens int) string {
	return Truncate(text, maxTokens*4, TruncateOptions{
		Marker: fmt.Sprintf("\n… truncated for summarization (%d chars total)", len(text)),
	})
}
