package server

// Regression tests for the /compact (handleWSCompact) turn lifecycle:
//
//  1. The compact goroutine must broadcast turn_end after its result frames
//     (compacting → response → context → turn_end), exactly like a regular
//     turn: a client that attaches mid-compact latches "busy" plus a pending
//     history refetch on session_state, and only turn_end converges it —
//     without the broadcast the pane stays busy/stale until some unrelated
//     turn ends.
//  2. The compact goroutine must register with rt.stream (begin/end) like
//     startTurn does: the delete path drains in-flight work via
//     cancelInFlight and then flushes/deletes under turnMu. Pre-fix the
//     compact held turnMu invisibly (no stream registration), so the delete
//     proceeded mid-compaction and the compact's success-path FlushSession
//     re-created the file Store.Delete had just removed (delete
//     resurrection).
//  3. The same registration lets session_close cancel + drain the compact:
//     closeRuntime must not return while the compact still holds turnMu.

import (
	"context"
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

// compactBlockingStub wraps blockingStub with a GenerateResponse that blocks
// until released (the context summarizer's provider entry point), so a
// /compact can be held mid-flight while the test drives teardown against it.
type compactBlockingStub struct {
	*blockingStub

	mu          sync.Mutex
	calls       int
	blocked     chan struct{} // closed when the first summarizer call enters
	release     chan struct{} // closed to unblock the summarizer
	releaseOnce sync.Once
}

func newCompactBlockingStub() *compactBlockingStub {
	return &compactBlockingStub{
		blockingStub: newBlockingStub(),
		blocked:      make(chan struct{}),
		release:      make(chan struct{}),
	}
}

func (s *compactBlockingStub) GenerateResponse(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	s.mu.Unlock()
	if first {
		close(s.blocked)
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	// A cancellation that raced the select above must still win: once the
	// context is done the summarizer reports it instead of a summary, so a
	// cancelled compact can never take its success path (and flush) again.
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	return llm.Response{Content: "summary of the middle"}, nil
}

func (s *compactBlockingStub) waitSummarizerBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-s.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("summarizer was never reached — the compact failed before summarizing")
	}
}

func (s *compactBlockingStub) releaseSummarizer() {
	s.releaseOnce.Do(func() { close(s.release) })
}

// seedCompactHistory fills the agent with enough history for CompactHistory
// to reach the summarizer: a head user message, a middle well above the
// 500-token minimum-middle guard, and the 12-message default tail
// (DefaultCompactKeepRecentMessages).
func seedCompactHistory(a *agent.Agent) {
	msgs := make([]llm.Message, 0, 30)
	msgs = append(msgs, llm.Message{Role: "user", Content: "overall task"})
	for i := 0; i < 28; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, llm.Message{Role: role, Content: strings.Repeat("progress log line with plenty of words ", 12)})
	}
	msgs = append(msgs, llm.Message{Role: "assistant", Content: "final answer"})
	a.Messages = msgs
}

// startCompactTestServer builds a one-connection compact test: server + agent
// wired to a summarizer-blocking stub (registered as the provider ITSELF, so
// the GenerateResponse override is the one the summarizer reaches — embedding
// alone would hand the manager the inner blockingStub and bypass it), history
// seeded, client attached.
func startCompactTestServer(t *testing.T) (*Server, *agent.Agent, *compactBlockingStub, *session.Store, string, *websocket.Conn) {
	t.Helper()
	stub := newCompactBlockingStub()
	t.Cleanup(stub.releaseSummarizer) // never leak a blocked summarizer goroutine
	dir := t.TempDir()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	s := newTestServer(t, a, &config.Config{})
	seedCompactHistory(a)
	srv := startWSServer(t, s)
	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	return s, a, stub, store, dir, conn
}

// TestWSCompactEmitsResultThenTurnEnd pins the frame order of a successful
// /compact: response and context first, then the turn_end broadcast. The
// client clears its compacting indicator on the response and converges a
// mid-compact attach's stale transcript on turn_end — a missing turn_end
// leaves the pane busy/stale until some unrelated turn ends.
func TestWSCompactEmitsResultThenTurnEnd(t *testing.T) {
	_, a, stub, _, _, conn := startCompactTestServer(t)

	if err := conn.WriteJSON(WSMessage{Type: "compact", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send compact: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "compacting" })
	stub.waitSummarizerBlocked(t)
	stub.releaseSummarizer()

	readMessagesUntil(t, conn, 10*time.Second, func(msgs []WSMessage) bool {
		resp, ctxMsg, end := -1, -1, -1
		for i, m := range msgs {
			switch {
			case m.Type == "response" && strings.HasPrefix(m.Content, "History compacted"):
				resp = i
			case m.Type == "context":
				ctxMsg = i
			case m.Type == "turn_end":
				end = i
			}
		}
		return resp >= 0 && ctxMsg >= 0 && end >= 0 && resp < end && ctxMsg < end
	})
}

// TestWSCompactDeleteDuringCompactNoResurrection verifies the compact's
// stream registration: deleting a session whose compact holds turnMu must
// cancel + drain the compact BEFORE removing the file, so the compact can
// never flush the file back afterwards (delete resurrection, same shape as
// TestDeleteSessionWithRunningTurnNoResurrection for regular turns).
func TestWSCompactDeleteDuringCompactNoResurrection(t *testing.T) {
	s, a, stub, store, dir, conn := startCompactTestServer(t)
	a.FlushSession() // the session exists on disk, like a resumed one
	sid := a.SessionID

	if err := conn.WriteJSON(WSMessage{Type: "compact", SessionID: sid}); err != nil {
		t.Fatalf("send compact: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "compacting" })
	stub.waitSummarizerBlocked(t)

	if err := conn.WriteJSON(WSMessage{Type: "session_delete", SessionID: sid}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "session_removed" && m.SessionID == sid
	})
	waitFor(t, 10*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})

	// The delete's drain cancelled the compact. Release the summarizer
	// anyway (pre-fix the cancel never reached it and the compact was still
	// blocked) and assert the file stays gone: the compact's success path
	// must not flush a deleted session back into existence. A bounded
	// negative window makes this deterministic — a resurrection flush lands
	// within milliseconds of the release, while a correctly cancelled
	// compact exited long before (its drain preceded the file delete).
	stub.releaseSummarizer()
	requireNever(t, 2*time.Second, "deleted session file was resurrected by the in-flight compact", func() bool {
		_, err := store.LoadInWorkingDir(dir, sid)
		return err == nil
	})
}

// TestWSCompactCloseDuringCompactCancelsAndDrains verifies the close path
// (✕ on the pane): closeRuntime must cancel the compact via its registered
// stream handles and drain it (wait for the goroutine to release turnMu)
// before evicting — pre-fix the compact was invisible to cancelInFlight and
// the close proceeded mid-compaction.
func TestWSCompactCloseDuringCompactCancelsAndDrains(t *testing.T) {
	s, a, stub, _, _, conn := startCompactTestServer(t)
	sid := a.SessionID
	rt, ok := s.registry.get(sid)
	if !ok {
		t.Fatal("setup: compact session runtime not registered")
	}

	if err := conn.WriteJSON(WSMessage{Type: "compact", SessionID: sid}); err != nil {
		t.Fatalf("send compact: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "compacting" })
	stub.waitSummarizerBlocked(t)

	if err := conn.WriteJSON(WSMessage{Type: "session_close", SessionID: sid}); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The close cancelled the compact: the summarizer returns on context
	// cancellation and the compact's error reply reaches the (still open)
	// socket even though the session was detached.
	readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "response" && strings.HasPrefix(m.Content, "Error:")
	})
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})
	// The drain must have waited for the compact goroutine: turnMu is free
	// once closeRuntime returned.
	waitFor(t, 2*time.Second, func() bool {
		if rt.turnMu.TryLock() {
			rt.turnMu.Unlock()
			return true
		}
		return false
	})
}
