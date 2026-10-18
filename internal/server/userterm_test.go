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
// user_term_opened on connect → typing a command streams user_term_output →
// 'exit' produces user_term_exit with code 0.
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

	// 1) Expect user_term_opened right after connect.
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
