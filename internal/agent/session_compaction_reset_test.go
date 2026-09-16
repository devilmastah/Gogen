package agent

import (
	"errors"
	"testing"

	"gogen/internal/llm"
)

// TestResetSessionStateClearsCompactionState pins the conversation-scoped
// reset: /new (ResetSessionState) must clear the previous conversation's
// compaction backoff and BOTH progress guards. Leaving them set would
// suppress emergency compaction / last-resort condensation on the new
// conversation until it grew past the stale message count, and the backoff
// would suppress the normal tier for up to 10 minutes.
func TestResetSessionStateClearsCompactionState(t *testing.T) {
	a, _, _ := newEmergencyTestAgent(t, true)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.noteCompactFailure(errors.New("summarization unavailable"))
	a.noteProgressFailure(&a.compactionGuards.emergency)
	a.noteProgressFailure(&a.compactionGuards.lastResort)

	if backoff, e := compactState(a); backoff.IsZero() || e == 0 {
		t.Fatalf("setup: backoff=%v emergency=%d, want both set", backoff, e)
	}
	if _, lr := guardsState(a); lr == 0 {
		t.Fatalf("setup: lastResort guard not set")
	}

	a.ResetSessionState()

	if backoff, e := compactState(a); !backoff.IsZero() || e != 0 {
		t.Fatalf("after /new: backoff=%v emergency=%d, want zero", backoff, e)
	}
	if e, lr := guardsState(a); e != 0 || lr != 0 {
		t.Fatalf("after /new: guards=(%d,%d), want (0,0)", e, lr)
	}
}

// TestRestoreSessionClearsCompactionState pins the same contract on the
// restore path: the restored conversation replaces the in-memory one, so
// the previous conversation's backoff/guards must not leak into it.
func TestRestoreSessionClearsCompactionState(t *testing.T) {
	a, _, _ := newEmergencyTestAgent(t, true)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.noteCompactFailure(errors.New("summarization unavailable"))
	a.noteProgressFailure(&a.compactionGuards.emergency)
	a.noteProgressFailure(&a.compactionGuards.lastResort)

	a.RestoreSession(SessionSnapshot{
		Messages: []llm.Message{{Role: "user", Content: "restored"}},
	}, "restored-id")

	if backoff, e := compactState(a); !backoff.IsZero() || e != 0 {
		t.Fatalf("after restore: backoff=%v emergency=%d, want zero", backoff, e)
	}
	if e, lr := guardsState(a); e != 0 || lr != 0 {
		t.Fatalf("after restore: guards=(%d,%d), want (0,0)", e, lr)
	}
}
