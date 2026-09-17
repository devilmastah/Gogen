package server

// The conditional session_attach: a client re-focusing a pane whose rendered
// transcript is still current (the attach carries the last history payload's
// historyEpoch and the newest rendered index) must NOT pay for a history
// snapshot — the server neither deep-clones nor ships it. When only an
// in-flight round's partial output is missing, the attach ships a
// rewind-only frame instead. Every mismatch (appends, epoch bump, stale
// index) falls back to the full snapshot.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// conditionalStub completes turns instantly except for call N (blockCall,
// 1-based), which streams a deterministic partial and blocks until released —
// the attach-while-streaming shape.
type conditionalStub struct {
	mu        sync.Mutex
	calls     int
	blockCall int
	entered   int
	releaseCh chan struct{}
}

func newConditionalStub(blockCall int) *conditionalStub {
	return &conditionalStub{blockCall: blockCall, releaseCh: make(chan struct{})}
}

func (s *conditionalStub) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.releaseCh:
	default:
		close(s.releaseCh)
	}
}

func (s *conditionalStub) waitStreaming() {
	for {
		s.mu.Lock()
		e := s.entered
		s.mu.Unlock()
		if e >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *conditionalStub) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *conditionalStub) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	if h.OnStart != nil {
		h.OnStart()
	}
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == s.blockCall {
		if h.OnToken != nil {
			h.OnToken("the quick ")
		}
		s.mu.Lock()
		s.entered++
		s.mu.Unlock()
		select {
		case <-s.releaseCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
	return &llm.StreamResult{Content: "done"}, nil
}

func (s *conditionalStub) ModelContextLimit(_ context.Context) (int, error) { return 1000, nil }
func (s *conditionalStub) SetThinkingLevel(string)                          {}
func (s *conditionalStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *conditionalStub) SetModel(string) error { return nil }
func (s *conditionalStub) ModelName() string     { return "conditional-model" }

func newConditionalServer(t *testing.T, stub llm.LLMProvider) (*Server, *agent.Agent) {
	t.Helper()
	dir := t.TempDir()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	s := NewServer(a, &config.Config{})
	return s, a
}

// assertNoHistoryUntilPayloadDone asserts absence of a history frame for sid
// WITHOUT deadline reads (a gorilla deadline read poisons the connection).
// The send queue is FIFO, and the attach goroutine's LAST writes are the two
// config echoes: reading until a follow-up list_sessions reply AND both
// configs have been seen therefore covers everything the attach goroutine
// could have written — a history frame would have had to appear before them.
func assertNoHistoryUntilPayloadDone(t *testing.T, conn *websocket.Conn, sid string) {
	t.Helper()
	// The conditional attach was sent BEFORE list_sessions, so any history
	// frame it might have produced is enqueued ahead of the sessions reply;
	// everything read up to (and including) that reply therefore covers the
	// attach goroutine's possible output.
	msgs := readMessagesUntil(t, conn, 5*time.Second, func(msgs []WSMessage) bool {
		for _, m := range msgs {
			if m.Type == "sessions" {
				return true
			}
		}
		return false
	})
	for _, m := range msgs {
		if m.Type == "history" && m.SessionID == sid {
			t.Fatalf("skipped attach must not ship a history frame (rewindOnly=%v, %d entries)",
				m.RewindOnly, len(m.History))
		}
	}
}

// collectAttachPayload reads the attach reply frames after session_state and
// returns the history frame, if any (nil when the payload carried none before
// the two config echoes closed the goroutine's writes). Frames are COLLECTED
// (not skipped) because the history frame precedes the configs and a plain
// readUntil would discard it.
func collectAttachPayload(t *testing.T, conn *websocket.Conn, sid string) *WSMessage {
	t.Helper()
	// The attach goroutine writes history (optional) BEFORE the config
	// echoes, so two configs close the payload.
	msgs := readMessagesUntil(t, conn, 5*time.Second, func(msgs []WSMessage) bool {
		count := 0
		for _, m := range msgs {
			if m.Type == "config" && m.SessionID == sid {
				count++
			}
		}
		return count >= 2
	})
	for i := range msgs {
		if msgs[i].Type == "history" && msgs[i].SessionID == sid {
			return &msgs[i]
		}
	}
	return nil
}

func TestConditionalAttachSkipsHistoryWhenCurrent(t *testing.T) {
	stub := newConditionalStub(0)
	s, a := newConditionalServer(t, stub)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	drainHandshakePayloads(t, conn)
	sid := a.SessionID

	// One completed turn: user + assistant.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })

	// Full attach: the payload carries the history and stamps the epoch.
	epoch, lastShipped := a.HistoryFingerprint()
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: sid}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	hist := collectAttachPayload(t, conn, sid)
	if hist == nil {
		t.Fatal("full attach must deliver a history frame")
	}
	if hist.RewindOnly {
		t.Fatal("full attach history must not be marked rewindOnly")
	}
	if hist.HistoryEpoch != epoch {
		t.Fatalf("history epoch = %d, want agent epoch %d", hist.HistoryEpoch, epoch)
	}
	lastEntry := -1
	for _, h := range hist.History {
		if h.Index > lastEntry {
			lastEntry = h.Index
		}
	}
	if lastEntry != lastShipped {
		t.Fatalf("payload last index = %d, want the agent fingerprint's lastShipped %d (HistoryShips sync broken)", lastEntry, lastShipped)
	}

	// Conditional attach with the SAME fingerprint: the history snapshot is
	// skipped entirely — session_state (read below as part of the payload
	// drain) + two config echoes and nothing else.
	if err := conn.WriteJSON(WSMessage{
		Type:              "session_attach",
		SessionID:         sid,
		KnownHistoryEpoch: &epoch,
		KnownHistoryIndex: &lastShipped,
	}); err != nil {
		t.Fatalf("conditional attach: %v", err)
	}
	if h := collectAttachPayload(t, conn, sid); h != nil {
		t.Fatalf("current client must not receive a history frame, got %d entries", len(h.History))
	}
	// The conditional attach must not ship a history frame at all: send a
	// follow-up request and check the FIFO stream up to (and including) the
	// attach goroutine's final config echoes for a stray history frame.
	if err := conn.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("list_sessions: %v", err)
	}
	assertNoHistoryUntilPayloadDone(t, conn, sid)

	// A stale index (the client missed the last entry) falls back to the
	// full snapshot.
	stale := lastShipped - 1
	if err := conn.WriteJSON(WSMessage{
		Type:              "session_attach",
		SessionID:         sid,
		KnownHistoryEpoch: &epoch,
		KnownHistoryIndex: &stale,
	}); err != nil {
		t.Fatalf("stale attach: %v", err)
	}
	if h := collectAttachPayload(t, conn, sid); h == nil {
		t.Fatal("stale index must fall back to the full history snapshot")
	} else if h.RewindOnly {
		t.Fatal("stale-index fallback must be a full frame, not rewindOnly")
	}
}

func TestConditionalAttachRewindOnlyWhenMidTurn(t *testing.T) {
	stub := newConditionalStub(1)
	s, a := newConditionalServer(t, stub)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	drainHandshakePayloads(t, conn)
	sid := a.SessionID

	// Turn 1 streams a partial and blocks: the client has rendered through
	// the acked user message (the fingerprint's lastShipped), so the
	// transcript is "current" — only the round's partial output is missing.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitStreaming()
	epoch, lastShipped := a.HistoryFingerprint()

	if err := conn.WriteJSON(WSMessage{
		Type:              "session_attach",
		SessionID:         sid,
		KnownHistoryEpoch: &epoch,
		KnownHistoryIndex: &lastShipped,
	}); err != nil {
		t.Fatalf("conditional attach: %v", err)
	}
	m := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "history" && m.SessionID == sid
	})
	if !m.RewindOnly {
		t.Fatal("current client with an in-flight round must get a rewind-only frame")
	}
	if len(m.History) != 0 {
		t.Fatalf("rewind-only frame must carry no history entries, got %d", len(m.History))
	}
	if m.Rewind == nil || m.Rewind.Content != "the quick " {
		t.Fatalf("rewind-only frame must carry the in-flight partial, got %+v", m.Rewind)
	}

	// Complete the turn; the reply is appended. The same fingerprint claim
	// is now stale: the full snapshot must come back.
	stub.release()
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "turn_end" && m.SessionID == sid
	})
	if err := conn.WriteJSON(WSMessage{
		Type:              "session_attach",
		SessionID:         sid,
		KnownHistoryEpoch: &epoch,
		KnownHistoryIndex: &lastShipped,
	}); err != nil {
		t.Fatalf("stale attach after turn: %v", err)
	}
	m = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "history" && m.SessionID == sid
	})
	if m.RewindOnly {
		t.Fatal("stale client after appends must get a full history frame")
	}
	if len(m.History) == 0 {
		t.Fatal("full history frame must carry entries")
	}
}

// streamingTurnStub streams one token and completes immediately, so a
// finished turn leaves its output in the runtime's round buffer (that buffer
// is only cleared when the NEXT turn starts).
type streamingTurnStub struct{ conditionalStub }

func (s *streamingTurnStub) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	if h.OnStart != nil {
		h.OnStart()
	}
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	if h.OnToken != nil {
		h.OnToken("the quick ")
	}
	return &llm.StreamResult{Content: "the quick brown fox"}, nil
}

// TestConditionalAttachIdleSkipsLeftoverRewind pins the idle half of the
// rewind branch: a turn that STREAMED output leaves that output in the round
// buffer until the next turn starts, so an idle session still holds a rewind
// snapshot. A client whose fingerprint says it is current already rendered
// that output (it streamed it live, or merged the attach rewind) — shipping
// it again as rewind-only would make the client merge content it already
// shows. Idle + current therefore sends nothing at all.
func TestConditionalAttachIdleSkipsLeftoverRewind(t *testing.T) {
	stub := &streamingTurnStub{conditionalStub: *newConditionalStub(0)}
	s, a := newConditionalServer(t, stub)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	drainHandshakePayloads(t, conn)
	sid := a.SessionID

	// One completed streaming turn: the session is idle.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })
	rt, ok := s.registry.get(sid)
	if !ok {
		t.Fatalf("runtime for %s missing", sid)
	}
	// Each ROUND's stream end resets the round buffer, so a finished turn
	// normally leaves it empty and the old rewind branch fell through to the
	// idle return anyway. Pin the gating itself: with leftover output
	// present, an idle current client must still get NOTHING rather than a
	// rewind-only frame it would merge over content it already shows.
	rt.live.AppendContent("leftover round output")
	if rt.live.Snapshot() == nil {
		t.Fatal("precondition: injected round output must be visible to Snapshot")
	}

	epoch, lastShipped := a.HistoryFingerprint()
	if err := conn.WriteJSON(WSMessage{
		Type:              "session_attach",
		SessionID:         sid,
		KnownHistoryEpoch: &epoch,
		KnownHistoryIndex: &lastShipped,
	}); err != nil {
		t.Fatalf("conditional attach: %v", err)
	}
	if h := collectAttachPayload(t, conn, sid); h != nil {
		t.Fatalf("idle current client must not receive a history frame (rewindOnly=%v, %d entries)",
			h.RewindOnly, len(h.History))
	}
	if err := conn.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("list_sessions: %v", err)
	}
	assertNoHistoryUntilPayloadDone(t, conn, sid)
}

func TestConditionalAttachFullOnEpochMismatch(t *testing.T) {
	stub := newConditionalStub(0)
	s, a := newConditionalServer(t, stub)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	drainHandshakePayloads(t, conn)
	sid := a.SessionID

	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "q1", SessionID: sid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" && m.SessionID == sid })

	// An epoch the client has not seen (reshape happened elsewhere) must
	// fall back to the full snapshot even when the index coincidentally
	// matches: the epoch is what makes the index comparison meaningful.
	_, lastShipped := a.HistoryFingerprint()
	future := a.HistoryEpoch() + 1
	if err := conn.WriteJSON(WSMessage{
		Type:              "session_attach",
		SessionID:         sid,
		KnownHistoryEpoch: &future,
		KnownHistoryIndex: &lastShipped,
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	hist := collectAttachPayload(t, conn, sid)
	if hist == nil {
		t.Fatal("epoch mismatch must fall back to the full history snapshot")
	}
	if hist.RewindOnly {
		t.Fatal("epoch mismatch fallback must be a full frame, not rewindOnly")
	}
}
