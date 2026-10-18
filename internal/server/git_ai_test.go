package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// newGitTestRepo creates a temp git repository with a committer identity.
func newGitTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rgit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	rgit("init")
	rgit("config", "user.email", "test@example.com")
	rgit("config", "user.name", "Test")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newEditorAIServer builds a web server over a temp git repo with the given
// provider (the test/embed path: the workspace ProviderFactory hands out
// exactly this provider, mirroring newWorkspaceFromAgent).
func newEditorAIServer(t *testing.T, dir string, prov llm.LLMProvider) (*Server, *agent.Agent) {
	t.Helper()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	return newTestServer(t, a, &config.Config{}), a
}

func startEditorTestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws/editor" {
			s.HandleWSEditor(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sendEditorMsg(t *testing.T, conn *websocket.Conn, msg WSMessage) WSMessage {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("send %s: %v", msg.Type, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var resp WSMessage
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("read reply to %s: %v", msg.Type, err)
	}
	return resp
}

// TestGitCommitMessageOneShot verifies the happy path through the real
// editor socket: the reply carries the mock's content, the provider saw ONE
// one-shot call with a single user message (staged diff + instruction), and
// NO session was created or modified (registry ids and history unchanged).
func TestGitCommitMessageOneShot(t *testing.T) {
	dir := newGitTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "a.txt")

	mock := llmtest.NewMockProvider()
	mock.Responses = []llm.Response{{Content: "feat: update a.txt"}}
	s, a := newEditorAIServer(t, dir, mock)
	editor := dialEditor(t, startEditorTestServer(t, s))

	beforeIDs := s.registry.activeIDs()
	beforeCount := a.MessageCount()

	resp := sendEditorMsg(t, editor, WSMessage{Type: "git_commit_message", RequestID: "ai-1"})
	if resp.Type != "git_commit_message_result" || resp.RequestID != "ai-1" {
		t.Fatalf("reply = %+v, want git_commit_message_result/ai-1", resp)
	}
	if !resp.Success || resp.Content != "feat: update a.txt" {
		t.Fatalf("reply = %+v, want success with the mock content", resp)
	}

	// One-shot: exactly one provider call, one user message, no tools.
	if mock.CallCount != 1 {
		t.Fatalf("provider called %d times, want exactly 1", mock.CallCount)
	}
	msgs := mock.LastMessages
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("messages = %+v, want a single user message", msgs)
	}
	if !strings.Contains(msgs[0].Content, "hello world") {
		t.Fatalf("prompt missing staged diff content:\n%s", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, gitCommitMessageInstruction) {
		t.Fatalf("prompt missing instruction:\n%s", msgs[0].Content)
	}

	// No session was created or modified by the call.
	if !reflect.DeepEqual(beforeIDs, s.registry.activeIDs()) {
		t.Fatalf("session ids changed: before=%v after=%v", beforeIDs, s.registry.activeIDs())
	}
	if a.MessageCount() != beforeCount {
		t.Fatalf("session history changed: %d -> %d messages", beforeCount, a.MessageCount())
	}
}

// TestGitCommitMessageErrors pins the clean-error paths: LLM failure,
// nothing staged, provider unconfigured, and no provider factory. Each must
// reply with Success=false + a non-empty error (the UI toasts and keeps the
// composer usable).
func TestGitCommitMessageErrors(t *testing.T) {
	t.Run("llm_error", func(t *testing.T) {
		dir := newGitTestRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", "a.txt")

		mock := llmtest.NewMockProvider()
		mock.GenerateErr = errors.New("boom")
		s, _ := newEditorAIServer(t, dir, mock)
		editor := dialEditor(t, startEditorTestServer(t, s))

		resp := sendEditorMsg(t, editor, WSMessage{Type: "git_commit_message", RequestID: "ai-err"})
		if resp.Success || !strings.Contains(resp.Error, "boom") {
			t.Fatalf("reply = %+v, want error containing boom", resp)
		}
	})

	t.Run("nothing_staged", func(t *testing.T) {
		dir := newGitTestRepo(t)
		mock := llmtest.NewMockProvider()
		s, _ := newEditorAIServer(t, dir, mock)
		editor := dialEditor(t, startEditorTestServer(t, s))

		resp := sendEditorMsg(t, editor, WSMessage{Type: "git_commit_message", RequestID: "ai-empty"})
		if resp.Success || !strings.Contains(resp.Error, "nothing staged") {
			t.Fatalf("reply = %+v, want nothing-staged error", resp)
		}
		if mock.CallCount != 0 {
			t.Fatalf("provider called %d times with nothing staged, want 0", mock.CallCount)
		}
	})

	t.Run("provider_unconfigured", func(t *testing.T) {
		dir := newGitTestRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", "a.txt")

		mock := llmtest.NewMockProvider()
		s, _ := newEditorAIServer(t, dir, mock)
		// A real OpenAIProvider with no key/URL anywhere in the config.
		s.ws.ProviderFactory = func() llm.LLMProvider {
			return llm.NewOpenAIProvider("", "", "", dir)
		}
		editor := dialEditor(t, startEditorTestServer(t, s))

		resp := sendEditorMsg(t, editor, WSMessage{Type: "git_commit_message", RequestID: "ai-nocfg"})
		if resp.Success || !strings.Contains(resp.Error, "no LLM provider configured") {
			t.Fatalf("reply = %+v, want unconfigured-provider error", resp)
		}
	})

	t.Run("no_factory", func(t *testing.T) {
		dir := newGitTestRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", "a.txt")

		mock := llmtest.NewMockProvider()
		s, _ := newEditorAIServer(t, dir, mock)
		s.ws.ProviderFactory = nil
		editor := dialEditor(t, startEditorTestServer(t, s))

		resp := sendEditorMsg(t, editor, WSMessage{Type: "git_commit_message", RequestID: "ai-nofactory"})
		if resp.Success || !strings.Contains(resp.Error, "no LLM provider configured") {
			t.Fatalf("reply = %+v, want unconfigured-provider error", resp)
		}
	})
}

// TestGitCommitMessageTruncatesLargeDiff verifies a staged diff larger than
// the cap is truncated in the prompt (the provider never sees more than the
// cap + instruction).
func TestGitCommitMessageTruncatesLargeDiff(t *testing.T) {
	dir := newGitTestRepo(t)
	big := strings.Repeat("line of code\n", 4000) // ~60 KB
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "big.txt")

	mock := llmtest.NewMockProvider()
	s, _ := newEditorAIServer(t, dir, mock)
	editor := dialEditor(t, startEditorTestServer(t, s))

	resp := sendEditorMsg(t, editor, WSMessage{Type: "git_commit_message", RequestID: "ai-big"})
	if !resp.Success {
		t.Fatalf("reply = %+v, want success", resp)
	}
	msgs := mock.LastMessages
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v, want one", msgs)
	}
	max := gitCommitMessageDiffMaxBytes + len(gitCommitMessageInstruction) + 64
	if len(msgs[0].Content) > max {
		t.Fatalf("prompt is %d bytes, want <= %d (diff must be truncated)", len(msgs[0].Content), max)
	}
	if !strings.Contains(msgs[0].Content, "diff truncated") {
		t.Fatalf("prompt missing truncation marker:\n...%s", msgs[0].Content[len(msgs[0].Content)-80:])
	}
}

// TestGitCommitMessageDoesNotBlockReadLoop pins the off-the-read-loop
// behavior: while the one-shot LLM call is in flight (up to
// gitCommitMessageTimeout), the editor socket must still serve other
// messages promptly. Before the fix the read loop serialized the LLM call,
// so the git_status reply only arrived after the LLM returned.
func TestGitCommitMessageDoesNotBlockReadLoop(t *testing.T) {
	dir := newGitTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "a.txt")

	release := make(chan struct{})
	mock := llmtest.NewMockProvider()
	mock.OnGenerate = func(ctx context.Context, _ []llm.Message) (llm.Response, error) {
		select {
		case <-release:
			return llm.Response{Content: "feat: slow message"}, nil
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}
	s, _ := newEditorAIServer(t, dir, mock)
	editor := dialEditor(t, startEditorTestServer(t, s))

	if err := editor.WriteJSON(WSMessage{Type: "git_commit_message", RequestID: "ai-slow"}); err != nil {
		t.Fatalf("send git_commit_message: %v", err)
	}
	// While the LLM call is in flight, the read loop must still serve other
	// messages: git_status must reply promptly (the short deadline fails if
	// the LLM call is still serialized on the read loop).
	if err := editor.WriteJSON(WSMessage{Type: "git_status", RequestID: "st-1"}); err != nil {
		t.Fatalf("send git_status: %v", err)
	}
	_ = editor.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp WSMessage
	if err := editor.ReadJSON(&resp); err != nil {
		t.Fatalf("git_status reply while LLM in flight: %v (read loop blocked by git_commit_message?)", err)
	}
	if resp.Type != "git_status_result" || resp.RequestID != "st-1" {
		t.Fatalf("reply = %+v, want git_status_result/st-1", resp)
	}

	close(release)
	_ = editor.SetReadDeadline(time.Now().Add(15 * time.Second))
	if err := editor.ReadJSON(&resp); err != nil {
		t.Fatalf("read git_commit_message reply: %v", err)
	}
	if resp.Type != "git_commit_message_result" || resp.RequestID != "ai-slow" {
		t.Fatalf("reply = %+v, want git_commit_message_result/ai-slow", resp)
	}
	if !resp.Success || resp.Content != "feat: slow message" {
		t.Fatalf("reply = %+v, want success with the mock content", resp)
	}
}

// TestGitStagedDiffTruncationIsRuneSafe stages a single-line file of
// multi-byte runes sized so the byte cap lands mid-rune, and verifies the
// truncated diff is still valid UTF-8 (a raw byte cut would split the rune).
func TestGitStagedDiffTruncationIsRuneSafe(t *testing.T) {
	dir := newGitTestRepo(t)
	// Stage a one-line file first to learn the exact diff-header length for
	// a new single-line file (the "@@ -0,0 +1 @@" hunk line is fixed for one
	// line, so the header stays the same for any one-line content).
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "f.txt")
	header := gitIn(t, dir, "diff", "--cached")
	hl := strings.Index(header, "\n+")
	if hl < 0 {
		t.Fatalf("unexpected diff header: %q", header)
	}
	hl++ // include the trailing newline

	// Pick a rune width w such that the cap (offset k from the first rune)
	// does NOT land on a rune boundary.
	k := gitCommitMessageDiffMaxBytes - hl - 1 // bytes past the "+" before the cut
	var buf []byte
	var width int
	for _, w := range []int{2, 3, 4} {
		if k%w != 0 {
			width = w
			break
		}
	}
	if width == 0 {
		// k divisible by 2, 3 and 4: shift the cut one byte with an ASCII
		// prefix so a 3-byte rune is still split (k-1 is not divisible by 3).
		width = 3
		buf = append(buf, 'a')
		k--
	}
	runeCount := k/width + 1 // guarantees the line is longer than the cut

	for i := 0; i < runeCount; i++ {
		buf = append(buf, runeBytes(width)...)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), buf, 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "f.txt")

	s, _ := newEditorAIServer(t, dir, llmtest.NewMockProvider())
	diff, err := s.gitStagedDiff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "diff truncated") {
		t.Fatal("diff missing truncation marker")
	}
	if !utf8.ValidString(diff) {
		t.Fatalf("truncated diff is not valid UTF-8 (byte cut split a rune at offset %d)", gitCommitMessageDiffMaxBytes)
	}
}

// runeBytes returns a valid UTF-8 encoding of a rune of the given byte width
// (widths 2–4; Unicode has no 5/6-byte encodings).
func runeBytes(width int) []byte {
	switch width {
	case 2:
		return []byte{0xC3, 0xA9} // é
	case 3:
		return []byte{0xE2, 0x82, 0xAC} // €
	default:
		return []byte{0xF0, 0x9F, 0x98, 0x80} // 😀
	}
}

// TestGitCommitViaWSEditor exercises the composer's commit path: a valid
// message commits the staged changes, an empty message is rejected
// server-side, and the reply correlates by requestId.
func TestGitCommitViaWSEditor(t *testing.T) {
	dir := newGitTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "a.txt")

	mock := llmtest.NewMockProvider()
	s, _ := newEditorAIServer(t, dir, mock)
	editor := dialEditor(t, startEditorTestServer(t, s))

	// Empty message rejected without touching git.
	resp := sendEditorMsg(t, editor, WSMessage{Type: "git_commit", RequestID: "gc-empty", Content: "   "})
	if resp.Success || !strings.Contains(resp.Error, "commit message is required") {
		t.Fatalf("empty commit reply = %+v, want required-message error", resp)
	}

	resp = sendEditorMsg(t, editor, WSMessage{Type: "git_commit", RequestID: "gc-1", Content: "feat: first commit"})
	if !resp.Success || resp.RequestID != "gc-1" {
		t.Fatalf("commit reply = %+v, want success/gc-1", resp)
	}
	subject := gitIn(t, dir, "log", "-1", "--format=%s")
	if subject != "feat: first commit" {
		t.Fatalf("commit subject = %q, want %q", subject, "feat: first commit")
	}
}

// TestGitAIEditorMessageParity asserts the new editor message types are
// registered in BOTH the chat-socket wsHandlers map and the HandleWSEditor
// switch (a missing registration in either silently drops the request).
func TestGitAIEditorMessageParity(t *testing.T) {
	for _, typ := range []string{"git_commit_message", "git_commit"} {
		if _, ok := wsHandlers[typ]; !ok {
			t.Fatalf("%s missing from wsHandlers map", typ)
		}
	}
}
