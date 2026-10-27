package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

func deltaTool(index int64, id, name, args string) openai.ChatCompletionChunkChoiceDeltaToolCall {
	return openai.ChatCompletionChunkChoiceDeltaToolCall{
		Index: index,
		ID:    id,
		Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
			Name:      name,
			Arguments: args,
		},
	}
}

// deltaToolFromJSON builds a tool-call delta from its raw wire JSON so the
// JSON.* presence metadata — which the SSE decoder populates and the index
// routing relies on to distinguish an omitted index from an explicit index=0
// — is set exactly as it would be in a real stream.
func deltaToolFromJSON(t *testing.T, raw string) openai.ChatCompletionChunkChoiceDeltaToolCall {
	t.Helper()
	var tc openai.ChatCompletionChunkChoiceDeltaToolCall
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		t.Fatal(err)
	}
	return tc
}

func TestMergeToolCallDeltaMultipleTools(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"a"}`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "b", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "", "", `{"path":"b"}`), accums, m)

	if len(accums) != 2 {
		t.Fatalf("got %d accums", len(accums))
	}
	if accums[0].ArgsStr != `{"path":"a"}` || accums[1].ArgsStr != `{"path":"b"}` {
		t.Fatalf("args = %#v", accums)
	}
}

func TestParseToolCallArgs(t *testing.T) {
	t.Parallel()
	_, err := parseToolCallArgs("{")
	if err == nil {
		t.Fatal("expected error for incomplete JSON")
	}
	_, err = parseToolCallArgs(`{"path":"x"`)
	if err == nil {
		t.Fatal("expected error for truncated JSON")
	}
	args, err := parseToolCallArgs(`{"path":"x"}`)
	if err != nil || args["path"] != "x" {
		t.Fatalf("got %#v err=%v", args, err)
	}
	args, err = parseToolCallArgs("")
	if err != nil || len(args) != 0 {
		t.Fatalf("empty args: %#v err=%v", args, err)
	}
}

func TestToolAccumsStreamComplete(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		accums []tcAccum
		want   bool
	}{
		{"empty", nil, false},
		{"name only incomplete brace", []tcAccum{{Name: "read_file", ArgsStr: "{"}}, false},
		{"name only empty args", []tcAccum{{Name: "read_file", ArgsStr: ""}}, false},
		{"complete empty object", []tcAccum{{Name: "read_file", ArgsStr: "{}"}}, true},
		{"complete json", []tcAccum{{Name: "read_file", ArgsStr: `{"path":"a"}`}}, true},
		{"partial json", []tcAccum{{Name: "read_file", ArgsStr: `{"path":`}}, false},
		{"two tools one partial", []tcAccum{
			{Name: "read_file", ArgsStr: `{}`},
			{Name: "glob", ArgsStr: `{"pattern":"`},
		}, false},
		{"two tools complete", []tcAccum{
			{Name: "read_file", ArgsStr: `{}`},
			{Name: "glob", ArgsStr: `{"pattern":"*.go"}`},
		}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := toolAccumsStreamComplete(tc.accums); got != tc.want {
				t.Fatalf("toolAccumsStreamComplete() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeltaIsTerminalToolSignal(t *testing.T) {
	t.Parallel()
	var delta openai.ChatCompletionChunkChoiceDelta
	if deltaIsTerminalToolSignal(delta, false) {
		t.Fatal("no tools yet")
	}
	if err := delta.UnmarshalJSON([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !deltaIsTerminalToolSignal(delta, true) {
		t.Fatal("expected {} to end tool stream")
	}
}

func TestMergeToolCallDeltaArgsContinuationMissingIndex(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"a"}`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "b", "read_file", ""), accums, m)
	// llama.cpp-style: continuation chunk defaults index to 0.
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"b"}`), accums, m)

	if accums[1].ArgsStr != `{"path":"b"}` {
		t.Fatalf("second tool args = %q, want b path", accums[1].ArgsStr)
	}
	if accums[0].ArgsStr != `{"path":"a"}` {
		t.Fatalf("first tool polluted: %q", accums[0].ArgsStr)
	}
}

// TestMergeToolCallDeltaExplicitIndexZeroContinuation pins the routing of an
// EXPLICIT index=0 args-only continuation while a higher-index tool is
// streaming. The default-index-0 splice must only apply when the wire OMITTED
// the index field; an explicit index=0 is a genuine continuation of tool 0
// and must land on tool 0, not on the last (highest-index) tool.
func TestMergeToolCallDeltaExplicitIndexZeroContinuation(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"a"`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "b", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "", "", `{"path":"b"}`), accums, m)
	// Explicit index=0 continuation carrying a true delta fragment: the
	// index field is PRESENT on the wire, so the fragment must continue
	// tool 0 (the pre-fix code spliced it onto tool 1 because 1 > 0).
	accums, _ = mergeToolCallDelta(deltaToolFromJSON(t, `{"index":0,"function":{"arguments":",\"offset\":10}"}}`), accums, m)

	if len(accums) != 2 {
		t.Fatalf("got %d accums, want 2", len(accums))
	}
	if accums[0].ArgsStr != `{"path":"a","offset":10}` {
		t.Fatalf("tool 0 args = %q, want explicit index-0 continuation spliced onto tool 0", accums[0].ArgsStr)
	}
	if accums[1].ArgsStr != `{"path":"b"}` {
		t.Fatalf("tool 1 polluted: %q", accums[1].ArgsStr)
	}
}

// TestMergeToolCallDeltaExplicitHighIndexContinuation verifies an explicit
// non-zero index routes through the index map (not the default-index-0
// splice), so an interleaved continuation of tool 2 is not spliced onto the
// last tool.
func TestMergeToolCallDeltaExplicitHighIndexContinuation(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(1, "b", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(2, "c", "glob", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(2, "", "", `{"pattern":"*.go"`), accums, m)
	// Explicit index=2 continuation: must land on tool 2 via the map.
	accums, _ = mergeToolCallDelta(deltaToolFromJSON(t, `{"index":2,"function":{"arguments":",\"recursive\":true}"}}`), accums, m)

	if accums[2].ArgsStr != `{"pattern":"*.go","recursive":true}` {
		t.Fatalf("tool 2 args = %q, want explicit index-2 continuation", accums[2].ArgsStr)
	}
	if accums[0].ArgsStr != "" || accums[1].ArgsStr != "" {
		t.Fatalf("tools 0/1 polluted: %#v", accums)
	}
}

func TestMergeToolArgsDeltaCumulativeAndReplay(t *testing.T) {
	t.Parallel()

	// Cumulative snapshots (not deltas): each chunk re-sends from the start.
	got := mergeToolArgsDelta(`{"path"`, `{"path":"x.go"}`, false)
	if got != `{"path":"x.go"}` {
		t.Fatalf("cumulative: got %q", got)
	}

	// Full-object replay after complete JSON → "invalid character '{' after top-level value"
	got = mergeToolArgsDelta(`{"pattern":"foo"}`, `{"pattern":"foo"}`, false)
	if got != `{"pattern":"foo"}` {
		t.Fatalf("replay: got %q", got)
	}

	// True delta still concatenates.
	got = mergeToolArgsDelta(`{"path":"`, `x.go"}`, false)
	if got != `{"path":"x.go"}` {
		t.Fatalf("delta: got %q", got)
	}
}

// TestMergeToolArgsDeltaCompleteFlag pins the cached completeness verdict
// (tcAccum.argsFinalized) driving the replay-ignore decision without
// re-validation, and that the cheap structural gate fallback (flag
// unavailable — direct callers, tests) produces identical results.
func TestMergeToolArgsDeltaCompleteFlag(t *testing.T) {
	t.Parallel()

	// Differently-formatted full-object replay after a complete object is
	// ignored whether completeness comes from the cached flag or from the
	// fallback structural gate + authoritative check.
	for _, complete := range []bool{true, false} {
		got := mergeToolArgsDelta(`{"pattern":"foo"}`, `{"pattern": "foo"}`, complete)
		if got != `{"pattern":"foo"}` {
			t.Fatalf("complete=%v: got %q, want replay ignored", complete, got)
		}
	}

	// Trailing whitespace after a complete object is still recognized.
	got := mergeToolArgsDelta(`{"pattern":"foo"} `, `{"pattern":"foo"}`, false)
	if got != `{"pattern":"foo"} ` {
		t.Fatalf("trailing-ws replay: got %q", got)
	}

	// A non-replay fragment after a complete object is appended, not dropped
	// (parity with the pre-cache behavior; only identical or '{'-prefixed
	// fragments are treated as replays).
	got = mergeToolArgsDelta(`{"pattern":"foo"}`, `,"recursive":true}`, true)
	if got != `{"pattern":"foo"},"recursive":true}` {
		t.Fatalf("continuation after complete: got %q", got)
	}

	// An incomplete buffer never triggers the replay branch: appended.
	got = mergeToolArgsDelta(`{"pattern":"fo`, `o"}`, false)
	if got != `{"pattern":"foo"}` {
		t.Fatalf("delta: got %q", got)
	}
}

// TestMergeToolCallDeltaFinalizesCompleteFirstFragment pins that an
// accumulator created with a complete arguments blob is finalized at
// creation: argsFinalized must be authoritative from the first fragment, so a
// subsequent differently-formatted full-object replay is ignored instead of
// being spliced on (which would corrupt ArgsStr and make
// toolAccumsStreamComplete report the stream incomplete forever).
func TestMergeToolCallDeltaFinalizesCompleteFirstFragment(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", `{"path":"a"}`), accums, m)
	if len(accums) != 1 {
		t.Fatalf("got %d accums", len(accums))
	}
	if !accums[0].argsFinalized {
		t.Fatal("expected argsFinalized=true for single-chunk complete args")
	}

	// Differently-formatted replay of the finished blob must be ignored.
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path": "a"}`), accums, m)
	if accums[0].ArgsStr != `{"path":"a"}` {
		t.Fatalf("args = %q, want replay ignored", accums[0].ArgsStr)
	}
	if !accums[0].argsFinalized {
		t.Fatal("replay should keep finalized flag set")
	}

	// A non-replay fragment after the finished blob is appended (parity with
	// pre-cache behavior) and invalidates the flag for re-evaluation.
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `,"offset":10}`), accums, m)
	if accums[0].ArgsStr != `{"path":"a"},"offset":10}` {
		t.Fatalf("args = %q, want continuation appended", accums[0].ArgsStr)
	}
	if accums[0].argsFinalized {
		t.Fatal("expected flag invalidated after buffer grew with a non-replay fragment")
	}
}

func TestMergeToolCallDeltaNewIDReusesIndex(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "search_code", `{"pattern":"foo"}`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "b", "read_file", `{"path":"x.go"}`), accums, m)

	if len(accums) != 2 {
		t.Fatalf("got %d accums, want 2", len(accums))
	}
	if accums[0].ArgsStr != `{"pattern":"foo"}` {
		t.Fatalf("first polluted: %q", accums[0].ArgsStr)
	}
	if accums[1].Name != "read_file" || accums[1].ArgsStr != `{"path":"x.go"}` {
		t.Fatalf("second = %#v", accums[1])
	}
}

func TestParseToolCallArgsRecoversDuplicatedJSON(t *testing.T) {
	t.Parallel()
	args, err := parseToolCallArgs(`{"path":"x.go"}{"path":"x.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if args["path"] != "x.go" {
		t.Fatalf("got %#v", args)
	}
}

// TestMergeToolCallDeltaFinalizesOncePerAccumulator verifies that streaming
// incremental argument fragments marks the accumulator finalized exactly once
// the JSON closes, and that subsequent identical fragments do not re-run the
// JSON validity check (the argsFinalized cache stays set).
func TestMergeToolCallDeltaFinalizesOncePerAccumulator(t *testing.T) {
	t.Parallel()
	m := make(map[int]int)
	var accums []tcAccum

	// Open the tool call, then stream `{"path":"x.go"}` one fragment at a time.
	accums, _ = mergeToolCallDelta(deltaTool(0, "a", "read_file", ""), accums, m)
	for _, frag := range []string{`{"path`, `":"x.go`, `"}`} {
		accums, _ = mergeToolCallDelta(deltaTool(0, "", "", frag), accums, m)
	}
	if len(accums) != 1 {
		t.Fatalf("got %d accums", len(accums))
	}
	if accums[0].ArgsStr != `{"path":"x.go"}` {
		t.Fatalf("args = %q", accums[0].ArgsStr)
	}
	if !accums[0].argsFinalized {
		t.Fatal("expected argsFinalized=true after complete JSON streamed")
	}

	// A subsequent replay fragment must keep the flag set (no re-validation).
	before := accums[0].argsFinalized
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "", `{"path":"x.go"}`), accums, m)
	// Replay is ignored by mergeToolArgsDelta, so the buffer is unchanged and
	// the flag remains.
	if !accums[0].argsFinalized || accums[0].argsFinalized != before {
		t.Fatalf("replay should keep finalized flag set; got %v", accums[0].argsFinalized)
	}

	// toolAccumsStreamComplete should report true via the cached flag.
	if !toolAccumsStreamComplete(accums) {
		t.Fatal("expected toolAccumsStreamComplete true for finalized accumulator")
	}
}

// TestCompleteJSONObject covers the cheap structural pre-check used to decide
// when the expensive json.Unmarshal validity check is worth running. The
// check is string-aware: braces inside quoted strings (including escaped
// quotes) must not count toward nesting depth.
func TestCompleteJSONObject(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":           false,
		`{`:          false,
		`}`:          false,
		`{}`:         true,
		`{"a":1}`:    true,
		`{"a":`:      false,
		`{}}`:        false,
		`{"a":"}"}`:  true, // brace inside a string must not confuse the scan
		`{"a":1}{"b`: false,
		`{"a":"\""}`: true, // escaped quote inside a string
	}
	for s, want := range cases {
		if got := completeJSONObject(s); got != want {
			t.Fatalf("completeJSONObject(%q) = %v, want %v", s, got, want)
		}
	}
}

// BenchmarkMergeToolCallDeltaTrueDeltaStream measures the per-fragment cost of
// streaming a multi-KB, brace-heavy arguments blob (patch_file-style) as true
// deltas through the accumulator. Every intermediate fragment ends with a '}'
// inside the diff string — the worst case for a naive per-fragment
// "is the buffer complete JSON?" check, which would re-scan and re-parse the
// whole growing buffer (O(n²) over the stream) because a '}' inside a quoted
// string is indistinguishable from the real closing brace without a
// string-aware scan. Per-fragment work must be O(1) plus one final
// validation, and allocations must stay flat regardless of the '}'-heavy
// content.
func BenchmarkMergeToolCallDeltaTrueDeltaStream(b *testing.B) {
	frags := []string{`{"file_path":"internal/llm/tool_call_accum.go","diff":"--- a/x`}
	for i := 0; i < 128; i++ {
		frags = append(frags, `\n+func f() {\n+}\n+func f() {\n+}`)
	}
	frags = append(frags, `"}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := make(map[int]int)
		var accums []tcAccum
		accums, _ = mergeToolCallDelta(deltaTool(0, "a", "patch_file", ""), accums, m)
		for _, f := range frags {
			accums, _ = mergeToolCallDelta(deltaTool(0, "", "", f), accums, m)
		}
		if !toolAccumsStreamComplete(accums) {
			b.Fatal("stream did not complete")
		}
	}
}

// TestArgsStructurallyCompleteUnicodeWhitespace pins the whitespace
// definition of the structural gate: it must match strings.TrimSpace
// (unicode.IsSpace), so a complete object padded with non-ASCII whitespace
// (e.g. NBSP) is recognized — an ASCII-only gate would keep argsFinalized
// false forever and let a full-object replay be spliced onto the buffer it
// was meant to ignore.
func TestArgsStructurallyCompleteUnicodeWhitespace(t *testing.T) {
	t.Parallel()
	complete := []string{
		"{}",
		"  {a: 1}  ",
		"\n\t{ \"a\": 1 }\r\n",
		"\u00a0{\"a\": 1}\u00a0", // NBSP padding: unicode whitespace, not ASCII
	}
	for _, s := range complete {
		if !argsStructurallyComplete(s) {
			t.Fatalf("argsStructurallyComplete(%q) = false, want true", s)
		}
	}
	incomplete := []string{
		"", "{", "}", " {", "} ", "{a", "a}", "\u00a0{", "}\u00a0", "\u00a0{\"a\": 1",
	}
	for _, s := range incomplete {
		if argsStructurallyComplete(s) {
			t.Fatalf("argsStructurallyComplete(%q) = true, want false", s)
		}
	}
}

// TestToolAccumFinalizesWithUnicodeWhitespace runs the full finalization
// path on an NBSP-padded complete arguments blob: it must be recognized as
// complete (argsFinalized), and a subsequent full-object replay must be
// ignored so the accumulated buffer stays authoritative.
func TestToolAccumFinalizesWithUnicodeWhitespace(t *testing.T) {
	acc := &tcAccum{ArgsStr: "\u00a0{\"a\": 1}\u00a0"}
	acc.maybeFinalizeArgs()
	if !acc.argsFinalized {
		t.Fatal("NBSP-padded complete args should finalize")
	}
	merged := mergeToolArgsDelta(acc.ArgsStr, `{"a": 1}`, acc.argsFinalized)
	if merged != acc.ArgsStr {
		t.Fatalf("replay after finalize = %q, want unchanged %q", merged, acc.ArgsStr)
	}
}

// TestMergeToolCallDeltaNameChangeStartsNewCall pins the no-ID case: a server
// that reuses index 0 and omits the ID made two calls, and the second one's
// name used to overwrite the first while its arguments were discarded as a
// "replay" — the model saw one call where it made two.
func TestMergeToolCallDeltaNameChangeStartsNewCall(t *testing.T) {
	m := map[int]int{}
	var accums []tcAccum
	accums, _ = mergeToolCallDelta(deltaTool(0, "id-1", "read_file", `{"path":"a"}`), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "write_file", `{"path":"b"}`), accums, m)

	if len(accums) != 2 {
		t.Fatalf("accumulators = %d, want 2 (a changed name is a new call)", len(accums))
	}
	if accums[0].Name != "read_file" || accums[1].Name != "write_file" {
		t.Fatalf("names = %q, %q; want read_file, write_file", accums[0].Name, accums[1].Name)
	}
	if !strings.Contains(accums[0].ArgsStr, `"a"`) || !strings.Contains(accums[1].ArgsStr, `"b"`) {
		t.Fatalf("args = %q, %q; the second call's arguments were dropped", accums[0].ArgsStr, accums[1].ArgsStr)
	}
}

// TestMergeToolCallDeltaSameNameContinuation: the name change rule must not
// split a normal continuation (same name, no ID).
func TestMergeToolCallDeltaSameNameContinuation(t *testing.T) {
	m := map[int]int{}
	var accums []tcAccum
	accums, _ = mergeToolCallDelta(deltaTool(0, "id-1", "read_file", ""), accums, m)
	accums, _ = mergeToolCallDelta(deltaTool(0, "", "read_file", `{"path":"a"}`), accums, m)
	if len(accums) != 1 {
		t.Fatalf("accumulators = %d, want 1 (same name): %+v", len(accums), accums)
	}
	if accums[0].ArgsStr != `{"path":"a"}` {
		t.Fatalf("args = %q", accums[0].ArgsStr)
	}
}

// TestRenumberExtractedToolCallsUniqueAcrossBatches pins the cross-round
// collision: a fixed "tc_extracted_<i>" gave every round the same IDs, so the
// history held duplicates and result lookups matched the wrong call.
func TestRenumberExtractedToolCallsUniqueAcrossBatches(t *testing.T) {
	first := []ToolCall{{Name: "read_file"}, {Name: "write_file"}}
	second := []ToolCall{{Name: "read_file"}, {Name: "write_file"}}
	renumberExtractedToolCalls(first)
	renumberExtractedToolCalls(second)

	seen := map[string]bool{}
	for _, tc := range append(append([]ToolCall{}, first...), second...) {
		if tc.ID == "" {
			t.Fatal("extracted tool call has no ID")
		}
		if seen[tc.ID] {
			t.Fatalf("duplicate tool-call ID %q across rounds", tc.ID)
		}
		seen[tc.ID] = true
	}
	// Indices stay dense per batch (they drive the model-order rendering).
	if first[0].Index != 0 || first[1].Index != 1 {
		t.Fatalf("indices = %d,%d; want 0,1", first[0].Index, first[1].Index)
	}
}
