package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	llmtest "gogen/internal/llm/llmtest"
)

// TestFSApplyPatchAppliesDiff exercises the fs_apply_patch WS handler end to
// end: a unified diff sent over the socket is applied to the working tree via
// the agent's patch engine, and the response reports success.
func TestFSApplyPatchAppliesDiff(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "greet.go")
	orig := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(target, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})

	diff := "--- a/greet.go\n+++ b/greet.go\n@@ -1,3 +1,4 @@\n package main\n \n+// greeting\n func main() {}\n"

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		ws := newWSConn(conn)
		s.handleFSWriteMessage(ws, context.Background(), WSMessage{
			Type: "fs_apply_patch",
			Diff: diff,
		})
	}))
	defer ts.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))

	var resp WSMessage
	if err := client.ReadJSON(&resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Type != "fs_apply_patch_result" {
		t.Fatalf("type = %q, want fs_apply_patch_result", resp.Type)
	}
	if !resp.Success {
		t.Fatalf("apply failed: %s", resp.Error)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "// greeting") {
		t.Fatalf("file was not patched:\n%s", got)
	}
}

// TestFSApplyPatchRejectsStaleDiff verifies that re-applying an already
// applied patch (context no longer matches) reports an error instead of
// corrupting the file.
func TestFSApplyPatchRejectsStaleDiff(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "greet.go")
	if err := os.WriteFile(target, []byte("package main\n\n// greeting\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})

	diff := "--- a/greet.go\n+++ b/greet.go\n@@ -1,3 +1,4 @@\n package main\n \n+// greeting\n func main() {}\n"

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		ws := newWSConn(conn)
		s.handleFSWriteMessage(ws, context.Background(), WSMessage{
			Type: "fs_apply_patch",
			Diff: diff,
		})
	}))
	defer ts.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))

	var resp WSMessage
	if err := client.ReadJSON(&resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Success {
		t.Fatal("expected apply of a stale diff to fail")
	}
	if resp.Error == "" {
		t.Fatal("expected an error message for a stale diff")
	}
	if resp.Result == "" {
		t.Fatal("expected the patch report in Result for a failed apply")
	}
}

// TestFSApplyPatchRejectsDeleteOnly verifies that a delete-only patch is
// refused: the WS message context carries no delete approver, so the
// executor's approval gate rejects it and the file is left untouched.
func TestFSApplyPatchRejectsDeleteOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "remove.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})

	diff := "--- a/remove.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n"

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		ws := newWSConn(conn)
		s.handleFSWriteMessage(ws, context.Background(), WSMessage{
			Type: "fs_apply_patch",
			Diff: diff,
		})
	}))
	defer ts.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))

	var resp WSMessage
	if err := client.ReadJSON(&resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Type != "fs_apply_patch_result" {
		t.Fatalf("type = %q, want fs_apply_patch_result", resp.Type)
	}
	if resp.Success {
		t.Fatal("expected a delete-only patch to be rejected")
	}
	if !strings.Contains(resp.Error, "delete blocked: approval is required") {
		t.Fatalf("unexpected error %q", resp.Error)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatal("file should still exist after a rejected delete")
	}
}

// TestFSApplyPatchWaitsForFSLock verifies the workspace filesystem
// lock: an editor write issued while another mutation holds fsMu (a running
// agent FS tool) waits instead of failing, then completes once the lock is
// released — the streaming turn itself no longer blocks editor writes.
func TestFSApplyPatchWaitsForFSLock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "greet.go")
	if err := os.WriteFile(target, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})

	diff := "--- a/greet.go\n+++ b/greet.go\n@@ -1,3 +1,4 @@\n package main\n \n+// greeting\n func main() {}\n"

	// An agent FS-mutating tool holds the workspace fsMu; the editor write
	// issued concurrently must wait, not fail (nil ws: the response write is
	// a silent no-op — the patch application is what we observe).
	s.ws.fsMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleFSWriteMessage(nil, context.Background(), WSMessage{Type: "fs_apply_patch", Diff: diff})
	}()
	select {
	case <-done:
		t.Fatal("fs_apply_patch completed while the workspace fsMu was held")
	case <-time.After(50 * time.Millisecond):
	}
	s.ws.fsMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fs_apply_patch did not complete after fsMu release")
	}
	data, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(data), "// greeting") {
		t.Fatalf("patch not applied: %v, content=%q", err, data)
	}
}
