package agent

import (
	"context"
	"errors"
	"testing"
)

// TestBlockedCommandRequiresApproval pins the policy change: blocklist
// matches are no longer hard failures — they escalate to an interactive
// command approver, and fail closed only when nobody can be asked.
func TestBlockedCommandRequiresApproval(t *testing.T) {
	t.Run("approved command proceeds", func(t *testing.T) {
		exec := NewExecutor(t.TempDir())
		var got CommandRequest
		ctx := ContextWithCommandApprover(context.Background(), func(_ context.Context, req CommandRequest) (bool, error) {
			got = req
			return true, nil
		})
		if err := exec.enforceCommandGuard(ctx, "python -c 'print(1)'"); err != nil {
			t.Fatalf("approved command should proceed: %v", err)
		}
		if got.Command != "python -c 'print(1)'" || got.Reason == "" {
			t.Fatalf("approver got %+v, want the command and matched pattern", got)
		}
	})

	t.Run("denied command blocks", func(t *testing.T) {
		exec := NewExecutor(t.TempDir())
		ctx := ContextWithCommandApprover(context.Background(), func(context.Context, CommandRequest) (bool, error) {
			return false, nil
		})
		err := exec.enforceCommandGuard(ctx, "sudo id")
		if !errors.Is(err, ErrCommandDenied) {
			t.Fatalf("err = %v, want ErrCommandDenied", err)
		}
	})

	t.Run("no approver fails closed", func(t *testing.T) {
		exec := NewExecutor(t.TempDir())
		err := exec.enforceCommandGuard(context.Background(), "sudo id")
		var blocked *CommandBlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("err = %v, want *CommandBlockedError", err)
		}
	})

	t.Run("safe command never prompts", func(t *testing.T) {
		exec := NewExecutor(t.TempDir())
		called := false
		ctx := ContextWithCommandApprover(context.Background(), func(context.Context, CommandRequest) (bool, error) {
			called = true
			return false, nil
		})
		if err := exec.enforceCommandGuard(ctx, "go test ./..."); err != nil {
			t.Fatalf("safe command should proceed: %v", err)
		}
		if called {
			t.Fatal("approver must not be consulted for a safe command")
		}
	})

	t.Run("allowlist violation stays a hard block", func(t *testing.T) {
		exec := NewExecutor(t.TempDir())
		exec.SetCommandGuard("allowlist", []string{"go"})
		called := false
		ctx := ContextWithCommandApprover(context.Background(), func(context.Context, CommandRequest) (bool, error) {
			called = true
			return true, nil
		})
		if err := exec.enforceCommandGuard(ctx, "npm test"); err == nil {
			t.Fatal("allowlist violation must not be approvable")
		}
		if called {
			t.Fatal("allowlist violations must not reach the command approver")
		}
	})
}
