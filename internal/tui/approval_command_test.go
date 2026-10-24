package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
)

// TestCommandApprovalModalRenders pins that the shared approval modal renders
// a blocked command (not a file list) when the request kind is "command".
func TestCommandApprovalModalRenders(t *testing.T) {
	m := &Model{}
	ar := &approvalRequest{
		kind:    "command",
		command: "python -c 'print(1)'",
		req:     agent.DeleteRequest{Reason: `(?i)\b(python3?|perl|ruby|node)\s+-[ce]\b`},
		reply:   make(chan bool, 1),
	}
	m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar})
	if m.modal != ModalApproval {
		t.Fatalf("modal = %v, want ModalApproval", m.modal)
	}
	out := stripANSI(m.renderApprovalModal())
	for _, want := range []string{"Command Approval Required", "python -c 'print(1)'", "Allow command?"} {
		if !strings.Contains(out, want) {
			t.Fatalf("modal missing %q:\n%s", want, out)
		}
	}
	m.dismissApproval(true)
	select {
	case got := <-ar.reply:
		if !got {
			t.Fatal("reply = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("no reply on the request's channel")
	}
}

// TestCommandApproverRoundTrip drives the command approver closure the same
// way the stream goroutine does (send request, block on own reply) and
// answers it from the "Update thread".
func TestCommandApproverRoundTrip(t *testing.T) {
	m := &Model{}
	send := &recordingSender{}
	approver := m.makeCommandApprover("s1", send)

	outcome := make(chan bool, 1)
	errc := make(chan error, 1)
	go func() {
		ok, err := approver(context.Background(), agent.CommandRequest{Command: "sudo id", Reason: "pat"})
		if err != nil {
			errc <- err
			return
		}
		outcome <- ok
	}()

	var ar *approvalRequest
	deadline := time.Now().Add(2 * time.Second)
	for ar == nil && time.Now().Before(deadline) {
		ar = send.take()
		if ar == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if ar == nil {
		t.Fatal("command approver did not send its request")
	}
	if ar.kind != "command" || ar.command != "sudo id" {
		t.Fatalf("request = %+v, want command kind with the command text", ar)
	}

	m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar})
	m.dismissApproval(false)
	select {
	case ok := <-outcome:
		if ok {
			t.Fatal("approver returned true after deny")
		}
	case err := <-errc:
		t.Fatalf("approver error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("approver did not return after dismiss")
	}
}
