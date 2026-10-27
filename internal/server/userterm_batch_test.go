//go:build unix

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"

	"github.com/gorilla/websocket"
)

// stretchUserTermBatch widens the user terminal's coalescing window for one
// test and restores it afterwards. A wide window makes the batching
// OBSERVABLE instead of scheduling-dependent: with the production 16 ms a
// loaded box can deliver two pty reads more than 16 ms apart, which flushes
// them as separate frames and would make a frame-count assertion flaky.
func stretchUserTermBatch(t *testing.T) {
	t.Helper()
	prev := userTermOutputBatch
	userTermOutputBatch = 200 * time.Millisecond
	t.Cleanup(func() { userTermOutputBatch = prev })
}

// TestWSUserTerminalOutputBatched pins M4a: the interactive shell's output is
// coalesced before it reaches the socket, so a burst does not become one
// WebSocket frame per ~4 KB pty read.
//
// It runs a command that emits ~256 KB in one burst and exits immediately
// afterwards, then asserts, over the frames the client actually received:
//  1. the concatenation of every user_term_output payload contains the whole
//     emitted text, in order (batching must neither drop, duplicate nor
//     reorder a byte — containment is the exact assertion here because the
//     echoed command line and the prompt are part of the stream too),
//  2. the number of user_term_output frames is far below the number of pty
//     reads the payload needs (unbatched, it is one frame per read — a
//     deliberately tolerant bound, not an exact frame count), and
//  3. all of it arrived BEFORE user_term_exit: the flush on shell exit must
//     deliver the pending batch ahead of the exit report, or the tail of the
//     output is lost with the socket.
func TestWSUserTerminalOutputBatched(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	stretchUserTermBatch(t)

	dir := t.TempDir()
	// ~256 KB of unique, ordered lines: ~64 minimum pty reads at 4 KB each.
	var payload strings.Builder
	for i := 0; payload.Len() < 256<<10; i++ {
		fmt.Fprintf(&payload, "L%05d %s\n", i, strings.Repeat("x", 56))
	}
	if err := os.WriteFile(filepath.Join(dir, "burst.txt"), []byte(payload.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := agent.NewExecutor(dir)
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

	if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
		t.Fatalf("send user_term_request: %v", err)
	}
	if _, ok := pump.waitFor("user_term_opened", 20*time.Second); !ok {
		t.Fatalf("no user_term_opened; frames: %v", pump.types())
	}

	// One burst, then the shell exits: the tail of the output is written
	// immediately before the exit, which is what exercises the flush-on-exit.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_input", Content: "cat burst.txt; exit\n"}); err != nil {
		t.Fatalf("send input: %v", err)
	}
	if _, ok := pump.waitFor("user_term_exit", 60*time.Second); !ok {
		t.Fatalf("no user_term_exit; frames: %v", pump.types())
	}

	// Collect only the output frames received BEFORE the exit report.
	var got strings.Builder
	frames := 0
	for _, m := range pump.snapshot() {
		if m.Type == "user_term_exit" {
			break
		}
		if m.Type != "user_term_output" {
			continue
		}
		frames++
		got.WriteString(m.Content)
	}
	// The pty's ONLCR turns every \n into \r\n; drop the CRs so the comparison
	// is against the file's own bytes. ALL CRs, not just "the CR of a CRLF
	// pair": a terminal stream is CR-LF text, and macOS delivered a stray CR
	// (one doubled CR in 256 KB, at a pty buffer boundary — the browser's
	// terminal treats it as a no-op cursor return). What this test pins is
	// that batching neither drops, duplicates nor reorders a BURST byte; the
	// line-end encoding belongs to the pty, and an unexpected CR there is not
	// lost output. (Byte counts below are therefore reported post-normalization
	// only to size the failure message, never as a correctness signal.)
	text := strings.ReplaceAll(got.String(), "\r", "")
	if !strings.Contains(text, payload.String()) {
		t.Fatalf("received output does not contain the full burst in order (CR-normalized): got %d bytes, want at least %d; %s; frames: %v",
			len(text), payload.Len(), burstDivergence(text, payload.String()), pump.terminalFrameTypes())
	}

	// One pty read delivers at most 4096 bytes, so the payload needs at least
	// this many reads — and therefore at least this many frames without
	// batching. Any bound below it pins the coalescing.
	readsLowerBound := (len(payload.String()) + 4095) / 4096
	// Tolerant bound (never an exact frame count): well under half the
	// minimum read count, so it pins the coalescing while leaving room for
	// scheduling noise (the window is 200 ms, and the whole burst is far
	// quicker than that when the box is responsive).
	t.Logf("%d bytes of output arrived in %d frame(s); at least %d pty reads were needed (unbatched: >= %d frames)",
		payload.Len(), frames, readsLowerBound, readsLowerBound)
	if frames*2 >= readsLowerBound {
		t.Fatalf("user_term_output frames = %d, want well under the %d minimum pty reads (output is not being batched)",
			frames, readsLowerBound)
	}
}

// TestUserTerminalFlushOutputDeliversPendingBatch pins the explicit flush the
// two teardown paths rely on (spawnUserTerminal's exit goroutine and the
// connection teardown in ws_upgrade): with a batch window nothing will expire
// on its own, pending output reaches the sink ONLY through FlushOutput, so this
// fails if the flush ever stops being wired up.
func TestUserTerminalFlushOutputDeliversPendingBatch(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	prev := userTermOutputBatch
	userTermOutputBatch = time.Hour // nothing flushes on a timer during this test
	t.Cleanup(func() { userTermOutputBatch = prev })

	out := newCollectOutput()
	ut, err := startUserTerminal(t.TempDir(), out.write)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer ut.Close()

	if err := ut.Write([]byte("echo flush-marker\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Give the shell ample time to produce the output, then confirm the batch
	// window (an hour) has held it back.
	time.Sleep(750 * time.Millisecond)
	if strings.Contains(out.String(), "flush-marker") {
		t.Fatalf("output reached the sink without a flush: %q", out.String())
	}
	ut.FlushOutput()
	out.waitContains(t, "flush-marker", 10*time.Second)
}

// burstDivergence reports WHERE the received text stops matching the emitted
// burst, so a platform-specific failure (macOS losing the tail when the pty
// master was severed too early, a dropped chunk) says which byte and what the
// bytes around it were, instead of only a byte count.
func burstDivergence(text, want string) string {
	probe := want
	if len(probe) > 200 {
		probe = probe[:200]
	}
	start := strings.Index(text, probe)
	if start < 0 {
		return "the burst's first 200 bytes are not present at all"
	}
	i := 0
	for i < len(want) && start+i < len(text) && text[start+i] == want[i] {
		i++
	}
	lo := i - 20
	if lo < 0 {
		lo = 0
	}
	wantHi := i + 20
	if wantHi > len(want) {
		wantHi = len(want)
	}
	textHi := start + i + 20
	if textHi > len(text) {
		textHi = len(text)
	}
	origin := "truncated after"
	if start+i >= len(text) {
		origin = "text ended"
	}
	return fmt.Sprintf("burst starts at %d, %d of its bytes match (%s byte %d): want[%d:%d]=%q got[%d:%d]=%q",
		start, i, origin, i, lo, wantHi, want[lo:wantHi], start+lo, textHi, text[start+lo:textHi])
}
