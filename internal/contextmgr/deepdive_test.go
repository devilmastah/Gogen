package contextmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gogen/internal/config"
	"gogen/internal/llm"
)

// regionProvider answers each summarization request with a recap named after
// the instruction it was given, and records the requests.
type regionProvider struct {
	stubProvider
	requests [][]llm.Message
}

func (p *regionProvider) GenerateResponse(_ context.Context, msgs []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	p.requests = append(p.requests, msgs)
	kind := "session"
	if msgs[len(msgs)-1].Content == progressSummaryInstruction {
		kind = "progress"
	}
	return llm.Response{Content: fmt.Sprintf("%s recap %d", kind, len(p.requests))}, nil
}

func (p *regionProvider) kinds() []string {
	out := make([]string, len(p.requests))
	for i, req := range p.requests {
		out[i] = "session"
		if req[len(req)-1].Content == progressSummaryInstruction {
			out[i] = "progress"
		}
	}
	return out
}

func newDeepDiveManager(p llm.LLMProvider, deepDive bool) *Manager {
	m := NewManager(p, Settings{CompactKeepRecentMessages: 2, ContextLimit: 1000000, CompactThreshold: 0.5, DeepDive: deepDive})
	m.minMiddleTokens = 0
	return m
}

func bulky(label string) llm.Message {
	return llm.Message{Role: "assistant", Content: label + " " + strings.Repeat("work ", 200)}
}

// rounds returns n bulky assistant messages followed by the two-message tail.
func rounds(prefix string, n int) []llm.Message {
	out := make([]llm.Message, 0, n+2)
	for i := 0; i < n; i++ {
		out = append(out, bulky(fmt.Sprintf("%s round %d", prefix, i)))
	}
	return append(out,
		llm.Message{Role: "assistant", Content: prefix + " tail one"},
		llm.Message{Role: "assistant", Content: prefix + " tail two"})
}

func roles(msgs []llm.Message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		switch {
		case strings.HasPrefix(m.Content, SummaryPrefix):
			parts[i] = "S1"
		case strings.HasPrefix(m.Content, ProgressSummaryPrefix):
			parts[i] = "S2"
		default:
			parts[i] = m.Content
			if len(parts[i]) > 24 {
				parts[i] = parts[i][:24]
			}
		}
	}
	return strings.Join(parts, " | ")
}

func TestDeepDiveCompactionLayout(t *testing.T) {
	first := llm.Message{Role: "user", Content: "first request"}
	current := llm.Message{Role: "user", Content: "current request: refactor the parser"}
	msgs := []llm.Message{first, bulky("earlier answer"), current}
	msgs = append(msgs, rounds("loop", 3)...)

	tests := []struct {
		name     string
		deepDive bool
		want     string
		kinds    []string
	}{
		{
			name:     "deep dive keeps the request between two summaries",
			deepDive: true,
			want:     "first request | S1 | current request: refacto | S2 | loop tail one | loop tail two",
			kinds:    []string{"session", "progress"},
		},
		{
			name:     "default mode keeps the single summary",
			deepDive: false,
			want:     "first request | S1 | loop tail one | loop tail two",
			kinds:    []string{"session"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &regionProvider{}
			out, _, err := newDeepDiveManager(p, tt.deepDive).Compact(context.Background(), msgs, CompactOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := roles(out); got != tt.want {
				t.Fatalf("layout:\n got %s\nwant %s", got, tt.want)
			}
			if got := strings.Join(p.kinds(), ","); got != strings.Join(tt.kinds, ",") {
				t.Fatalf("summaries = %s, want %s", got, strings.Join(tt.kinds, ","))
			}
		})
	}
}

// TestDeepDiveRequestSurvivesRepeatedCompactions pins the long-turn path:
// after the first compaction, later ones in the same turn rewrite only the
// progress summary. Everything up to the request stays byte-identical, and
// the progress request carries that prefix as its context.
func TestDeepDiveRequestSurvivesRepeatedCompactions(t *testing.T) {
	p := &regionProvider{}
	m := newDeepDiveManager(p, true)
	current := llm.Message{Role: "user", Content: "current request"}
	msgs := append([]llm.Message{{Role: "user", Content: "first"}, bulky("old"), current}, rounds("r0", 3)...)

	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prefix := append([]llm.Message(nil), out[:3]...)
	for pass := 1; pass <= 3; pass++ {
		before := len(p.requests)
		msgs = append(out, rounds(fmt.Sprintf("r%d", pass), 3)...)
		out, _, err = m.Compact(context.Background(), msgs, CompactOptions{})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got := roles(out); !strings.HasPrefix(got, "first | S1 | current request | S2 | ") {
			t.Fatalf("pass %d layout: %s", pass, got)
		}
		for i := range prefix {
			if out[i].Content != prefix[i].Content {
				t.Fatalf("pass %d: message %d changed:\n got %q\nwant %q", pass, i, out[i].Content, prefix[i].Content)
			}
		}
		if calls := p.kinds()[before:]; len(calls) != 1 || calls[0] != "progress" {
			t.Fatalf("pass %d summaries = %v, want one progress summary", pass, calls)
		}
		req := p.requests[len(p.requests)-1]
		if req[2].Content != current.Content {
			t.Fatalf("pass %d: progress request context does not end at the request: %s", pass, roles(req[:3]))
		}
	}
}

func TestDeepDiveNewRequestRollsForward(t *testing.T) {
	p := &regionProvider{}
	m := newDeepDiveManager(p, true)
	msgs := append([]llm.Message{{Role: "user", Content: "first"}, bulky("old"), {Role: "user", Content: "request one"}}, rounds("a", 3)...)
	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	msgs = append(out, llm.Message{Role: "user", Content: "request two"})
	msgs = append(msgs, rounds("b", 3)...)
	before := len(p.requests)
	out, _, err = m.Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := roles(out), "first | S1 | request two | S2 | b tail one | b tail two"; got != want {
		t.Fatalf("layout:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(out[1].Content, "session recap 1") {
		t.Fatal("the session summary was not rewritten to cover request one")
	}
	if got := strings.Join(p.kinds()[before:], ","); got != "session,progress" {
		t.Fatalf("summaries = %s, want session,progress", got)
	}
}

func TestDeepDiveRequestSkipsNotices(t *testing.T) {
	tests := []struct {
		name   string
		notice string
	}{
		{"job exit", JobNoticePrefix + "bg-1 (go test ./...) exited with code 0"},
		{"subagent report", SubagentNoticePrefix + "scout] found the parser in internal/parse"},
		{"read_image caption", ReadImagePrefix + "shot.png — image/png, 40 KB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := []llm.Message{{Role: "user", Content: "first"}, bulky("old"), {Role: "user", Content: "the request"}, bulky("work"), {Role: "user", Content: tt.notice}}
			msgs = append(msgs, rounds("after", 2)...)
			out, _, err := newDeepDiveManager(&regionProvider{}, true).Compact(context.Background(), msgs, CompactOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := roles(out), "first | S1 | the request | S2 | after tail one | after tail two"; got != want {
				t.Fatalf("layout:\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestDeepDiveRequestInTailChangesNothing(t *testing.T) {
	p := &regionProvider{}
	msgs := []llm.Message{{Role: "user", Content: "first"}, bulky("a"), bulky("b"), {Role: "user", Content: "recent"}, {Role: "assistant", Content: "ok"}}
	out, _, err := newDeepDiveManager(p, true).Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := roles(out), "first | S1 | recent | ok"; got != want {
		t.Fatalf("layout:\n got %s\nwant %s", got, want)
	}
}

// TestDeepDiveCurrentRequestInTail pins that an older request in the middle
// is not kept as "the current request" when the newest one is already in
// the tail: everything before the tail is earlier conversation.
func TestDeepDiveCurrentRequestInTail(t *testing.T) {
	p := &regionProvider{}
	msgs := []llm.Message{
		{Role: "user", Content: "first"}, bulky("a"),
		{Role: "user", Content: "old request"}, bulky("old work 1"), bulky("old work 2"),
		{Role: "user", Content: "current request"}, {Role: "assistant", Content: "ok"},
	}
	out, _, err := newDeepDiveManager(p, true).Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := roles(out), "first | S1 | current request | ok"; got != want {
		t.Fatalf("layout:\n got %s\nwant %s", got, want)
	}
	if got := strings.Join(p.kinds(), ","); got != "session" {
		t.Fatalf("summaries requested = %s, want one session summary", got)
	}
}

// TestDeepDiveChooseCompactKeep pins that the keep is sized on what the
// deep-dive layout frees: the verbatim request frees nothing, so a large
// request in the middle must not make a too-large keep look sufficient.
func TestDeepDiveChooseCompactKeep(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "user", Content: "the request"},
		bulky("w1"), bulky("w2"), bulky("w3"), bulky("w4"),
	}
	counts := []int{10, 1000, 100, 100, 100, 100}
	tests := []struct {
		name     string
		deepDive bool
		want     int
	}{
		{"default counts the request", false, 4},
		{"deep dive frees only the work since", true, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(nil, Settings{CompactKeepRecentMessages: 4, ContextLimit: 1000000, CompactThreshold: 0.5, DeepDive: tt.deepDive})
			m.minMiddleTokens = 0
			if got := m.ChooseCompactKeep(msgs, counts, 150, nil); got != tt.want {
				t.Fatalf("keep = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestSettingsFromConfig pins the config to Settings mapping every session
// and the CLI start use, including context_mode.
func TestSettingsFromConfig(t *testing.T) {
	if got := SettingsFromConfig(nil); got != DefaultSettings() {
		t.Fatalf("nil config = %+v, want the defaults", got)
	}
	tests := []struct {
		name string
		mode string
		want bool
	}{
		{"deep dive", config.ContextModeDeepDive, true},
		{"deep dive in another case", " Deep_Dive ", true},
		{"default", config.DefaultContextMode, false},
		{"empty", "", false},
		{"unknown", "deepdive", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := config.Defaults()
			c.ContextMode = tt.mode
			c.SmartCompact = "on"
			c.CompactThreshold = 0.7
			got := SettingsFromConfig(&c)
			if got.DeepDive != tt.want {
				t.Fatalf("DeepDive = %v, want %v", got.DeepDive, tt.want)
			}
			if !got.SmartCompact || got.CompactThreshold != 0.7 || got.CompactLastResort != c.CompactLastResort {
				t.Fatalf("other fields not carried: %+v", got)
			}
		})
	}
}

// TestDeepDiveForcedFallsBackToSingleSummary pins that a forced compaction
// in deep dive still drops a message: when the layout around the request
// would keep the count (or refuse both regions), the single summary is used.
func TestDeepDiveForcedFallsBackToSingleSummary(t *testing.T) {
	oldSummary := llm.Message{Role: "assistant", Content: SummaryPrefix + strings.Repeat("old facts ", 100)}
	call := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}}
	result := llm.Message{Role: "tool", Content: "file body", ToolCallID: "c1"}
	tests := []struct {
		name  string
		since llm.Message
	}{
		{"progress region of one message", bulky("big step")},
		{"progress summary refused as not smaller", llm.Message{Role: "assistant", Content: "ok"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := []llm.Message{{Role: "user", Content: "first"}, oldSummary, {Role: "user", Content: "the request"}, tt.since, call, result}
			out, _, err := newDeepDiveManager(&regionProvider{}, true).Compact(context.Background(), msgs, CompactOptions{Keep: NoTailKeep, Forced: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(out) >= len(msgs) {
				t.Fatalf("forced compaction did not shrink: %s", roles(out))
			}
			if !strings.HasPrefix(out[1].Content, SummaryPrefix) || len(out[2].ToolCalls) != 1 || out[3].ToolCallID != "c1" {
				t.Fatalf("layout: %s", roles(out))
			}
		})
	}
}

// TestSummaryPreserveListUnchanged pins the default-mode preservation list
// byte for byte; the deep-dive progress list shares its work bullets.
func TestSummaryPreserveListUnchanged(t *testing.T) {
	const want = `Preserve:
- The user's original goal and any changes to it
- Files touched and why they matter
- Key findings from tool results (errors, line numbers, search hits)
- Technical decisions made
- Errors encountered and how they were fixed
- Pending work and the current state`
	if summaryPreserveList != want {
		t.Fatalf("summaryPreserveList changed:\n%s", summaryPreserveList)
	}
	if !strings.Contains(progressSummaryInstruction, "Preserve:\n"+summaryWorkBullets+"\n- What is done for the current request and what remains") {
		t.Fatal("progress instruction does not use the shared work bullets")
	}
}

// TestProgressSummaryInstructionCut pins that the progress summarizer is told
// to cut at the current request, not at "the most recent user message". Job /
// subagent / read_image notices in the progress region are user-role; a
// literal "most recent user message" cut would drop the tool work before them.
func TestProgressSummaryInstructionCut(t *testing.T) {
	if strings.Contains(progressSummaryInstruction, "most recent user message") {
		t.Fatal("progress instruction still cuts at the most recent user message")
	}
	for _, want := range []string{
		"current request",
		"Job notices, subagent notices, and read_image captions",
		"must not become the cut point",
	} {
		if !strings.Contains(progressSummaryInstruction, want) {
			t.Fatalf("progress instruction missing %q", want)
		}
	}
}

// TestDeepDiveRefusalByCaller pins who falls back when the layout around the
// request frees nothing: a normal compaction returns the history unchanged
// with no error (nothing failed, so the caller does not back off, and the
// prefix up to the request stays), an emergency one uses the single summary.
func TestDeepDiveRefusalByCaller(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: SummaryPrefix + "earlier"},
		{Role: "user", Content: "the request " + strings.Repeat("detail ", 2000)},
		{Role: "assistant", Content: ProgressSummaryPrefix + "so far"},
		{Role: "assistant", Content: "ok"},
		{Role: "assistant", Content: "tail"},
	}
	tests := []struct {
		name      string
		emergency bool
		want      string
	}{
		{"normal compaction leaves the history unchanged", false, roles(msgs)},
		{"emergency falls back to the single summary", true, "first | S1 | tail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &regionProvider{}
			m := NewManager(p, Settings{CompactKeepRecentMessages: 1, ContextLimit: 1000000, CompactThreshold: 0.5, DeepDive: true})
			out, _, err := m.Compact(context.Background(), msgs, CompactOptions{Emergency: tt.emergency})
			if err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			if got := roles(out); got != tt.want {
				t.Fatalf("layout:\n got %s\nwant %s", got, tt.want)
			}
			if !tt.emergency && len(p.requests) != 0 {
				t.Fatalf("unchanged history made %d summarization calls", len(p.requests))
			}
		})
	}
}

// TestDeepDiveNotSmallerStillRefuses pins the refusal that does back off: a
// summary that came back no smaller spent a summarizer call, so the error
// is returned as in the single-summary layout.
func TestDeepDiveNotSmallerStillRefuses(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: SummaryPrefix + "earlier"},
		{Role: "user", Content: "the request"},
		{Role: "assistant", Content: "ok"},
		{Role: "assistant", Content: "tail"},
	}
	m := NewManager(&regionProvider{}, Settings{CompactKeepRecentMessages: 1, ContextLimit: 1000000, CompactThreshold: 0.5, DeepDive: true})
	m.minMiddleTokens = 0
	if _, _, err := m.Compact(context.Background(), msgs, CompactOptions{}); !errors.Is(err, ErrSummaryNotSmaller) {
		t.Fatalf("err = %v, want ErrSummaryNotSmaller", err)
	}
}

// TestDeepDiveRegionGuards pins that each region keeps the single-summary
// guards: the smaller-summary refusal, the minimum size (bypassed when
// forced), and image tokens not counting toward that minimum.
func TestDeepDiveRegionGuards(t *testing.T) {
	mid := func(label string) llm.Message {
		return llm.Message{Role: "assistant", Content: label + " " + strings.Repeat("work ", 50)}
	}
	first := llm.Message{Role: "user", Content: "first"}
	req := llm.Message{Role: "user", Content: "the request"}
	tail := []llm.Message{{Role: "assistant", Content: "tail one"}, {Role: "assistant", Content: "tail two"}}
	image := llm.Message{Role: "user", Content: ReadImagePrefix + "a.png", Images: []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA"}}}
	tests := []struct {
		name    string
		msgs    []llm.Message
		zeroMin bool
		forced  bool
		want    string
	}{
		{"a progress summary that is not smaller is refused", []llm.Message{first, bulky("old"), req, {Role: "assistant", Content: "ok"}}, true, false,
			"first | S1 | the request | ok | tail one | tail two"},
		{"forced bypasses the minimum in both regions", []llm.Message{first, mid("old"), req, mid("a"), mid("b")}, false, true,
			"first | S1 | the request | S2 | tail one | tail two"},
		{"image tokens do not count toward the minimum", []llm.Message{first, bulky("a"), bulky("b"), bulky("c"), req, image, {Role: "assistant", Content: "looked"}}, false, false,
			"first | S1 | the request | [read_image] a.png | looked | tail one | tail two"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(&regionProvider{}, Settings{CompactKeepRecentMessages: 2, ContextLimit: 1000000, CompactThreshold: 0.5, DeepDive: true})
			if tt.zeroMin {
				m.minMiddleTokens = 0
			}
			msgs := append(append([]llm.Message(nil), tt.msgs...), tail...)
			out, _, err := m.Compact(context.Background(), msgs, CompactOptions{Forced: tt.forced})
			if err != nil {
				t.Fatal(err)
			}
			if got := roles(out); got != tt.want {
				t.Fatalf("layout:\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

// TestDeepDiveProgressRoster pins that the progress summary carries the
// executed-call roster, so call ids survive the rewrite.
func TestDeepDiveProgressRoster(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "first"}, bulky("old"), {Role: "user", Content: "the request"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call_9", Name: "background_job"}}},
		{Role: "tool", ToolCallID: "call_9", Content: "started job bg-3"},
		bulky("more"),
		{Role: "assistant", Content: "tail one"}, {Role: "assistant", Content: "tail two"},
	}
	out, _, err := newDeepDiveManager(&regionProvider{}, true).Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out[3].Content, ProgressSummaryPrefix) || !strings.Contains(out[3].Content, "call_9") {
		t.Fatalf("progress summary lacks the roster: %s", out[3].Content)
	}
}

// TestDeepDiveOneRegionSkipped pins the per-region guards with the default
// minimum size and cached counts: a small progress region stays verbatim
// while the region before the request is summarized.
func TestDeepDiveOneRegionSkipped(t *testing.T) {
	p := &regionProvider{}
	msgs := []llm.Message{
		{Role: "user", Content: "first"}, bulky("old a"), bulky("old b"), bulky("old c"),
		{Role: "user", Content: "the request"}, {Role: "assistant", Content: "small step"},
		{Role: "assistant", Content: "tail one"}, {Role: "assistant", Content: "tail two"},
	}
	counts := make([]int, len(msgs))
	for i, msg := range msgs {
		counts[i] = ComputeMessageTokens(msg)
	}
	m := NewManager(p, Settings{CompactKeepRecentMessages: 2, ContextLimit: 1000000, CompactThreshold: 0.5, DeepDive: true})
	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{Counts: counts})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := roles(out), "first | S1 | the request | small step | tail one | tail two"; got != want {
		t.Fatalf("layout:\n got %s\nwant %s", got, want)
	}
	if got := strings.Join(p.kinds(), ","); got != "session" {
		t.Fatalf("summaries = %s, want session", got)
	}
}

// TestDeepDivePinsRemapped pins that pinned tail messages keep pointing at
// the same messages after the deep-dive layout.
func TestDeepDivePinsRemapped(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: "first"}, bulky("earlier"), {Role: "user", Content: "the request"}}
	msgs = append(msgs, rounds("loop", 3)...)
	pinned := map[int]struct{}{len(msgs) - 1: {}}
	out, pins, err := newDeepDiveManager(&regionProvider{}, true).Compact(context.Background(), msgs, CompactOptions{Pinned: pinned})
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 {
		t.Fatalf("pins = %v", pins)
	}
	for i := range pins {
		if out[i].Content != msgs[len(msgs)-1].Content {
			t.Fatalf("pin %d points at %q", i, out[i].Content)
		}
	}
}

func TestNoteSummaryIndex(t *testing.T) {
	head := llm.Message{Role: "user", Content: "first"}
	s1 := llm.Message{Role: "assistant", Content: SummaryPrefix + "x"}
	s2 := llm.Message{Role: "assistant", Content: ProgressSummaryPrefix + "x"}
	req := llm.Message{Role: "user", Content: "req"}
	work := llm.Message{Role: "assistant", Content: "w"}
	notice := llm.Message{Role: "user", Content: JobNoticePrefix + "bg-1 exited"}
	tests := []struct {
		name     string
		msgs     []llm.Message
		deepDive bool
		want     int
	}{
		{"default takes the latest summary", []llm.Message{head, s1, req, work}, false, 1},
		{"deep dive skips a summary before the request", []llm.Message{head, s1, req, work}, true, -1},
		{"deep dive takes the progress summary", []llm.Message{head, s1, req, s2, work}, true, 3},
		{"deep dive with only the first request", []llm.Message{head, s1, work}, true, 1},
		{"a notice is not a request", []llm.Message{head, s1, req, s2, notice}, true, 3},
		{"no summary", []llm.Message{head, work}, false, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NoteSummaryIndex(tt.msgs, tt.deepDive); got != tt.want {
				t.Fatalf("index = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestFirstUserIndexSkipsMachineMessages pins that the head kept verbatim
// is the first user request, by the same rule as the deep-dive request.
func TestFirstUserIndexSkipsMachineMessages(t *testing.T) {
	tests := []struct {
		name string
		msgs []llm.Message
		want int
		ok   bool
	}{
		{"first message is the request", []llm.Message{{Role: "user", Content: "hi"}}, 0, true},
		{"a notice before the request", []llm.Message{{Role: "user", Content: JobNoticePrefix + "bg-1 exited"}, {Role: "user", Content: "hi"}}, 1, true},
		{"a legacy summary before the request", []llm.Message{{Role: "user", Content: SummaryPrefix + "x"}, {Role: "user", Content: "hi"}}, 1, true},
		{"only machine messages", []llm.Message{{Role: "user", Content: ReadImagePrefix + "a.png"}}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := firstUserIndex(tt.msgs)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("firstUserIndex = %d, %v, want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDeepDiveFreeable(t *testing.T) {
	s1 := llm.Message{Role: "assistant", Content: SummaryPrefix + "x"}
	s2 := llm.Message{Role: "assistant", Content: ProgressSummaryPrefix + "x"}
	req := llm.Message{Role: "user", Content: "req"}
	work := llm.Message{Role: "assistant", Content: "w"}
	head := llm.Message{Role: "user", Content: "first"}
	tests := []struct {
		name string
		msgs []llm.Message
		min  int
		want int
	}{
		{"no request: everything between head and tail", []llm.Message{head, work, work, work}, 0, 20},
		{"request and summaries untouched", []llm.Message{head, s1, req, s2, work}, 0, 0},
		{"progress summary plus new work is rewritten", []llm.Message{head, s1, req, s2, work, work, work}, 0, 30},
		{"two summaries charge the second allowance", []llm.Message{head, work, work, req, work, work, work}, 0, 40 - EmergencySummaryAllowance},
		{"only the region before the request", []llm.Message{head, work, work, req, work}, 0, 20},
		{"no message dropped frees nothing", []llm.Message{head, work, req, work, work}, 0, 0},
		{"a region under the minimum is not summarized", []llm.Message{head, work, work, req, work, work}, 15, 20},
		{"every region under the minimum frees nothing", []llm.Message{head, s1, req, work, work}, 25, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts := make([]int, len(tt.msgs))
			for i := range counts {
				counts[i] = 10
			}
			if got := NewManager(nil, DefaultSettings()).deepDiveFreeable(tt.msgs, counts, 0, len(tt.msgs)-1, tt.min); got != tt.want {
				t.Fatalf("freeable = %d, want %d", got, tt.want)
			}
		})
	}
}
