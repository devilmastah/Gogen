//go:build debug

package server

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── Debug-only transport instruments (live harness) ─────────────────────
// The jsdom harness (tmp/live_stall_detach.js) cannot create real TCP
// backpressure — its WebSocket drains instantly — so these env vars inject
// the stall server-side. Every knob is off by default; with none set the
// write path is byte-for-byte unchanged. This file is compiled out of
// production builds (see ws_conn_release.go).
//
//	GOGEN_WS_SENDQ_SIZE         override wsSendQueueSize (default 4096). A
//	                            tiny queue under a stalling writer overflows
//	                            quickly, so wsEnqueueWait's timeout fires
//	                            and broadcast detaches the socket — the
//	                            "stops mid-turn" candidate (DEBUG_PLAN.md C).
//	GOGEN_WS_STALL_MS           sleep this long before every data write once
//	                            stalling has begun (simulated slow client).
//	GOGEN_WS_STALL_AFTER_MS     begin stalling only once the connection has
//	                            been alive this long, so the harness can set
//	                            up panes and turns at normal speed first.
//	GOGEN_WS_STALL_FOR_MS       end stalling this long after it began, so
//	                            the writer drains the queue and a re-attach
//	                            can recover; 0/unset = stall for the
//	                            connection's lifetime.
//	GOGEN_WS_STALL_FIRST_CONN=1 stall only the first connection, so a
//	                            reconnect (e.g. a stall ≥ wsWriteTimeout that
//	                            kills the writer) is clean.
type wsDebugConfig struct {
	sendQSize  int
	stall      time.Duration
	stallAfter time.Duration
	stallFor   time.Duration
	firstConn  bool
}

var (
	wsDebugOnce  sync.Once
	wsDebugCfg   wsDebugConfig
	wsDebugConns atomic.Uint64 // debug-only: connection ordinal for GOGEN_WS_STALL_FIRST_CONN
)

// wsDebugConfigLoad reads the GOGEN_WS_STALL_*/GOGEN_WS_SENDQ_SIZE env vars
// once per process (they cannot change mid-run) and returns the config.
func wsDebugConfigLoad() wsDebugConfig {
	wsDebugOnce.Do(func() {
		cfg := wsDebugConfig{}
		if v := strings.TrimSpace(os.Getenv("GOGEN_WS_SENDQ_SIZE")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.sendQSize = n
			}
		}
		ms := func(name string) time.Duration {
			if v := strings.TrimSpace(os.Getenv(name)); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					return time.Duration(n) * time.Millisecond
				}
			}
			return 0
		}
		cfg.stall = ms("GOGEN_WS_STALL_MS")
		cfg.stallAfter = ms("GOGEN_WS_STALL_AFTER_MS")
		cfg.stallFor = ms("GOGEN_WS_STALL_FOR_MS")
		cfg.firstConn = strings.TrimSpace(os.Getenv("GOGEN_WS_STALL_FIRST_CONN")) == "1"
		wsDebugCfg = cfg
	})
	return wsDebugCfg
}

// wsDebugSendQueueSize returns the GOGEN_WS_SENDQ_SIZE override, or 0 to
// keep the default wsSendQueueSize.
func wsDebugSendQueueSize() int {
	return wsDebugConfigLoad().sendQSize
}

// wsStallState holds the per-connection stall window for the live harness.
type wsStallState struct {
	enabled bool
	start   time.Time
	end     time.Time
	stall   time.Duration
}

// newWSStallState computes this connection's stall window from the
// GOGEN_WS_STALL_* config. Zero stall = the write path is untouched.
func newWSStallState() wsStallState {
	cfg := wsDebugConfigLoad()
	s := wsStallState{
		enabled: cfg.stall > 0,
		start:   time.Now().Add(cfg.stallAfter),
		stall:   cfg.stall,
	}
	s.end = s.start.Add(cfg.stallFor)
	if cfg.stallFor == 0 {
		s.end = s.start.Add(24 * time.Hour) // unset = stall for the connection's lifetime
	}
	if cfg.firstConn && wsDebugConns.Add(1) > 1 {
		s.enabled = false // only the first connection stalls; reconnects are clean
	}
	return s
}

// delay returns how long to sleep before a data write at time now, or 0 if
// stalling is not currently in effect.
func (s wsStallState) delay(now time.Time) time.Duration {
	if !s.enabled || !now.After(s.start) || !now.Before(s.end) {
		return 0
	}
	return s.stall
}
