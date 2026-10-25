// Package statusdisplay pushes live Gogen turn status to a LAN "status
// screen" running the ttgo/status firmware (a TTGO T4 ESP32 display) using
// the firmware's plain HTTP API: POST /status with one JSON row per update,
// DELETE /status?source=X to clear it.
//
// The reporter is a passive observer of the agent's turn events (see
// agent.ConfigureTurnObserver): Notify only enqueues the latest event, and a
// single worker goroutine coalesces bursts and posts the newest state. A
// slow, unreachable, or misconfigured display can therefore never delay a
// turn — updates are best-effort and dropped on failure, because the next
// event carries the full row.
//
// The feature is off by default and configured live from the settings modal
// / gogen.conf (status_display / status_display_host / status_display_token
// / status_display_source), see Configure.
package statusdisplay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/debuglog"
)

// DefaultSource is the display row (feed) name used when neither the config
// nor the project directory yields one; DefaultHost is the mDNS name
// advertised by the ttgo/status firmware (mirrors the config default).
// sourceMax matches the firmware's SRC_MAX-1 (a longer source is truncated
// there anyway, and two long names could collide into one row).
const (
	DefaultSource = "gogen"
	DefaultHost   = config.DefaultStatusDisplayHost
	sourceMax     = 31
)

// Row lifetimes in seconds (refreshed by every update, so a live turn never
// expires). A running row survives a long model prefill; an outcome stays
// visible for a few minutes before the display frees the slot.
const (
	runningTTLSeconds = 900
	endedTTLSeconds   = 300
)

// Request bounds: a normal update gets a generous timeout, shutdown a short
// one so process exit never waits on an unreachable display.
const (
	sendTimeout     = 2 * time.Second
	shutdownTimeout = 500 * time.Millisecond
	// probeTimeout bounds the auto-discovery GET /info so an unreachable
	// host (e.g. an mDNS name that never resolves) costs one short attempt.
	probeTimeout = 2 * time.Second
)

// refreshInterval is how often an in-flight turn re-sends its row so the
// elapsed time keeps ticking. Without it a long model prefill shows a static
// "thinking…" for minutes, which reads as a frozen screen. A var so tests
// can shorten it.
var refreshInterval = 5 * time.Second

// logf routes diagnostics to the debug log (opt-in, file-backed; a no-op when
// no debug log is configured). It deliberately does NOT write to stderr: the
// reporter logs from background goroutines at arbitrary times, and a raw line
// while the TUI owns the terminal desyncs Bubble Tea's inline renderer (the
// same reason runTUI routes startup notices through the managed render path).
func logf(format string, args ...any) {
	debuglog.Write("statusdisplay", fmt.Sprintf(format, args...), "", nil)
}

// Config is one applied configuration. The zero value is disabled.
type Config struct {
	Enabled bool
	// Host is the display's mDNS name, IP, or full base URL.
	Host string
	// Token is the optional shared secret the firmware requires on mutating
	// endpoints (sent as X-Status-Token).
	Token string
	// Source is the display row name (feed id).
	Source string
	// workDir is the process working directory; it feeds the title's
	// project tag ("Gogen@ttgo: ...") so rows of different instances are
	// tellable apart on the panel.
	workDir string
}

// op is one queued piece of work: a row update, or a clear of an old target
// (disable / retarget / rename) so the stale row cannot linger for its TTL.
type op struct {
	clear bool
	cfg   Config
	ev    agent.TurnEvent
	// relay is the transport in effect when the op was QUEUED (the sink, or
	// nil for the HTTP client). The worker delivers the op through it rather
	// than through whatever sink happens to be installed when the op is
	// dequeued: the sink can change while work is pending (ProbeWithRelay
	// upgrades the browser relay to the server transport on a successful
	// probe), and replaying an op through a transport it was not queued for
	// hands a stale clear or row to a recipient that never asked for it.
	relay func(row []byte, clear bool, source string)
}

var (
	mu      sync.Mutex
	cfg     = Config{Source: DefaultSource}
	pending op
	dirty   bool
	started bool
	// lastSent dedupes identical successful rows against one target
	// (repeated stall events must not re-POST the same bytes).
	lastSent string
	// lastFailed tracks the error-log transition: one line when a display
	// goes away, one when it comes back.
	lastFailed bool

	// activeTurn/turnStart/lastSet drive the liveness ticker: while a
	// top-level turn is running the row is re-sent every refreshInterval
	// with the elapsed time appended, so the screen visibly ticks even when
	// no event arrives for minutes.
	activeTurn bool
	turnStart  time.Time
	lastSet    agent.TurnEvent

	// autoSource is the derived row name when the config leaves source
	// empty; derived once so a working-dir change cannot rename a live row.
	autoSource string

	// probeSeq orders concurrent auto-probes: only the newest probe's result
	// is applied, so a slow probe of an old host can never re-enable a
	// target the user has since changed.
	probeSeq uint64

	// sink, when non-nil, replaces the HTTP transport: built rows are handed
	// to it instead (the web server's browser-mode relay). See SetSink.
	sink func(row []byte, clear bool, source string)

	wake = make(chan struct{}, 1)
)

// SetSink routes built rows to fn instead of the HTTP client, or restores
// the HTTP transport when fn is nil. The web server installs a sink in
// `status_display: browser` mode: the server still owns the turn state and
// builds every row, but the browser performs the POST/DELETE, because the
// browser may be on the display's network when the server is not. fn
// receives the row JSON (nil for a clear), a clear flag, and the row name.
func SetSink(fn func(row []byte, clear bool, source string)) {
	mu.Lock()
	sink = fn
	lastSent = "" // the transport changed: re-send the current row
	mu.Unlock()
}

// client posts to the display. Proxy is deliberately nil: the display is on
// the LAN and a configured HTTP_PROXY would otherwise route private
// addresses through the proxy (where they fail). The timeouts keep an
// unreachable display from holding the worker for long.
var client = &http.Client{
	Timeout: sendTimeout,
	Transport: &http.Transport{
		Proxy:             nil,
		DialContext:       (&net.Dialer{Timeout: 1500 * time.Millisecond}).DialContext,
		MaxIdleConns:      2,
		IdleConnTimeout:   30 * time.Second,
		ForceAttemptHTTP2: false,
	},
}

// Configure applies the runtime settings; it is safe to call at any time
// (the settings modal applies changes live through this). Disabling, or
// changing the host or row name, clears the row on the previous target
// best-effort so it cannot linger for its TTL.
//
// source is the display row name. Empty means AUTO: "gogen-<project dir>",
// so several Gogen instances in different projects land on different rows
// without configuration. The derived name is stable for the process (a later
// working-directory change does not rename the row). workdir is the process
// working directory, used for both the auto name and the title's project tag.
func Configure(enabled bool, host, token, source, workdir string) {
	host = strings.TrimSpace(host)
	if host == "" && enabled {
		// An enabled feature with no host is a misconfiguration: fall back
		// to the firmware's mDNS default rather than silently doing nothing.
		host = DefaultHost
	}
	workdir = strings.TrimSpace(workdir)

	mu.Lock()
	if source == "" {
		// Derive once: a working-dir change (web workspace switch) must not
		// rename the row under a running instance. An unknown directory
		// returns "" and is NOT cached, so a later Configure with a real
		// directory still derives the project name.
		if autoSource == "" {
			autoSource = sourceForDir(workdir)
		}
		source = autoSource
	} else {
		// An explicit row name wins; clearing it later re-derives the auto
		// name from the then-current directory.
		autoSource = ""
	}
	source = truncate(strings.TrimSpace(source), sourceMax)
	if source == "" {
		source = DefaultSource
	}
	next := Config{Enabled: enabled, Host: host, Token: token, Source: source, workDir: workdir}
	prev := cfg
	cfg = next
	changed := prev != next
	if changed {
		lastSent = ""
	}
	queuedClear := false
	if prev.Enabled && prev.Host != "" && (!next.Enabled || prev.Host != next.Host || prev.Source != next.Source) {
		pending = op{clear: true, cfg: prev, relay: sink}
		dirty = true
		queuedClear = true
		startWorkerLocked()
	}
	mu.Unlock()
	if changed || queuedClear {
		signal()
	}
}

// Probe looks for the status screen and enables reporting when it answers;
// it is the `status_display: auto` entry point (the default). The probe runs
// on its own goroutine with a short timeout, so startup never waits on the
// network and an absent display costs nothing. Repeated calls supersede each
// other: only the newest probe's result is applied, so a slow probe of an old
// host cannot re-enable a target the user has since changed.
func Probe(host, token, source, workdir string) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = DefaultHost
	}
	mu.Lock()
	probeSeq++
	seq := probeSeq
	mu.Unlock()
	go func() {
		info, ok := probeDisplay(host, token)
		if !ok {
			return // absent or not the status-screen firmware: stay off
		}
		mu.Lock()
		stale := seq != probeSeq
		mu.Unlock()
		if stale {
			return
		}
		Configure(true, host, token, source, workdir)
		logf("found %s (%s, api %s) - reporting enabled",
			host, info.Hostname, info.APIVersion)
	}()
}

// ProbeWithRelay is the `status_display: browser` entry point: the SERVER
// probes for the display first and, when it answers, sends the rows itself —
// the server transport takes precedence, because it is the reliable sender
// (no browser tab needed, one writer, works headless). Only when the server
// cannot reach the display does the relay carry the rows: the web server
// passes its browser-mode sink, and the browser — which may be on the
// display's network when the server is not — performs the POST/DELETE.
//
// The relay is installed and reporting enabled immediately, so a turn that
// starts while the probe is in flight is not lost; a successful probe then
// upgrades the transport to the server.
func ProbeWithRelay(host, token, source, workdir string, relay func(row []byte, clear bool, source string)) {
	SetSink(relay)
	Configure(true, host, token, source, workdir)

	host = strings.TrimSpace(host)
	if host == "" {
		host = DefaultHost
	}
	mu.Lock()
	probeSeq++
	seq := probeSeq
	mu.Unlock()
	go func() {
		info, ok := probeDisplay(host, token)
		if !ok {
			return // the relay stays installed: the browser is the sender
		}
		mu.Lock()
		stale := seq != probeSeq
		mu.Unlock()
		if stale {
			return
		}
		SetSink(nil) // server transport wins
		Configure(true, host, token, source, workdir)
		logf("found %s (%s, api %s) - reporting from the server", host, info.Hostname, info.APIVersion)
	}()
}

// displayInfo is the subset of the firmware's GET /info the probe reads.
type displayInfo struct {
	Hostname   string `json:"hostname"`
	APIVersion string `json:"api_version"`
}

// probeDisplay fetches /info and reports whether the host runs the
// ttgo/status firmware (its JSON carries api_version). Read-only endpoints
// are open even when the display has a token configured; the token is sent
// anyway so a future firmware that gates /info keeps working.
func probeDisplay(host, token string) (displayInfo, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(host)+"/info", nil)
	if err != nil {
		return displayInfo{}, false
	}
	if token != "" {
		req.Header.Set("X-Status-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return displayInfo{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return displayInfo{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return displayInfo{}, false
	}
	var info displayInfo
	if err := json.Unmarshal(body, &info); err != nil || info.APIVersion == "" {
		return displayInfo{}, false
	}
	return info, true
}

// Notify implements the agent turn observer. It runs synchronously on the
// turn goroutine and only replaces the pending state, so it must stay cheap.
// Nested subagent turns are ignored: the display tracks the top-level
// session.
func Notify(ev agent.TurnEvent) {
	if ev.Depth > 0 {
		return
	}
	mu.Lock()
	c := cfg
	if !c.Enabled || c.Host == "" {
		mu.Unlock()
		return
	}
	pending = op{cfg: c, ev: ev, relay: sink}
	dirty = true
	switch ev.Phase {
	case agent.TurnPhaseStarted:
		turnStart = time.Now()
		activeTurn = true
	case agent.TurnPhaseEnded:
		activeTurn = false
	}
	lastSet = ev
	startWorkerLocked()
	mu.Unlock()
	signal()
}

// Shutdown clears the display row best-effort, for process exit: the row
// must not linger for its TTL after Gogen is gone. Bounded by
// shutdownTimeout so exit never waits on the network.
func Shutdown() {
	mu.Lock()
	c := cfg
	relay := sink
	mu.Unlock()
	if !c.Enabled || c.Host == "" {
		return
	}
	if relay != nil {
		// Browser mode: hand the clear to the relay (the tabs may already be
		// gone; the row then expires by TTL).
		relay(nil, true, c.Source)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := sendClear(ctx, c); err != nil {
		return // best effort: an exit must not be held up by the display
	}
	mu.Lock()
	lastSent = ""
	mu.Unlock()
}

// signal wakes the worker without blocking (a pending wake is enough).
func signal() {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// startWorkerLocked starts the single worker (and the liveness ticker) on
// first use. The caller holds mu.
func startWorkerLocked() {
	if started {
		return
	}
	started = true
	go worker()
	go ticker()
}

// ticker re-sends the in-flight turn's row every refreshInterval so the
// elapsed time keeps advancing. It is a no-op while no turn is running (or
// the reporter is off), and the worker's dedupe drops a refresh whose row
// did not actually change.
func ticker() {
	for {
		// Read the interval under mu: tests shorten it, and the race
		// detector rightly objects to an unsynchronized read.
		mu.Lock()
		interval := refreshInterval
		mu.Unlock()
		time.Sleep(interval)

		mu.Lock()
		if !cfg.Enabled || cfg.Host == "" || !activeTurn {
			mu.Unlock()
			continue
		}
		pending = op{cfg: cfg, ev: lastSet, relay: sink}
		dirty = true
		mu.Unlock()
		signal()
	}
}

// turnElapsed returns the current (or just-finished) turn's duration, 0 when
// no turn has run yet. Read by the row builder so the message can tick.
func turnElapsed() time.Duration {
	mu.Lock()
	defer mu.Unlock()
	if turnStart.IsZero() {
		return 0
	}
	return time.Since(turnStart)
}

// worker coalesces queued operations: it drains the pending state until
// nothing is dirty, and only then sleeps for the next wake.
func worker() {
	for range wake {
		for {
			mu.Lock()
			if !dirty {
				mu.Unlock()
				break
			}
			p := pending
			dirty = false
			mu.Unlock()
			process(p)
		}
	}
}

// process sends one operation, deduping identical rows and logging only
// failure/recovery transitions. When the op was queued for a relay (browser
// mode) the row is handed to that relay instead of the HTTP client; the relay
// travels WITH the op, so a sink swapped while work was pending cannot receive
// an op it was not installed for (see op.relay).
func process(p op) {
	relay := p.relay
	if p.clear {
		if relay != nil {
			relay(nil, true, p.cfg.Source)
			mu.Lock()
			lastSent = ""
			mu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		err := sendClear(ctx, p.cfg)
		cancel()
		noteResult(p.cfg, "", err)
		return
	}
	body, err := buildRow(p.ev, p.cfg, turnElapsed())
	if err != nil {
		// Only an un-marshalable row reaches this (no user data is in the
		// body), so drop it rather than looping.
		logf("build row: %v", err)
		return
	}
	key := p.cfg.Host + "\x00" + string(body)
	mu.Lock()
	duplicate := lastSent == key
	mu.Unlock()
	if duplicate {
		return
	}
	if relay != nil {
		// The relay is the browser: a broadcast cannot fail here (an
		// unreachable display fails there, silently), so the row counts as
		// sent for the dedupe.
		relay(body, false, p.cfg.Source)
		mu.Lock()
		lastSent = key
		mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	err = sendStatus(ctx, p.cfg, body)
	cancel()
	noteResult(p.cfg, key, err)
}

// noteResult records a send outcome. key == "" marks a successful clear
// (the target no longer shows our row, so a later re-enable must re-send).
func noteResult(c Config, key string, err error) {
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		if !lastFailed {
			lastFailed = true
			logf("%s: %v", displayName(c), err)
		}
		return
	}
	if lastFailed {
		lastFailed = false
		logf("%s: updates resumed", displayName(c))
	}
	lastSent = key
}

// sourceForDir derives the default row name from the project directory:
// "gogen-<basename>", so two Gogen instances in different projects land on
// different display rows with no configuration. Returns "" when the directory
// is unknown ("." / "" / a root) — the caller then falls back to
// DefaultSource. Spaces become dashes and the tag is capped so the name fits
// the firmware's 31-char source field.
func sourceForDir(dir string) string {
	base := dirBase(dir)
	if base == "" {
		return ""
	}
	base = asciiOnly(base)
	base = strings.Map(func(r rune) rune {
		if r == ' ' {
			return '-'
		}
		return r
	}, base)
	return DefaultSource + "-" + truncate(base, 24)
}

// displayName names a target in logs without ever printing the token.
func displayName(c Config) string {
	if c.Host == "" {
		return "(no host)"
	}
	return c.Host
}

// baseURL turns a configured host (bare name, IP, or full URL) into the
// display's base URL. The scheme defaults to http:// — the firmware serves
// plain HTTP.
func baseURL(host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return ""
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	return strings.TrimRight(h, "/")
}

// sendStatus posts one row.
func sendStatus(ctx context.Context, c Config, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL(c.Host)+"/status", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(req, c)
}

// sendClear deletes this source's row.
func sendClear(ctx context.Context, c Config) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		baseURL(c.Host)+"/status?source="+url.QueryEscape(c.Source), nil)
	if err != nil {
		return err
	}
	return do(req, c)
}

// do performs a request, sets the optional auth header, drains a bounded
// response body, and treats any non-2xx as a failure. The body is never
// user-visible (the firmware answers "ok" or a short error), so it is
// discarded.
func do(req *http.Request, c Config) error {
	if c.Token != "" {
		req.Header.Set("X-Status-Token", c.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status display answered HTTP %s", resp.Status)
	}
	return nil
}
