package llm

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
)

type tcAccum struct {
	Index   int
	ID      string
	Name    string
	ArgsStr string
	Started bool
	// argsFinalized is cached once ArgsStr is verified to be a complete JSON
	// object via toolArgsFullyReceived. Set at accumulator creation and reset
	// to false whenever ArgsStr grows; applyToolCallDelta passes it to
	// mergeToolArgsDelta so full-object replays are ignored without
	// re-validating the buffer. The cheap structural pre-check
	// (argsStructurallyComplete) avoids re-validating an incomplete buffer on
	// every streamed argument fragment.
	argsFinalized bool
}

func toolCallDeltaArgsOnly(tc openai.ChatCompletionChunkChoiceDeltaToolCall) bool {
	return tc.ID == "" && tc.Function.Name == "" && tc.Function.Arguments != ""
}

// mergeToolCallDelta folds one streamed tool-call fragment into the accumulators.
// Local OpenAI-compatible servers often omit index on argument continuations (defaulting
// to 0), which would otherwise splice tool N's JSON onto tool 0.
func mergeToolCallDelta(
	tc openai.ChatCompletionChunkChoiceDeltaToolCall,
	tcAccums []tcAccum,
	tcIndexMap map[int]int,
) ([]tcAccum, int) {
	tcIdx := int(tc.Index)

	if toolCallDeltaArgsOnly(tc) {
		if len(tcAccums) > 0 {
			lastIdx := len(tcAccums) - 1
			// Local OpenAI-compatible servers often omit the index field on
			// argument continuations, and the decoded zero value (0) would
			// otherwise splice the fragment onto tool 0. Only apply the
			// default-index-0 splice when the wire truly omitted the index:
			// an EXPLICIT index=0 is a genuine continuation of tool 0 and
			// must route through the index map — otherwise a server
			// streaming tools 1..N in parallel with tool 0's continuation
			// would mis-splice the fragment onto the last (highest-index)
			// tool.
			if !tc.JSON.Index.Valid() {
				if last := tcAccums[lastIdx]; last.Index > tcIdx {
					return applyToolCallDelta(tcAccums, lastIdx, tc)
				}
			}
		}
		if mapIdx, ok := tcIndexMap[tcIdx]; ok {
			return applyToolCallDelta(tcAccums, mapIdx, tc)
		}
		if len(tcAccums) > 0 {
			lastIdx := len(tcAccums) - 1
			return applyToolCallDelta(tcAccums, lastIdx, tc)
		}
	}

	if mapIdx, ok := tcIndexMap[tcIdx]; ok {
		// Some servers reuse index 0 for every sequential tool call. A new
		// non-empty ID means a distinct call — do not append onto the prior one.
		if tc.ID != "" && tcAccums[mapIdx].ID != "" && tc.ID != tcAccums[mapIdx].ID {
			tcIndexMap[tcIdx] = len(tcAccums)
			return appendToolCallAccum(tcAccums, tcIdx, tc)
		}
		// A changed NAME is the same signal even when the server sends no ID:
		// folding it into the prior accumulator overwrote the name and threw
		// the second call's arguments away as a "replay" of the first, so the
		// model saw one call where it made two.
		if tc.Function.Name != "" && tcAccums[mapIdx].Name != "" && tc.Function.Name != tcAccums[mapIdx].Name {
			tcIndexMap[tcIdx] = len(tcAccums)
			return appendToolCallAccum(tcAccums, tcIdx, tc)
		}
		return applyToolCallDelta(tcAccums, mapIdx, tc)
	}

	tcIndexMap[tcIdx] = len(tcAccums)
	return appendToolCallAccum(tcAccums, tcIdx, tc)
}

// appendToolCallAccum appends a fresh accumulator for a new tool call and
// runs the completion check immediately, so argsFinalized is authoritative
// from the first fragment: a single-chunk complete arguments blob must be
// recognized as such, otherwise a subsequent differently-formatted replay
// would be spliced onto it. The check is O(1) for the common incomplete
// first fragment.
func appendToolCallAccum(tcAccums []tcAccum, idx int, tc openai.ChatCompletionChunkChoiceDeltaToolCall) ([]tcAccum, int) {
	tcAccums = append(tcAccums, tcAccum{
		Index:   idx,
		ID:      tc.ID,
		Name:    tc.Function.Name,
		ArgsStr: tc.Function.Arguments,
	})
	acc := &tcAccums[len(tcAccums)-1]
	acc.maybeFinalizeArgs()
	return tcAccums, len(tcAccums) - 1
}

func applyToolCallDelta(tcAccums []tcAccum, idx int, tc openai.ChatCompletionChunkChoiceDeltaToolCall) ([]tcAccum, int) {
	acc := &tcAccums[idx]
	if tc.ID != "" {
		acc.ID = tc.ID
	}
	if tc.Function.Name != "" {
		acc.Name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		// Pass the cached completeness verdict so the replay-ignore branch
		// does not re-validate the accumulated buffer on every fragment.
		merged := mergeToolArgsDelta(acc.ArgsStr, tc.Function.Arguments, acc.argsFinalized)
		if merged != acc.ArgsStr {
			// Buffer grew/replaced: invalidate the finalized cache so it is
			// re-evaluated lazily by maybeFinalizeArgs.
			acc.argsFinalized = false
			acc.ArgsStr = merged
		}
		acc.maybeFinalizeArgs()
	}
	return tcAccums, idx
}

// maybeFinalizeArgs runs the expensive json.Unmarshal validity check exactly
// once per accumulator and only when the raw buffer structurally looks like a
// complete single JSON object (argsStructurallyComplete — an O(1) check on
// the leading/trailing bytes). While braces are still open — the common
// streaming case — it returns before the O(n) TrimSpace copy, so no JSON
// validation runs and no per-fragment copy of the growing buffer is made.
// This preserves toolArgsFullyReceived semantics while avoiding O(n²)
// re-validation of an incomplete growing buffer.
func (a *tcAccum) maybeFinalizeArgs() {
	if a.argsFinalized {
		return
	}
	// Cheap O(1) gate on the raw bytes before the O(n) TrimSpace: only a
	// buffer that starts with '{' and ends with '}' (modulo surrounding
	// whitespace) can be a complete single JSON object. True-delta streams
	// close the object only with the final fragment, so this early-returns
	// on every non-final fragment without copying or parsing.
	if !argsStructurallyComplete(a.ArgsStr) {
		return
	}
	s := strings.TrimSpace(a.ArgsStr)
	if completeJSONObject(s) {
		a.argsFinalized = toolArgsFullyReceived(a.ArgsStr)
	}
}

// argsStructurallyComplete reports whether s starts with '{' and ends with
// '}' modulo surrounding whitespace, examining only the leading and trailing
// whitespace runs (O(1) for typical fragments). It is a necessary — not
// sufficient — condition for s to be a complete single JSON object;
// callers must still run the authoritative toolArgsFullyReceived check. Used
// to gate the O(n) TrimSpace + json.Unmarshal validation so it runs at most
// once per accumulator instead of on every streamed argument fragment.
//
// Whitespace is defined with unicode.IsSpace — the same set strings.TrimSpace
// trims — so this gate and the authoritative check can never disagree: an
// ASCII-only definition would miss a buffer with leading/trailing non-ASCII
// whitespace (e.g. NBSP), keep argsFinalized false forever, and let a
// full-object replay be spliced onto the buffer it was meant to ignore.
func argsStructurallyComplete(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsSpace(r) {
			break
		}
		i += size
	}
	j := len(s)
	for j > i {
		r, size := utf8.DecodeLastRuneInString(s[:j])
		if !unicode.IsSpace(r) {
			break
		}
		j -= size
	}
	return i < j && s[i] == '{' && s[j-1] == '}'
}

// completeJSONObject reports whether s is a single JSON object spanning the
// whole buffer: it starts with '{' and the string-aware scanner (which skips
// quoted strings, honoring escapes) finds the matching '}' exactly at the
// end. Unlike a naive brace-depth count, braces inside strings do not confuse
// it. Used by maybeFinalizeArgs as the cheap pre-check before the
// authoritative json.Unmarshal validation.
func completeJSONObject(s string) bool {
	_, end := extractJSONObject(s, 0)
	return end == len(s)
}

// mergeToolArgsDelta combines streamed argument fragments.
// Providers may send true deltas, cumulative snapshots, or full-object replays;
// naive concatenation of the latter two yields "invalid character '{' ..." errors.
//
// existingComplete is the caller's cached verdict on whether existing is a
// complete, validated JSON object (see tcAccum.argsFinalized), maintained by
// the accumulator so this hot path never re-validates the growing buffer.
// When the flag is unavailable (direct callers, tests), the function falls
// back to a two-stage structural gate — an O(1) raw-brace check
// (argsStructurallyComplete) followed by the string-aware completeJSONObject
// scan — before the authoritative toolArgsFullyReceived check. The scan is
// what distinguishes a fragment boundary landing right after a '}' inside a
// quoted string from the object's real closing brace, so the full TrimSpace
// + json.Unmarshal runs at most once per streamed arguments blob instead of
// per fragment.
func mergeToolArgsDelta(existing, delta string, existingComplete bool) string {
	if delta == "" {
		return existing
	}
	if existing == "" {
		return delta
	}
	// Cumulative snapshot: each chunk re-sends args from the start.
	if strings.HasPrefix(delta, existing) {
		return delta
	}
	// Exact replay of the same fragment/object.
	if delta == existing {
		return existing
	}
	// Already-complete JSON followed by another object start — ignore the
	// replay (common when servers re-emit the finished arguments blob).
	if existingComplete || (argsStructurallyComplete(existing) && completeJSONObject(strings.TrimSpace(existing)) && toolArgsFullyReceived(existing)) {
		trimmed := strings.TrimSpace(delta)
		if trimmed == strings.TrimSpace(existing) || strings.HasPrefix(trimmed, "{") {
			return existing
		}
	}
	return existing + delta
}

func parseToolCallArgs(argsStr string) (map[string]any, error) {
	s := strings.TrimSpace(argsStr)
	if s == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	err := json.Unmarshal([]byte(s), &args)
	if err == nil {
		if args == nil {
			return map[string]any{}, nil
		}
		return args, nil
	}
	// Recovery for duplicated complete objects: {"a":1}{"a":1}
	dec := json.NewDecoder(strings.NewReader(s))
	if decErr := dec.Decode(&args); decErr == nil && args != nil {
		return args, nil
	}
	return nil, err
}

// toolArgsFullyReceived reports whether streamed tool arguments form complete JSON.
func toolArgsFullyReceived(argsStr string) bool {
	s := strings.TrimSpace(argsStr)
	// Empty means args have not started yet (name-only delta) — not complete.
	if s == "" {
		return false
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return false
	}
	var m map[string]any
	return json.Unmarshal([]byte(s), &m) == nil
}

// toolAccumsStreamComplete reports whether every accumulated tool call has a name
// and fully received arguments (used to detect llama.cpp tool streams that end
// without finish_reason or [DONE]).
func toolAccumsStreamComplete(accums []tcAccum) bool {
	if len(accums) == 0 {
		return false
	}
	for i := range accums {
		acc := &accums[i]
		if acc.Name == "" {
			return false
		}
		// Use the cached finalized flag from streaming; fall back to a fresh
		// check for accumulators built outside the streaming merge (tests).
		if acc.argsFinalized {
			continue
		}
		if !toolArgsFullyReceived(acc.ArgsStr) {
			return false
		}
		acc.argsFinalized = true
	}
	return true
}

// deltaIsTerminalToolSignal reports llama.cpp's empty-delta chunk that ends a
// tool-call stream when finish_reason is omitted but the connection stays open.
func deltaIsTerminalToolSignal(delta openai.ChatCompletionChunkChoiceDelta, haveTools bool) bool {
	if !haveTools {
		return false
	}
	return deltaIsEmptyDelta(delta)
}

func deltaIsEmptyDelta(delta openai.ChatCompletionChunkChoiceDelta) bool {
	if delta.Content != "" || delta.Refusal != "" || len(delta.ToolCalls) > 0 {
		return false
	}
	for _, field := range delta.JSON.ExtraFields {
		if !field.Valid() {
			continue
		}
		raw := strings.TrimSpace(field.Raw())
		if raw != "" && raw != "null" && raw != `""` {
			return false
		}
	}
	raw := strings.TrimSpace(delta.RawJSON())
	return raw == "{}" || raw == ""
}
