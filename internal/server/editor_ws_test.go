package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	llmtest "gogen/internal/llm/llmtest"
)

// dialEditor opens a websocket to the /ws/editor endpoint of a running test
// server (the real mux path, so routing/upgrade/auth are exercised).
func dialEditor(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/editor", nil)
	if err != nil {
		t.Fatalf("dial /ws/editor: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestWSEditorEndpointFSRoundTrip exercises the editor transport split: the
// editor socket serves fs_list/fs_write with requestId correlation, ignores
// chat-shaped messages, and keeps working afterwards.
func TestWSEditorEndpointFSRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws/editor":
			s.HandleWSEditor(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	editor := dialEditor(t, srv)

	// fs_list over the editor socket.
	if err := editor.WriteJSON(WSMessage{Type: "fs_list", RequestID: "ed-1"}); err != nil {
		t.Fatalf("send fs_list: %v", err)
	}
	_ = editor.SetReadDeadline(time.Now().Add(5 * time.Second))
	var listResp WSMessage
	if err := editor.ReadJSON(&listResp); err != nil {
		t.Fatalf("read fs_list_result: %v", err)
	}
	if listResp.Type != "fs_list_result" || listResp.RequestID != "ed-1" {
		t.Fatalf("fs_list response = %+v, want fs_list_result/ed-1", listResp)
	}
	if !listResp.Success {
		t.Fatalf("fs_list failed: %s", listResp.Error)
	}

	// fs_write over the editor socket.
	if err := editor.WriteJSON(WSMessage{Type: "fs_write", RequestID: "ed-2", Path: "hello.txt", Content: "hello editor"}); err != nil {
		t.Fatalf("send fs_write: %v", err)
	}
	_ = editor.SetReadDeadline(time.Now().Add(5 * time.Second))
	var writeResp WSMessage
	if err := editor.ReadJSON(&writeResp); err != nil {
		t.Fatalf("read fs_write_result: %v", err)
	}
	if writeResp.Type != "fs_write_result" || writeResp.RequestID != "ed-2" || !writeResp.Success {
		t.Fatalf("fs_write response = %+v, want successful fs_write_result/ed-2", writeResp)
	}
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(got) != "hello editor" {
		t.Fatalf("file content = %q, %v; want %q", got, err, "hello editor")
	}

	// A non-editor message (chat) sent over the editor socket must be ignored:
	// the server should not respond with anything chat-shaped. We assert the
	// socket stays usable by issuing a real editor request afterwards.
	if err := editor.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("send list_sessions: %v", err)
	}
	if err := editor.WriteJSON(WSMessage{Type: "fs_list", RequestID: "ed-3"}); err != nil {
		t.Fatalf("send fs_list #2: %v", err)
	}
	_ = editor.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp3 WSMessage
	if err := editor.ReadJSON(&resp3); err != nil {
		t.Fatalf("read fs_list_result #2: %v", err)
	}
	if resp3.Type != "fs_list_result" || resp3.RequestID != "ed-3" {
		t.Fatalf("fs_list #2 response = %+v, want fs_list_result/ed-3", resp3)
	}
}

// TestWSEditorGitStatusRoundTrip exercises the v2 git_status payload over the
// real editor socket: the reply carries pre-bucketed gitStatus lists
// (including the staged-only file the v1 parser dropped) plus the legacy flat
// gitEntries (Unstaged+Untracked) for stale tabs.
func TestWSEditorGitStatusRoundTrip(t *testing.T) {
	dir := newGitTestRepo(t)
	// Initial commit so the repo has a branch header.
	if err := os.WriteFile(filepath.Join(dir, "work.go"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "work.go")
	gitIn(t, dir, "commit", "-m", "init")
	// Staged-only file: the v1 vanishing-bug case (worktree column ' ').
	if err := os.WriteFile(filepath.Join(dir, "staged.go"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "staged.go")
	// Unstaged file (committed, then modified in the worktree).
	if err := os.WriteFile(filepath.Join(dir, "work.go"), []byte("b2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untracked file.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})
	editor := dialEditor(t, startEditorTestServer(t, s))

	resp := sendEditorMsg(t, editor, WSMessage{Type: "git_status", RequestID: "gs-1"})
	if resp.Type != "git_status_result" || resp.RequestID != "gs-1" || !resp.Success {
		t.Fatalf("reply = %+v, want successful git_status_result/gs-1", resp)
	}
	gs := resp.GitStatus
	if gs == nil {
		t.Fatalf("gitStatus payload missing: %+v", resp)
	}
	if gs.Branch == "" {
		t.Fatalf("branch header missing: %+v", gs)
	}
	if !reflect.DeepEqual(gs.Staged, []GitStatusEntry{{Path: "staged.go", Status: "A"}}) {
		t.Fatalf("staged = %#v, want staged.go/A", gs.Staged)
	}
	if !reflect.DeepEqual(gs.Unstaged, []GitStatusEntry{{Path: "work.go", Status: "M"}}) {
		t.Fatalf("unstaged = %#v, want work.go/M", gs.Unstaged)
	}
	if !reflect.DeepEqual(gs.Untracked, []GitStatusEntry{{Path: "new.txt", Status: "U"}}) {
		t.Fatalf("untracked = %#v, want new.txt/U", gs.Untracked)
	}
	if len(gs.Unmerged) != 0 {
		t.Fatalf("unmerged = %#v, want empty", gs.Unmerged)
	}
	// Legacy flat list = Unstaged + Untracked.
	if !reflect.DeepEqual(resp.GitEntries, []GitStatusEntry{
		{Path: "work.go", Status: "M"},
		{Path: "new.txt", Status: "U"},
	}) {
		t.Fatalf("legacy gitEntries = %#v, want unstaged+untracked", resp.GitEntries)
	}
}
