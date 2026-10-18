package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	llmtest "gogen/internal/llm/llmtest"
)

// newGitTestRepoWithOrigin creates a temp git repo (with committer identity)
// whose "origin" remote is a local bare repo, so git_push can round-trip
// without any network. It returns the work dir, the bare repo dir, and the
// current branch name.
func newGitTestRepoWithOrigin(t *testing.T) (dir, bare, branch string) {
	t.Helper()
	dir = newGitTestRepo(t)
	bare = t.TempDir()
	gitIn(t, bare, "init", "--bare")
	// An initial commit so the repo has a branch to push.
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "seed.txt")
	gitIn(t, dir, "commit", "-m", "initial")
	branch = gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
	gitIn(t, dir, "remote", "add", "origin", bare)
	return dir, bare, branch
}

// TestWSEditorGitStageCommitPushRoundTrip exercises the full editor-socket
// mutation flow against a temp repo with a local bare origin: stage (single
// path and all), commit, and push — verifying the bare repo actually
// received the commit.
func TestWSEditorGitStageCommitPushRoundTrip(t *testing.T) {
	dir, bare, branch := newGitTestRepoWithOrigin(t)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})
	editor := dialEditor(t, startEditorTestServer(t, s))

	// Stage a single path.
	resp := sendEditorMsg(t, editor, WSMessage{Type: "git_stage", RequestID: "st-1", Paths: []string{"b.txt"}})
	if resp.Type != "git_stage_result" || resp.RequestID != "st-1" || !resp.Success {
		t.Fatalf("git_stage reply = %+v, want successful git_stage_result/st-1", resp)
	}
	// Stage everything (empty paths = `git add -A`): picks up c.txt too.
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_stage", RequestID: "st-2"})
	if resp.Type != "git_stage_result" || !resp.Success {
		t.Fatalf("git_stage (all) reply = %+v, want success", resp)
	}

	// git_status must now show both files staged.
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_status", RequestID: "gs-1"})
	if !resp.Success || resp.GitStatus == nil {
		t.Fatalf("git_status reply = %+v, want success with gitStatus", resp)
	}
	staged := map[string]string{}
	for _, e := range resp.GitStatus.Staged {
		staged[e.Path] = e.Status
	}
	if len(staged) != 2 || staged["b.txt"] != "A" || staged["c.txt"] != "A" {
		t.Fatalf("staged = %#v, want b.txt/A and c.txt/A", resp.GitStatus.Staged)
	}

	// Commit.
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_commit", RequestID: "gc-1", Content: "feat: b and c"})
	if resp.Type != "git_commit_result" || resp.RequestID != "gc-1" || !resp.Success {
		t.Fatalf("git_commit reply = %+v, want successful git_commit_result/gc-1", resp)
	}

	// Push the branch to the local bare origin.
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_push", RequestID: "gp-1", Branch: branch})
	if resp.Type != "git_push_result" || resp.RequestID != "gp-1" || !resp.Success {
		t.Fatalf("git_push reply = %+v, want successful git_push_result/gp-1", resp)
	}

	// The bare origin must now point at the same commit as the work repo.
	localHead := gitIn(t, dir, "rev-parse", "HEAD")
	remoteHead := gitIn(t, bare, "rev-parse", "HEAD")
	if localHead != remoteHead {
		t.Fatalf("origin HEAD = %s, work repo HEAD = %s; push did not land", remoteHead, localHead)
	}

	// A second push with an empty branch (bare `git push`) must succeed too
	// (upstream was set by the first `-u` push).
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_push", RequestID: "gp-2"})
	if !resp.Success {
		t.Fatalf("git_push (no branch) reply = %+v, want success", resp)
	}
}

// TestWSEditorGitUnstage verifies git_unstage over the editor socket: a
// single path unstages only that path, and empty paths unstage everything.
func TestWSEditorGitUnstage(t *testing.T) {
	dir, _, _ := newGitTestRepoWithOrigin(t)
	for _, name := range []string{"x.txt", "y.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "x.txt")
	gitIn(t, dir, "add", "y.txt")

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})
	editor := dialEditor(t, startEditorTestServer(t, s))

	// Unstage a single path.
	resp := sendEditorMsg(t, editor, WSMessage{Type: "git_unstage", RequestID: "us-1", Paths: []string{"x.txt"}})
	if resp.Type != "git_unstage_result" || resp.RequestID != "us-1" || !resp.Success {
		t.Fatalf("git_unstage reply = %+v, want successful git_unstage_result/us-1", resp)
	}
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_status", RequestID: "gs-1"})
	if !resp.Success || resp.GitStatus == nil {
		t.Fatalf("git_status reply = %+v, want success with gitStatus", resp)
	}
	if len(resp.GitStatus.Staged) != 1 || resp.GitStatus.Staged[0].Path != "y.txt" {
		t.Fatalf("staged = %#v, want only y.txt", resp.GitStatus.Staged)
	}

	// Unstage everything (empty paths).
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_unstage", RequestID: "us-2"})
	if !resp.Success {
		t.Fatalf("git_unstage (all) reply = %+v, want success", resp)
	}
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_status", RequestID: "gs-2"})
	if !resp.Success || resp.GitStatus == nil {
		t.Fatalf("git_status reply = %+v, want success with gitStatus", resp)
	}
	if len(resp.GitStatus.Staged) != 0 {
		t.Fatalf("staged = %#v, want empty", resp.GitStatus.Staged)
	}
}

// TestWSEditorGitIndexLockError simulates a concurrent git mutation holding
// the index lock (the agent-side git tools deliberately do NOT take fsMu):
// the editor mutation must fail fast with a clean index.lock error, not hang.
func TestWSEditorGitIndexLockError(t *testing.T) {
	dir, _, _ := newGitTestRepoWithOrigin(t)
	if err := os.WriteFile(filepath.Join(dir, "d.txt"), []byte("d\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, []byte("simulated concurrent lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(lock) })

	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{})
	editor := dialEditor(t, startEditorTestServer(t, s))

	if err := editor.WriteJSON(WSMessage{Type: "git_stage", RequestID: "st-lock", Paths: []string{"d.txt"}}); err != nil {
		t.Fatalf("send git_stage: %v", err)
	}
	// A short deadline proves the failure is fast (git refuses the lock
	// immediately instead of blocking behind the simulated holder).
	_ = editor.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp WSMessage
	if err := editor.ReadJSON(&resp); err != nil {
		t.Fatalf("read reply (hung behind index.lock?): %v", err)
	}
	if resp.Type != "git_stage_result" || resp.RequestID != "st-lock" || resp.Success {
		t.Fatalf("reply = %+v, want failed git_stage_result/st-lock", resp)
	}
	if !strings.Contains(resp.Error, "index.lock") {
		t.Fatalf("error = %q, want it to mention index.lock", resp.Error)
	}

	// Once the lock is gone, the same mutation succeeds — the socket is
	// still healthy.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_stage", RequestID: "st-after", Paths: []string{"d.txt"}})
	if !resp.Success {
		t.Fatalf("git_stage after lock release = %+v, want success", resp)
	}
}

// TestWSEditorHandlerParity asserts that the /ws/editor socket's type lists
// (editorReadTypes/editorWriteTypes, used by HandleWSEditor) and the
// wsHandlers map (main chat socket) agree on which types are editor FS
// messages. The two registrations must be kept in lockstep: missing either
// one silently drops the message.
func TestWSEditorHandlerParity(t *testing.T) {
	readSet := make(map[string]bool, len(editorReadTypes))
	writeSet := make(map[string]bool, len(editorWriteTypes))
	for _, typ := range editorReadTypes {
		if writeSet[typ] {
			t.Fatalf("type %q appears in both editorReadTypes and editorWriteTypes", typ)
		}
		readSet[typ] = true
	}
	for _, typ := range editorWriteTypes {
		writeSet[typ] = true
	}

	// Every wsHandlers entry tagged as an FS handler must be in the
	// matching editor list (the editor socket would drop it otherwise).
	for typ, e := range wsHandlers {
		switch e.kind {
		case wsKindFSRead:
			if !readSet[typ] {
				t.Errorf("wsHandlers routes %q to the FS-read handler, but it is not in editorReadTypes (editor socket would drop it)", typ)
			}
		case wsKindFSWrite:
			if !writeSet[typ] {
				t.Errorf("wsHandlers routes %q to the FS-write handler, but it is not in editorWriteTypes (editor socket would drop it)", typ)
			}
		}
	}

	// Conversely, every editor-list type must be registered in wsHandlers
	// with the matching kind (the main chat socket would drop it otherwise).
	for typ := range readSet {
		if wsHandlers[typ].kind != wsKindFSRead {
			t.Errorf("editorReadTypes contains %q, but wsHandlers does not route it to the FS-read handler", typ)
		}
	}
	for typ := range writeSet {
		if wsHandlers[typ].kind != wsKindFSWrite {
			t.Errorf("editorWriteTypes contains %q, but wsHandlers does not route it to the FS-write handler", typ)
		}
	}
}
