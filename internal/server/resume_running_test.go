package server

// The user-visible symptom: "can't resume my other responding session".
// session_resume of a session with a RUNNING turn had its whole reply
// (response/clear_chat/history/config) blocked on turnMu.RLock() — a turn
// holds turnMu for its entire duration, so the resume appeared to do nothing
// until the turn ended. The reply path is internally synchronized now; it
// must arrive while the turn is still blocked. Resuming the connection's
// CURRENT session is a separate no-op contract, pinned by
// TestResumeCurrentSessionIsNoopAck: a plain acknowledgement with no
// clear_chat/history wipe-rebuild.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

func TestResumeRunningSessionReplyNotBlockedByTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Start a turn in the default session and keep it blocked: turnMu stays
	// held for the whole turn, exactly like a real responding session.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hold", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// Open pane B: the connection's current pane is now B, so resuming A is
	// the reported "resume my OTHER responding session" flow (resuming the
	// current session is the no-op ack pinned by
	// TestResumeCurrentSessionIsNoopAck, and a session this connection still
	// shows as a pane resolves to the focus-pane reply instead).
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("session_new: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "response" && m.SessionID != "" && m.SessionID != a.SessionID
	})
	// Drain B's attach payload (state/clear_chat/history/config).
	for i := 0; i < 4; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}
	// Close A's pane on this connection (the client's ✕/re-key detach): the
	// blocked turn keeps its runtime registered, so the resume below is a
	// genuine switch.
	if err := conn.WriteJSON(WSMessage{Type: "session_detach", SessionID: a.SessionID}); err != nil {
		t.Fatalf("session_detach: %v", err)
	}

	// Resume A (still responding). Its attach reply — session_state,
	// history, config — must arrive while A's turn is STILL blocked
	// (before releaseN): the turn lock is never taken on the reply path.
	t0 := time.Now()
	if err := conn.WriteJSON(WSMessage{Type: "session_resume", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send resume: %v", err)
	}
	state := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == a.SessionID
	})
	if !state.TurnActive {
		t.Fatal("resume session_state must report turnActive=true (the turn is running)")
	}
	// The resume reply is the attach sequence: the history follows the
	// state, then the configs.
	hist := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.SessionID == a.SessionID &&
			(m.Type == "history" || m.Type == "config" || m.Type == "response" || m.Type == "clear_chat")
	})
	if hist.Type != "history" {
		t.Fatalf("resume sequence after session_state = %q, want history (the reply is the attach sequence)", hist.Type)
	}
	if len(hist.History) == 0 {
		t.Fatal("resumed history empty (user message missing)")
	}
	if cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.SessionID == a.SessionID && (m.Type == "config" || m.Type == "response" || m.Type == "clear_chat")
	}); cfg.Type != "config" {
		t.Fatalf("resume sequence after history = %q, want config (the reply is the attach sequence)", cfg.Type)
	}
	elapsed := time.Since(t0)
	// The turn is still blocked — the reply must not have waited for turnMu.
	// Generous bound for scheduling noise; the old behavior was "never until
	// the turn ends".
	if elapsed > 2*time.Second {
		t.Fatalf("resume reply took %v while the turn was running; must not block on turnMu", elapsed)
	}

	// Cleanup: release the turn and let its persist finish before TempDir teardown.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", a.SessionID+".json"))
		return err == nil
	})
}

// TestResumeRunningSessionSendsSessionState pins the convergence latch: a
// session_resume of a session with a RUNNING turn must send session_state
// (turnActive=true) before the reply, so the client latches the turn-end
// refetch that converges the transcript. The reply's history snapshot cannot
// contain the in-flight reply, and the client only re-attaches on turn_end
// when session_state said the turn was active. Pre-fix, resume sent no
// session_state and the resumed pane stayed missing the in-flight exchange
// until a manual refocus/reconnect.
func TestResumeRunningSessionSendsSessionState(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	// Drain the connect handshake's session_state (default session, idle).
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Start a turn in the default session and keep it blocked so the turn is
	// genuinely running (turnMu held) when the resume arrives.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "hold", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)

	// Open B, then close A's pane on this connection: the resume below is a
	// genuine switch (a session this connection still shows resolves to the
	// focus-pane reply instead).
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("session_new: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "response" && m.SessionID != "" && m.SessionID != a.SessionID
	})
	for i := 0; i < 4; i++ {
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return true })
	}
	if err := conn.WriteJSON(WSMessage{Type: "session_detach", SessionID: a.SessionID}); err != nil {
		t.Fatalf("session_detach: %v", err)
	}

	// Resume A (still responding). The attach reply must carry session_state
	// with turnActive=true BEFORE the history — the convergence latch that
	// makes the client re-attach at turn_end — and no response/clear_chat
	// frames (the resume reply is the attach sequence).
	if err := conn.WriteJSON(WSMessage{Type: "session_resume", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send resume: %v", err)
	}
	sawState := false
	for {
		m := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
			return m.SessionID == a.SessionID &&
				(m.Type == "session_state" || m.Type == "history" || m.Type == "config" ||
					m.Type == "response" || m.Type == "clear_chat")
		})
		if m.Type == "history" {
			break
		}
		if m.Type == "response" || m.Type == "clear_chat" {
			t.Fatalf("resume sent a %q frame; the reply is the attach sequence", m.Type)
		}
		if m.Type == "session_state" && !sawState {
			sawState = true
			if !m.TurnActive {
				t.Fatal("resume session_state must report turnActive=true (the turn is running)")
			}
		}
	}
	if !sawState {
		t.Fatal("resume must send session_state before the history (the convergence latch)")
	}

	// Cleanup: release the turn and let its persist finish before TempDir teardown.
	stub.releaseN(1)
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, ".gogen", "sessions", a.SessionID+".json"))
		return err == nil
	})
}

// TestResumeCurrentSessionIsNoopAck pins the wire contract for the no-op
// resume: targeting the session the connection is already on (a typed
// "/resume <current-id>", or any explicit current-id resume) replies with a
// plain acknowledgement — no sessionAction (so no clear_chat) and no
// transcript frame. The client keeps the on-screen transcript instead of
// flashing a wipe/rebuild; the async context/config echo still refreshes the
// toolbar and badge. ("resume latest" never lands here: it resolves to the
// newest session OTHER than the current one, pinned in session_ops_test.go.)
func TestResumeCurrentSessionIsNoopAck(t *testing.T) {
	s, a, _ := newLifecycleServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// The connection's pane is the default session (the handshake attach),
	// so this resume targets the session the connection is already on.
	if err := conn.WriteJSON(WSMessage{Type: "session_resume", SessionID: a.SessionID}); err != nil {
		t.Fatalf("send resume: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "response" && m.SessionID == a.SessionID
	})
	if want := "Already on session " + a.SessionID + "."; resp.Content != want {
		t.Fatalf("no-op resume reply = %q, want %q", resp.Content, want)
	}
	if resp.SessionAction != "" {
		t.Fatalf("no-op resume sessionAction = %q, want none (no clear_chat)", resp.SessionAction)
	}
	// The async context echo (connection-scoped — no sessionId) closes the
	// no-op sequence: no clear_chat/history frame may precede it (a bogus
	// clear/history would be enqueued synchronously, before the goroutine's
	// context frame; the handshake sends neither for an empty session).
	for {
		m := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
			return m.Type == "context" ||
				((m.Type == "clear_chat" || m.Type == "history") && m.SessionID == a.SessionID)
		})
		if m.Type == "context" {
			break
		}
		t.Fatalf("no-op resume sent a %q frame; the transcript must not be touched", m.Type)
	}
}

// TestResumeHistoryPrecedesClientReactions pins the ordering guarantee the
// pane's transcript swap depends on: the resume's history frame is enqueued in
// the same read-loop turn as session_state, so the traffic the client fires
// the moment it sees session_state — the left-behind session's detach (whose
// orphan eviction broadcasts session_detached) and the sidebar list refresh —
// can never be written ahead of it. A goroutine-built history could be
// overtaken by those frames, leaving the pane on its empty state for the whole
// snapshot build ("the UI blocks longer on refresh").
func TestResumeHistoryPrecedesClientReactions(t *testing.T) {
	s, a, store := newLifecycleServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// A saved session with history, not attached to this connection.
	store.Save("refresh-target", agent.SessionSnapshot{
		WorkingDir: s.ws.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
	})

	// The resume AND the exact reactions the client sends on session_state:
	// release the left-behind session, refresh the sidebar list.
	for _, m := range []WSMessage{
		{Type: "session_resume", SessionID: "refresh-target"},
		{Type: "session_detach", SessionID: a.SessionID},
		{Type: "list_sessions"},
	} {
		if err := conn.WriteJSON(m); err != nil {
			t.Fatalf("send %s: %v", m.Type, err)
		}
	}

	// The history must arrive before any sidebar payload written by the
	// reactions above.
	m := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return (m.Type == "history" && m.SessionID == "refresh-target") || m.Type == "sessions"
	})
	if m.Type != "history" {
		t.Fatalf("first transcript/sidebar frame = %q, want the resume history (the transcript swap must not wait behind sidebar traffic)", m.Type)
	}

	// The detach handler runs after the resume in the connection's read loop,
	// i.e. possibly still in flight when the test body returns: its orphan
	// eviction flushes the left-behind session into the store. Wait for that
	// eviction to complete (it removes the runtime as its last step), so the
	// write cannot race the caller's t.TempDir() removal.
	waitFor(t, 5*time.Second, func() bool {
		_, stillLive := s.registry.get(a.SessionID)
		return !stillLive
	})
}

// TestResumeReplyIsAttachSequence pins the wire-shape unification: a
// successful resume of a session this connection does not already show
// replies with the ATTACH sequence — session_state → history → config — and
// no response/clear_chat frames. The client re-keys and rebuilds the
// transcript through the same path as session_attach, so the two cannot
// drift.
func TestResumeReplyIsAttachSequence(t *testing.T) {
	s, _, store := newLifecycleServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// A saved session this connection has never attached: every frame for it
	// comes from the resume itself, in the payload goroutine's write order.
	store.Save("resume-target", agent.SessionSnapshot{
		WorkingDir: s.ws.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err := conn.WriteJSON(WSMessage{Type: "session_resume", SessionID: "resume-target"}); err != nil {
		t.Fatalf("send resume: %v", err)
	}
	var got []WSMessage
	for i := 0; i < 4; i++ {
		got = append(got, readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
			return m.SessionID == "resume-target"
		}))
	}
	names := make([]string, 0, len(got))
	for _, m := range got {
		names = append(names, m.Type)
	}
	want := []string{"session_state", "history", "config", "config"}
	for i := range want {
		if got[i].Type != want[i] {
			t.Fatalf("resume reply[%d] = %q, want %q (full sequence: %v)", i, got[i].Type, want[i], names)
		}
	}
	if len(got[1].History) == 0 {
		t.Fatal("resume history empty (the saved message is missing)")
	}
	if got[0].TurnActive {
		t.Fatal("an idle resume reported turnActive")
	}
}
