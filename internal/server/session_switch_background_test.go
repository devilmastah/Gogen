package server

// Tests for the "a session switch must not destroy background work" rule.
//
// The reported symptom: switching sessions in the web UI killed the
// background work the user had started (execute_command background=true
// jobs, persistent terminals). The switch releases the old pane's
// attachment (app.js sendSessionDetach → session_detach), which used to
// orphan-evict the runtime, and the eviction tail (evictRuntime →
// Agent.Close) kills exactly that work — even though the session itself
// stays saved and reopens against the very same live runtime.
//
// Policy pinned here: an eviction that only recycles MEMORY (a pane switch,
// a page refresh, a closed tab, registry-cap pressure) preserves live
// background work; an explicit end-of-session (✕ close, delete, shutdown)
// still kills it.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	llmtest "gogen/internal/llm/llmtest"
)

// startTestBackgroundJob launches a long job that writes marker when it
// finishes naturally. Returns the job id. The marker is the ground truth
// for "the process actually completed its work": a killed job never writes
// it, a surviving one writes it after the sleep.
func startTestBackgroundJob(t *testing.T, a *agent.Agent, dir string) (id, marker string) {
	t.Helper()
	marker = filepath.Join(dir, "marker-"+a.SessionID)
	id, err := a.StartBackgroundCommand(context.Background(), "sleep 30; echo done > "+marker)
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	if st, err := a.BackgroundJobStatus(id); err != nil || !strings.Contains(st, "RUNNING") {
		t.Fatalf("job status after start: %q, %v; want RUNNING", st, err)
	}
	return id, marker
}

func jobRunning(t *testing.T, a *agent.Agent, id string) bool {
	t.Helper()
	st, err := a.BackgroundJobStatus(id)
	return err == nil && strings.Contains(st, "RUNNING")
}

// TestSessionSwitchKeepsBackgroundJobs is the reported bug, end to end over a
// real websocket: with a background job running in session A, switching the
// pane to a new session (typed /new — the client's re-key, followed by the
// session_detach it sends for the session it left behind) must not kill A's
// job. Switching back must land on the SAME live runtime with the job still
// running and pollable.
func TestSessionSwitchKeepsBackgroundJobs(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sessionA := a.SessionID
	id, marker := startTestBackgroundJob(t, a, dir)

	// Switch: the pane re-keys to a fresh session, then releases A's
	// attachment (app.js sendSessionDetach on the re-key).
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "/new", SessionID: sessionA}); err != nil {
		t.Fatalf("send typed /new: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	sessionB := resp.SessionID
	if sessionB == "" || sessionB == sessionA {
		t.Fatalf("/new response sessionId = %q, want a fresh session != %q", sessionB, sessionA)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" })
	configAfterClear(t, conn, sessionA)
	if err := conn.WriteJSON(WSMessage{Type: "session_detach", SessionID: sessionA}); err != nil {
		t.Fatalf("send session_detach: %v", err)
	}

	// A must survive the switch with its job intact.
	if _, ok := s.registry.get(sessionA); !ok {
		t.Fatal("switching sessions unregistered the old session")
	}
	if !jobRunning(t, a, id) {
		t.Fatal("switching sessions killed the old session's background job")
	}
	requireNever(t, 500*time.Millisecond, "the background job of the switched-away session completed its work", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})

	// Switch back: the same live runtime (not a store reload), job included.
	rtBefore, _ := s.registry.get(sessionA)
	if err := conn.WriteJSON(WSMessage{Type: "session_resume", SessionID: sessionA}); err != nil {
		t.Fatalf("send session_resume: %v", err)
	}
	// The resume reply IS the attach sequence: session_state re-keys the
	// pane, then the configs. A has no messages, so the conditional attach
	// legitimately skips the empty history snapshot (same as focusing any
	// unchanged pane).
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "session_state" && m.SessionID == sessionA
	})
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	rtAfter, ok := s.registry.get(sessionA)
	if !ok {
		t.Fatal("the session is no longer registered after switching back")
	}
	if rtAfter != rtBefore {
		t.Fatal("switching back built a fresh runtime: the background job would be unreachable")
	}
	if !jobRunning(t, a, id) {
		t.Fatal("the background job is not pollable after switching back")
	}
	// The model can still see and wait on it.
	if list := a.BackgroundJobsList(); !strings.Contains(list, id) {
		t.Fatalf("background_job list after switching back does not carry the job: %q", list)
	}
}

// TestPageRefreshKeepsBackgroundJobs pins the same rule for the other way a
// client stops watching a session: the tab goes away (a refresh), teardown
// detaches the last client. The session is idle — no turn, no job owner on
// the wire — but its job is still running, so the runtime stays.
func TestPageRefreshKeepsBackgroundJobs(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID
	id, marker := startTestBackgroundJob(t, a, dir)

	conn.Close() // the refresh / tab close

	waitFor(t, 5*time.Second, func() bool {
		rt, ok := s.registry.get(sid)
		return ok && rt.viewerCount() == 0
	})
	if _, ok := s.registry.get(sid); !ok {
		t.Fatal("the refresh unregistered a session that still owns a running background job")
	}
	if !jobRunning(t, a, id) {
		t.Fatal("the refresh killed the session's background job")
	}
	requireNever(t, 500*time.Millisecond, "the background job of the refreshed session completed its work", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
}

// TestBackgroundWorkDrainedThenEvicted pins the other half of the policy:
// keeping a session resident is a deferral, not a leak. Once the last job
// finishes, the background-idle hook re-runs the orphan check and the
// clientless runtime is evicted exactly as it would have been.
func TestBackgroundWorkDrainedThenEvicted(t *testing.T) {
	dir := t.TempDir()
	r := newSessionRegistry(4)
	a := agent.NewAgent(llmtest.NewMockProvider(), agent.NewExecutor(dir), nil)
	a.SetSessionID("a")
	rt := newSessionRuntime(a)
	r.register("a", rt)

	ws := newFakeWSConn()
	rt.attach(ws)

	// A short job pins the runtime across the switch.
	if _, err := a.StartBackgroundCommand(context.Background(), "sleep 0.2"); err != nil {
		t.Fatalf("start background job: %v", err)
	}
	rt.detach(ws)
	if _, ok := r.get("a"); !ok {
		t.Fatal("the switched-away session was evicted while its job ran")
	}
	if !rt.agent.HasBackgroundWork() {
		t.Fatal("the background job did not outlive the detach")
	}

	// Once it drains, the idle hook retires the now-genuinely-idle runtime.
	waitFor(t, 10*time.Second, func() bool {
		_, ok := r.get("a")
		return !ok
	})
	if !rt.evicted.Load() {
		t.Fatal("the drained session was unregistered without the evicted flag")
	}
}

// TestExplicitCloseStillKillsBackgroundJobs pins the end-of-session half:
// pressing ✕ (session_close) means the session is done, so its background
// job must be killed, not merely deferred. Without this the guard would
// turn every explicit close into a resident session.
func TestExplicitCloseStillKillsBackgroundJobs(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID
	id, marker := startTestBackgroundJob(t, a, dir)

	if err := conn.WriteJSON(WSMessage{Type: "session_close", SessionID: sid}); err != nil {
		t.Fatalf("send session_close: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(sid)
		return !ok
	})
	// The job's process group must be gone and its bookkeeping cleared.
	waitFor(t, 5*time.Second, func() bool {
		_, err := a.BackgroundJobStatus(id)
		return err != nil && strings.Contains(err.Error(), "unknown background job")
	})
	requireNever(t, 750*time.Millisecond, "the background job of an explicitly closed session completed its work", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
}

// TestSwitchKeepsSessionWithBackgroundSubagent pins the background-subagent
// half end to end: a parent whose continuable background child is still live
// must not be orphan-evicted when the client stops watching it (the eviction
// hook cancels every live child of an evicted parent) — and once the child
// is released, the parent retires like any other idle session, so deferring
// the eviction does not leak the runtime.
func TestSwitchKeepsSessionWithBackgroundSubagent(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	a.SetSubagentsEnabled(true)
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour // keep the child registered for the assertions
	t.Cleanup(func() { waitForChildSpawnsSettled(t, s) })

	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	sid := a.SessionID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childID, err := sp.SpawnBackground(ctx, a, "long background job", "", 0)
	if err != nil {
		t.Fatalf("spawn background child: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, ok := s.registry.get(childID)
		return ok
	})

	// The client's last attachment goes away (a pane switch / refresh).
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		rt, found := s.registry.get(sid)
		return found && rt.viewerCount() == 0
	})
	rt, ok := s.registry.get(sid)
	if !ok {
		t.Fatal("a session with a live background subagent was evicted when the client stopped watching it")
	}
	if rt.evicted.Load() {
		t.Fatal("a session with a live background subagent was marked evicted")
	}
	// The child is untouched: the parent's eviction hook never fired.
	if _, ok := s.registry.get(childID); !ok {
		t.Fatal("the parent's eviction took its background child with it")
	}

	// Releasing the child drains the parent's background work: the parent is
	// evictable again, and the release path is what re-checks it (a
	// background drain must not leave the runtime resident forever).
	sp.cancelAll(sid)
	waitFor(t, 5*time.Second, func() bool {
		_, found := s.registry.get(sid)
		return !found
	})
}
