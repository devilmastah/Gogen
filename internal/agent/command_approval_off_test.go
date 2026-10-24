package agent

import (
	"context"
	"errors"
	"testing"
)

// TestCommandApprovalOffHardBlocks pins the command_approval switch: with
// approval required (the default) a blocklist match escalates to the session's
// approver; with approval off the same match keeps the hard block and the
// approver is never consulted. Off is the "no interactive prompt" setting for
// headless or scripted setups — it never ALLOWS the command (that stays
// command_safety: off).
func TestCommandApprovalOffHardBlocks(t *testing.T) {
	cases := []struct {
		name        string
		approval    bool
		approve     bool
		wantErr     error // sentinel checked with errors.Is/As; nil = allowed
		wantAsked   bool  // whether the approver must be consulted
		blocklisted string
	}{
		{
			name:        "required + user allows",
			approval:    true,
			approve:     true,
			wantAsked:   true,
			blocklisted: "sudo rm -rf /",
		},
		{
			name:        "required + user denies",
			approval:    true,
			approve:     false,
			wantErr:     ErrCommandDenied,
			wantAsked:   true,
			blocklisted: "sudo ls",
		},
		{
			name:        "off + hard block, no prompt",
			approval:    false,
			approve:     true,
			wantErr:     &CommandBlockedError{},
			blocklisted: "sudo ls",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := NewExecutorWithGuard(t.TempDir(), NewCommandGuard("blocklist", nil))
			exec.SetCommandApproval(tc.approval)

			asked := false
			ctx := ContextWithCommandApprover(context.Background(), CommandApprover(
				func(context.Context, CommandRequest) (bool, error) {
					asked = true
					return tc.approve, nil
				}))

			err := exec.enforceCommandGuard(ctx, tc.blocklisted)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("guard error = %v, want allowed", err)
				}
			} else {
				var blocked *CommandBlockedError
				if !errors.As(err, &blocked) && !errors.Is(err, tc.wantErr) {
					t.Fatalf("guard error = %v, want %v", err, tc.wantErr)
				}
			}
			if asked != tc.wantAsked {
				t.Fatalf("approver consulted = %v, want %v", asked, tc.wantAsked)
			}
		})
	}
}

// TestCommandApprovalDefaultsToRequired pins the default so an existing
// config (no command_approval key) keeps the new escalation behavior.
func TestCommandApprovalDefaultsToRequired(t *testing.T) {
	if !NewExecutor(t.TempDir()).CommandApprovalRequired() {
		t.Fatal("a fresh executor must require approval by default")
	}
}

// TestCommandApprovalOffUnaffectedByAllowlistMode pins the scope: the switch
// only applies to blocklist escalation — an allowlist violation is a hard
// block either way.
func TestCommandApprovalOffUnaffectedByAllowlistMode(t *testing.T) {
	exec := NewExecutorWithGuard(t.TempDir(), NewCommandGuard("allowlist", []string{"ls"}))
	exec.SetCommandApproval(false)
	var blocked *CommandBlockedError
	err := exec.enforceCommandGuard(context.Background(), "rm -rf /tmp/x")
	if err == nil || errors.As(err, &blocked) {
		t.Fatalf("allowlist violation = %v, want a non-blocklist rejection", err)
	}
}
