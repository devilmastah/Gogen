package agent

import (
	"strings"
	"testing"

	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// fullSaveCountingStore is a SessionPersister that also reports a snapshot
// payload size, so the delta-folding policy can be driven from a test.
type fullSaveCountingStore struct {
	snapBytes   int64
	fullSaves   int
	deltaSaves  int
	savedMsgs   int
	lastPayload SessionSnapshot
}

func (s *fullSaveCountingStore) SaveSized(id string, snap SessionSnapshot) (int64, error) {
	s.fullSaves++
	s.lastPayload = snap
	return s.snapBytes, nil
}

func (s *fullSaveCountingStore) Save(id string, snap SessionSnapshot) error {
	_, err := s.SaveSized(id, snap)
	return err
}

func (s *fullSaveCountingStore) AppendMessages(id string, snap SessionSnapshot, totalMsgCount int) error {
	s.deltaSaves++
	s.savedMsgs = totalMsgCount
	return nil
}

func (s *fullSaveCountingStore) LoadInWorkingDir(string, string) (SessionSnapshot, error) {
	return SessionSnapshot{}, nil
}
func (s *fullSaveCountingStore) List(string) ([]SessionInfo, error) { return nil, nil }
func (s *fullSaveCountingStore) LatestID(string) (string, error)    { return "", nil }
func (s *fullSaveCountingStore) Delete(string, string) error        { return nil }
func (s *fullSaveCountingStore) TouchSession(string, string) error  { return nil }

// newPersistPolicyAgent builds an agent wired to store with a stable identity.
func newPersistPolicyAgent(t *testing.T, store SessionPersister) *Agent {
	t.Helper()
	a := NewAgent(llmtest.NewMockProvider(), NewExecutor(t.TempDir()), nil)
	a.SetSessionID("sess-persist-policy")
	a.SessionStore = store
	return a
}

func (a *Agent) appendTestMessage(content string) {
	a.appendMessage(llm.Message{Role: "user", Content: content})
	a.sessionDirty.Store(true)
}

// TestDeltaFoldingReplacesTheMessageCountRule pins the persistence policy that
// replaced "full snapshot every 5 messages": a delta REWRITES every message
// since the last full save, so folding it in on a message count made the total
// write volume quadratic in session length. A long series of small increments
// against a large snapshot must stay on the incremental path.
func TestDeltaFoldingReplacesTheMessageCountRule(t *testing.T) {
	store := &fullSaveCountingStore{snapBytes: 1 << 20} // a 1 MiB snapshot
	a := newPersistPolicyAgent(t, store)

	a.appendTestMessage("first prompt")
	a.FlushSession()
	if store.fullSaves != 1 {
		t.Fatalf("first save must be a full snapshot, got %d full / %d delta", store.fullSaves, store.deltaSaves)
	}

	// 20 more small messages: the old rule full-saved at the 6th.
	for i := 0; i < 20; i++ {
		a.appendTestMessage("small increment")
		a.FlushSession()
	}
	if store.fullSaves != 1 {
		t.Fatalf("small increments rewrote the whole snapshot: %d full saves (want 1)", store.fullSaves)
	}
	if store.deltaSaves != 20 {
		t.Fatalf("delta saves = %d, want 20", store.deltaSaves)
	}
	if store.savedMsgs != 21 {
		t.Fatalf("delta covered %d messages, want 21", store.savedMsgs)
	}
}

// TestDeltaFoldsWhenItGrowsPastTheThreshold: once the pending delta is a
// meaningful fraction of the snapshot, it is folded into a full snapshot.
func TestDeltaFoldsWhenItGrowsPastTheThreshold(t *testing.T) {
	store := &fullSaveCountingStore{snapBytes: 4 << 20} // 4 MiB snapshot → 1 MiB threshold
	a := newPersistPolicyAgent(t, store)

	a.appendTestMessage("first prompt")
	a.FlushSession()
	if store.fullSaves != 1 {
		t.Fatalf("first save = %d full, want 1", store.fullSaves)
	}

	// Push the pending delta past the 1 MiB threshold in a few big messages.
	big := strings.Repeat("x", 400<<10)
	for i := 0; i < 3; i++ {
		a.appendTestMessage(big)
		a.FlushSession()
	}
	if store.fullSaves < 2 {
		t.Fatalf("pending delta never folded into a full snapshot (full=%d delta=%d)", store.fullSaves, store.deltaSaves)
	}
}

// TestPendingDeltaBytes covers the estimate that drives the decision.
func TestPendingDeltaBytes(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: strings.Repeat("a", 100)},
		{Role: "assistant", Content: "hi", Reasoning: strings.Repeat("r", 10),
			ToolCalls: []llm.ToolCall{{ID: "id", Name: "read_file", ArgsStr: strings.Repeat("j", 20)}}},
	}
	if got := pendingDeltaBytes(msgs, 0); got < 100+10+20+2*160 {
		t.Fatalf("pendingDeltaBytes = %d, too small to cover the message content", got)
	}
	if got := pendingDeltaBytes(msgs, 1); got >= pendingDeltaBytes(msgs, 0) {
		t.Fatalf("suffix estimate %d must be smaller than the whole list", got)
	}
	if got := pendingDeltaBytes(msgs, len(msgs)); got != 0 {
		t.Fatalf("empty suffix = %d, want 0", got)
	}
}
