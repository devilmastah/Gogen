package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// /compact runs off the Update thread (it makes an LLM summarization
// call); these tests pin the guards, the input-band indicator, and the
// result handling without a live provider.
func TestCmdCompactGuards(t *testing.T) {
	t.Run("refuses while a turn is streaming", func(t *testing.T) {
		m := dragModel(t)
		m.streaming = true
		handled, _, cmd := m.dispatchCommand("/compact")
		if !handled || cmd != nil {
			t.Fatalf("handled=%v cmd=%v, want refused with no cmd", handled, cmd != nil)
		}
		if !strings.Contains(strings.Join(m.chatLines, "\n"), "wait for the current turn") {
			t.Fatalf("guard line missing: %s", strings.Join(m.chatLines, "\n"))
		}
		if m.compacting {
			t.Fatal("guard must not start a compaction")
		}
	})

	t.Run("refuses while already compacting", func(t *testing.T) {
		m := dragModel(t)
		m.lives.Active().compacting = true
		m.compacting = true
		_, _, cmd := m.dispatchCommand("/compact")
		if cmd != nil {
			t.Fatal("second compact must not start")
		}
		if !strings.Contains(strings.Join(m.chatLines, "\n"), "already running") {
			t.Fatalf("refusal line missing: %s", strings.Join(m.chatLines, "\n"))
		}
	})

	t.Run("idle starts the async compaction", func(t *testing.T) {
		m := dragModel(t)
		handled, _, cmd := m.dispatchCommand("/compact")
		if !handled || cmd == nil {
			t.Fatalf("handled=%v cmd=%v, want async start", handled, cmd != nil)
		}
		if !m.compacting || !m.lives.Active().compacting || m.lives.Active().compactCancel == nil {
			t.Fatal("compacting state not set")
		}
		// The input band shows the compacting indicator, not the textarea.
		if out := m.renderMainColumn(); !strings.Contains(out, "compacting") {
			t.Fatalf("input band missing compacting indicator:\n%s", out)
		}
	})

	t.Run("submit queues while compacting", func(t *testing.T) {
		// Steering: the composer is never locked. A running /compact owns
		// Messages, so a turn cannot START — but the message is queued and
		// the drain waits for the compaction gate (drainDeliveries gates
		// on m.compacting too), so it runs after the compact finishes.
		m := dragModel(t)
		m.keys = DefaultKeyMap
		m.lives.Active().compacting = true
		m.compacting = true
		m.textarea.SetValue("hello")
		_, _, ok := m.handleSubmitKey(keyMsg("enter"))
		// The returned command is the composer refocus (cursor blink) — a
		// turn must NOT start.
		if !ok {
			t.Fatal("enter must be consumed")
		}
		if m.streaming {
			t.Fatal("turn started during compaction")
		}
		if q := m.lives.Active().steerQ.Items(); len(q) != 1 || q[0].Text != "hello" {
			t.Fatalf("queue = %v, want one queued item [hello]", q)
		}
		// The queue cannot drain while the compaction runs.
		if cmd := m.drainDeliveries(); cmd != nil {
			t.Fatal("drain must wait for the compaction gate")
		}
	})
}

func TestCompactResultMsg(t *testing.T) {
	t.Run("success rebuilds the transcript around the summary", func(t *testing.T) {
		m := dragModel(t)
		m.lives.Active().compacting = true
		m.compacting = true
		m.agent.Messages = []llm.Message{
			{Role: "user", Content: "original task"},
			{Role: "assistant", Content: "summary of the middle"},
		}
		m.appendChatLine("stale pre-compact line")
		mod, _ := m.Update(compactResultMsg{agent: m.agent, err: nil})
		got := mod.(*Model)
		if got.compacting || got.lives.Active().compacting || got.lives.Active().compactCancel != nil {
			t.Fatal("compacting state not cleared")
		}
		joined := strings.Join(got.chatLines, "\n")
		if !strings.Contains(joined, "History compacted") {
			t.Fatalf("notice missing: %s", joined)
		}
		if !strings.Contains(joined, "summary of the middle") {
			t.Fatalf("transcript not rebuilt from Messages: %s", joined)
		}
		if strings.Contains(joined, "stale pre-compact line") {
			t.Fatalf("stale pre-compact history survived the rebuild: %s", joined)
		}
	})

	t.Run("failure reports the error", func(t *testing.T) {
		m := dragModel(t)
		m.lives.Active().compacting = true
		m.compacting = true
		mod, _ := m.Update(compactResultMsg{agent: m.agent, err: errors.New("boom")})
		got := mod.(*Model)
		if got.compacting {
			t.Fatal("compacting state not cleared")
		}
		if !strings.Contains(strings.Join(got.chatLines, "\n"), "Compact failed: boom") {
			t.Fatalf("error line missing: %s", strings.Join(got.chatLines, "\n"))
		}
	})

	t.Run("user-cancelled failure is not double-reported", func(t *testing.T) {
		m := dragModel(t)
		s1 := m.lives.Active()
		s1.compacting = true
		s1.compactUserCancelled = true
		m.compacting = true
		mod, _ := m.Update(compactResultMsg{agent: m.agent, err: context.Canceled})
		got := mod.(*Model)
		if strings.Contains(strings.Join(got.chatLines, "\n"), "Compact failed") {
			t.Fatalf("user cancel surfaced a failure line: %s", strings.Join(got.chatLines, "\n"))
		}
		if got.lives.Active().compactUserCancelled {
			t.Fatal("user-cancel flag must reset")
		}
	})

	t.Run("focus change while compacting skips the rebuild", func(t *testing.T) {
		m := dragModel(t)
		m.lives.Active().compacting = true
		m.compacting = true
		compacted := m.agent
		a2 := newSwitchTestAgent(t)
		m.agent = a2 // simulate a session switch during the compaction
		mod, _ := m.Update(compactResultMsg{agent: compacted, err: nil})
		got := mod.(*Model)
		if got.statusMsg != "✓ History compacted" {
			t.Fatalf("statusMsg = %q", got.statusMsg)
		}
		if strings.Contains(strings.Join(got.chatLines, "\n"), "History compacted (") {
			t.Fatal("rebuild must be skipped when the agent changed")
		}
	})

	t.Run("a turn started before the result skips the rebuild", func(t *testing.T) {
		m := dragModel(t)
		m.lives.Active().compacting = true
		m.compacting = true
		m.streaming = true // a delivery turn started while compaction ran
		mod, _ := m.Update(compactResultMsg{agent: m.agent, err: nil})
		got := mod.(*Model)
		if !got.streaming {
			t.Fatal("result must not touch the running turn's mirror")
		}
		if strings.Contains(strings.Join(got.chatLines, "\n"), "History compacted (") {
			t.Fatal("rebuild must be skipped mid-turn (Messages owned by the stream)")
		}
	})
}

// ctrl+c while compacting cancels the compaction instead of quitting the
// app (the old behavior quit mid-summarization).
func TestCancelKeyCancelsCompact(t *testing.T) {
	m := dragModel(t)
	s1 := m.lives.Active()
	s1.compacting = true
	m.compacting = true
	cancelled := false
	s1.compactCancel = func() { cancelled = true }

	_, cmd, _ := m.handleCancelKey()
	if cmd != nil {
		t.Fatal("cancel during compact must not quit")
	}
	if !cancelled {
		t.Fatal("compact cancel func not called")
	}
	if m.compacting || s1.compacting || s1.compactCancel != nil {
		t.Fatal("compacting state not cleared")
	}
	if !strings.Contains(strings.Join(m.chatLines, "\n"), "Compact cancelled.") {
		t.Fatalf("cancel notice missing: %s", strings.Join(m.chatLines, "\n"))
	}
}

// Cancelling a compaction clears the mirror itself, so it owns the band
// boundary bookkeeping the result handler performs: the reserved
// progress-strip row returns to the composer (the frame must stay exactly
// terminal-height — a shorter frame leaves a row of the previous frame
// unowned on screen, which is how "ghost" rows appear in the panel) and the
// composer placeholder flips back from the queued-turn wording.
func TestCancelKeyCompactRestoresInputBand(t *testing.T) {
	m := dragModel(t)
	m.SetSize(m.width, m.height)
	s1 := m.lives.Active()
	s1.compacting = true
	m.compacting = true
	s1.compactCancel = func() {}
	m.applyComposerPlaceholder()
	m.relayout()

	if got := strings.Count(m.View().Content, "\n") + 1; got != m.height {
		t.Fatalf("setup: frame height = %d, want %d", got, m.height)
	}
	queuedPlaceholder := m.textarea.Placeholder

	_, _, _ = m.handleCancelKey()
	if got := strings.Count(m.View().Content, "\n") + 1; got != m.height {
		t.Fatalf("after cancelling the compaction the frame is %d rows, want %d (the strip's row must return to the composer)", got, m.height)
	}
	if m.textarea.Placeholder == queuedPlaceholder {
		t.Fatalf("composer placeholder still reads %q after the compaction ended", queuedPlaceholder)
	}
}

// The shared cancel handler keeps its old contracts: cancel a running
// turn (clearing the mirror immediately — the epoch guard makes that
// safe) and quit when idle.
func TestCancelKeyTurnAndIdle(t *testing.T) {
	t.Run("turn: cancel, no quit", func(t *testing.T) {
		m := dragModel(t)
		s1 := m.lives.Active()
		s1.streaming = true
		m.streaming = true
		s1.cancel = func() {}

		_, _, _ = m.handleCancelKey()
		if m.quitting {
			t.Fatal("turn cancel must not quit")
		}
		if m.streaming {
			t.Fatal("mirror must clear immediately (epoch guard covers stragglers)")
		}
		if !s1.streaming {
			t.Fatal("session flag must stay set until the terminal arrives")
		}
		if !strings.Contains(strings.Join(m.chatLines, "\n"), "Cancelled.") {
			t.Fatal("cancel notice missing")
		}
	})

	t.Run("idle: quit", func(t *testing.T) {
		m := dragModel(t)
		_, cmd, _ := m.handleCancelKey()
		if cmd == nil {
			t.Fatal("idle ctrl+c must return tea.Quit")
		}
		if !m.quitting {
			t.Fatal("idle ctrl+c must flush and mark quitting")
		}
	})
}

// Regression: ctrl+c cancels a running /compact, and /compact may restart
// immediately — while the cancelled run's goroutine is still unwinding its
// LLM call. The late compactResultMsg of the SUPERSEDED run carries the old
// generation (compactSeq); it must be dropped entirely instead of clearing
// the new run's flags, orphaning its cancel func, or surfacing the old
// cancellation as a fresh "Compact failed" error.
func TestSupersededCompactionResultDropped(t *testing.T) {
	m := dragModel(t)
	s1 := m.lives.Active()

	// Compaction A starts; the user cancels it; B starts before A's
	// goroutine has finished unwinding.
	_, _, _ = m.dispatchCommand("/compact")
	if s1.compactSeq != 1 {
		t.Fatalf("compactSeq = %d, want 1 after the first /compact", s1.compactSeq)
	}
	if _, cmd, _ := m.handleCancelKey(); cmd != nil {
		t.Fatal("ctrl+c during compact must not quit")
	}
	_, _, _ = m.dispatchCommand("/compact") // B
	cancelledB := false
	s1.compactCancel = func() { cancelledB = true }
	if !m.compacting || !s1.compacting || s1.compactSeq != 2 {
		t.Fatalf("restart did not arm B: mirror=%v session=%v seq=%d",
			m.compacting, s1.compacting, s1.compactSeq)
	}

	t.Run("A's stale terminal is ignored", func(t *testing.T) {
		mod, _ := m.Update(compactResultMsg{agent: m.agent, err: context.Canceled, seq: 1})
		got := mod.(*Model)
		if !got.compacting || !got.lives.Active().compacting {
			t.Fatal("stale result cleared B's compacting flags")
		}
		got.lives.Active().compactCancel()
		if !cancelledB {
			t.Fatal("stale result replaced B's cancel func")
		}
		if got.statusMsg != "" {
			t.Fatalf("stale cancellation surfaced: %q", got.statusMsg)
		}
		if strings.Contains(strings.Join(got.chatLines, "\n"), "Compact failed") {
			t.Fatal("stale cancellation appended a failure line")
		}
	})

	t.Run("B's own terminal still finalizes", func(t *testing.T) {
		mod, _ := m.Update(compactResultMsg{agent: m.agent, err: nil, seq: 2})
		got := mod.(*Model)
		if got.compacting || got.lives.Active().compacting || got.lives.Active().compactCancel != nil {
			t.Fatal("live result did not clear B's flags")
		}
		if !strings.Contains(strings.Join(got.chatLines, "\n"), "History compacted") {
			t.Fatal("live result did not report success")
		}
	})
}
