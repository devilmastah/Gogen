package server

import (
	"context"
	"testing"
	"time"

	"gogen/internal/agent"
)

// TestApprovalHoldTimerSparesReattachedClient pins the hold timer's scope: it
// exists to answer "nobody is looking", so it must do nothing when a client is
// attached again when it fires. The old blanket deny answered a prompt the
// re-attached client was looking at, and also killed approvals raised AFTER the
// detach that armed the timer.
func TestApprovalHoldTimerSparesReattachedClient(t *testing.T) {
	rt := newTestRuntime(t)
	rt.approvalHold = 50 * time.Millisecond

	ws := newFakeWSConn()
	rt.clientsMu.Lock()
	rt.clients[ws] = struct{}{}
	rt.clientsMu.Unlock()

	done := make(chan bool, 1)
	go func() {
		approved, err := rt.deleteApprover()(context.Background(), agent.DeleteRequest{Paths: []string{"x"}})
		if err != nil {
			t.Errorf("approver error: %v", err)
		}
		done <- approved
	}()
	id := parkedApprovalID(t, rt)

	// Last client detaches: arms the auto-deny hold. Then somebody re-attaches
	// before it expires.
	rt.detach(ws)
	addTestClient(rt)

	select {
	case approved := <-done:
		t.Fatalf("the hold timer denied a pending approval despite a re-attached client (approved=%v)", approved)
	case <-time.After(250 * time.Millisecond):
	}

	// The re-attached client's answer still decides it.
	rt.completeApproval(id, true)
	select {
	case approved := <-done:
		if !approved {
			t.Fatal("the re-attached client's approval was not honoured")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approver did not return after the client answered")
	}
}

// TestApprovalHoldTimerDeniesWithNobodyAttached is the other half: with nobody
// attached when the hold expires, the pending approval is denied so the turn
// does not hang.
func TestApprovalHoldTimerDeniesWithNobodyAttached(t *testing.T) {
	rt := newTestRuntime(t)
	rt.approvalHold = 50 * time.Millisecond

	ws := newFakeWSConn()
	rt.clientsMu.Lock()
	rt.clients[ws] = struct{}{}
	rt.clientsMu.Unlock()

	done := make(chan bool, 1)
	go func() {
		approved, err := rt.deleteApprover()(context.Background(), agent.DeleteRequest{Paths: []string{"x"}})
		if err != nil {
			t.Errorf("approver error: %v", err)
		}
		done <- approved
	}()
	parkedApprovalID(t, rt)
	rt.detach(ws)

	select {
	case approved := <-done:
		if approved {
			t.Fatal("an unattended approval must be denied, not approved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hold timer never denied the parked approval")
	}
}
