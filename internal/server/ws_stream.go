package server

import (
	"context"
	"sync"
	"time"
)

// wsTokenFlushInterval is the server's token-flush cadence for stream and
// thinking frames. It must not be much finer than the web client's paint
// cadence (STREAM_RENDER_INTERVAL = 32ms in app.js), which coalesces token
// arrivals into at most one paint-aligned render: at high token rates
// (100+ tok/s per session, several panes streaming concurrently) a 16ms
// interval emits ~60 frames/sec/session that the client coalesces away
// anyway — pure JSON-parse and wakeup overhead with zero visible latency
// benefit. 32ms matches the client's render interval 1:1: every frame can
// be painted, so perceived latency is unchanged while the frame rate is
// halved.
const wsTokenFlushInterval = 32 * time.Millisecond

// wsConnStream owns the in-flight stream cancel handle for one session
// runtime. Allocated on the heap so the stream goroutine and the cancel
// callers (read loops, shutdown, eviction) share clear ownership (not ad-hoc
// locals).
type wsConnStream struct {
	mu     sync.Mutex
	cancel context.CancelFunc // cancels the stream goroutine (cancelInFlight)
	errCh  chan error
}

// cancelInFlight stops the entire stream: cancels the turn context (which
// propagates to the LLM call) and waits for the stream goroutine to finish
// its deferred cleanup.
//
// If the stream does not exit within wsStreamDrainWait, errCh is kept so a
// later cancel/message can keep waiting. Clearing it on timeout made the next
// tryAcquireTurn fail with "agent is busy" while a stuck command still held
// turnMu.
//
// The bool result reports whether a stream was actually in flight (a
// registered cancel handle existed when called). The web cancel handler uses
// it to decide whether the cancelled/turn_end pair will follow: when nothing
// was in flight it answers its sender with cancel_ack instead, so the
// sender's per-cancel bookkeeping can drain (statement callers ignore the
// result; calling convention is unchanged for them).
func (s *wsConnStream) cancelInFlight() bool {
	s.mu.Lock()
	wasInFlight := s.cancel != nil
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	prevErr := s.errCh
	s.mu.Unlock()
	if prevErr == nil {
		return wasInFlight
	}
	// Wait for cancel repair (appendCanceledToolResults + FlushSession).
	if drainStreamErr(prevErr) {
		s.mu.Lock()
		if s.errCh == prevErr {
			s.errCh = nil
		}
		s.mu.Unlock()
	}
	return wasInFlight
}

// begin registers a cancel handle for a new stream. Caller must already have
// cancelled any prior stream. Returns the error channel the stream goroutine
// should signal on exit.
func (s *wsConnStream) begin(cancel context.CancelFunc) chan error {
	s.mu.Lock()
	s.cancel = cancel
	s.errCh = make(chan error, 1)
	errCh := s.errCh
	s.mu.Unlock()
	return errCh
}

func (s *wsConnStream) end() {
	s.mu.Lock()
	s.cancel = nil
	s.errCh = nil
	s.mu.Unlock()
}
