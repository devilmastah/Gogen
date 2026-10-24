package server

// Tests for the nested-subagent correctness fixes:
//   - bootstrap / "resume latest" never restore a child session
//     (TestBootstrapRestoresParentNotChild)
//   - the subagent outcome is persisted and reaches the sessions payload
//     (TestSubagentOutcomePersistedInPayload)
//   - a re-attached child restores its runtime privileges
//     (TestReattachedChildRestoresRuntimePrivileges)
//   - deleting a parent evicts its registered child runtimes instead of
//     leaving them to resurrect their cascade-deleted files
//     (TestDeleteParentEvictsRegisteredChild)
//   - the same cascade holds when the deleted parent's runtime is already
//     gone (orphan/cap eviction) but its background child is still running
//     (TestDeleteParentUnregisteredEvictsChild)

import (
	"context"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/session"
)

// newestChildID returns the most recently updated saved child of parentID
// (excluding exclude, when non-empty). The store list is most-recent-first.
func newestChildID(t *testing.T, store *session.Store, dir, parentID, exclude string) string {
	t.Helper()
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, si := range list {
		if si.ParentID == parentID && si.ID != exclude {
			return si.ID
		}
	}
	t.Fatalf("no nested child of %s in the store", parentID)
	return ""
}

// TestBootstrapRestoresParentNotChild verifies the restart bootstrap picks
// the most recent TOP-LEVEL session: a subagent that finished last must not
// become the default session after a server restart / all-panes-closed.
func TestBootstrapRestoresParentNotChild(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, store := newContinuationServer(t, stub, dir)

	parentID := "parent-sess"
	if err := store.Save(parentID, agent.SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // the child must be strictly newer
	if err := store.Save("child-sess", agent.SessionSnapshot{
		WorkingDir: dir,
		ParentID:   parentID,
		Messages:   []llm.Message{{Role: "user", Content: "job"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Precondition: the child really is the most recently updated session.
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].ID != "child-sess" {
		t.Fatalf("precondition: newest session = %q, want the child", list[0].ID)
	}

	// Every runtime evicted (restart / last pane closed): bootstrap.
	for _, id := range s.registry.activeIDs() {
		if rt, ok := s.registry.get(id); ok {
			s.registry.closeRuntime(rt)
		}
	}
	if s.registry.first() != nil {
		t.Fatal("registry must be empty before bootstrap")
	}
	rt := s.createBootstrapSession()
	if rt.agent.SessionID != parentID {
		t.Fatalf("bootstrap restored %q, want the parent %q (children must not become the default)", rt.agent.SessionID, parentID)
	}
}

// TestSubagentOutcomePersistedInPayload verifies the final outcome of a
// foreground child is persisted and carried in the sessions payload, so the
// sidebar can render the true result after a reload/restart (when the
// subagent_started/finished events are not replayed).
func TestSubagentOutcomePersistedInPayload(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "good report"}}
		return p
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	listPayload := func() WSMessage {
		if err := conn.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
			t.Fatalf("list_sessions: %v", err)
		}
		return readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "sessions" })
	}

	sp := newSubagentSpawner(s)
	// Successful spawn: status persisted as "success".
	if _, err := sp.Spawn(context.Background(), a, "good job", "", 0); err != nil {
		t.Fatal(err)
	}
	okChild := newestChildID(t, store, dir, a.SessionID, "")
	msg := listPayload()
	if e := sessionPayloadEntry(msg.Sessions, okChild); e == nil || e.SubagentStatus != "success" {
		t.Fatalf("successful child payload = %+v, want subagentStatus success", e)
	}

	// Failed (cancelled) spawn: status persisted as "failed".
	s.ws.ProviderFactory = func() llm.LLMProvider { return &blockingProvider{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sp.Spawn(ctx, a, "bad job", "", 0)
		done <- err
	}()
	var failChild string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID && id != okChild {
				failChild = id
				return true
			}
		}
		return false
	})
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return after cancel")
	}
	failChild = newestChildID(t, store, dir, a.SessionID, okChild)
	msg = listPayload()
	if e := sessionPayloadEntry(msg.Sessions, failChild); e == nil || e.SubagentStatus != "failed" {
		t.Fatalf("failed child payload = %+v, want subagentStatus failed", e)
	}
}

// TestReattachedChildRestoresRuntimePrivileges verifies a child reopened
// from the store (attach after eviction / restart) restores its runtime
// parent link, nested flag, and the D6 delete-approval override — without
// them a reopened child could be cap-evicted mid-task and headless delete
// approvals would never reach the parent.
func TestReattachedChildRestoresRuntimePrivileges(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "child report"}}
		return p
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	sp := newSubagentSpawner(s)
	if _, err := sp.Spawn(context.Background(), a, "job", "", 0); err != nil {
		t.Fatal(err)
	}
	childID := newestChildID(t, store, dir, a.SessionID, "")
	if _, ok := s.registry.get(childID); ok {
		t.Fatal("finished child must be unregistered")
	}

	// Re-attach the child (the sidebar "switch to subagent" after restart):
	// the runtime is rebuilt from the store.
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: childID}); err != nil {
		t.Fatalf("attach child: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == childID
	})
	rt, ok := s.registry.get(childID)
	if !ok {
		t.Fatal("attached child not registered")
	}
	if !rt.nested {
		t.Fatal("re-attached child lost the nested (cap-exemption) flag")
	}
	if rt.parentID != a.SessionID {
		t.Fatalf("re-attached child parentID = %q, want %q", rt.parentID, a.SessionID)
	}
	if rt.approverOverride == nil {
		t.Fatal("re-attached child missing the D6 delete-approval override")
	}
}

// TestDeleteParentEvictsRegisteredChild verifies deleting a parent drains
// and evicts its registered child runtimes alongside the store's cascade
// file delete: a still-running child must be cancelled, its runtime
// removed, and its file must NOT be resurrected by the spawner's final
// outcome flush (delete resurrection, same shape as the root guard).
func TestDeleteParentEvictsRegisteredChild(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{}
	}
	a.SetSubagentsEnabled(true)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (newSubagentSpawner(s)).Spawn(ctx, a, "long job", "", 0)
		done <- err
	}()
	var childID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID {
				childID = id
				return true
			}
		}
		return false
	})
	// Open the child as a pane (attached → registered client).
	if err := conn.WriteJSON(WSMessage{Type: "session_attach", SessionID: childID}); err != nil {
		t.Fatalf("attach child: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == childID
	})

	// Delete the PARENT: the child must be evicted and notified.
	if err := conn.WriteJSON(WSMessage{Type: "session_delete", SessionID: a.SessionID}); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_removed" && m.SessionID == childID
	})
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(childID)
		return !ok
	})
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("spawn must fail after the parent was deleted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return after the parent was deleted")
	}
	// The child's file is gone (cascade) and stays gone (no resurrection
	// by the spawner's final outcome flush).
	if _, err := store.LoadInWorkingDir(dir, childID); err == nil {
		t.Fatal("child file must be gone after the parent delete")
	}
	assertFileNotResurrected(t, store, dir, childID, 500*time.Millisecond)
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, si := range list {
		if si.ID == childID {
			t.Fatal("deleted child still listed in the store")
		}
	}
}

// assertFileNotResurrected polls for a window and fails if the session file
// reappears — a late persist (e.g. a cancelled turn's final flush) that
// would recreate a file the store's cascade delete just removed.
func assertFileNotResurrected(t *testing.T, store *session.Store, dir, id string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if _, err := store.LoadInWorkingDir(dir, id); err == nil {
			t.Fatalf("session %s file was resurrected after the delete", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDeleteParentUnregisteredEvictsChild covers the unregistered-parent
// variant of the delete cascade: the parent runtime was already
// orphan-evicted (no viewers, no running turn) while its background child
// keeps running (held — orphan eviction deliberately does not cancel
// children). Deleting the parent by id must still drain + evict the
// registered child: its file is cascade-deleted and must NOT be
// resurrected by the cancelled turn's final flush.
func TestDeleteParentUnregisteredEvictsChild(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{}
	}
	a.SetSubagentsEnabled(true)
	sp := continuableSpawner(t, s)

	// Background child: its turn runs (blocked on the provider) after the
	// spawn returns, so the parent itself is idle.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childID, err := sp.SpawnBackground(ctx, a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(childID)
		return ok
	})
	// Persist the child up front so the cascade delete (and its absence
	// afterwards) is meaningful.
	childRt, ok := s.registry.get(childID)
	if !ok {
		t.Fatal("child runtime not registered")
	}
	childRt.agent.FlushSession()
	if _, err := store.LoadInWorkingDir(dir, childID); err != nil {
		t.Fatalf("child must be persisted before the delete: %v", err)
	}

	// Evict the parent (no attached clients, idle) while its background child
	// keeps running. This is the explicit eviction tail, NOT evictOrphaned:
	// an unwatched parent that still owns a live background child is no
	// longer evictable as an orphan (keepsBackgroundWork — see
	// TestSwitchKeepsSessionWithBackgroundSubagent), and the tail here does
	// not fire the eviction hook either, so the child stays registered.
	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("parent runtime not registered")
	}
	s.registry.evictRuntime(parentRt)
	if _, ok := s.registry.get(a.SessionID); ok {
		t.Fatal("parent must be unregistered after eviction")
	}
	if _, ok := s.registry.get(childID); !ok {
		t.Fatal("child must stay registered while its parent is evicted")
	}

	// Delete the parent by id (the sidebar ✕ on a session whose runtime is
	// gone): the unregistered path must still drain + evict the child.
	var pane *sessionRuntime
	if _, _, err := s.sessionDelete(context.Background(), nil, &pane, a.SessionID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}

	// The child runtime is evicted and its file is gone — and stays gone
	// (no resurrection by the cancelled turn's final flush).
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(childID)
		return !ok
	})
	// The child record is released from the spawner's registry promptly too
	// (the unregistered path now fires the eviction hook like the
	// registered one — otherwise the record would linger until the
	// retention timer).
	waitFor(t, 5*time.Second, func() bool {
		return sp.Get(childID) == nil
	})
	if _, err := store.LoadInWorkingDir(dir, childID); err == nil {
		t.Fatal("child file must be gone after the parent delete")
	}
	assertFileNotResurrected(t, store, dir, childID, 500*time.Millisecond)
	// The parent's file is gone too, and neither session is listed.
	if _, err := store.LoadInWorkingDir(dir, a.SessionID); err == nil {
		t.Fatal("parent file must be gone after the delete")
	}
	list, err := store.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, si := range list {
		if si.ID == childID || si.ID == a.SessionID {
			t.Fatalf("deleted session %s still listed in the store", si.ID)
		}
	}
}

// heldProvider streams nothing and ignores context cancellation until the
// test releases it — a turn that outlives the delete drain (stuck turn).
// started is closed the moment GenerateResponseStream is entered, so the
// test can wait for the turn to be genuinely blocked in the provider.
type heldProvider struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (p *heldProvider) Name() string          { return "held" }
func (p *heldProvider) ModelName() string     { return "held-model" }
func (p *heldProvider) SetModel(string) error { return nil }
func (p *heldProvider) SetThinkingLevel(string) {
}
func (p *heldProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (p *heldProvider) GenerateResponse(context.Context, []llm.Message, map[string]struct{}, []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}
func (p *heldProvider) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	if h != nil && h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	p.once.Do(func() { close(p.started) })
	<-p.release // deliberately ignores ctx: the turn outlives the delete drain
	return nil, ctx.Err()
}
func (p *heldProvider) ModelContextLimit(context.Context) (int, error) { return 1000, nil }

// TestDeleteParentEvictsStuckChild pins the stuck-turn variant of the
// delete cascade: a background child whose turn outlives the drain (it
// ignores cancellation) is still drained + evicted, and every file a late
// write recreates after the cascade is swept — the release path's outcome
// flush by the delete path's eviction sweep, and the turn's own final
// flush (once the turn finally ends) by the spawn goroutine's orphan sweep.
func TestDeleteParentEvictsStuckChild(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	release := make(chan struct{})
	started := make(chan struct{})
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &heldProvider{release: release, started: started}
	}
	a.SetSubagentsEnabled(true)
	sp := continuableSpawner(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childID, err := sp.SpawnBackground(ctx, a, "stuck job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(childID)
		return ok
	})
	// Persist the child up front so the cascade delete (and the absence
	// afterwards) is meaningful.
	childRt, ok := s.registry.get(childID)
	if !ok {
		t.Fatal("child runtime not registered")
	}
	childRt.agent.FlushSession()
	if _, err := store.LoadInWorkingDir(dir, childID); err != nil {
		t.Fatalf("child must be persisted before the delete: %v", err)
	}
	// Wait for the turn to be genuinely blocked in the provider before
	// deleting — otherwise the delete races the turn's startup and the
	// drain finds nothing stuck (the stream is cancelled before the
	// provider is reached, so the turn just ends).
	waitFor(t, 5*time.Second, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})
	var pane *sessionRuntime
	if _, _, err := s.sessionDelete(context.Background(), nil, &pane, a.SessionID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(childID)
		return !ok
	})
	if _, err := store.LoadInWorkingDir(dir, childID); err == nil {
		t.Fatal("child file must be gone after the parent delete")
	}

	// Let the stuck turn end: its final flush re-creates the file once
	// more, and the spawn goroutine's orphan sweep must remove it. The
	// resurrection is transient (the sweep runs microseconds after the
	// flush), so settle past it and verify the file stays gone — a broken
	// sweep would leave the resurrected file behind.
	close(release)
	time.Sleep(300 * time.Millisecond)
	assertFileNotResurrected(t, store, dir, childID, 500*time.Millisecond)
}
