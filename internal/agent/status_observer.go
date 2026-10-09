package agent

import (
	"sync"
	"time"

	"gogen/internal/llm"
)

// TurnPhase identifies one kind of turn-lifecycle notification delivered to
// the process-wide turn observer (see ConfigureTurnObserver).
type TurnPhase uint8

const (
	// TurnPhaseStarted fires once when a user turn begins streaming.
	TurnPhaseStarted TurnPhase = iota
	// TurnPhaseRoundStart fires at the start of every round after the
	// first, i.e. when the model is called again after a tool round.
	TurnPhaseRoundStart
	// TurnPhaseTool fires immediately before a tool executes, on the turn
	// goroutine (parallel batches fire every call before launching any).
	TurnPhaseTool
	// TurnPhaseProgress fires after each round's response and usage are
	// recorded — the natural point to refresh a context-usage display.
	TurnPhaseProgress
	// TurnPhaseCompacting fires when context compaction starts.
	TurnPhaseCompacting
	// TurnPhaseCondensed fires when the last-resort condensation replaced
	// a message that could not fit the context window.
	TurnPhaseCondensed
	// TurnPhaseStalled fires when the provider stream has produced no
	// chunk for several seconds while the round is still streaming.
	TurnPhaseStalled
	// TurnPhaseRetry fires when a failed stream attempt is retried.
	TurnPhaseRetry
	// TurnPhaseEnded fires once when the turn finishes, with Err set for a
	// failed turn and Cancelled set for an interrupted one.
	TurnPhaseEnded
)

// TurnEvent is one turn-status notification. Events are advisory: an
// observer must never affect the turn, and must not block (the shipped
// statusdisplay client only enqueues and returns).
type TurnEvent struct {
	Phase TurnPhase
	// Agent is the agent that owns the turn. It is valid for the duration
	// of the event only; an observer that needs a slow probe (e.g.
	// ContextStats, which tokenizes) should snapshot what it needs and do
	// the work on its own goroutine.
	Agent *Agent
	// SessionID/Label/Depth identify the session: the label is the derived
	// or renamed session title (possibly empty), Depth > 0 marks a nested
	// subagent turn.
	SessionID string
	Label     string
	Depth     int
	// Tool is the tool name for TurnPhaseTool.
	Tool string
	// Detail carries phase-specific text: the retry reason, the
	// condensation note. Empty otherwise.
	Detail string
	// Err is the turn's terminating error for TurnPhaseEnded (nil on
	// success).
	Err error
	// Cancelled reports that the turn's context was cancelled and the
	// error is the cancellation, not a failure.
	Cancelled bool
	// At is the event timestamp.
	At time.Time
}

var (
	turnObserverMu sync.RWMutex
	turnObserver   func(TurnEvent)
)

// ConfigureTurnObserver installs the process-wide turn observer (nil
// removes it). The observer is called synchronously on the turn goroutine
// for every turn-lifecycle event; it must not block. It is advisory only:
// its return value is ignored and it must not touch the agent.
//
// Configured once at startup by the host (see statusdisplay) because the
// events are process-wide rather than per-session; a nil observer (the
// default) costs one atomic read per event.
func ConfigureTurnObserver(fn func(TurnEvent)) {
	turnObserverMu.Lock()
	turnObserver = fn
	turnObserverMu.Unlock()
}

// turnObserverInstalled reports whether an observer is configured. Used to
// skip the handler-wrapping allocation on the common (observer-less) path.
func turnObserverInstalled() bool {
	turnObserverMu.RLock()
	ok := turnObserver != nil
	turnObserverMu.RUnlock()
	return ok
}

// notifyTurnEvent delivers ev to the installed observer, if any.
func notifyTurnEvent(ev TurnEvent) {
	turnObserverMu.RLock()
	fn := turnObserver
	turnObserverMu.RUnlock()
	if fn == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	fn(ev)
}

// fireTurnEvent stamps and delivers an event built by newTurnEvent.
func (a *Agent) fireTurnEvent(ev TurnEvent) {
	notifyTurnEvent(ev)
}

// newTurnEvent snapshots the agent's identity for the given phase. Call it
// on the turn goroutine (it reads SessionID, which the session lifecycle
// owns).
func (a *Agent) newTurnEvent(phase TurnPhase) TurnEvent {
	return TurnEvent{
		Phase:     phase,
		Agent:     a,
		SessionID: a.SessionID,
		Label:     a.SessionLabelSnapshot(),
		Depth:     a.SubagentDepth(),
	}
}

// observeStreamHandlers chains the turn observer onto the four stream
// callbacks that report context maintenance and provider-stream hiccups, so
// a status client sees "compacting"/"condensed"/"waiting"/"retrying" like
// the live UI does. The host's callbacks are preserved (called first); the
// original handlers struct is never mutated. h must be non-nil.
func (a *Agent) observeStreamHandlers(h *llm.StreamHandlers) *llm.StreamHandlers {
	if !turnObserverInstalled() {
		return h
	}
	orig := h
	w := *h
	w.OnCompacting = func() {
		if orig.OnCompacting != nil {
			orig.OnCompacting()
		}
		a.fireTurnEvent(a.newTurnEvent(TurnPhaseCompacting))
	}
	w.OnCondensed = func(note string) {
		if orig.OnCondensed != nil {
			orig.OnCondensed(note)
		}
		ev := a.newTurnEvent(TurnPhaseCondensed)
		ev.Detail = note
		a.fireTurnEvent(ev)
	}
	w.OnStreamStall = func() {
		if orig.OnStreamStall != nil {
			orig.OnStreamStall()
		}
		a.fireTurnEvent(a.newTurnEvent(TurnPhaseStalled))
	}
	w.OnStreamRetry = func(reason string) {
		if orig.OnStreamRetry != nil {
			orig.OnStreamRetry(reason)
		}
		ev := a.newTurnEvent(TurnPhaseRetry)
		ev.Detail = reason
		a.fireTurnEvent(ev)
	}
	return &w
}
