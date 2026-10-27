package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// The background replay buffer holds ONLY display-only notes: every other
// rendering event is already reflected in Messages by the time focus
// returns and both drains discard it, so buffering it would just pin the
// payload (a tool result, a diff, an image data-URL) in memory until the
// drain threw it away.
func TestLiveSessionPendingBuffer(t *testing.T) {
	s := &liveSession{id: "s9"}

	t.Run("rendering events are dropped", func(t *testing.T) {
		s.enqueue(streamToolResultMsg{
			sid:    s.id,
			seq:    1,
			name:   "execute_command",
			result: strings.Repeat("x", 1<<20),
		})
		s.enqueue(streamTokenMsg{token: "batch", sid: s.id})
		if got := len(s.pending); got != 0 {
			t.Fatalf("pending = %d, want 0 (payload must not be buffered)", got)
		}
		if out := s.popAll(); len(out) != 0 {
			t.Fatalf("popAll = %v, want empty", out)
		}
	})

	t.Run("notes buffered in fifo order", func(t *testing.T) {
		s.enqueue(condensedNoteMsg{note: "note a", sid: s.id})
		s.enqueue(condensedNoteMsg{note: "note b", sid: s.id})
		out := s.popAll()
		if len(out) != 2 {
			t.Fatalf("popAll = %v, want [note a note b]", out)
		}
		if note, ok := out[0].(condensedNoteMsg); !ok || note.note != "note a" {
			t.Fatalf("out[0] = %#v, want note a", out[0])
		}
		if left := s.popAll(); len(left) != 0 {
			t.Fatalf("second popAll = %v, want empty", left)
		}
	})

	t.Run("note flood respects cap", func(t *testing.T) {
		s2 := &liveSession{id: "s10"}
		for i := 0; i < maxPendingStreamEvents+8; i++ {
			s2.enqueue(condensedNoteMsg{note: "x", sid: s2.id})
		}
		if got := len(s2.pending); got != maxPendingStreamEvents {
			t.Fatalf("len = %d, want cap %d", got, maxPendingStreamEvents)
		}
	})
}

// Focusing a streaming session renders the full committed history (web
// attach parity) instead of a join notice: the SnapshotMessages render is
// authoritative, and the drained buffer is discarded (replaying it would
// duplicate the finished rounds).
func TestJoinStreamingSessionRendersHistory(t *testing.T) {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	a2 := newSwitchTestAgent(t)
	a2.Messages = []llm.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
	}
	target := &liveSession{id: "s2", label: "bg", agent: a2, streaming: true}
	// Buffered while in the background: committed content (already in the
	// snapshot) plus the in-flight round's head (accepted residual).
	target.enqueue(streamStartMsg{})
	target.enqueue(streamTokenMsg{token: "first answer"})
	target.enqueue(streamTokenMsg{token: "in-flight head"})

	m := &Model{lives: newLiveSessions(newSwitchTestAgent(t)), viewport: vp}
	m.lives.Add(target.agent, "bg")
	m.joinStreamingSession(target, target.popAll())

	if len(target.pending) != 0 {
		t.Fatal("pending buffer not drained")
	}
	if !target.transcriptStale {
		t.Fatal("transcriptStale must latch on join")
	}
	joined := strings.Join(m.chatLines, "\n")
	if !strings.Contains(joined, "mid-turn") {
		t.Fatal("join notice missing")
	}
	if !strings.Contains(joined, "first question") || !strings.Contains(joined, "first answer") {
		t.Fatalf("committed history missing from transcript:\n%s", joined)
	}
	// The in-flight round's head is the accepted residual: not in Messages
	// and not replayed from the buffer — the turn-end rebuild heals it.
	if strings.Contains(joined, "in-flight head") {
		t.Fatalf("in-flight head must not be replayed:\n%s", joined)
	}
	// Committed content appears exactly once (snapshot, not snapshot+replay).
	if n := strings.Count(joined, "first answer"); n != 1 {
		t.Fatalf("committed line rendered %d times, want 1:\n%s", n, joined)
	}
}

// switchToLive drains the replay buffer on both sides of the focus flip, so
// an event emitted between the two drains (gate still closed) is caught by
// the second drain instead of being stranded in the buffer — and the drained
// events are discarded: content committed before the join snapshot appears
// exactly once (rendered by the snapshot, never replayed).
func TestSwitchToLiveDrainsAroundFlip(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	a2.Messages = []llm.Message{{Role: "user", Content: "early question"}}
	s2 := m.lives.Add(a2, "second")
	s2.streaming = true
	// Committed before the join snapshot: the drain must DISCARD these
	// (a replay would duplicate the snapshot's line).
	s2.enqueue(streamStartMsg{sid: s2.id})
	s2.enqueue(streamTokenMsg{token: "early question", sid: s2.id})

	m.switchToLive(1)

	if len(s2.pending) != 0 {
		t.Fatalf("pending not fully drained around the flip: %d left", len(s2.pending))
	}
	if !s2.transcriptStale {
		t.Fatal("mid-turn join must latch transcriptStale")
	}
	joined := strings.Join(m.chatLines, "\n")
	if n := strings.Count(joined, "early question"); n != 1 {
		t.Fatalf("committed line rendered %d times, want exactly 1 (snapshot, no stale replay):\n%s", n, joined)
	}
}

type fakeProgram struct{ sent *[]tea.Msg }

func (f *fakeProgram) Send(m tea.Msg) { *f.sent = append(*f.sent, m) }

func TestAdapterBuffersWhenBackground(t *testing.T) {
	var sent []tea.Msg
	prog := &fakeProgram{sent: &sent}
	sess := &liveSession{id: "s3"}
	sess.focused.Store(false)

	ad := NewStreamAdapter(sess.id, 0, prog, sess)
	ad.send(streamTokenMsg{token: "z"})
	if len(sent) != 0 {
		t.Fatal("background event must not be sent to the program")
	}
	if n := len(sess.pending); n != 0 {
		t.Fatalf("pending = %d, want 0 (only display-only notes are buffered)", n)
	}
	ad.send(condensedNoteMsg{note: "note", sid: sess.id})
	if n := len(sess.pending); n != 1 {
		t.Fatalf("pending = %d, want the buffered note", n)
	}

	sess.focused.Store(true)
	ad.send(streamTokenMsg{token: "y"})
	if len(sent) != 1 || len(sess.pending) != 1 {
		t.Fatal("focused event must send directly")
	}
}

// Turn-end convergence: after a mid-turn join (transcriptStale), the
// focused session's transcript is re-rendered from the authoritative
// history once the turn's terminal lands — including the completed reply
// the join's snapshot could not contain.
func TestJoinTranscriptConvergesAtTurnEnd(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	a2.Messages = []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	s2 := m.lives.Add(a2, "second")
	s2.streaming = true
	s2.turnSeq = 1
	m.switchToLive(1)
	if !s2.transcriptStale {
		t.Fatal("join must latch transcriptStale")
	}

	// The turn completes: the final reply is committed to Messages before
	// the terminal message reaches the Update thread.
	a2.Messages = append(a2.Messages, llm.Message{Role: "assistant", Content: "final reply"})
	_, _ = m.handleTurnFinishedMsg(s2.id, 1, nil)

	if s2.transcriptStale {
		t.Fatal("turn end must clear transcriptStale")
	}
	// The transcript equals a fresh renderMessages(Messages): the transient
	// join notice is gone and the completed reply is present.
	want := renderMessages(a2.SnapshotMessages(), a2.WorkingDir, a2.CurrentModel(), a2.Mode.String(), m.verbose)
	if strings.Join(m.chatLines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("converged transcript != fresh renderMessages(Messages):\ngot:\n%s\nwant:\n%s",
			strings.Join(m.chatLines, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(strings.Join(m.chatLines, "\n"), "final reply") {
		t.Fatalf("converged transcript missing the completed reply:\n%s", m.chatLines)
	}
}

// A focused turn that never had a mid-turn join must NOT rebuild at turn
// end: the incremental transcript (and scroll position) is preserved.
func TestFocusedTurnWithoutJoinDoesNotRebuild(t *testing.T) {
	m := dragModel(t)
	m.chatLines = []string{"user line", "assistant streaming line"}
	s1 := m.lives.Active()
	s1.streaming = true
	s1.turnSeq = 1
	// The turn goroutine commits the reply before its terminal lands.
	m.agent.Messages = append(m.agent.Messages, llm.Message{Role: "assistant", Content: "committed reply"})

	_, _ = m.handleTurnFinishedMsg(s1.id, 1, nil)

	if s1.transcriptStale {
		t.Fatal("no join must not set transcriptStale")
	}
	if got := strings.Join(m.chatLines, "\n"); got != "user line\nassistant streaming line" {
		t.Fatalf("transcript rebuilt without a join: %q", got)
	}
}

// fakeStreamProvider is a minimal LLMProvider that returns a fixed reply:
// it drives REAL agent turns (with their Messages appends) from a
// background goroutine in the join race tests. When the handler set carries
// an OnToken callback it streams a few tokens through it (feeding the
// round buffer via the adapter handlers); with nil/empty handlers it stays
// silent.
type fakeStreamProvider struct{}

func (fakeStreamProvider) GenerateResponse(ctx context.Context, messages []llm.Message, allowedTools map[string]struct{}, extraTools []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "answer"}, nil
}

func (fakeStreamProvider) GenerateResponseStream(ctx context.Context, messages []llm.Message, allowedTools map[string]struct{}, extraTools []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if h != nil && h.OnToken != nil {
		for i := 0; i < 16; i++ {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			h.OnToken(fmt.Sprintf("tok%d ", i))
		}
	}
	return &llm.StreamResult{Content: "answer"}, nil
}

func (fakeStreamProvider) ModelContextLimit(ctx context.Context) (int, error) { return 100000, nil }

func (fakeStreamProvider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}

func (fakeStreamProvider) SetModel(id string) error { return nil }

func (fakeStreamProvider) ModelName() string { return "fake-model" }

func (fakeStreamProvider) SetThinkingLevel(level string) {}

// TestJoinConcurrentWithTurn runs real turns (Messages appends) on a
// streaming session while the Update thread repeatedly joins and leaves it:
// the join path snapshots via SnapshotMessages and must be race-free
// against the turn's appends (mirrors the server's
// TestContextStatsConcurrentWithTurn). Run with -race.
func TestJoinConcurrentWithTurn(t *testing.T) {
	m := dragModel(t)
	dir := t.TempDir()
	p := fakeStreamProvider{}
	exec := agent.NewExecutorWithGuard(dir, agent.NewCommandGuard("", nil))
	cm := contextmgr.NewManager(p, contextmgr.Settings{ContextLimit: 100000})
	a2 := agent.NewAgent(p, exec, cm)
	s2 := m.lives.Add(a2, "second")
	s2.streaming = true
	s2.turnSeq = 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			if _, err := a2.StreamProcessInput(ctx, fmt.Sprintf("question %d", i), nil); err != nil {
				return
			}
		}
	}()

	// Join/leave the streaming session repeatedly: every join snapshots
	// Messages (joinStreamingSession) while the turn goroutine appends.
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			m.switchToLive(1)
		} else {
			m.switchToLive(0)
		}
	}
	cancel()
	<-done
}

// discardProgram drops every Send: the round-buffer race test only cares
// about the buffer, and discarding keeps the batcher timer goroutines from
// racing on a shared sent slice.
type discardProgram struct{}

func (discardProgram) Send(m tea.Msg) {}

// TestJoinRoundBufferConcurrentWithTurn runs real turns whose stream
// goroutine feeds the round buffer (via the adapter handlers) while the
// Update thread repeatedly joins the session (round.Snapshot + render):
// the buffer must be race-free under concurrent append/snapshot (mirrors
// the server's liveTurnState contract). Run with -race.
func TestJoinRoundBufferConcurrentWithTurn(t *testing.T) {
	m := dragModel(t)
	dir := t.TempDir()
	p := fakeStreamProvider{}
	exec := agent.NewExecutorWithGuard(dir, agent.NewCommandGuard("", nil))
	cm := contextmgr.NewManager(p, contextmgr.Settings{ContextLimit: 100000})
	a2 := agent.NewAgent(p, exec, cm)
	s2 := m.lives.Add(a2, "second")
	s2.streaming = true
	s2.turnSeq = 1
	adapter := NewStreamAdapter(s2.id, 1, discardProgram{}, s2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			if _, err := a2.StreamProcessInput(ctx, fmt.Sprintf("question %d", i), adapter.Handlers()); err != nil {
				return
			}
		}
	}()

	// Join/leave the streaming session repeatedly: every join snapshots
	// the round buffer (joinStreamingSession) while the turn goroutine
	// appends to it.
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			m.switchToLive(1)
		} else {
			m.switchToLive(0)
		}
	}
	cancel()
	<-done
}

// Watch A stream a few tokens, switch to B, switch back mid-round: the
// transcript must contain the full history, the current reply from its
// FIRST token, and continue live from the pre-seeded line (no duplicate
// assistant line).
func TestJoinRendersInFlightRound(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	a2.Messages = []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
	}
	s2 := m.lives.Add(a2, "second")
	s2.streaming = true
	s2.turnSeq = 1
	// The round buffer holds what streamed while s2 was focused (the
	// adapter appends regardless of focus).
	s2.round.AppendThinking("let me think")
	s2.round.AppendContent("partial re")

	// Watch a few tokens, switch away, come back mid-round.
	m.switchToLive(1)
	m.switchToLive(0)
	m.switchToLive(1)

	joined := strings.Join(m.chatLines, "\n")
	if !strings.Contains(joined, "q1") || !strings.Contains(joined, "a1") {
		t.Fatalf("committed history missing from transcript:\n%s", joined)
	}
	if !strings.Contains(joined, "partial re") {
		t.Fatalf("in-flight reply missing from its first token:\n%s", joined)
	}
	if !strings.Contains(joined, "<thinking>") {
		t.Fatalf("in-flight thinking missing from transcript:\n%s", joined)
	}
	// Pre-seeded stream state: the live tail continues the SAME line.
	if m.streamAssistantLine < 0 || m.streamAssistantLine >= len(m.chatLines) {
		t.Fatalf("assistant line not pre-seeded: %d", m.streamAssistantLine)
	}
	if got := m.chatLines[m.streamAssistantLine]; !strings.HasSuffix(got, "partial re") {
		t.Fatalf("assistant line = %q, want suffix %q", got, "partial re")
	}
	// Live continuation appends seamlessly (no new assistant line).
	before := len(m.chatLines)
	m.handleStreamToken("ply")
	if len(m.chatLines) != before {
		t.Fatalf("live token opened a new line: %d -> %d", before, len(m.chatLines))
	}
	if got := m.chatLines[m.streamAssistantLine]; !strings.HasSuffix(got, "partial reply") {
		t.Fatalf("assistant line = %q, want suffix %q", got, "partial reply")
	}
}

// Join between rounds: the just-completed round's content is in Messages
// (the snapshot renders it) and the round buffer was cleared at round end,
// so the join must not render it a second time.
func TestJoinBetweenRoundsNoDuplicate(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	a2.Messages = []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "completed answer"},
	}
	s2 := m.lives.Add(a2, "second")
	s2.streaming = true
	s2.turnSeq = 1
	// The round completed: content committed to Messages, buffer cleared
	// at round end (the adapter's OnStreamEnd does this).
	s2.round.AppendContent("completed answer")
	s2.round.Reset()

	m.switchToLive(1)
	joined := strings.Join(m.chatLines, "\n")
	if n := strings.Count(joined, "completed answer"); n != 1 {
		t.Fatalf("completed round rendered %d times, want 1:\n%s", n, joined)
	}
	// Between rounds: nothing in flight, so the join notice shows.
	if !strings.Contains(joined, "mid-turn") {
		t.Fatalf("join notice expected between rounds:\n%s", joined)
	}
}

// The round buffer clears at round end and turn start — no leak across
// turns, bounded memory (one round's output at a time).
func TestRoundBufferResets(t *testing.T) {
	s := &liveSession{id: "s9"}
	s.round.AppendThinking("think")
	s.round.AppendContent("abc")
	s.round.ToolStart(0, "call_1", "read_file")
	s.round.AppendToolArgs(0, `{"path": "a.go"}`)
	rw := s.round.Snapshot()
	if rw == nil || rw.Content != "abc" || rw.Thinking != "think" || len(rw.ToolCalls) != 1 {
		t.Fatalf("snapshot = %+v", rw)
	}
	if rw.ToolCalls[0].Name != "read_file" || rw.ToolCalls[0].Args != `{"path": "a.go"}` {
		t.Fatalf("tool call = %+v", rw.ToolCalls[0])
	}
	// Round end (OnStreamEnd): cleared.
	s.round.Reset()
	if rw := s.round.Snapshot(); rw != nil {
		t.Fatalf("buffer must clear on round end: %+v", rw)
	}
	// Turn start (OnStart) on an already-empty buffer: still empty.
	s.round.Reset()
	if rw := s.round.Snapshot(); rw != nil {
		t.Fatalf("buffer must stay empty after turn start: %+v", rw)
	}
}

// The adapter feeds the round buffer from the handler callbacks whether
// the session is focused or not (the pending buffer only accumulates while
// unfocused — that is the gap the round buffer closes), and round end
// clears it.
func TestAdapterFeedsRoundBufferRegardlessOfFocus(t *testing.T) {
	var sent []tea.Msg
	prog := &fakeProgram{sent: &sent}
	sess := &liveSession{id: "s3"}
	sess.focused.Store(false)
	ad := NewStreamAdapter(sess.id, 1, prog, sess)
	h := ad.Handlers()
	h.OnThinkingToken("hmm")
	h.OnToken("hello ")
	h.OnToolCallStart(0, "call_1", "read_file")
	h.OnToolCallArgsDelta(0, "call_1", "", `{"path": "a.go"}`)

	rw := sess.round.Snapshot()
	if rw == nil {
		t.Fatal("round buffer empty after adapter events")
	}
	if rw.Thinking != "hmm" || rw.Content != "hello " {
		t.Fatalf("buffer = %+v", rw)
	}
	if len(rw.ToolCalls) != 1 || rw.ToolCalls[0].Name != "read_file" || rw.ToolCalls[0].Args != `{"path": "a.go"}` {
		t.Fatalf("tool calls = %+v", rw.ToolCalls)
	}
	// Round end clears the buffer (the next join must not re-render it).
	h.OnStreamEnd()
	if rw := sess.round.Snapshot(); rw != nil {
		t.Fatalf("buffer must clear on round end: %+v", rw)
	}
}
