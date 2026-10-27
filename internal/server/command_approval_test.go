package server

import (
	"context"
	"testing"
	"time"

	"gogen/internal/agent"
)

// TestCommandApproverFailClosedWithoutClients pins the safety property: with
// no attached client the blocked-command approval is denied immediately and
// never parked, so an unattended turn cannot hang on a prompt nobody sees.
func TestCommandApproverFailClosedWithoutClients(t *testing.T) {
	rt := newTestRuntime(t)
	ok, err := rt.commandApprover()(context.Background(), agent.CommandRequest{Command: "sudo id", Reason: "pat"})
	if err != nil || ok {
		t.Fatalf("approver = (%v,%v), want fail-closed false,nil", ok, err)
	}
	if n := parkedApprovalCount(rt); n != 0 {
		t.Fatalf("parked %d approvals, want 0 (must never park unattended)", n)
	}
}

// TestCommandApproverRoundTrip pins the park → broadcast → complete path on
// the shared approvals map: a client-attached request is broadcast as a
// command_approval frame and resolved by completeApproval.
func TestCommandApproverRoundTrip(t *testing.T) {
	rt := newTestRuntime(t)
	ws := newFakeWSConn()
	rt.attach(ws)
	defer rt.detach(ws)

	done := make(chan struct{})
	var approved bool
	var aerr error
	go func() {
		approved, aerr = rt.commandApprover()(context.Background(), agent.CommandRequest{
			Command: "python -c 'print(1)'",
			Reason:  "pat",
		})
		close(done)
	}()

	id := parkedApprovalID(t, rt)

	got := false
	waitFor(t, time.Second, func() bool {
		for {
			select {
			case out := <-ws.sendQ:
				if out == nil || out.msg == nil {
					continue
				}
				m := *out.msg
				if m.Type == "command_approval" && m.ApprovalID == id {
					if m.Command != "python -c 'print(1)'" || m.Reason != "pat" {
						t.Errorf("command_approval payload = %+v", m)
					}
					got = true
					return true
				}
			default:
				return false
			}
		}
	})
	if !got {
		t.Fatal("no command_approval broadcast")
	}

	rt.completeApproval(id, true)
	select {
	case <-done:
		if aerr != nil || !approved {
			t.Fatalf("approver = (%v,%v), want true,nil", approved, aerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approver did not resolve after completeApproval")
	}
}
