//go:build unix

package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"

	"github.com/gorilla/websocket"
)

// TestWSUserShellClosedOnDisconnect pins the teardown half of the lazy spawn:
// the shell created by user_term_request (not at connect) is killed when the
// connection goes away. The pid comes from the shell itself ($$), so the check
// is on the real process rather than on a protocol message.
func TestWSUserShellClosedOnDisconnect(t *testing.T) {
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
	pump := startWSFramePump(conn)

	if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
		_ = conn.Close()
		t.Fatalf("send request: %v", err)
	}
	if _, ok := pump.waitFor("user_term_opened", 15*time.Second); !ok {
		_ = conn.Close()
		t.Fatalf("no user_term_opened; frames: %v", pump.types())
	}
	if err := conn.WriteJSON(WSMessage{Type: "user_term_input", Content: "echo shellpid=$$\n"}); err != nil {
		_ = conn.Close()
		t.Fatalf("send input: %v", err)
	}
	out, ok := pump.waitForOutput("shellpid=", 15*time.Second)
	if !ok {
		_ = conn.Close()
		t.Fatalf("shell pid never reached the client, output %q", out)
	}
	m := regexp.MustCompile(`shellpid=(\d+)`).FindStringSubmatch(out)
	if m == nil {
		_ = conn.Close()
		t.Fatalf("could not parse the shell pid out of %q", out)
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		_ = conn.Close()
		t.Fatalf("pid %q: %v", m[1], err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		_ = conn.Close()
		t.Fatalf("shell pid %d is not running while connected: %v", pid, err)
	}

	// Disconnect: the handler's teardown defer must close the holder, which
	// kills the shell's process group and releases the pty.
	_ = conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return // ESRCH: reaped, so the shell is really gone
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL) // do not leak it if the assertion fails
			t.Fatalf("lazily spawned shell pid %d survived the disconnect", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
