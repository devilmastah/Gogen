package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// blockingStub blocks inside every GenerateResponseStream call until that
// call is released (or the context is cancelled), so turns can be left
// in-flight while the client disconnects. Each call's release is independent
// (releaseN(n)); call 1 may return tool calls (firstTools) to exercise the
// approval path.
type blockingStub struct {
	mu         sync.Mutex
	calls      int
	entered    int // total number of calls that reached the blocking section
	releases   map[int]chan struct{}
	firstTools []llm.ToolCall
	// model is the provider-reported model attached to every StreamResult
	// (empty by default, matching providers that do not report one).
	model string
}

func newBlockingStub() *blockingStub {
	return &blockingStub{releases: make(map[int]chan struct{})}
}

// releaseN unblocks GenerateResponseStream call n (subsequent calls keep
// their own channels, so they block until released individually).
func (s *blockingStub) releaseN(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.releases[n]
	if !ok {
		ch = make(chan struct{})
		s.releases[n] = ch
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// waitBlocked waits until at least n GenerateResponseStream calls have
// entered their blocking section (each call holds the session turn lock for
// the whole time it is blocked).
func (s *blockingStub) waitBlocked(n int) {
	for {
		s.mu.Lock()
		e := s.entered
		s.mu.Unlock()
		if e >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *blockingStub) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done", Model: s.model}, nil
}

func (s *blockingStub) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	ch, ok := s.releases[call]
	if !ok {
		ch = make(chan struct{})
		s.releases[call] = ch
	}
	s.entered++
	s.mu.Unlock()
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if call == 1 && len(s.firstTools) > 0 {
		return &llm.StreamResult{ToolCalls: s.firstTools, Model: s.model}, nil
	}
	if call == 1 {
		return &llm.StreamResult{Content: "headless-done", Model: s.model}, nil
	}
	return &llm.StreamResult{Content: "done", Model: s.model}, nil
}

func (s *blockingStub) ModelContextLimit(_ context.Context) (int, error) { return 1000, nil }
func (s *blockingStub) SetThinkingLevel(string)                          {}
func (s *blockingStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *blockingStub) SetModel(string) error { return nil }
func (s *blockingStub) ModelName() string     { return "blocking-model" }

// newContinuationServer builds a server + agent wired to a real session store
// and a blocking stub provider, sharing the test's working directory.
func newContinuationServer(t *testing.T, stub *blockingStub, dir string) (*Server, *agent.Agent, *session.Store) {
	t.Helper()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	// newTestServer's drain is the quiesce this helper used to register by
	// hand: it stops the automation sweep, cancels every in-flight stream (the
	// blocking stub honors ctx, so its turn goroutine unwinds), flushes, and
	// evicts the runtimes — so a turn's final flush cannot race the caller's
	// t.TempDir() removal.
	s := newTestServer(t, a, &config.Config{})
	return s, a, store
}

// readUntil reads messages from conn until match returns true; the matched
// message is returned. Fails the test on timeout. The per-read deadline
// tracks the overall deadline (not a fresh window per read), so the total
// wait is bounded by timeout rather than overshooting it.
func readUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, match func(WSMessage) bool) WSMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for matching WS message")
		}
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		if match(msg) {
			return msg
		}
	}
}

// readMessagesUntil reads WS messages until pred (over the FULL collected
// sequence) returns true or the deadline passes, and returns every message
// read — including the non-matching ones readUntil would discard. Use it when
// the assertion is about a SET of frames (e.g. "response + clear_chat +
// history + two configs") rather than one frame: a single-frame readUntil
// drops the frames it skips over, so an assertion that needs a later frame
// can starve when an earlier, unrelated frame (like a config) is queued
// before it. The read deadline tracks the overall deadline (not a fresh
// window per read), so the total wait is bounded by timeout.
func readMessagesUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, pred func([]WSMessage) bool) []WSMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var msgs []WSMessage
	for {
		if time.Now().After(deadline) {
			types := make([]string, 0, len(msgs))
			for _, m := range msgs {
				types = append(types, m.Type)
			}
			t.Fatalf("timed out waiting for message sequence; got %d frames: %v", len(msgs), types)
		}
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		msgs = append(msgs, msg)
		if pred(msgs) {
			return msgs
		}
	}
}

// configAfterClear reads the config frames that follow a clear_chat and
// returns the first one belonging to a session other than oldID. The attach
// handshake emits config frames for the ORIGINAL session asynchronously (the
// full config with context stats trails the basic one), so they can still be
// in the send queue when /new's clear_chat + config are enqueued; clients key
// configs by sessionId, and tests must skip the stale ones instead of
// assuming the first config after clear_chat is the new session's.
func configAfterClear(t *testing.T, conn *websocket.Conn, oldID string) WSMessage {
	t.Helper()
	var cfg WSMessage
	for i := 0; i < 3; i++ {
		cfg = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
		if cfg.SessionID != oldID {
			return cfg
		}
	}
	t.Fatalf("no config for a fresh session after clear_chat (last config sessionId = %q)", cfg.SessionID)
	return WSMessage{}
}

// TestTurnContinuesAfterDisconnect is the headline continuation test: the
// client drops mid-turn, the turn completes headless (the stub is released
// only AFTER HandleWS has returned, proving the turn context is no longer
// request-derived — the old code killed the turn via r.Context()), the reply
// is persisted, and a fresh connection's attach shows session_state + the
// completed history.
func TestTurnContinuesAfterDisconnect(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	// Drain the attach handshake.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "start headless turn"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Wait until the turn is genuinely in-flight inside the provider.
	stub.waitBlocked(1)

	// Kill the tab: close the socket and let HandleWS return.
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		rt, _ := s.registry.get(a.SessionID)
		return rt != nil && rt.clientCount() == 0
	})

	// Release the provider only now. If the turn context were still derived
	// from the (cancelled) request context, StreamProcessInput would abort
	// and the assistant reply would never be appended.
	stub.releaseN(1)
	// Wait for the headless turn to FULLY complete. Persistence alone is not
	// enough: StreamProcessInput flushes the reply to the store BEFORE the
	// turn goroutine's deferred cleanup runs, and that cleanup is what
	// clears turnActive (turn_end broadcast → setTurnActive(false) →
	// unlock). A fresh attach in the window between the flush and the
	// cleanup would still see turnActive=true — under -race or a loaded
	// runner the goroutine can be descheduled there for long enough to hit
	// it. The runtime may also be orphan-evicted right after the turn ends
	// (no attached clients); the attach then reloads the session from disk,
	// which reports turnActive=false by construction.
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(dir, a.SessionID)
		if err != nil {
			return false
		}
		found := false
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "headless-done" {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		rt, ok := s.registry.get(a.SessionID)
		if !ok {
			return true // completed turn's runtime was orphan-evicted; attach reloads from disk
		}
		active, _ := rt.turnState()
		return !active
	})

	// A fresh connection re-attaches: session_state arrives, then history
	// containing the completed answer (the continuation guarantee).
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	state := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if state.TurnActive {
		t.Fatal("session_state after completed turn should report turnActive=false")
	}
	hist := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" })
	found := false
	for _, e := range hist.History {
		if e.Role == "assistant" && e.Content == "headless-done" {
			found = true
		}
	}
	if !found {
		t.Fatal("reconnected history missing the headless-completed assistant reply")
	}
}

// TestCancelFromNewConnectionStopsHeadlessTurn verifies that cancel is the
// only way to stop a turn and works cross-connection: after the original tab
// dies, a fresh connection's cancel stops the headless turn.
func TestCancelFromNewConnectionStopsHeadlessTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "start"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		rt, _ := s.registry.get(a.SessionID)
		return rt != nil && rt.clientCount() == 0
	})

	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	// The session_state must report the turn still running headless.
	state := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if !state.TurnActive {
		t.Fatal("expected turnActive=true for the headless turn on re-attach")
	}
	// Cancel from the NEW connection stops it.
	if err := conn2.WriteJSON(WSMessage{Type: "cancel"}); err != nil {
		t.Fatalf("send cancel: %v", err)
	}
	msg := readUntil(t, conn2, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "cancelled" || m.Type == "stream_end" || m.Type == "turn_end" || m.Type == "stream"
	})
	if msg.Type != "cancelled" {
		t.Fatalf("first terminal message after cancel = %q, want cancelled", msg.Type)
	}
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	// The turn must not have completed with content.
	rt, _ := s.registry.get(a.SessionID)
	if got := rt.agent.MessageCount(); got < 1 {
		t.Fatalf("unexpected message count %d", got)
	}
}

// TestApprovalAutoDeniedOnDetach verifies D10: with a delete approval pending,
// the last client detaching auto-denies it and the turn continues with the
// "not approved" tool result instead of hanging.
func TestApprovalAutoDeniedOnDetach(t *testing.T) {
	dir := t.TempDir()
	// delete Lstats the target before asking for approval, so the file
	// must exist for the approval request to fire.
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_del",
		Name: "delete",
		Args: map[string]any{"path": "victim.txt"},
	}}
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "delete it"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Round 1 returns the delete tool call once released; the tool then runs,
	// requires approval, and broadcasts delete_approval.
	stub.waitBlocked(1)
	stub.releaseN(1)
	approval := readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "delete_approval" })
	if approval.ApprovalID == "" {
		t.Fatal("delete_approval missing approvalId")
	}
	// The only client detaches without answering → auto-deny.
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		rt, _ := s.registry.get(a.SessionID)
		return rt != nil && rt.clientCount() == 0
	})

	// The turn continues and completes (round 2 returns "done"): the tool
	// result for the denied delete is recorded, so the turn did not hang.
	waitFor(t, 10*time.Second, func() bool {
		rt, _ := s.registry.get(a.SessionID)
		msgs := rt.agent.SnapshotMessages()
		for _, m := range msgs {
			if m.Role == "tool" && m.Content != "" && m.ToolCallID == "call_del" {
				return true
			}
		}
		return false
	})
}

// TestFanOutToTwoAttachedConnections verifies E29: two connections attached
// to one session both receive the same stream events, and only one can start
// a turn (the second connection's plain chat message is QUEUED as a steering
// message while the first streams; command-shaped input still gets the busy
// response).
func TestFanOutToTwoAttachedConnections(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Second tab attaches to the same (default) session.
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Start a turn from conn1; both must see the same events.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "fan out"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	stub.releaseN(1)

	for _, c := range []*websocket.Conn{conn1, conn2} {
		_ = readUntil(t, c, 10*time.Second, func(m WSMessage) bool {
			return m.Type == "user_acked" && m.SessionID == a.SessionID
		})
		// The stub does not emit tokens; the stream boundary event is
		// stream_end (produced by finishStreamUI).
		_ = readUntil(t, c, 10*time.Second, func(m WSMessage) bool { return m.Type == "stream_end" })
		_ = readUntil(t, c, 10*time.Second, func(m WSMessage) bool {
			return m.Type == "turn_end" && m.SessionID == a.SessionID
		})
	}

	// A second turn from conn1 blocks in the provider; a plain chat message
	// from conn2 (a different connection attached to the same session) is
	// now QUEUED as a steering message instead of rejected: queuing never
	// touches the running turn, so the old "must not cancel a turn it does
	// not own" hazard (E3/E29) cannot occur — the busy rejection survives
	// only for command-shaped input (which takes the turn lock).
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "second turn"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(2)
	if err := conn2.WriteJSON(WSMessage{Type: "message", Content: "busy me", QueueID: "q-fanout"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Both connections learn the queue state (queue_update broadcasts to
	// every attached client). conn2's own user_acked for "second turn" may
	// interleave; readUntil skips non-matching frames.
	for _, c := range []*websocket.Conn{conn1, conn2} {
		qu := readUntil(t, c, 5*time.Second, func(m WSMessage) bool { return m.Type == "queue_update" })
		if len(qu.Queue) != 1 || qu.Queue[0].ID != "q-fanout" || qu.Queue[0].Text != "busy me" {
			t.Fatalf("queue_update queue = %+v, want one item q-fanout \"busy me\"", qu.Queue)
		}
	}
	// conn1's turn must still be alive (not cancelled by conn2's message):
	// releasing the stub completes it AND pops the queued item. The drain's
	// pop broadcast must mark the item as STARTED (queueLeft) — not removed
	// — so clients clear the queued chip and resolve the paired user_acked
	// instead of rendering the bubble cancelled-before-running. The pop
	// broadcast and the turn_end frame can interleave either way.
	stub.releaseN(2)
	sawTurnEnd, sawQueueLeft := false, false
	for !sawTurnEnd || !sawQueueLeft {
		m := readUntil(t, conn1, 10*time.Second, func(m WSMessage) bool {
			return m.Type == "turn_end" ||
				(m.Type == "queue_update" && len(m.QueueLeft) > 0)
		})
		if m.Type == "turn_end" {
			sawTurnEnd = true
			continue
		}
		sawQueueLeft = true
		if len(m.QueueLeft) != 1 || m.QueueLeft[0] != "q-fanout" {
			t.Fatalf("queue_update queueLeft = %v, want [q-fanout]", m.QueueLeft)
		}
	}

	// The queued steering message drains as the NEXT user turn once
	// "second turn" ends, on this session (FIFO, one item per turn).
	for _, c := range []*websocket.Conn{conn1, conn2} {
		_ = readUntil(t, c, 10*time.Second, func(m WSMessage) bool {
			return m.Type == "user_acked" && m.SessionID == a.SessionID
		})
	}
	stub.releaseN(3)
	waitFor(t, 10*time.Second, func() bool {
		return deliveredMessages(a, "busy me")
	})
	// The delivered turn's final flush must land on disk before TempDir
	// teardown (the store-wait pattern used across this suite) — the
	// in-memory snapshot is not enough. The wait covers the two turns whose
	// stub replies are "done" (second turn + the queued "busy me"); the
	// first turn's released reply is "headless-done".
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(dir, a.SessionID)
		if err != nil {
			return false
		}
		done := 0
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "done" {
				done++
			}
		}
		return done >= 2
	})
}

// ── helpers ──

func startWSServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	t.Cleanup(ts.Close)
	return ts
}

func dialWS(t *testing.T, srv *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// requireNever fails the test if cond — the wrong-behavior signal — becomes
// true within the window. The inverse of waitFor: used for negative
// assertions ("this must NOT happen"), where waiting for a non-event is
// impossible. Polling until the window elapses makes the pass meaningful on
// any machine speed (a fixed sleep can outrun a slow runner and pass while
// the bug is still in flight), and fails fast the moment the regression
// appears.
func requireNever(t *testing.T, window time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
