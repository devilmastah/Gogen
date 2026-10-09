package agent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"gogen/internal/llm"
)

// statusObserverRecorder collects turn events for assertions.
type statusObserverRecorder struct {
	mu     sync.Mutex
	events []TurnEvent
}

func (r *statusObserverRecorder) observe(ev TurnEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *statusObserverRecorder) snapshot() []TurnEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TurnEvent(nil), r.events...)
}

// installStatusObserver installs a recording observer and restores the nil
// default when the test ends.
func installStatusObserver(t *testing.T) *statusObserverRecorder {
	t.Helper()
	rec := &statusObserverRecorder{}
	ConfigureTurnObserver(rec.observe)
	t.Cleanup(func() { ConfigureTurnObserver(nil) })
	return rec
}

func phasesOf(events []TurnEvent) []TurnPhase {
	out := make([]TurnPhase, len(events))
	for i, ev := range events {
		out[i] = ev.Phase
	}
	return out
}

// TestTurnObserverTurnLifecycle pins the event sequence for one successful
// turn: Started → Progress (the round completed) → Ended, each carrying the
// owning agent and a timestamp.
func TestTurnObserverTurnLifecycle(t *testing.T) {
	provider := &overflowProvider{}
	a, _ := newOverflowTestAgent(t, provider)
	rec := installStatusObserver(t)

	if _, err := a.StreamProcessInput(context.Background(), "hi", nil); err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}
	got := rec.snapshot()
	want := []TurnPhase{TurnPhaseStarted, TurnPhaseProgress, TurnPhaseEnded}
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", phasesOf(got), want)
	}
	for i, w := range want {
		if got[i].Phase != w {
			t.Fatalf("phase %d = %v, want %v", i, got[i].Phase, w)
		}
		if got[i].Agent != a {
			t.Fatalf("event %d agent = %p, want %p", i, got[i].Agent, a)
		}
		if got[i].At.IsZero() {
			t.Fatalf("event %d has no timestamp", i)
		}
	}
	if end := got[len(got)-1]; end.Err != nil || end.Cancelled {
		t.Fatalf("end = %+v, want a clean finish", end)
	}
}

// TestTurnObserverReportsFailure pins that a failed turn ends with its
// error on the Ended event.
func TestTurnObserverReportsFailure(t *testing.T) {
	boom := errors.New("boom")
	provider := &overflowProvider{err: boom}
	a, _ := newOverflowTestAgent(t, provider)
	rec := installStatusObserver(t)

	if _, err := a.StreamProcessInput(context.Background(), "hi", nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the provider error", err)
	}
	got := rec.snapshot()
	end := got[len(got)-1]
	if end.Phase != TurnPhaseEnded {
		t.Fatalf("last phase = %v, want TurnPhaseEnded", end.Phase)
	}
	if !errors.Is(end.Err, boom) {
		t.Fatalf("end.Err = %v, want the provider error", end.Err)
	}
	if end.Cancelled {
		t.Fatal("end.Cancelled = true for a plain failure")
	}
}

// TestTurnObserverReportsCancellation pins that a turn whose context was
// cancelled reports Cancelled (a status client maps that to "idle", not
// "error").
func TestTurnObserverReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := &overflowProvider{cancel: cancel, err: context.Canceled}
	a, _ := newOverflowTestAgent(t, provider)
	rec := installStatusObserver(t)

	if _, err := a.StreamProcessInput(ctx, "hi", nil); err == nil {
		t.Fatal("expected a cancellation error")
	}
	got := rec.snapshot()
	end := got[len(got)-1]
	if end.Phase != TurnPhaseEnded || !end.Cancelled {
		t.Fatalf("end = %+v, want TurnPhaseEnded with Cancelled", end)
	}
}

// TestObserveStreamHandlersChainsObserver pins the callback chaining: the
// host's callbacks still run (first) and the observer sees every wrapped
// phase, without mutating the original handler struct.
func TestObserveStreamHandlersChainsObserver(t *testing.T) {
	rec := installStatusObserver(t)
	a := &Agent{}
	var hostCalls []string
	h := &llm.StreamHandlers{
		OnCompacting:  func() { hostCalls = append(hostCalls, "compacting") },
		OnCondensed:   func(note string) { hostCalls = append(hostCalls, "condensed:"+note) },
		OnStreamStall: func() { hostCalls = append(hostCalls, "stall") },
		OnStreamRetry: func(reason string) { hostCalls = append(hostCalls, "retry:"+reason) },
	}
	w := a.observeStreamHandlers(h)
	if w == h {
		t.Fatal("observeStreamHandlers returned the original struct, want a wrapped copy")
	}
	w.OnCompacting()
	w.OnCondensed("note")
	w.OnStreamStall()
	w.OnStreamRetry("why")

	if want := []string{"compacting", "condensed:note", "stall", "retry:why"}; !slices.Equal(hostCalls, want) {
		t.Fatalf("host calls = %v, want %v", hostCalls, want)
	}
	got := rec.snapshot()
	want := []TurnPhase{TurnPhaseCompacting, TurnPhaseCondensed, TurnPhaseStalled, TurnPhaseRetry}
	if !slices.Equal(phasesOf(got), want) {
		t.Fatalf("phases = %v, want %v", phasesOf(got), want)
	}
	if got[1].Detail != "note" || got[3].Detail != "why" {
		t.Fatalf("details = %q, %q; want note, why", got[1].Detail, got[3].Detail)
	}
	if h.OnCompacting == nil || h.OnCondensed == nil || h.OnStreamStall == nil || h.OnStreamRetry == nil {
		t.Fatal("the original handler struct was mutated")
	}
}

// TestObserveStreamHandlersNoOpWithoutObserver pins the cheap default path:
// with no observer installed the handlers pass through unchanged (no
// allocation, no wrapper).
func TestObserveStreamHandlersNoOpWithoutObserver(t *testing.T) {
	ConfigureTurnObserver(nil)
	a := &Agent{}
	h := &llm.StreamHandlers{}
	if w := a.observeStreamHandlers(h); w != h {
		t.Fatal("without an observer the handlers must pass through unchanged")
	}
}
