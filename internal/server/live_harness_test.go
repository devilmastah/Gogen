package server

// Live harness server for the browser-level reproduction (scripts/
// live_paneswitch.js). Serves the full web app (static UI + /ws) with a
// provider that streams tokens for a few seconds per turn, so the jsdom
// harness can drive the REAL app.js against the REAL server and observe the
// sidebar session-list rows while a turn is running.
//
// Debug-only transport stall (candidate C, tmp/DEBUG_PLAN.md): the jsdom
// harness cannot create real TCP backpressure, so the write path can be
// stalled server-side via GOGEN_WS_SENDQ_SIZE / GOGEN_WS_STALL_MS /
// GOGEN_WS_STALL_AFTER_MS / GOGEN_WS_STALL_FOR_MS / GOGEN_WS_STALL_FIRST_CONN
// (see wsDebugConfigLoad in ws_conn_debug.go). These knobs only exist in
// debug builds, so run the harness with -tags debug. GOGEN_LIVE_CALL_MS
// overrides the stub turn length. Example — queue-full detach → headless
// completion → re-attach recovery (tmp/live_stall_detach.js):
//
//	GOGEN_LIVE_HARNESS=1 \
//	GOGEN_WS_SENDQ_SIZE=2 GOGEN_WS_STALL_MS=6000 \
//	GOGEN_WS_STALL_AFTER_MS=3000 GOGEN_WS_STALL_FOR_MS=7000 \
//	GOGEN_LIVE_CALL_MS=25000 \
//	go test -tags debug ./internal/server -run TestLiveHarnessServer -v &
//	sleep 3 && node tmp/live_stall_detach.js
//
// The stall must exceed enqueueJSON's 5s queue-full timeout: with the tiny
// queue plus a 6s-per-write stall, the first blocked send times out, the
// socket is detached, and the turn keeps running headless (see the script's
// header for the full walkthrough).
//
// Run: go test ./internal/server -run TestLiveHarnessServer -v &
// The port is written to /tmp/gogen-live-port.txt.

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// liveStreamStub streams tokens for a bounded duration per call, then
// returns a completed result — realistic enough to keep the client's stream
// rendering and turn machinery exercised end to end.
type liveStreamStub struct {
	mu      sync.Mutex
	callLen time.Duration
}

func (s *liveStreamStub) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *liveStreamStub) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	s.mu.Lock()
	length := s.callLen
	s.mu.Unlock()
	if length <= 0 {
		length = 2500 * time.Millisecond
	}
	if h.OnStart != nil {
		h.OnStart()
	}
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	var content strings.Builder
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(length)
	defer deadline.Stop()
	words := []string{"alpha ", "beta ", "gamma ", "delta ", "epsilon ", "zeta ", "eta ", "theta "}
	i := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			if h.OnStreamEnd != nil {
				h.OnStreamEnd()
			}
			return &llm.StreamResult{Content: content.String()}, nil
		case <-tick.C:
			w := words[i%len(words)]
			i++
			if h.OnThinkingToken != nil {
				h.OnThinkingToken("(reasoning " + w + ")")
			}
			if h.OnToken != nil {
				h.OnToken(w)
			}
			content.WriteString(w)
		}
	}
}

func (s *liveStreamStub) ModelContextLimit(_ context.Context) (int, error) { return 1000, nil }
func (s *liveStreamStub) SetThinkingLevel(string)                          {}
func (s *liveStreamStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *liveStreamStub) SetModel(string) error { return nil }
func (s *liveStreamStub) ModelName() string     { return "live-model" }

func TestLiveHarnessServer(t *testing.T) {
	if os.Getenv("GOGEN_LIVE_HARNESS") == "" {
		t.Skip("set GOGEN_LIVE_HARNESS=1 to run the live harness server")
	}
	dir := t.TempDir()
	callLen := 15 * time.Second
	if v := os.Getenv("GOGEN_LIVE_CALL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			callLen = time.Duration(ms) * time.Millisecond
		}
	}
	stub := &liveStreamStub{callLen: callLen}
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	// Pre-load the default session with a large history so the attach
	// snapshot (deep clone under statsMu) is slow enough for the in-flight
	// turn's stream events to interleave ahead of the attach's history —
	// the user-visible "reply stops" case.
	if os.Getenv("GOGEN_LIVE_BIG_SESSION") != "" {
		var msgs []llm.Message
		for i := 0; i < 400; i++ {
			msgs = append(msgs, llm.Message{
				Role:      "user",
				Content:   fmt.Sprintf("question number %d in a long history", i),
				CreatedAt: time.Now().Add(-time.Duration(400-i) * time.Minute).Truncate(time.Millisecond),
			}, llm.Message{
				Role:      "assistant",
				Content:   fmt.Sprintf("answer number %d padding padding padding padding padding", i),
				CreatedAt: time.Now().Add(-time.Duration(400-i) * time.Minute).Truncate(time.Millisecond),
			})
		}
		a.RestoreSessionLocal(agent.SessionSnapshot{Messages: msgs}, a.SessionID)
	}
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	s := newTestServer(t, a, &config.Config{})

	// Bind a fixed port so the jsdom harness can connect.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	portFile := "/tmp/gogen-live-port.txt"
	if err := os.WriteFile(portFile, []byte(addr), 0o644); err != nil {
		t.Fatalf("write port file: %v", err)
	}
	t.Logf("live harness on http://%s (port file %s)", addr, portFile)
	if err := s.Start(context.Background(), addr); err != nil {
		t.Fatalf("server: %v", err)
	}
}
