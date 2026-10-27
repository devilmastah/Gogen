package tui

import (
	"strings"
	"testing"
)

// TestModelSwitchDuringStreamDefersTranscriptRebuild pins B23: rebuilding the
// transcript while a turn streams reads Messages from under the stream
// goroutine and replaces chatLines, leaving the live assistant/thinking line
// indices pointing at the wrong lines. The switch is reported on the status
// line and the transcript is rebuilt at the turn boundary instead.
func TestModelSwitchDuringStreamDefersTranscriptRebuild(t *testing.T) {
	m := newQueueTestModel()
	m.chatLines = []string{"line-before-switch"}
	s := m.focusedSession()
	m.streaming = true

	model, _ := m.handleModelSwitchMsg(modelSwitchMsg{agent: m.agent, out: "model: gpt-4o"})
	m2 := model.(*Model)
	if len(m2.chatLines) != 1 || m2.chatLines[0] != "line-before-switch" {
		t.Fatalf("chatLines rebuilt mid-stream: %#v", m2.chatLines)
	}
	if !strings.Contains(m2.statusMsg, "gpt-4o") {
		t.Fatalf("statusMsg = %q, want the switch notice", m2.statusMsg)
	}
	if !s.transcriptStale {
		t.Fatal("transcript not marked stale; the turn-end rebuild will skip it")
	}
}
