package statusdisplay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
)

// stubProvider is the minimal llm.LLMProvider needed to build an agent for
// the context-stats test.
type stubProvider struct{}

func (stubProvider) GenerateResponse(context.Context, []llm.Message, map[string]struct{}, []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}

func (stubProvider) GenerateResponseStream(context.Context, []llm.Message, map[string]struct{}, []llm.Tool, *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (stubProvider) ModelContextLimit(context.Context) (int, error) { return 0, nil }
func (stubProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (stubProvider) SetModel(string) error   { return nil }
func (stubProvider) ModelName() string       { return "stub" }
func (stubProvider) SetThinkingLevel(string) {}

// resetForTest disables the reporter around a test and clears the
// process-stable auto row name (each test acts as a fresh process). The
// shared worker goroutine (and its dedupe state) is left running.
func resetForTest(t *testing.T) {
	t.Helper()
	Configure(false, "", "", "", "")
	mu.Lock()
	autoSource = ""
	mu.Unlock()
	t.Cleanup(func() {
		Configure(false, "", "", "", "")
		mu.Lock()
		autoSource = ""
		mu.Unlock()
	})
}

type recordedReq struct {
	method string
	path   string
	query  string
	token  string
	body   string
}

func newRecordingServer(t *testing.T) (*httptest.Server, *sync.Mutex, *[]recordedReq) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recordedReq{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			token:  r.Header.Get("X-Status-Token"),
			body:   string(body),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &mu, &reqs
}

// waitForReqs waits until at least n requests were recorded.
func waitForReqs(t *testing.T, mu *sync.Mutex, reqs *[]recordedReq, n int) {
	t.Helper()
	waitForReqsWithin(t, mu, reqs, n, 3*time.Second)
}

// waitForReqsWithin is waitForReqs with an explicit timeout (the liveness
// ticker test must tolerate a ticker that is mid-sleep from an earlier
// test's interval).
func waitForReqsWithin(t *testing.T, mu *sync.Mutex, reqs *[]recordedReq, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(*reqs)
		mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	got := len(*reqs)
	mu.Unlock()
	t.Fatalf("requests = %d, want at least %d", got, n)
}

func reqAt(t *testing.T, mu *sync.Mutex, reqs *[]recordedReq, i int) recordedReq {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if i >= len(*reqs) {
		t.Fatalf("request %d missing (have %d)", i, len(*reqs))
	}
	return (*reqs)[i]
}

func countReqs(mu *sync.Mutex, reqs *[]recordedReq) int {
	mu.Lock()
	defer mu.Unlock()
	return len(*reqs)
}

// TestWorkerPostsUpdatesAndClears drives the whole client against a fake
// display: an update carries the token header and the running row, the turn
// end flips it to "done", and disabling clears the row with DELETE.
func TestWorkerPostsUpdatesAndClears(t *testing.T) {
	srv, mu, reqs := newRecordingServer(t)
	resetForTest(t)

	Configure(true, srv.URL, "s3cret", "gogen-test", "")
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStarted})
	waitForReqs(t, mu, reqs, 1)

	first := reqAt(t, mu, reqs, 0)
	if first.method != http.MethodPost || first.path != "/status" {
		t.Fatalf("first request = %s %s, want POST /status", first.method, first.path)
	}
	if first.token != "s3cret" {
		t.Fatalf("token header = %q, want s3cret", first.token)
	}
	var row statusRow
	if err := json.Unmarshal([]byte(first.body), &row); err != nil {
		t.Fatalf("unmarshal row: %v (body %q)", err, first.body)
	}
	if row.Source != "gogen-test" || row.State != "running" || row.Message == "" || row.TTL != runningTTLSeconds {
		t.Fatalf("running row = %+v", row)
	}

	Notify(agent.TurnEvent{Phase: agent.TurnPhaseEnded})
	waitForReqs(t, mu, reqs, 2)
	second := reqAt(t, mu, reqs, 1)
	if err := json.Unmarshal([]byte(second.body), &row); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	if row.State != "done" || !strings.HasPrefix(row.Message, "done") || row.TTL != endedTTLSeconds {
		t.Fatalf("done row = %+v", row)
	}

	Configure(false, srv.URL, "s3cret", "gogen-test", "")
	waitForReqs(t, mu, reqs, 3)
	clear := reqAt(t, mu, reqs, 2)
	if clear.method != http.MethodDelete || clear.path != "/status" || clear.query != "source=gogen-test" {
		t.Fatalf("clear = %s %s?%s, want DELETE /status?source=gogen-test", clear.method, clear.path, clear.query)
	}
	if clear.token != "s3cret" {
		t.Fatalf("clear token header = %q, want s3cret", clear.token)
	}
}

// TestWorkerDedupesIdenticalRows pins the coalescing: a repeated identical
// event (e.g. the provider reporting stalls) must not re-POST the same row.
func TestWorkerDedupesIdenticalRows(t *testing.T) {
	srv, mu, reqs := newRecordingServer(t)
	resetForTest(t)

	Configure(true, srv.URL, "", "gogen-dedupe", "")
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStalled})
	waitForReqs(t, mu, reqs, 1)
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStalled})
	time.Sleep(150 * time.Millisecond)
	if got := countReqs(mu, reqs); got != 1 {
		t.Fatalf("requests = %d, want 1 (identical row deduped)", got)
	}
}

// TestWorkerRefreshesWhileTurnRuns pins the liveness ticker: while a turn is
// running the row is re-sent on its own with the elapsed time advancing,
// even though no new agent event arrives (a long model prefill otherwise
// leaves a static "thinking…" on the screen). Ending the turn stops it.
func TestWorkerRefreshesWhileTurnRuns(t *testing.T) {
	srv, reqMu, reqs := newRecordingServer(t)
	resetForTest(t)

	// refreshInterval and turnStart are package state guarded by the
	// package mutex (named reqMu's sibling: the local reqMu is the fake
	// server's).
	mu.Lock()
	old := refreshInterval
	refreshInterval = 50 * time.Millisecond
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		refreshInterval = old
		mu.Unlock()
	})

	Configure(true, srv.URL, "", "gogen-refresh", "")
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStarted})
	waitForReqsWithin(t, reqMu, reqs, 1, 8*time.Second)

	// Backdate the turn so the next refresh carries a visible elapsed
	// suffix; the first second of a turn has none, and an unchanged row is
	// deduped (no request).
	mu.Lock()
	turnStart = time.Now().Add(-2 * time.Minute)
	mu.Unlock()

	waitForReqsWithin(t, reqMu, reqs, 2, 8*time.Second)
	if body := reqAt(t, reqMu, reqs, 1).body; !strings.Contains(body, "2m") {
		t.Fatalf("refresh body = %q, want an elapsed suffix", body)
	}

	// Ending the turn stops the refreshes. Let the ended row (and any
	// in-flight tick) land, then assert the request count is stable and the
	// last row is the outcome.
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseEnded})
	time.Sleep(150 * time.Millisecond)
	settled := countReqs(reqMu, reqs)
	time.Sleep(250 * time.Millisecond)
	if got := countReqs(reqMu, reqs); got != settled {
		t.Fatalf("requests grew %d -> %d after the turn ended", settled, got)
	}
	if last := reqAt(t, reqMu, reqs, settled-1); !strings.Contains(last.body, "\"done\"") {
		t.Fatalf("last body = %q, want the ended row", last.body)
	}
}

// TestNotifyIgnoresSubagentTurns pins that nested turns do not drive the
// display (a child session would otherwise flap the single top-level row).
func TestNotifyIgnoresSubagentTurns(t *testing.T) {
	srv, mu, reqs := newRecordingServer(t)
	resetForTest(t)

	Configure(true, srv.URL, "", "gogen-sub", "")
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStarted, Depth: 1})
	time.Sleep(150 * time.Millisecond)
	if got := countReqs(mu, reqs); got != 0 {
		t.Fatalf("requests = %d, want 0 for a subagent turn", got)
	}
}

// TestConfigureFallsBackToDefaults pins the empty-value normalization: an
// enabled reporter with no host uses the firmware's mDNS default, and an
// empty source with no known directory falls back to the default row name.
func TestConfigureFallsBackToDefaults(t *testing.T) {
	resetForTest(t)
	Configure(true, "   ", "", "  ", "")

	mu.Lock()
	got := cfg
	mu.Unlock()
	if got.Host != DefaultHost {
		t.Fatalf("host = %q, want %q", got.Host, DefaultHost)
	}
	if got.Source != DefaultSource {
		t.Fatalf("source = %q, want %q", got.Source, DefaultSource)
	}
	if !got.Enabled {
		t.Fatal("enabled = false, want true")
	}
	Configure(false, "", "", "", "")
}

// TestConfigureAutoSource pins the multi-instance row naming: an empty
// source derives "gogen-<project dir>" (stable across later Configure calls
// with a different directory), and an explicit source always wins.
func TestConfigureAutoSource(t *testing.T) {
	resetForTest(t)

	Configure(true, "host", "", "", "/home/gompa/workspace/ttgo")
	mu.Lock()
	got := cfg
	mu.Unlock()
	if got.Source != "gogen-ttgo" {
		t.Fatalf("auto source = %q, want gogen-ttgo", got.Source)
	}
	if got.workDir != "/home/gompa/workspace/ttgo" {
		t.Fatalf("workDir = %q, want the configured directory", got.workDir)
	}

	// A later Configure with another directory (e.g. a web workspace switch)
	// must NOT rename the live row.
	Configure(true, "host", "", "", "/home/gompa/workspace/Gogen")
	mu.Lock()
	got = cfg
	mu.Unlock()
	if got.Source != "gogen-ttgo" {
		t.Fatalf("source after dir change = %q, want the derived gogen-ttgo", got.Source)
	}

	// Explicit source wins over the auto name.
	Configure(true, "host", "", "gogen-web", "/home/gompa/workspace/Gogen")
	mu.Lock()
	got = cfg
	mu.Unlock()
	if got.Source != "gogen-web" {
		t.Fatalf("explicit source = %q, want gogen-web", got.Source)
	}

	// Clearing it re-derives from the current directory.
	Configure(true, "host", "", "", "/home/gompa/workspace/Gogen")
	mu.Lock()
	got = cfg
	mu.Unlock()
	if got.Source != "gogen-Gogen" {
		t.Fatalf("re-derived source = %q, want gogen-Gogen", got.Source)
	}
	Configure(false, "", "", "", "")
}

// TestSourceForDir pins the auto row-name derivation.
func TestSourceForDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{".", ""},
		{"/", ""},
		{"/home/gompa/workspace/ttgo", "gogen-ttgo"},
		{"/home/gompa/workspace/ttgo/", "gogen-ttgo"},
		{"/home/gompa/workspace/my project", "gogen-my-project"},
		{"/home/gompa/workspace/café", "gogen-caf??"},
	}
	for _, tc := range cases {
		if got := sourceForDir(tc.in); got != tc.want {
			t.Fatalf("sourceForDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// newProbeServer serves a GET /info response for the auto-probe tests.
func newProbeServer(t *testing.T, info string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, info)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitForEnabled polls the package config until Enabled matches want.
func waitForEnabled(t *testing.T, want bool) Config {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := cfg
		mu.Unlock()
		if got.Enabled == want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	got := cfg
	mu.Unlock()
	t.Fatalf("cfg.Enabled = %v, want %v (host %q)", got.Enabled, want, got.Host)
	return got
}

// TestSinkRelaysRowsAndClears pins browser mode's transport: with a sink
// installed the reporter hands the built row (and the clear) to it instead of
// the HTTP client, so the web server can relay them to the browser — which
// may be on the display's network when the server is not.
// relayed is one recorded sink call (see TestSinkRelaysRowsAndClears).
type relayed struct {
	row    string
	clear  bool
	source string
}

func TestSinkRelaysRowsAndClears(t *testing.T) {
	resetForTest(t)

	var mu sync.Mutex
	var got []relayed
	SetSink(func(row []byte, clear bool, source string) {
		mu.Lock()
		got = append(got, relayed{row: string(row), clear: clear, source: source})
		mu.Unlock()
	})
	t.Cleanup(func() { SetSink(nil) })

	// An unreachable host proves no HTTP is attempted: the sink is the only
	// transport while installed.
	Configure(true, "unreachable.invalid", "", "", "/home/gompa/workspace/ttgo")
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStarted})
	first := waitForRelayed(t, &mu, &got, 1)[0]
	if first.clear || first.source != "gogen-ttgo" {
		t.Fatalf("first relay = %+v, want a row for gogen-ttgo", first)
	}
	if !strings.Contains(first.row, `"source":"gogen-ttgo"`) || !strings.Contains(first.row, `"running"`) {
		t.Fatalf("row = %q, want the running gogen-ttgo row", first.row)
	}

	Notify(agent.TurnEvent{Phase: agent.TurnPhaseEnded})
	second := waitForRelayed(t, &mu, &got, 2)[1]
	if second.clear || !strings.Contains(second.row, `"done"`) {
		t.Fatalf("second relay = %+v, want the done row", second)
	}

	// Disabling relays a clear so the browser deletes the row.
	Configure(false, "unreachable.invalid", "", "", "/home/gompa/workspace/ttgo")
	third := waitForRelayed(t, &mu, &got, 3)[2]
	if !third.clear || third.source != "gogen-ttgo" {
		t.Fatalf("third relay = %+v, want a clear for gogen-ttgo", third)
	}
}

// TestProcessKeepsTheQueueTimeTransport pins the transport contract that made
// TestSinkRelaysRowsAndClears flaky: an operation carries the relay that was
// installed when it was QUEUED, so a sink installed afterwards (a test's fresh
// sink, or ProbeWithRelay upgrading the browser relay to the server transport)
// only ever receives operations queued for it. Replaying a queued clear through
// the new sink handed one test the previous test's clear ("first relay =
// {clear:true source:gogen-Gogen}").
func TestProcessKeepsTheQueueTimeTransport(t *testing.T) {
	resetForTest(t)
	srv, rmu, reqs := newRecordingServer(t)

	var smu sync.Mutex
	var sinkCalls []relayed
	SetSink(func(row []byte, clear bool, source string) {
		smu.Lock()
		sinkCalls = append(sinkCalls, relayed{row: string(row), clear: clear, source: source})
		smu.Unlock()
	})
	t.Cleanup(func() { SetSink(nil) })

	// An op queued while the HTTP transport was active (relay nil).
	process(op{clear: true, cfg: Config{Enabled: true, Host: srv.URL, Source: "stale-row"}})

	waitForReqs(t, rmu, reqs, 1)
	rmu.Lock()
	var deletes int
	for _, r := range *reqs {
		if r.method == http.MethodDelete {
			deletes++
		}
	}
	rmu.Unlock()
	if deletes != 1 {
		t.Fatalf("display DELETEs = %d, want 1 (the clear must go over HTTP, its queue-time transport)", deletes)
	}
	smu.Lock()
	n := len(sinkCalls)
	smu.Unlock()
	if n != 0 {
		t.Fatalf("the newly installed sink received %d call(s) from an op queued before it: %+v", n, sinkCalls)
	}
}

// waitForRelayed waits until n sink calls were recorded.
func waitForRelayed(t *testing.T, mu *sync.Mutex, got *[]relayed, n int) []relayed {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		if len(*got) >= n {
			out := append([]relayed(nil), (*got)[:n]...)
			mu.Unlock()
			return out
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	have := len(*got)
	mu.Unlock()
	t.Fatalf("sink calls = %d, want at least %d", have, n)
	return nil
}

// TestProbeEnablesWhenFound pins the `status_display: auto` path: the probe
// hits GET /info and enables reporting (with the auto row name) when the
// firmware answers.
func TestProbeEnablesWhenFound(t *testing.T) {
	resetForTest(t)
	srv := newProbeServer(t, `{"hostname":"statusscreen","api_version":"1.0"}`, http.StatusOK)

	Probe(srv.URL, "", "", "/home/gompa/workspace/ttgo")
	got := waitForEnabled(t, true)
	if got.Host != srv.URL {
		t.Fatalf("host = %q, want %q", got.Host, srv.URL)
	}
	if got.Source != "gogen-ttgo" {
		t.Fatalf("source = %q, want the derived gogen-ttgo", got.Source)
	}
	Configure(false, "", "", "", "")
}

// TestProbeStaysOffWhenAbsent pins that an unreachable or non-status-screen
// host leaves the feature disabled: auto costs nothing when there is no
// display.
func TestProbeStaysOffWhenAbsent(t *testing.T) {
	resetForTest(t)

	// 404 on /info: some other server on the configured host.
	srv := newProbeServer(t, "nope", http.StatusNotFound)
	Probe(srv.URL, "", "", "/home/gompa/workspace/ttgo")
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	got := cfg
	mu.Unlock()
	if got.Enabled {
		t.Fatal("cfg.Enabled = true after a 404 probe")
	}

	// 200 but not the status-screen JSON (no api_version).
	srv2 := newProbeServer(t, `{"hello":"world"}`, http.StatusOK)
	Probe(srv2.URL, "", "", "/home/gompa/workspace/ttgo")
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	got = cfg
	mu.Unlock()
	if got.Enabled {
		t.Fatal("cfg.Enabled = true for a non-status-screen JSON")
	}
}

// waitForTransport polls the package transport until relay is (not) the
// active sender.
func waitForTransport(t *testing.T, wantRelay bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		active := sink != nil
		mu.Unlock()
		if active == wantRelay {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("transport relay=%v not reached", wantRelay)
}

// TestProbeWithRelayPrefersServer pins the browser-mode precedence: when the
// server itself can reach the display, the server sends the rows (the relay
// is not used) even though a relay is installed for the fallback.
func TestProbeWithRelayPrefersServer(t *testing.T) {
	resetForTest(t)
	t.Cleanup(func() { SetSink(nil) })

	var mu sync.Mutex
	statusPosts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			_, _ = io.WriteString(w, `{"hostname":"statusscreen","api_version":"1.0"}`)
		case "/status":
			mu.Lock()
			statusPosts++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	var relayCalls int32
	ProbeWithRelay(srv.URL, "", "", "/home/gompa/workspace/ttgo", func([]byte, bool, string) {
		atomic.AddInt32(&relayCalls, 1)
	})

	// The successful probe upgrades the transport to the server.
	waitForTransport(t, false)
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStarted})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		posts := statusPosts
		mu.Unlock()
		if posts >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	posts := statusPosts
	mu.Unlock()
	if posts < 1 {
		t.Fatal("server did not POST the row to the reachable display")
	}
	if got := atomic.LoadInt32(&relayCalls); got != 0 {
		t.Fatalf("relay calls = %d, want 0 while the server can reach the display", got)
	}
}

// TestProbeWithRelayFallsBackToRelay pins the fallback: when the server
// cannot reach the display, the relay carries the rows (the browser sends).
func TestProbeWithRelayFallsBackToRelay(t *testing.T) {
	resetForTest(t)
	t.Cleanup(func() { SetSink(nil) })

	var mu sync.Mutex
	statusPosts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			mu.Lock()
			statusPosts++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNotFound) // /info: not a status screen
	}))
	t.Cleanup(srv.Close)

	var relayCalls int32
	ProbeWithRelay(srv.URL, "", "", "/home/gompa/workspace/ttgo", func([]byte, bool, string) {
		atomic.AddInt32(&relayCalls, 1)
	})

	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStarted})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&relayCalls) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&relayCalls); got < 1 {
		t.Fatalf("relay calls = %d, want the fallback relay to carry the row", got)
	}
	mu.Lock()
	posts := statusPosts
	mu.Unlock()
	if posts != 0 {
		t.Fatalf("server POSTs = %d, want 0 for an unreachable display", posts)
	}
}

// TestBuildRowProjectTag pins the title's project tag: the panel shows
// "Gogen@<dir>: <label>", which is how concurrent instances are told apart.
func TestBuildRowProjectTag(t *testing.T) {
	body, err := buildRow(agent.TurnEvent{Phase: agent.TurnPhaseStarted, Label: "fix parser"},
		Config{Source: "gogen-ttgo", workDir: "/home/gompa/workspace/ttgo"}, 0)
	if err != nil {
		t.Fatalf("buildRow: %v", err)
	}
	var row statusRow
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if row.Title != "Gogen@ttgo: fix parser" {
		t.Fatalf("title = %q, want Gogen@ttgo: fix parser", row.Title)
	}

	// Unknown directory: no tag.
	body, err = buildRow(agent.TurnEvent{Phase: agent.TurnPhaseStarted, Label: "fix parser"},
		Config{Source: "gogen", workDir: ""}, 0)
	if err != nil {
		t.Fatalf("buildRow: %v", err)
	}
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if row.Title != "Gogen: fix parser" {
		t.Fatalf("title = %q, want Gogen: fix parser", row.Title)
	}
}

// TestBuildRowPhases pins the phase → row mapping (state, message, TTL) with
// no agent attached (no context bar).
func TestBuildRowPhases(t *testing.T) {
	cases := []struct {
		name        string
		ev          agent.TurnEvent
		wantState   string
		wantMessage string
		wantTTL     int
	}{
		{
			name:        "started",
			ev:          agent.TurnEvent{Phase: agent.TurnPhaseStarted},
			wantState:   "running",
			wantMessage: "thinking...",
			wantTTL:     runningTTLSeconds,
		},
		{
			name:        "tool",
			ev:          agent.TurnEvent{Phase: agent.TurnPhaseTool, Tool: "patch_file"},
			wantState:   "running",
			wantMessage: "patch_file",
			wantTTL:     runningTTLSeconds,
		},
		{
			name:        "compacting",
			ev:          agent.TurnEvent{Phase: agent.TurnPhaseCompacting},
			wantState:   "running",
			wantMessage: "compacting...",
			wantTTL:     runningTTLSeconds,
		},
		{
			name:        "stalled",
			ev:          agent.TurnEvent{Phase: agent.TurnPhaseStalled},
			wantState:   "running",
			wantMessage: "waiting...",
			wantTTL:     runningTTLSeconds,
		},
		{
			name:        "retry collapses newlines",
			ev:          agent.TurnEvent{Phase: agent.TurnPhaseRetry, Detail: "stream interrupted:\nconnection reset"},
			wantState:   "running",
			wantMessage: "retrying: stream interrupted: connection reset",
			wantTTL:     runningTTLSeconds,
		},
		{
			name:        "done",
			ev:          agent.TurnEvent{Phase: agent.TurnPhaseEnded},
			wantState:   "done",
			wantMessage: "done",
			wantTTL:     endedTTLSeconds,
		},
		{
			name:        "error",
			ev:          agent.TurnEvent{Phase: agent.TurnPhaseEnded, Err: errString("boom")},
			wantState:   "error",
			wantMessage: "error: boom",
			wantTTL:     endedTTLSeconds,
		},
		{
			name:        "cancelled",
			ev:          agent.TurnEvent{Phase: agent.TurnPhaseEnded, Err: context.Canceled, Cancelled: true},
			wantState:   "idle",
			wantMessage: "cancelled",
			wantTTL:     endedTTLSeconds,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := buildRow(tc.ev, Config{Source: "gogen"}, 0)
			if err != nil {
				t.Fatalf("buildRow: %v", err)
			}
			var row statusRow
			if err := json.Unmarshal(body, &row); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if row.State != tc.wantState || row.Message != tc.wantMessage || row.TTL != tc.wantTTL {
				t.Fatalf("row = state %q msg %q ttl %d, want %q %q %d",
					row.State, row.Message, row.TTL, tc.wantState, tc.wantMessage, tc.wantTTL)
			}
			if row.Progress != -1 {
				t.Fatalf("progress = %v, want -1 (no agent / unresolved window)", row.Progress)
			}
			if row.Title != "Gogen" {
				t.Fatalf("title = %q, want Gogen", row.Title)
			}
		})
	}
}

// TestBuildRowLabelAndAscii pins the two rendering constraints: the title
// carries the session label on the display's FIRST label line ("Gogen:
// <label>", two-line mode) and every display-bound string is printable ASCII
// (the firmware's fonts have no glyph above 0x7E, so "…" and "·" rendered as
// several junk glyphs — review F9 on the display side).
func TestBuildRowLabelAndAscii(t *testing.T) {
	label := "café · " + strings.Repeat("x", 80) + " …"
	body, err := buildRow(agent.TurnEvent{Phase: agent.TurnPhaseStarted, Label: label}, Config{Source: "gogen"}, 0)
	if err != nil {
		t.Fatalf("buildRow: %v", err)
	}
	var row statusRow
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The non-ASCII bytes are replaced one-for-one ('?' per UTF-8 byte).
	if !strings.HasPrefix(row.Title, "Gogen: caf?? ?? ") {
		t.Fatalf("title = %q, want the sanitized Gogen: <label> prefix", row.Title)
	}
	for i := 0; i < len(row.Title); i++ {
		if row.Title[i] < 0x20 || row.Title[i] > 0x7e {
			t.Fatalf("title %q has a non-ASCII byte at %d", row.Title, i)
		}
	}
	if row.Message != "thinking..." {
		t.Fatalf("message = %q, want the ASCII activity", row.Message)
	}
	if n := len([]rune(row.Title)); n > titleMax {
		t.Fatalf("title runes = %d, want <= %d", n, titleMax)
	}
	if n := len([]rune(row.Message)); n > msgMax {
		t.Fatalf("message runes = %d, want <= %d", n, msgMax)
	}

	// A unicode error message must be sanitized too.
	body, err = buildRow(agent.TurnEvent{Phase: agent.TurnPhaseEnded, Err: errString("déjà vu — “boom”")}, Config{Source: "gogen"}, 0)
	if err != nil {
		t.Fatalf("buildRow: %v", err)
	}
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i := 0; i < len(row.Message); i++ {
		if row.Message[i] < 0x20 || row.Message[i] > 0x7e {
			t.Fatalf("message %q has a non-ASCII byte at %d", row.Message, i)
		}
	}
	if !strings.HasPrefix(row.Message, "error: ") {
		t.Fatalf("message = %q, want the error prefix", row.Message)
	}
}

// TestBuildRowElapsed pins the elapsed-time suffix: present while a turn
// runs (the ticker's visible liveness), reported as the turn duration on a
// clean finish, and omitted for the first sub-second row.
func TestBuildRowElapsed(t *testing.T) {
	cases := []struct {
		name    string
		ev      agent.TurnEvent
		elapsed time.Duration
		want    string
	}{
		{
			name:    "running with elapsed",
			ev:      agent.TurnEvent{Phase: agent.TurnPhaseTool, Tool: "patch_file"},
			elapsed: 2*time.Minute + 3*time.Second,
			want:    "patch_file 2m03s",
		},
		{
			name:    "done reports the turn duration",
			ev:      agent.TurnEvent{Phase: agent.TurnPhaseEnded},
			elapsed: 65 * time.Second,
			want:    "done 1m05s",
		},
		{
			name:    "sub-second omits the suffix",
			ev:      agent.TurnEvent{Phase: agent.TurnPhaseStarted},
			elapsed: 400 * time.Millisecond,
			want:    "thinking...",
		},
		{
			name:    "hours",
			ev:      agent.TurnEvent{Phase: agent.TurnPhaseStarted},
			elapsed: 2*time.Hour + 5*time.Minute,
			want:    "thinking... 2h05m",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := buildRow(tc.ev, Config{Source: "gogen"}, tc.elapsed)
			if err != nil {
				t.Fatalf("buildRow: %v", err)
			}
			var row statusRow
			if err := json.Unmarshal(body, &row); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if row.Message != tc.want {
				t.Fatalf("message = %q, want %q", row.Message, tc.want)
			}
		})
	}
}

// TestBuildRowContextProgress attaches a real agent with a known context
// limit and asserts the context usage wires through as the bar's progress
// (the message deliberately carries no token counts: the bar percentage is
// the context indicator and the label budget is tiny).
func TestBuildRowContextProgress(t *testing.T) {
	p := stubProvider{}
	mgr := contextmgr.NewManager(p, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(p, &agent.Executor{WorkingDir: "."}, mgr)
	a.Messages = []llm.Message{{Role: "user", Content: "hello world, this is a context test"}}

	body, err := buildRow(agent.TurnEvent{Phase: agent.TurnPhaseStarted, Agent: a}, Config{Source: "gogen"}, 0)
	if err != nil {
		t.Fatalf("buildRow: %v", err)
	}
	var row statusRow
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if row.Progress <= 0 || row.Progress > 1 {
		t.Fatalf("progress = %v, want 0..1", row.Progress)
	}
	if !strings.HasPrefix(row.Message, "thinking... ") || !strings.Contains(row.Message, "/") {
		t.Fatalf("message = %q, want the activity plus a context suffix", row.Message)
	}
}

// TestBaseURL pins host normalization (bare name, host:port, full URL).
func TestBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"statusscreen.local", "http://statusscreen.local"},
		{"192.168.1.205", "http://192.168.1.205"},
		{"192.168.1.205:8080", "http://192.168.1.205:8080"},
		{"http://statusscreen.local", "http://statusscreen.local"},
		{"http://statusscreen.local/", "http://statusscreen.local"},
		{"  statusscreen.local  ", "http://statusscreen.local"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := baseURL(tc.in); got != tc.want {
			t.Fatalf("baseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLogfGoesToDebugLog pins the TUI-safety contract: reporter diagnostics
// go to the opt-in debug log file, never to stderr (a raw stderr line while
// the TUI owns the terminal desyncs its inline renderer).
func TestLogfGoesToDebugLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.log")
	debuglog.Configure(path, "test-session")
	t.Cleanup(func() { debuglog.Configure("", "") })

	logf("found %s (api %s)", "statusscreen.local", "1.0")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	if !strings.Contains(string(data), "found statusscreen.local (api 1.0)") {
		t.Fatalf("debug log = %q, want the formatted message", data)
	}
	if !strings.Contains(string(data), `"location":"statusdisplay"`) {
		t.Fatalf("debug log = %q, want the statusdisplay location", data)
	}
}

// TestFormatTokens pins the compact token formatting used in the context
// suffix ("368k/1M").
func TestFormatTokens(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{84200, "84.2k"},
		{100000, "100k"},
		{368000, "368k"},
		{1000000, "1M"},
		{2000000, "2M"},
	}
	for _, tc := range cases {
		if got := formatTokens(tc.in); got != tc.want {
			t.Fatalf("formatTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatElapsed pins the compact duration rendering.
func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, ""},
		{999 * time.Millisecond, ""},
		{time.Second, "1s"},
		{42 * time.Second, "42s"},
		{60 * time.Second, "1m00s"},
		{2*time.Minute + 3*time.Second, "2m03s"},
		{time.Hour, "1h00m"},
		{2*time.Hour + 5*time.Minute, "2h05m"},
	}
	for _, tc := range cases {
		if got := formatElapsed(tc.in); got != tc.want {
			t.Fatalf("formatElapsed(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// errString is a plain error for table tests.
type errString string

func (e errString) Error() string { return string(e) }

// TestQueuedClearSurvivesANotify pins the separate clear slot: moving the row
// to another host queues a "clear the old display" op, and a Notify arriving
// before the worker wakes must not overwrite it (the old single slot left the
// stale row on the previous display until its TTL — up to 900 s).
func TestQueuedClearSurvivesANotify(t *testing.T) {
	resetForTest(t)

	release := make(chan struct{})
	entered := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	var mu sync.Mutex
	var got []relayed
	SetSink(func(row []byte, clear bool, source string) {
		// Hold the worker inside its first delivery so the following Configure
		// and Notify are both queued before anything else is processed.
		enterOnce.Do(func() {
			close(entered)
			<-release
		})
		mu.Lock()
		got = append(got, relayed{row: string(row), clear: clear, source: source})
		mu.Unlock()
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		SetSink(nil)
	})

	Configure(true, "host-a.invalid", "", "gogen-a", t.TempDir())
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseStarted})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the sink")
	}

	// Row moves to another host (queues the clear), then a Notify lands before
	// the worker wakes.
	Configure(true, "host-b.invalid", "", "gogen-b", t.TempDir())
	Notify(agent.TurnEvent{Phase: agent.TurnPhaseEnded})
	releaseOnce.Do(func() { close(release) })

	relayedOps := waitForRelayed(t, &mu, &got, 3)
	if relayedOps[0].clear || relayedOps[0].source != "gogen-a" {
		t.Fatalf("first relay = %+v, want a row for gogen-a", relayedOps[0])
	}
	if !relayedOps[1].clear || relayedOps[1].source != "gogen-a" {
		t.Fatalf("second relay = %+v, want the queued clear for gogen-a (overwritten by the Notify?)", relayedOps[1])
	}
	if relayedOps[2].clear || relayedOps[2].source != "gogen-b" {
		t.Fatalf("third relay = %+v, want a row for gogen-b", relayedOps[2])
	}
}
