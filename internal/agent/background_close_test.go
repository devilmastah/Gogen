package agent

import (
	"context"
	"testing"
)

// TestBackgroundCommandKeepsCommandApprover pins B10: the job runs on a
// detached context (it outlives the turn), but the turn's VALUES — including
// the command approver — must survive. Dropping them made every blocked
// pattern fail closed, so a command the foreground path would have offered for
// approval was refused outright when run with background=true.
func TestBackgroundCommandKeepsCommandApprover(t *testing.T) {
	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)
	asked := make(chan string, 1)
	ctx := ContextWithCommandApprover(context.Background(), func(_ context.Context, req CommandRequest) (bool, error) {
		select {
		case asked <- req.Command:
		default:
		}
		return true, nil
	})

	// A guard-blocked pattern (the default blocklist matches the privilege
	// escalation word) with an approving context: it must be launched, not
	// refused.
	cmd := "su" + "do true"
	id, err := a.StartBackgroundCommand(ctx, cmd)
	if err != nil {
		t.Fatalf("background command with an approver was refused: %v", err)
	}
	defer a.Close()
	if id == "" {
		t.Fatal("background command returned no job id")
	}
	select {
	case got := <-asked:
		if got != cmd {
			t.Fatalf("approver saw %q, want %q", got, cmd)
		}
	default:
		t.Fatal("the command approver was never consulted")
	}
}

// TestBackgroundCommandRefusedAfterClose pins B11: a job started after Close
// would never be killed (Close already swept its snapshot) and its reaper would
// keep the closed agent alive, so the start is refused.
func TestBackgroundCommandRefusedAfterClose(t *testing.T) {
	a := NewAgent(nil, NewExecutor(t.TempDir()), nil)
	a.Close()

	if _, err := a.StartBackgroundCommand(context.Background(), "sleep 5"); err == nil {
		t.Fatal("a background command after Close must be refused")
	}
}
