package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// newHoldServer builds a continuation-style server whose session runtimes
// carry the given approval-hold window (web_approval_hold_secs / F2).
func newHoldServer(t *testing.T, stub *blockingStub, dir string, holdSecs int) (*Server, *agent.Agent) {
	t.Helper()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SessionStore = store
	s := newTestServer(t, a, &config.Config{WebApprovalHoldSecs: holdSecs})
	return s, a
}

// startWSServer starts a server and registers the cleanup.
func startHoldWSServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	t.Cleanup(ts.Close)
	return ts
}

// startDeleteApprovalTurn begins a turn whose first round returns a
// delete tool call, waits for the broadcast delete_approval, and
// returns the approval message.
func startDeleteApprovalTurn(t *testing.T, stub *blockingStub, conn *websocket.Conn, path string) WSMessage {
	t.Helper()
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_del",
		Name: "delete",
		Args: map[string]any{"path": path},
	}}
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "delete it"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	stub.releaseN(1)
	return readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "delete_approval" })
}

// toolResultFor reports whether the agent has a tool result for id.
func toolResultFor(a *agent.Agent, id string) bool {
	for _, m := range a.SnapshotMessages() {
		if m.Role == "tool" && m.ToolCallID == id {
			return true
		}
	}
	return false
}

// TestApprovalHoldAllowsReattachToAnswer verifies F2: with a hold configured,
// the last client detaching does NOT auto-deny a pending approval; a
// reconnecting client is re-notified of the pending request and can answer
// it, after which the turn completes (the file is deleted).
func TestApprovalHoldAllowsReattachToAnswer(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	s, a := newHoldServer(t, stub, dir, 5)
	srv := startHoldWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	approval := startDeleteApprovalTurn(t, stub, conn, "victim.txt")

	// Detach without answering. The approval must NOT be denied during the
	// hold window: the turn keeps waiting for an answer.
	conn.Close()
	requireNever(t, 1*time.Second, "approval was auto-denied during the hold window", func() bool {
		return toolResultFor(a, "call_del")
	})

	// A reconnected client re-attaches and is re-notified of the pending
	// approval (it missed the original broadcast).
	conn2 := dialWS(t, srv, "/ws")
	// The pending-approval re-notification is written during attach, which
	// precedes the handshake's session_state — so collect until we have both,
	// in either order.
	var re WSMessage
	seenState := false
	deadline := time.Now().Add(5 * time.Second)
	for !seenState || re.ApprovalID == "" {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for session_state + delete_approval re-notification on re-attach")
		}
		_ = conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
		var m WSMessage
		if err := conn2.ReadJSON(&m); err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.Type == "session_state" {
			seenState = true
		}
		if m.Type == "delete_approval" {
			re = m
		}
	}
	if re.ApprovalID != approval.ApprovalID {
		t.Fatalf("re-notified approval id = %q, want %q", re.ApprovalID, approval.ApprovalID)
	}
	if err := conn2.WriteJSON(WSMessage{
		Type:       "delete_approval_response",
		ApprovalID: re.ApprovalID,
		Approved:   true,
		SessionID:  a.SessionID,
	}); err != nil {
		t.Fatalf("send approval response: %v", err)
	}

	// The turn completes and the delete is applied.
	waitFor(t, 10*time.Second, func() bool {
		_, err := os.Stat(victim)
		return os.IsNotExist(err)
	})
}

// TestApprovalHoldExpiryAutoDenies verifies F2's timeout: when no client
// re-attaches (or answers) within the hold window, the pending approval is
// auto-denied and the turn continues with the "not approved" tool result.
func TestApprovalHoldExpiryAutoDenies(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	s, a := newHoldServer(t, stub, dir, 1) // 1s hold
	srv := startHoldWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	startDeleteApprovalTurn(t, stub, conn, "victim.txt")
	conn.Close()

	// After the hold expires the approval is auto-denied: the turn completes
	// with a tool result and the file survives (delete was denied).
	waitFor(t, 10*time.Second, func() bool { return toolResultFor(a, "call_del") })
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("file was deleted despite the approval never being granted")
	}
}

// TestApprovalImmediateDenyWithoutHold pins the D10 default: with no hold
// configured, detaching the last client denies pending approvals right away.
func TestApprovalImmediateDenyWithoutHold(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	s, a := newHoldServer(t, stub, dir, 0) // no hold — default behavior
	srv := startHoldWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	startDeleteApprovalTurn(t, stub, conn, "victim.txt")
	conn.Close()

	waitFor(t, 10*time.Second, func() bool { return toolResultFor(a, "call_del") })
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("file was deleted despite the approval never being granted")
	}
}
