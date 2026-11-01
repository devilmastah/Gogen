package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/codeindex"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/session"
)

// TestWorkingDirChangeRequiresGlobalMode verifies the web-UI working-directory
// change (the "config" WS message, handleWSConfig) is gated on global mode:
// in project mode the server rejects the change and leaves the workspace dir
// untouched, while in global mode the same request succeeds and syncs to the
// session agents.
func TestWorkingDirChangeRequiresGlobalMode(t *testing.T) {
	t.Run("project mode rejected", func(t *testing.T) {
		s, _, _ := newLifecycleServer(t)
		srv := startWSServer(t, s)
		defer srv.Close()

		conn := dialWS(t, srv, "/ws")
		defer conn.Close()
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
		cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
		sid := cfg.SessionID
		if cfg.GlobalMode {
			t.Fatal("attach config GlobalMode = true, want false (project mode)")
		}
		_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

		newDir := t.TempDir()
		if err := conn.WriteJSON(WSMessage{Type: "config", WorkingDir: newDir, SessionID: sid}); err != nil {
			t.Fatalf("send config: %v", err)
		}
		resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
		if resp.Kind != "workspace" || resp.Success || !strings.Contains(resp.Content, "global mode") {
			t.Fatalf("rejection notice = %+v, want workspace error about global mode", resp)
		}
		if got := s.ws.GetWorkingDir(); got == newDir {
			t.Fatalf("working dir changed to %q despite project mode", got)
		}
	})

	t.Run("global mode allowed", func(t *testing.T) {
		dir := t.TempDir()
		prov := llmtest.NewMockProvider()
		exec := agent.NewExecutor(dir)
		ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
		a := agent.NewAgent(prov, exec, ctxMgr)
		a.GlobalMode = true
		store := session.NewStoreWithOptions(true, session.StoreOptions{})
		a.SessionStore = store
		s := newTestServer(t, a, &config.Config{})
		srv := startWSServer(t, s)
		defer srv.Close()

		conn := dialWS(t, srv, "/ws")
		defer conn.Close()
		readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
		cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
		sid := cfg.SessionID
		if !cfg.GlobalMode {
			t.Fatal("attach config GlobalMode = false, want true (global mode)")
		}
		_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

		newDir := t.TempDir()
		if err := conn.WriteJSON(WSMessage{Type: "config", WorkingDir: newDir, SessionID: sid}); err != nil {
			t.Fatalf("send config: %v", err)
		}
		echo := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.WorkingDir == newDir })
		if !echo.GlobalMode {
			t.Fatal("working-dir echo GlobalMode = false, want true")
		}
		if got := s.ws.GetWorkingDir(); got != newDir {
			t.Fatalf("workspace working dir = %q, want %q", got, newDir)
		}
		// The change must have been applied to every session agent too
		// (applyWorkingDirToAll runs off the read loop).
		waitFor(t, 5*time.Second, func() bool {
			for _, id := range s.registry.activeIDs() {
				rt, ok := s.registry.get(id)
				if !ok || rt.agent.WorkingDir != newDir {
					return false
				}
			}
			return true
		})
	})
}

// TestApplyWorkingDirToAllSkipsBusySession verifies the working-dir sweep
// never blocks on a session whose turn is running (and is not interrupted):
// that session is skipped and reported, the idle sessions are updated, and
// the call returns within a bounded time instead of hanging forever on a
// blocking turnMu.Lock().
func TestApplyWorkingDirToAllSkipsBusySession(t *testing.T) {
	dirOld := t.TempDir()
	dirNew := t.TempDir()

	s, a, _ := newLifecycleServer(t)
	a.SetWorkingDir(dirOld)

	// A second live session whose turn is "running" (turnMu held, the way a
	// stuck turn holds it for its entire duration).
	busy := s.ws.NewSessionAgent(nil, session.NewID())
	busy.SetWorkingDir(dirOld)
	busyID := busy.SessionID
	rtBusy := newSessionRuntime(busy)
	s.registry.register(busyID, rtBusy)
	rtBusy.turnMu.Lock()
	defer rtBusy.turnMu.Unlock()

	done := make(chan []string, 1)
	go func() { done <- s.applyWorkingDirToAll(dirNew) }()

	var skipped []string
	select {
	case skipped = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("applyWorkingDirToAll blocked on a busy session (blocking Lock?)")
	}
	if len(skipped) != 1 || skipped[0] != busyID {
		t.Fatalf("skipped = %v, want [%s]", skipped, busyID)
	}
	if got := a.WorkingDir; got != dirNew {
		t.Fatalf("idle session working dir = %q, want %q", got, dirNew)
	}
	if got := busy.WorkingDir; got != dirOld {
		t.Fatalf("busy session working dir = %q, want %q (must be left untouched)", got, dirOld)
	}
}

// TestWorkingDirSkipMessage reports the busy sessions by id so the client can
// see which panes did not move.
type blockEmbed struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockEmbed) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case <-b.started:
	default:
		close(b.started)
	}
	<-b.release
	return nil, context.Canceled
}

func TestWorkspaceSetWorkingDirDoesNotWaitOnIndex(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := codeindex.Open(dirA)
	emb := &blockEmbed{started: make(chan struct{}), release: make(chan struct{})}
	go ix.Rebuild(context.Background(), emb, codeindex.Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5})
	select {
	case <-emb.started:
	case <-time.After(2 * time.Second):
		t.Fatal("rebuild did not start")
	}
	ws := &Workspace{WorkingDir: dirA, codeIndex: ix}
	done := make(chan struct{})
	go func() {
		ws.SetWorkingDir(dirB)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SetWorkingDir waited on the index")
	}
	close(emb.release)
	if got := ix.Dir(); got != dirA {
		t.Fatalf("index folder = %q, want %q", got, dirA)
	}
	if got := ws.GetWorkingDir(); got != dirB {
		t.Fatalf("workspace folder = %q, want %q", got, dirB)
	}
}

func TestApplyWorkingDirMovesIndexBeforeTurnLock(t *testing.T) {
	dirOld := t.TempDir()
	dirNew := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirOld, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, a, _ := newLifecycleServer(t)
	ix := codeindex.Open(dirOld)
	s.ws.codeIndex = ix
	a.SetCodeIndex(ix)
	a.SetWorkingDir(dirOld)
	emb := &blockEmbed{started: make(chan struct{}), release: make(chan struct{})}
	go ix.Rebuild(context.Background(), emb, codeindex.Identity{Model: "m", ChunkLines: 40, ChunkOverlap: 5})
	select {
	case <-emb.started:
	case <-time.After(2 * time.Second):
		t.Fatal("rebuild did not start")
	}
	rt := s.registry.first()
	if rt == nil {
		t.Fatal("no session")
	}
	done := make(chan struct{})
	go func() {
		s.applyWorkingDirToAll(dirNew)
		close(done)
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatal("sweep finished while the rebuild held the index")
		default:
		}
		if !rt.turnMu.TryLock() {
			t.Fatal("turn lock held while the index rebuild was still running")
		}
		rt.turnMu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	close(emb.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not finish")
	}
	if got := ix.Dir(); got != dirNew {
		t.Fatalf("index folder = %q, want %q", got, dirNew)
	}
	if got := a.WorkingDir; got != dirNew {
		t.Fatalf("session folder = %q, want %q", got, dirNew)
	}
}

func TestWorkingDirSkipMessage(t *testing.T) {
	msg := workingDirSkipMessage("/work", []string{"sess-1", "sess-2"})
	if !strings.Contains(msg, "/work") || !strings.Contains(msg, "2 session(s)") ||
		!strings.Contains(msg, "sess-1") || !strings.Contains(msg, "sess-2") {
		t.Fatalf("skip message = %q", msg)
	}
}
