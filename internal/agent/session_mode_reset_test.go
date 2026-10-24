package agent

import (
	"context"
	"testing"

	"gogen/internal/llm"
)

// TestStartNewSessionResetsPlanMode pins the fresh-session mode reset: the
// typed /new path (and the replacement session after deleting the current
// one) reuses the SAME agent, so without the reset a session left in plan
// mode hands the read-only tool set to a brand-new conversation — the model
// silently loses write_file/patch_file/execute_command with nothing in the
// empty transcript to explain it. The web's /new builds a fresh agent (act by
// construction); this keeps the in-place paths at parity.
func TestStartNewSessionResetsPlanMode(t *testing.T) {
	a := &Agent{Provider: &statsStubProvider{}, WorkingDir: "/tmp", SessionID: "old-session"}
	a.Mode = ModePlan

	if _, err := a.startNewSession("fresh-session"); err != nil {
		t.Fatalf("startNewSession: %v", err)
	}
	if a.SessionID != "fresh-session" {
		t.Fatalf("SessionID = %q, want fresh-session", a.SessionID)
	}
	if a.Mode != ModeAct {
		t.Fatalf("mode after /new = %v, want act (a fresh session must not inherit plan mode)", a.Mode)
	}
}

// TestForkSessionKeepsPlanMode pins the deliberate exception: a fork is the
// same work continued (an edit-resend of an earlier message), so it keeps the
// source session's mode — only FRESH sessions reset to act.
func TestForkSessionKeepsPlanMode(t *testing.T) {
	a := &Agent{
		Provider:   &statsStubProvider{},
		WorkingDir: "/tmp",
		SessionID:  "src-session",
		Messages: []llm.Message{
			{Role: "user", Content: "plan the refactor"},
			{Role: "assistant", Content: "here is the plan"},
		},
	}
	a.Mode = ModePlan

	if err := a.ForkSession(context.Background(), "last", "forked-session"); err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if a.SessionID != "forked-session" {
		t.Fatalf("SessionID = %q, want forked-session", a.SessionID)
	}
	if a.Mode != ModePlan {
		t.Fatalf("mode after /fork = %v, want plan (a fork continues the same work)", a.Mode)
	}
}
