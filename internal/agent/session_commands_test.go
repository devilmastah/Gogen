package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

type stubSessionStore struct {
	sessions map[string]SessionSnapshot
	order    []string
	saveErr  error
}

func (s *stubSessionStore) AppendMessages(id string, snap SessionSnapshot, _ int) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	// In tests, append to messages in the existing snapshot.
	if s.sessions != nil {
		if existing, ok := s.sessions[id]; ok {
			existing.Messages = append(existing.Messages, snap.Messages...)
			s.sessions[id] = existing
			return nil
		}
	}
	return s.Save(id, snap)
}

func (s *stubSessionStore) Save(id string, snap SessionSnapshot) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.sessions == nil {
		s.sessions = make(map[string]SessionSnapshot)
	}
	if _, ok := s.sessions[id]; !ok {
		s.order = append(s.order, id)
	}
	s.sessions[id] = snap
	return nil
}

func (s *stubSessionStore) LoadInWorkingDir(workingDir, id string) (SessionSnapshot, error) {
	snap, ok := s.sessions[id]
	if !ok {
		return SessionSnapshot{}, errNotFound
	}
	return snap, nil
}

func (s *stubSessionStore) List(workingDir string) ([]SessionInfo, error) {
	var out []SessionInfo
	for _, id := range s.order {
		snap := s.sessions[id]
		out = append(out, SessionInfo{
			ID:           id,
			MessageCount: len(snap.Messages),
			Label:        llm.SessionLabel(snap.Messages),
			ParentID:     snap.ParentID,
		})
	}
	return out, nil
}

func (s *stubSessionStore) LatestID(workingDir string) (string, error) {
	if len(s.order) == 0 {
		return "", errNotFound
	}
	return s.order[len(s.order)-1], nil
}

func (s *stubSessionStore) Delete(workingDir, id string) error {
	if _, ok := s.sessions[id]; !ok {
		return errNotFound
	}
	delete(s.sessions, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

func (s *stubSessionStore) TouchSession(workingDir, id string) error {
	return nil // no-op for tests
}

type errString string

func (e errString) Error() string { return string(e) }

var errNotFound = errString("not found")

func TestHandleSessionCommandNew(t *testing.T) {
	store := &stubSessionStore{}
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   "/tmp",
		SessionStore: store,
		SessionID:    "old-session",
		Messages:     []llm.Message{{Role: "user", Content: "hello"}},
	}
	result, handled, err := a.HandleSessionCommand(context.Background(), "/new", "new-session")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if result.Action != SessionActionClearChat {
		t.Fatalf("action=%q", result.Action)
	}
	if a.SessionID != "new-session" || len(a.Messages) != 0 {
		t.Fatalf("session=%s messages=%d", a.SessionID, len(a.Messages))
	}
	if _, ok := store.sessions["old-session"]; !ok {
		t.Fatal("old session not saved")
	}
}

func TestHandleSessionCommandResumeList(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"s1": {Messages: []llm.Message{{Role: "user", Content: "first task here"}}},
	}}
	store.order = []string{"s1"}
	a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "s1"}

	result, handled, err := a.HandleSessionCommand(context.Background(), "/resume", "")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(result.Output, "Saved sessions:") {
		t.Fatalf("output=%q", result.Output)
	}
	if !strings.Contains(result.Output, "← current") {
		t.Fatalf("expected current marker: %q", result.Output)
	}
	if !strings.Contains(result.Output, "first task here") {
		t.Fatalf("expected label: %q", result.Output)
	}
}

func TestHandleSessionCommandResumeLatest(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"current": {Messages: []llm.Message{{Role: "user", Content: "current"}}},
		"older":   {Messages: []llm.Message{{Role: "user", Content: "older task"}}},
	}}
	store.order = []string{"older", "current"}
	a := &Agent{Provider: &statsStubProvider{}, WorkingDir: "/tmp", SessionStore: store, SessionID: "current", Messages: []llm.Message{{Role: "user", Content: "current"}}}

	result, handled, err := a.HandleSessionCommand(context.Background(), "resume latest", "")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if a.SessionID != "older" {
		t.Fatalf("got session %s", a.SessionID)
	}
	if !strings.Contains(result.Output, "older task") {
		t.Fatalf("output=%q", result.Output)
	}
}

func TestResolveResumeTarget(t *testing.T) {
	t.Run("plain id passes through trimmed", func(t *testing.T) {
		a := &Agent{WorkingDir: "/tmp", SessionStore: &stubSessionStore{}}
		got, err := a.ResolveResumeTarget("  abc ")
		if err != nil || got != "abc" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("latest picks newest other top-level session", func(t *testing.T) {
		store := &stubSessionStore{sessions: map[string]SessionSnapshot{
			"newest": {Messages: []llm.Message{{Role: "user", Content: "newest"}}},
			"older":  {Messages: []llm.Message{{Role: "user", Content: "older"}}},
		}}
		store.order = []string{"newest", "older"}
		a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "current"}
		got, err := a.ResolveResumeTarget("latest")
		if err != nil || got != "newest" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("latest skips current and nested sessions", func(t *testing.T) {
		store := &stubSessionStore{sessions: map[string]SessionSnapshot{
			"current": {Messages: []llm.Message{{Role: "user", Content: "c"}}},
			"child":   {Messages: []llm.Message{{Role: "user", Content: "n"}}, ParentID: "current"},
			"older":   {Messages: []llm.Message{{Role: "user", Content: "o"}}},
		}}
		store.order = []string{"current", "child", "older"}
		a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "current"}
		got, err := a.ResolveResumeTarget("latest")
		if err != nil || got != "older" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("latest with only the current session errors", func(t *testing.T) {
		store := &stubSessionStore{sessions: map[string]SessionSnapshot{
			"current": {Messages: []llm.Message{{Role: "user", Content: "c"}}},
		}}
		store.order = []string{"current"}
		a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "current"}
		if _, err := a.ResolveResumeTarget("latest"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("empty arg errors", func(t *testing.T) {
		a := &Agent{WorkingDir: "/tmp", SessionStore: &stubSessionStore{}}
		if _, err := a.ResolveResumeTarget("  "); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("no store errors", func(t *testing.T) {
		a := &Agent{WorkingDir: "/tmp"}
		if _, err := a.ResolveResumeTarget("abc"); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestHandleSessionCommandResumeByID(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"abc": {Messages: []llm.Message{{Role: "user", Content: "restore me"}}, Mode: "plan"},
	}}
	store.order = []string{"abc"}
	a := &Agent{Provider: &statsStubProvider{}, WorkingDir: "/tmp", SessionStore: store, SessionID: "other"}

	_, handled, err := a.HandleSessionCommand(context.Background(), "resume abc", "")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if a.SessionID != "abc" || a.Mode != ModePlan {
		t.Fatalf("session=%s mode=%s", a.SessionID, a.Mode)
	}
}

func TestResumeSessionShowsContextUsage(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"abc": {Messages: []llm.Message{
			{Role: "user", Content: strings.Repeat("word ", 500)},
			{Role: "assistant", Content: strings.Repeat("reply ", 200)},
		}},
	}}
	store.order = []string{"abc"}
	ctxMgr := contextmgr.NewManager(&statsStubProvider{limit: 8000}, contextmgr.Settings{ContextLimit: 8000})
	a := &Agent{
		Provider:     &statsStubProvider{limit: 8000},
		Context:      ctxMgr,
		WorkingDir:   "/tmp",
		SessionStore: store,
		SessionID:    "other",
	}

	result, handled, err := a.HandleSessionCommand(context.Background(), "resume abc", "")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(result.Output, "context:") {
		t.Fatalf("expected context line in output, got %q", result.Output)
	}
}

func TestHandleSessionCommandDelete(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"keep": {Messages: []llm.Message{{Role: "user", Content: "stay"}}},
		"gone": {Messages: []llm.Message{{Role: "user", Content: "bye"}}},
	}}
	store.order = []string{"keep", "gone"}
	a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "other"}

	result, handled, err := a.HandleSessionCommand(context.Background(), "resume del gone", "new-one")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if result.Action != SessionActionNone {
		t.Fatalf("action=%q", result.Action)
	}
	if _, ok := store.sessions["gone"]; ok {
		t.Fatal("session file should be deleted")
	}
	if !strings.Contains(result.Output, "Deleted session gone") {
		t.Fatalf("output=%q", result.Output)
	}
}

func TestHandleSessionCommandDeleteCurrent(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"current": {Messages: []llm.Message{{Role: "user", Content: "active"}}},
	}}
	store.order = []string{"current"}
	a := &Agent{Provider: &statsStubProvider{}, WorkingDir: "/tmp", SessionStore: store, SessionID: "current", Messages: []llm.Message{{Role: "user", Content: "active"}}}

	result, handled, err := a.HandleSessionCommand(context.Background(), "/resume del current", "fresh-id")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if result.Action != SessionActionClearChat {
		t.Fatalf("action=%q", result.Action)
	}
	if a.SessionID != "fresh-id" || len(a.Messages) != 0 {
		t.Fatalf("session=%s messages=%d", a.SessionID, len(a.Messages))
	}
}

func TestPersistSessionRecordsError(t *testing.T) {
	store := &stubSessionStore{saveErr: errString("disk full")}
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   "/tmp",
		SessionStore: store,
		SessionID:    "s1",
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}
	a.persistSession()
	err := a.ConsumePersistError()
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("expected persist error, got %v", err)
	}
	if a.ConsumePersistError() != nil {
		t.Fatal("expected consume to clear error")
	}

	store.saveErr = nil
	a.persistSession()
	if a.ConsumePersistError() != nil {
		t.Fatal("expected successful save to clear persist error")
	}
}

func TestRestoreSessionKeepsProjectProfileSameDir(t *testing.T) {
	a := &Agent{WorkingDir: "/tmp/project", projectProfile: "stale"}
	a.RestoreSession(SessionSnapshot{
		WorkingDir:     "/tmp/project",
		ProjectProfile: "Working directory: /tmp/project\nTop-level directories: cmd/, internal/\n",
		Messages:       []llm.Message{{Role: "user", Content: "hi"}},
	}, a.SessionID)
	if a.projectProfile != "Working directory: /tmp/project\nTop-level directories: cmd/, internal/\n" {
		t.Fatalf("projectProfile=%q", a.projectProfile)
	}
}

func TestRestoreSessionClearsProjectProfileDifferentDir(t *testing.T) {
	a := &Agent{WorkingDir: "/tmp/other", projectProfile: "stale"}
	a.RestoreSession(SessionSnapshot{
		WorkingDir:     "/tmp/project",
		ProjectProfile: "Working directory: /tmp/project\n",
		Messages:       []llm.Message{{Role: "user", Content: "hi"}},
	}, a.SessionID)
	if a.projectProfile != "" {
		t.Fatalf("expected empty projectProfile, got %q", a.projectProfile)
	}
}

func TestRestoreSessionClearsProjectProfileWhenMissing(t *testing.T) {
	a := &Agent{WorkingDir: "/tmp/project", projectProfile: "stale"}
	a.RestoreSession(SessionSnapshot{
		WorkingDir: "/tmp/project",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
	}, a.SessionID)
	if a.projectProfile != "" {
		t.Fatalf("expected empty projectProfile, got %q", a.projectProfile)
	}
}

func TestRestoreSessionClearsPinsAndUsage(t *testing.T) {
	pins := NewPinManager()
	pins.pinned[0] = struct{}{}
	pins.pinned[3] = struct{}{}
	a := &Agent{
		WorkingDir:    "/tmp/project",
		PinManager:    pins,
		lastTurnUsage: &llm.Usage{PromptTokens: 100, CachedTokens: 80},
		UsageAccum:    UsageAccumulator{TotalPromptTokens: 500, TotalCompletionTokens: 50, TotalCachedTokens: 200, TotalTurns: 3},
		Messages:      []llm.Message{{Role: "user", Content: "old"}},
	}
	a.RestoreSession(SessionSnapshot{
		WorkingDir:     "/tmp/project",
		ProjectProfile: "profile",
		Messages:       []llm.Message{{Role: "user", Content: "restored"}},
	}, a.SessionID)
	if len(a.PinManager.PinnedIndices()) != 0 {
		t.Fatalf("pins not cleared: %v", a.PinManager.PinnedIndices())
	}
	if a.lastTurnUsage != nil {
		t.Fatalf("lastTurnUsage=%v, want nil", a.lastTurnUsage)
	}
	if a.UsageAccum != (UsageAccumulator{}) {
		t.Fatalf("UsageAccum=%+v, want zero", a.UsageAccum)
	}
	if a.projectProfile != "profile" {
		t.Fatalf("projectProfile=%q", a.projectProfile)
	}
}

func TestPersistSessionStoresProjectProfile(t *testing.T) {
	dir := t.TempDir()
	store := &stubSessionStore{}
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   dir,
		SessionStore: store,
		SessionID:    "s1",
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}
	a.persistSession()
	snap, ok := store.sessions["s1"]
	if !ok {
		t.Fatal("session not saved")
	}
	if snap.ProjectProfile == "" {
		t.Fatal("expected projectProfile to be detected and saved")
	}
	if a.projectProfile == "" {
		t.Fatal("expected in-memory projectProfile to be set")
	}
}

// TestForkLastSkipsTruncatedGhostTurn reproduces the real-world fork case
// (session 92a84ada → d792036a): a turn that ended with a reasoning-only
// ghost after a complete tool round. "fork last" must skip the ghost and land
// on the last assistant message that produced output, and must NOT carry the
// tool round into the forked session.
func TestForkLastSkipsTruncatedGhostTurn(t *testing.T) {
	store := &stubSessionStore{}
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   "/tmp",
		SessionStore: store,
		SessionID:    "orig",
		Messages: []llm.Message{
			{Role: "user", Content: "investigate"},
			{Role: "assistant", Content: "Let me check the flow.", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
			{Role: "tool", Content: "file contents", ToolCallID: "c1"},
			{Role: "assistant", Content: "", Reasoning: "OK let me now step back and put together the analysis."},
		},
	}
	if err := a.ForkSession(context.Background(), "last", "forked"); err != nil {
		t.Fatal(err)
	}
	if a.SessionID != "forked" {
		t.Fatalf("session=%s, want forked", a.SessionID)
	}
	if len(a.Messages) != 2 {
		t.Fatalf("forked history has %d messages, want 2 (ghost + tool round excluded)", len(a.Messages))
	}
	last := a.Messages[len(a.Messages)-1]
	if last.Role != "assistant" || last.Content != "Let me check the flow." {
		t.Fatalf("fork point should be the last assistant with output, got %+v", last)
	}
	if len(last.ToolCalls) != 0 {
		t.Fatalf("fork point tool calls should be stripped, got %+v", last.ToolCalls)
	}
	for _, m := range a.Messages {
		if m.Role == "tool" {
			t.Fatalf("tool round must not be forked: %+v", m)
		}
		if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("ghost must not be forked: %+v", m)
		}
	}
}

// TestForkLastToolCallOnlyForkPointDropsEmptyAssistant verifies that forking
// from a tool-call-only assistant message (empty content) strips the tool
// calls and drops the resulting fully-empty message instead of leaving a
// ghost behind in the forked session.
func TestForkLastToolCallOnlyForkPointDropsEmptyAssistant(t *testing.T) {
	a := &Agent{
		Provider:   &statsStubProvider{},
		WorkingDir: "/tmp",
		SessionID:  "orig",
		Messages: []llm.Message{
			{Role: "user", Content: "implement it"},
			{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
		},
	}
	if err := a.ForkSession(context.Background(), "last", "forked"); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 1 {
		t.Fatalf("forked history has %d messages, want 1 (empty fork point dropped)", len(a.Messages))
	}
	if a.Messages[0].Role != "user" {
		t.Fatalf("last message should be the user turn, got %+v", a.Messages[0])
	}
}

// TestForkIndexOnGhostWalksBack verifies explicit index forks never land on an
// invisible assistant message: the fork point walks back to the nearest
// visible message.
func TestForkIndexOnGhostWalksBack(t *testing.T) {
	a := &Agent{
		Provider:   &statsStubProvider{},
		WorkingDir: "/tmp",
		SessionID:  "orig",
		Messages: []llm.Message{
			{Role: "user", Content: "a"},
			{Role: "assistant", Content: "reply 1"},
			{Role: "user", Content: "b"},
			{Role: "assistant", Content: "", Reasoning: "thinking only"},
		},
	}
	if err := a.ForkSession(context.Background(), "3", "forked"); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 3 {
		t.Fatalf("forked history has %d messages, want 3", len(a.Messages))
	}
	if last := a.Messages[len(a.Messages)-1]; last.Role != "user" || last.Content != "b" {
		t.Fatalf("fork point should walk back to visible message, got %+v", last)
	}
}

// TestForkLastNoVisibleAssistant verifies that when every assistant message is
// a ghost, "fork last" reports an error instead of forking from a ghost.
func TestForkLastNoVisibleAssistant(t *testing.T) {
	a := &Agent{
		Provider:   &statsStubProvider{},
		WorkingDir: "/tmp",
		SessionID:  "orig",
		Messages: []llm.Message{
			{Role: "user", Content: "a"},
			{Role: "assistant", Content: "", Reasoning: "thinking only"},
		},
	}
	err := a.ForkSession(context.Background(), "last", "forked")
	if err == nil {
		t.Fatal("expected error when no visible assistant message exists")
	}
	if !strings.Contains(err.Error(), "no assistant message") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDeleteCurrentSessionRequiresNewIDBeforeDelete pins the guard order in
// deleteSessionByID: with an empty replacement id the current session must
// NOT be deleted — the error is returned before the destructive call, so the
// agent never keeps a SessionID whose file is already gone (the next flush
// would resurrect it).
func TestDeleteCurrentSessionRequiresNewIDBeforeDelete(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"current": {Messages: []llm.Message{{Role: "user", Content: "active"}}},
	}}
	store.order = []string{"current"}
	a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "current"}

	_, _, err := a.HandleSessionCommand(context.Background(), "resume del current", "")
	if err == nil {
		t.Fatal("expected error for an empty replacement session id")
	}
	if _, ok := store.sessions["current"]; !ok {
		t.Fatal("current session was deleted before the replacement-id check")
	}
	if a.SessionID != "current" {
		t.Fatalf("SessionID = %q, want the (undeleted) current session", a.SessionID)
	}
}

// TestSetSessionOneshotRoundTrip pins the locked setter wiring: the -p marker
// set through SetSessionOneshot is visible to doPersist readers and cleared
// by a wholesale session reset.
func TestSetSessionOneshotRoundTrip(t *testing.T) {
	a := &Agent{Provider: &statsStubProvider{}, WorkingDir: "/tmp", SessionID: "s"}
	a.SetSessionOneshot(true)
	if !a.SessionOneshot {
		t.Fatal("SetSessionOneshot(true) did not publish")
	}
	a.ResetSessionState()
	if a.SessionOneshot {
		t.Fatal("ResetSessionState must clear the oneshot marker")
	}
}
