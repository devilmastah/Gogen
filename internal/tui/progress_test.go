package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
)

func TestProgressAnimating(t *testing.T) {
	m := Model{
		streaming:     true,
		progressPhase: progressThinking,
		spinner:       newProgressSpinner(),
	}
	if !m.progressAnimating() {
		t.Fatal("thinking should animate")
	}
	m.progressPhase = progressTool
	if !m.progressAnimating() {
		t.Fatal("tool should animate")
	}
	m.progressPhase = progressActive
	if m.progressAnimating() {
		t.Fatal("active streaming should not animate")
	}
	m.progressPhase = progressThinking
	m.streaming = false
	if m.progressAnimating() {
		t.Fatal("stopped stream should not animate")
	}
}

func TestSetProgressRestartsTick(t *testing.T) {
	m := Model{
		streaming:     true,
		progressPhase: progressActive,
		spinner:       newProgressSpinner(),
	}
	cmd := m.setProgress(progressThinking, "thinking")
	if cmd == nil {
		t.Fatal("expected tick cmd when re-entering thinking")
	}
	if m.progressLabel != "thinking" {
		t.Fatalf("label=%q", m.progressLabel)
	}
	cmd = m.setProgress(progressThinking, "thinking")
	if cmd != nil {
		t.Fatal("already animating should not restart tick")
	}
}

func TestActiveToolNameLifecycle(t *testing.T) {
	m := newStreamTestModel()

	m.handleStreamToolCall(0, "tc0", "read_file")
	if m.activeToolName != "read_file" {
		t.Fatalf("activeToolName after announce=%q, want read_file", m.activeToolName)
	}
	m.handleStreamToolResult("tc0", "read_file", "ok", true)
	if m.activeToolName != "" {
		t.Fatalf("activeToolName after result=%q, want empty", m.activeToolName)
	}

	m.handleStreamToolCall(1, "tc1", "patch_file")
	if m.activeToolName != "patch_file" {
		t.Fatalf("activeToolName after second announce=%q, want patch_file", m.activeToolName)
	}
	m.resetStreamState(false)
	if m.activeToolName != "" {
		t.Fatalf("activeToolName after reset=%q, want empty", m.activeToolName)
	}
}

func TestRenderProgressInput(t *testing.T) {
	ta := textarea.New()
	ta.SetHeight(3)
	m := Model{
		streaming:     true,
		progressPhase: progressActive,
		spinner:       newProgressSpinner(),
		textarea:      ta,
	}
	got := m.renderProgressInput()
	if !strings.Contains(got, "streaming") {
		t.Fatalf("active render=%q", got)
	}
	// Steering: the strip is ONE row — the composer renders below it
	// (renderMainColumn combines them; SetSize reserves the strip's row
	// out of the textarea's, so the band height never changes).
	if h := lipgloss.Height(got); h != 1 {
		t.Fatalf("progress input height=%d, want 1 (single strip row)", h)
	}
	// A tool whose arguments are streaming in should be named, not generic.
	m.progressPhase = progressActive
	m.activeToolName = "patch_file"
	got = m.renderProgressInput()
	if !strings.Contains(got, "patch_file") || !strings.Contains(got, "preparing") {
		t.Fatalf("active tool render=%q", got)
	}
	m.progressPhase = progressTool
	m.progressLabel = "running read_file"
	got = m.renderProgressInput()
	if !strings.Contains(got, "read_file") {
		t.Fatalf("tool render=%q", got)
	}
	if h := lipgloss.Height(got); h != 1 {
		t.Fatalf("tool progress height=%d, want 1", h)
	}
}

// TestRenderProgressInputThinking pins the WAITING-for-the-model
// indicator: right after a submit (or a queued item's drain, or a
// tool-round boundary) the phase is progressThinking with no speed line
// yet — the strip must still render the spinner + label, or the busy
// indicator shows nothing at all (an empty row above the composer) until
// the first stats message arrives.
func TestRenderProgressInputThinking(t *testing.T) {
	m := Model{
		streaming:     true,
		progressPhase: progressThinking,
		spinner:       newProgressSpinner(),
		textarea:      textarea.New(),
	}
	got := m.renderProgressInput()
	if !strings.Contains(got, "thinking") {
		t.Fatalf("thinking render=%q, want spinner + label", stripANSI(got))
	}
	if h := lipgloss.Height(got); h != 1 {
		t.Fatalf("thinking strip height=%d, want 1", h)
	}
	// A custom label (the "resuming"/compaction variants) renders too.
	m.progressLabel = "resuming"
	if got := m.renderProgressInput(); !strings.Contains(got, "resuming") {
		t.Fatalf("custom label render=%q", stripANSI(got))
	}
	m.progressLabel = ""
}

func TestStreamStatsProgressLine(t *testing.T) {
	m := newStreamTestModel()

	// Not streaming: stats are ignored (a superseded turn's straggler).
	// The drop happens in handleMsg (dispatch), not in the handler, so go
	// through Update — calling the handler directly would bypass the gate.
	m.Update(streamStatsMsg{toksPerSec: 42})
	if m.streamSpeedLine != "" {
		t.Fatalf("stats applied while not streaming: %q", m.streamSpeedLine)
	}

	m.streaming = true
	m.progressPhase = progressActive
	m.Update(streamStatsMsg{toksPerSec: 42})
	if m.streamSpeedLine == "" {
		t.Fatal("streamSpeedLine not set after stats")
	}
	if got := m.renderProgressInput(); !strings.Contains(got, "42 tok/s") {
		t.Fatalf("progress line missing the rate: %q", got)
	}

	// The rate shows while a tool's arguments stream in too (the args
	// count toward the shared meter).
	m.activeToolName = "patch_file"
	if got := m.renderProgressInput(); !strings.Contains(got, "42 tok/s") {
		t.Fatalf("preparing line missing the rate: %q", got)
	}

	// The rate shows on the thinking indicator too (thinking tokens
	// feed the shared meter).
	m.activeToolName = ""
	m.progressPhase = progressThinking
	m.progressLabel = "thinking"
	m.spinner = newProgressSpinner()
	if got := m.renderProgressInput(); !strings.Contains(got, "42 tok/s") {
		t.Fatalf("thinking line missing the rate: %q", got)
	}

	// Turn end / cancel clears the rate with the progress.
	m.clearProgress()
	if m.streamSpeedLine != "" {
		t.Fatalf("streamSpeedLine not cleared: %q", m.streamSpeedLine)
	}
}

// TestStreamSignalProgressTransitions pins the progress-strip surfaces for
// the silent streaming windows (added after the "thinking spinner with no
// feedback" reports): mid-turn compaction flips the strip to a compacting
// indicator, a stream stall relabels the spinner only while nothing flows,
// and a stream retry labels the muted regeneration. Every later progress
// event restores the normal label, and idle turns drop all three signals.
func TestStreamSignalProgressTransitions(t *testing.T) {
	newBusy := func() *Model {
		return &Model{streaming: true, progressPhase: progressThinking, spinner: newProgressSpinner()}
	}

	t.Run("mid-turn compaction shows the compacting indicator", func(t *testing.T) {
		m := newBusy()
		m.handleStreamCompactingMsg()
		if m.progressPhase != progressCompacting {
			t.Fatalf("phase = %v, want progressCompacting", m.progressPhase)
		}
		if m.progressLabel != "compacting history" {
			t.Fatalf("label = %q", m.progressLabel)
		}
		if !m.progressAnimating() {
			t.Fatal("compacting must animate the spinner")
		}
		if got := m.renderProgressInput(); !strings.Contains(got, "compacting history") {
			t.Fatalf("strip missing the compacting label: %q", got)
		}
		// The round boundary after the compaction restores the normal
		// thinking label.
		m.handleStreamRoundEndMsg()
		if m.progressPhase != progressThinking || m.progressLabel != "thinking" {
			t.Fatalf("after round end: phase=%v label=%q", m.progressPhase, m.progressLabel)
		}
	})

	t.Run("stall relabels thinking only while nothing flows", func(t *testing.T) {
		m := newBusy()
		m.handleStreamStallMsg()
		if m.progressPhase != progressThinking || m.progressLabel != "still waiting on model" {
			t.Fatalf("phase=%v label=%q", m.progressPhase, m.progressLabel)
		}
		// A token resumes streaming: active phase, label cleared.
		m.handleStreamTokenMsg(streamTokenMsg{token: "x"})
		if m.progressPhase != progressActive {
			t.Fatalf("phase after token = %v, want progressActive", m.progressPhase)
		}
		// A stall while tokens flow is moot — it must not touch the
		// active phase.
		m.handleStreamStallMsg()
		if m.progressPhase != progressActive {
			t.Fatalf("stall during active phase = %v, want untouched", m.progressPhase)
		}
	})

	t.Run("retry relabels for the muted regeneration", func(t *testing.T) {
		m := newBusy()
		m.handleStreamRetryMsg(streamRetryMsg{reason: "stream interrupted"})
		if m.progressPhase != progressThinking || m.progressLabel != "retrying stream (stream interrupted)" {
			t.Fatalf("phase=%v label=%q", m.progressPhase, m.progressLabel)
		}
		if got := m.renderProgressInput(); !strings.Contains(got, "retrying stream") {
			t.Fatalf("strip missing the retry label: %q", got)
		}
		// The retry's regeneration is silent: the stall signal fires during
		// it and must NOT replace the reason with the generic wait label —
		// the reason is the explanation for exactly that silence.
		m.handleStreamStallMsg()
		if m.progressLabel != "retrying stream (stream interrupted)" {
			t.Fatalf("stall clobbered the retry label: %q", m.progressLabel)
		}
		// A token from the retry clears the retry state: a later stall
		// relabels normally.
		m.handleStreamTokenMsg(streamTokenMsg{token: "x"})
		m.handleStreamRoundStartMsg()
		m.handleStreamStallMsg()
		if m.progressLabel != "still waiting on model" {
			t.Fatalf("stall after resumed output = %q, want the wait label", m.progressLabel)
		}
	})

	t.Run("idle turns drop the signals", func(t *testing.T) {
		m := newBusy()
		m.streaming = false
		// Dispatched through Update: handleMsg owns the "not streaming → drop
		// this stream-render event" gate for every stream message.
		m.Update(streamCompactingMsg{})
		m.Update(streamStallMsg{})
		m.Update(streamRetryMsg{reason: "stream interrupted"})
		if m.progressPhase != progressThinking || m.progressLabel != "" {
			t.Fatalf("idle turn mutated the strip: phase=%v label=%q", m.progressPhase, m.progressLabel)
		}
	})
}
