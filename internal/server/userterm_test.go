package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"

	"github.com/gorilla/websocket"
)

// collectOutput is a thread-safe sink for UserTerminal output.
type collectOutput struct {
	mu  sync.Mutex
	buf strings.Builder
	ch  chan struct{} // signaled on every write
}

func newCollectOutput() *collectOutput {
	return &collectOutput{ch: make(chan struct{}, 64)}
}

func (c *collectOutput) write(s string) {
	c.mu.Lock()
	c.buf.WriteString(s)
	c.mu.Unlock()
	select {
	case c.ch <- struct{}{}:
	default:
	}
}

func (c *collectOutput) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *collectOutput) waitContains(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if strings.Contains(c.String(), substr) {
			return
		}
		select {
		case <-c.ch:
		case <-deadline:
			t.Fatalf("timed out waiting for %q in output; got %q", substr, c.String())
		}
	}
}

// TestUserTerminalEchoAndExit spawns a real shell on a pty and verifies the
// full lifecycle: output streams, resize is accepted, exit is reported.
func TestUserTerminalEchoAndExit(t *testing.T) {
	// Pin the shell: $SHELL on dev machines (fish etc.) can be slow or
	// interactive-config heavy; /bin/sh is deterministic everywhere.
	t.Setenv("SHELL", "/bin/sh")
	out := newCollectOutput()
	ut, err := startUserTerminal(t.TempDir(), out.write)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer ut.Close()

	if ut.Title() == "" {
		t.Fatal("expected non-empty shell title")
	}
	if err := ut.Write([]byte("echo hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	out.waitContains(t, "hello", 10*time.Second)

	if err := ut.Resize(120, 40); err != nil {
		t.Fatalf("resize: %v", err)
	}

	if err := ut.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	select {
	case <-ut.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("shell did not exit after 'exit'")
	}
	if code := ut.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// Writes after exit must fail cleanly, not panic.
	if err := ut.Write([]byte("x")); err == nil {
		t.Fatal("expected error writing to exited shell")
	}
}

// TestWSUserTerminal drives the interactive user shell over a real WebSocket:
// the shell is spawned lazily on the first user_term_request, which answers
// user_term_opened → typing a command streams user_term_output → 'exit'
// produces user_term_exit with code 0.
func TestWSUserTerminal(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	exec := agent.NewExecutor(t.TempDir())
	a := agent.NewAgent(&termStreamStubProvider{}, exec, nil)
	s := newTestServer(t, a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:]+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 0) The server no longer spawns the shell at connect: ask for it.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
		t.Fatalf("send user_term_request: %v", err)
	}

	// 1) Expect user_term_opened in response to that request.
	deadline := time.Now().Add(15 * time.Second)
	opened := false
	for !opened {
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read (opened): %v", err)
		}
		switch msg.Type {
		case "user_term_opened":
			opened = true
			if msg.Content == "" {
				t.Fatal("user_term_opened without shell title")
			}
			if msg.WorkingDir == "" {
				t.Fatal("user_term_opened without working dir")
			}
		case "user_term_output", "config", "history", "response", "sessions":
			// handshake/banner noise — ignore
		case "user_term_exit":
			t.Fatalf("user shell failed to start: %q", msg.Content)
		}
	}

	// 2) Type a command; expect its output to stream back.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_input", Content: "echo hello\n"}); err != nil {
		t.Fatalf("send input: %v", err)
	}
	var outputs strings.Builder
	deadline = time.Now().Add(15 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read (output): %v", err)
		}
		switch msg.Type {
		case "user_term_output":
			outputs.WriteString(msg.Content)
			if strings.Contains(outputs.String(), "hello") {
				goto gotOutput
			}
		case "user_term_exit":
			t.Fatalf("shell exited early: %q", msg.Content)
		}
	}
gotOutput:

	// 3) Exit the shell; expect user_term_exit with code 0.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_input", Content: "exit\n"}); err != nil {
		t.Fatalf("send exit: %v", err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read (exit): %v", err)
		}
		if msg.Type == "user_term_exit" {
			if msg.Code != 0 {
				t.Fatalf("user_term_exit code = %d, want 0 (%q)", msg.Code, msg.Content)
			}
			return
		}
		// Trailing output (e.g. the echoed "exit") may arrive before the event.
		if msg.Type != "user_term_output" {
			t.Fatalf("unexpected message %q while waiting for user_term_exit", msg.Type)
		}
	}
}

// wsFramePump buffers every frame a test WebSocket receives in the background.
// Reading in a goroutine is what makes an ABSENCE assertion possible: a read
// deadline that expires poisons a gorilla connection ("repeated read on failed
// websocket connection"), so a test cannot simply time out a read and continue.
type wsFramePump struct {
	mu     sync.Mutex
	frames []WSMessage
}

func startWSFramePump(conn *websocket.Conn) *wsFramePump {
	p := &wsFramePump{}
	go func() {
		for {
			var m WSMessage
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			p.mu.Lock()
			p.frames = append(p.frames, m)
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *wsFramePump) snapshot() []WSMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]WSMessage(nil), p.frames...)
}

// types lists the frame types received so far (failure diagnostics).
func (p *wsFramePump) types() []string {
	var out []string
	for _, m := range p.snapshot() {
		out = append(out, m.Type)
	}
	return out
}

// terminalFrameTypes lists the user_term_* frames received so far.
func (p *wsFramePump) terminalFrameTypes() []string {
	var out []string
	for _, m := range p.snapshot() {
		if strings.HasPrefix(m.Type, "user_term_") {
			out = append(out, m.Type)
		}
	}
	return out
}

func (p *wsFramePump) count(typ string) int {
	n := 0
	for _, m := range p.snapshot() {
		if m.Type == typ {
			n++
		}
	}
	return n
}

// waitFor waits up to timeout for the first frame of type typ.
func (p *wsFramePump) waitFor(typ string, timeout time.Duration) (WSMessage, bool) {
	deadline := time.Now().Add(timeout)
	for {
		for _, m := range p.snapshot() {
			if m.Type == typ {
				return m, true
			}
		}
		if time.Now().After(deadline) {
			return WSMessage{}, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForOutput waits until the accumulated user_term_output frames contain
// substr, returning everything received so far.
func (p *wsFramePump) waitForOutput(substr string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		var b strings.Builder
		for _, m := range p.snapshot() {
			if m.Type == "user_term_output" {
				b.WriteString(m.Content)
			}
		}
		if strings.Contains(b.String(), substr) {
			return b.String(), true
		}
		if time.Now().After(deadline) {
			return b.String(), false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWSUserShellLazySpawn pins the lazy spawn (M4b): the shell is created on
// the first user_term_request, not at connect. A connection that never asks must
// create no pty — observable as "no user_term_* frame at all", since a /bin/sh
// started on a pty writes its prompt immediately — and must tolerate input and
// resize frames (empty holder). The first request spawns exactly one shell; two
// further requests reuse it instead of spawning another.
func TestWSUserShellLazySpawn(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	exec := agent.NewExecutor(t.TempDir())
	a := agent.NewAgent(&termStreamStubProvider{}, exec, nil)
	s := newTestServer(t, a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:]+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	pump := startWSFramePump(conn)

	// The handshake completes without a shell.
	if _, ok := pump.waitFor("config", 15*time.Second); !ok {
		t.Fatalf("no config frame; frames: %v", pump.types())
	}
	// Input/resize before any request are no-ops (empty holder) and must not
	// spawn a shell behind our back.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_input", Content: "echo should-not-run\n"}); err != nil {
		t.Fatalf("send input: %v", err)
	}
	if err := conn.WriteJSON(WSMessage{Type: "user_term_resize", Cols: 100, Rows: 30}); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	time.Sleep(750 * time.Millisecond)
	if got := pump.terminalFrameTypes(); len(got) != 0 {
		t.Fatalf("terminal frames without a user_term_request: %v (all: %v)", got, pump.types())
	}

	// First request: exactly one shell.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	opened, ok := pump.waitFor("user_term_opened", 15*time.Second)
	if !ok {
		t.Fatalf("no user_term_opened after the request; frames: %v", pump.types())
	}
	if opened.Content == "" || opened.WorkingDir == "" {
		t.Fatalf("user_term_opened = %+v, want shell title and working dir", opened)
	}
	// The lazily created shell is live: it answers input.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_input", Content: "echo lazy-shell-ok\n"}); err != nil {
		t.Fatalf("send input: %v", err)
	}
	if out, ok := pump.waitForOutput("lazy-shell-ok", 15*time.Second); !ok {
		t.Fatalf("lazily spawned shell produced no output, got %q", out)
	}
	// Two rapid requests must reuse the live shell (holder claim), not spawn a
	// second one.
	for i := 0; i < 2; i++ {
		if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
			t.Fatalf("send request %d: %v", i, err)
		}
	}
	time.Sleep(750 * time.Millisecond)
	if n := pump.count("user_term_opened"); n != 1 {
		t.Fatalf("user_term_opened frames = %d, want 1; frames: %v", n, pump.types())
	}
	if n := pump.count("user_term_exit"); n != 0 {
		t.Fatalf("unexpected user_term_exit; frames: %v", pump.types())
	}
}

// TestUserTermHolderClaimIsAtomic pins the claim spawnUserTerminal relies on to
// keep two concurrent user_term_requests from spawning two shells: of N racers
// exactly one wins trySet, the rest must close what they started.
func TestUserTermHolderClaimIsAtomic(t *testing.T) {
	t.Parallel()
	h := &userTermHolder{}
	const racers = 8
	var wg sync.WaitGroup
	won := make(chan *UserTerminal, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ut := &UserTerminal{}
			if h.trySet(ut) {
				won <- ut
			}
		}()
	}
	wg.Wait()
	close(won)
	if got := len(won); got != 1 {
		t.Fatalf("holders claimed = %d, want 1", got)
	}
	if h.get() == nil {
		t.Fatal("holder empty after a successful claim")
	}
}
