package streamutil

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"gogen/internal/llm"
)

// recordingSink records which Sink method the builder dispatched to, in
// order. onCall (optional) observes each dispatch for ordering tests.
// The calls slice is mutex-guarded: the SpeedMeter's ticker dispatches
// OnStreamStats from its own goroutine, interleaved with the test's
// synchronous callback invocations.
type recordingSink struct {
	mu     sync.Mutex
	calls  []string
	onCall func(name string)
}

func (r *recordingSink) record(name string) {
	r.mu.Lock()
	r.calls = append(r.calls, name)
	r.mu.Unlock()
	if r.onCall != nil {
		r.onCall(name)
	}
}

// saw reports whether name was recorded (in any position).
func (r *recordingSink) saw(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c == name {
			return true
		}
	}
	return false
}

// lastIs reports whether the most recent record is name.
func (r *recordingSink) lastIs(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls) > 0 && r.calls[len(r.calls)-1] == name
}

func (r *recordingSink) OnStart()                     { r.record("OnStart") }
func (r *recordingSink) OnRoundStart()                { r.record("OnRoundStart") }
func (r *recordingSink) OnStreamOpened()              { r.record("OnStreamOpened") }
func (r *recordingSink) OnStreamActivity()            { r.record("OnStreamActivity") }
func (r *recordingSink) OnCompacting()                { r.record("OnCompacting") }
func (r *recordingSink) OnCondensed(note string)      { r.record("OnCondensed") }
func (r *recordingSink) OnStreamStall()               { r.record("OnStreamStall") }
func (r *recordingSink) OnStreamRetry(reason string)  { r.record("OnStreamRetry") }
func (r *recordingSink) OnThinkingToken(token string) { r.record("OnThinkingToken") }
func (r *recordingSink) OnToken(token string)         { r.record("OnToken") }
func (r *recordingSink) OnStreamEnd()                 { r.record("OnStreamEnd") }
func (r *recordingSink) OnReplyModel(model string)    { r.record("OnReplyModel") }
func (r *recordingSink) OnToolCallStart(index int, id, name string) {
	r.record("OnToolCallStart")
}
func (r *recordingSink) OnToolCallArgsDelta(index int, id, name, argsDelta string) {
	r.record("OnToolCallArgsDelta")
}
func (r *recordingSink) OnToolCall(tc llm.ToolCall) { r.record("OnToolCall") }
func (r *recordingSink) OnRecoverPartialStream()    { r.record("OnRecoverPartialStream") }
func (r *recordingSink) OnToolExecute(name string)  { r.record("OnToolExecute") }
func (r *recordingSink) OnToolOutput(id, name, command, chunk string) {
	r.record("OnToolOutput")
}
func (r *recordingSink) OnToolOutputEnd(id string, success bool) { r.record("OnToolOutputEnd") }
func (r *recordingSink) OnToolResult(id, name, result string, success bool) {
	r.record("OnToolResult")
}
func (r *recordingSink) OnMessageAppended(index int, role string) {
	r.record("OnMessageAppended")
}
func (r *recordingSink) StopStream() bool {
	r.record("StopStream")
	return false
}
func (r *recordingSink) OnStreamStats(toksPerSec float64) { r.record("OnStreamStats") }

var _ Sink = (*recordingSink)(nil)

// newTestConfig builds batchers whose send callbacks record into events
// (the long interval keeps the timer goroutine out of the test).
func newTestConfig(events *[]string) HandlersConfig {
	return HandlersConfig{
		Tokens: NewTokenBatcher(func(think bool, text string) {
			kind := "token"
			if think {
				kind = "think"
			}
			*events = append(*events, kind+":"+text)
		}, time.Hour),
		Args: NewArgsBatcher(func(index int, id, name, delta string) {
			*events = append(*events, "args:"+delta)
		}, time.Hour),
	}
}

// TestBuildStreamHandlersWiresEveryCallback is the exhaustiveness guard
// behind the Sink interface: every field of llm.StreamHandlers must be
// wired by BuildStreamHandlers to the Sink method of the same name. A
// new callback added to llm.StreamHandlers fails this test until the
// builder — and therefore every host's Sink implementation — handles it.
func TestBuildStreamHandlersWiresEveryCallback(t *testing.T) {
	rec := &recordingSink{}
	h := BuildStreamHandlers(rec, HandlersConfig{
		Tokens: NewTokenBatcher(func(bool, string) {}, time.Hour),
		Args:   NewArgsBatcher(func(int, string, string, string) {}, time.Hour),
	})

	v := reflect.ValueOf(h).Elem()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fv := v.Field(i)
		if fv.IsNil() {
			t.Fatalf("llm.StreamHandlers.%s is not wired by BuildStreamHandlers", field.Name)
		}
		rec.calls = nil
		switch fn := fv.Interface().(type) {
		case func():
			fn()
		case func() bool:
			fn()
		case func(string):
			fn("s")
		case llm.StreamCallback:
			fn("s")
		case func(int, string):
			fn(0, "assistant")
		case func(int, string, string):
			fn(0, "id", "name")
		case func(int, string, string, string):
			fn(0, "id", "name", "{}")
		case func(llm.ToolCall):
			fn(llm.ToolCall{Index: 0, ID: "id", Name: "name"})
		case func(string, string, string, string):
			fn("id", "name", "cmd", "chunk")
		case func(string, bool):
			fn("id", true)
		case func(string, string, string, bool):
			fn("id", "name", "result", true)
		default:
			t.Fatalf("llm.StreamHandlers.%s: unhandled signature %s", field.Name, field.Type)
		}
		if len(rec.calls) != 1 || rec.calls[0] != field.Name {
			t.Fatalf("llm.StreamHandlers.%s dispatched to %v, want [%s]", field.Name, rec.calls, field.Name)
		}
	}
}

// TestBuildStreamHandlersFlushOrdering pins the shared ordering
// discipline: content and args are flushed before the events that assume
// the consumer already has them, and the round boundary is processed
// after the round's content.
func TestBuildStreamHandlersFlushOrdering(t *testing.T) {
	var events []string
	rec := &recordingSink{onCall: func(name string) { events = append(events, "sink:"+name) }}
	h := BuildStreamHandlers(rec, newTestConfig(&events))

	h.OnToken("hello")
	h.OnToolCallArgsDelta(0, "id1", "read_file", `{"p":`)
	h.OnToolCallStart(0, "id1", "read_file")
	h.OnToolCall(llm.ToolCall{Index: 0, ID: "id1", Name: "read_file"})
	h.OnStreamEnd()

	want := []string{
		"sink:OnToken",
		"sink:OnToolCallArgsDelta",
		"token:hello", // flushed before the start frame, not by the timer
		"args:{\"p\":",
		"sink:OnToolCallStart",
		"sink:OnToolCall",
		"sink:OnStreamEnd",
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

// TestBuildStreamHandlersModelStampPrecededByContent pins that the
// model stamp is preceded by a content flush (the consumer must have
// created the bubble before it is stamped).
func TestBuildStreamHandlersModelStampPrecededByContent(t *testing.T) {
	var events []string
	rec := &recordingSink{onCall: func(name string) { events = append(events, "sink:"+name) }}
	h := BuildStreamHandlers(rec, newTestConfig(&events))

	h.OnToken("answer")
	h.OnReplyModel("model-x")

	want := []string{"sink:OnToken", "token:answer", "sink:OnReplyModel"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

// TestBuildStreamHandlersRoundRearm pins the batcher lifecycle across a
// round boundary: the round-end flush+close must not swallow the next
// round's tokens — the round start re-arms the batchers.
func TestBuildStreamHandlersRoundRearm(t *testing.T) {
	var events []string
	rec := &recordingSink{onCall: func(name string) { events = append(events, "sink:"+name) }}
	h := BuildStreamHandlers(rec, newTestConfig(&events))

	h.OnStart()
	h.OnToken("round1")
	h.OnStreamEnd()
	h.OnRoundStart()
	h.OnToken("round2")
	h.OnStreamEnd()

	want := []string{
		"sink:OnStart",
		"sink:OnToken",
		"token:round1",
		"sink:OnStreamEnd",
		"sink:OnRoundStart",
		"sink:OnToken",
		"token:round2",
		"sink:OnStreamEnd",
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}
