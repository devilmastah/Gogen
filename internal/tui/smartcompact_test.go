package tui

import (
	"strings"
	"testing"
)

func TestCmdSmartCompact(t *testing.T) {
	m := dragModel(t)
	if m.agent.Context.SmartCompact() {
		t.Fatal("smart compact default is on")
	}
	handled, quit, cmd := m.dispatchCommand("/smartcompact on")
	if !handled || quit || cmd != nil {
		t.Fatalf("on: handled=%v quit=%v cmd=%v", handled, quit, cmd != nil)
	}
	if !m.agent.Context.SmartCompact() {
		t.Fatal("smart compact stayed off")
	}
	joined := strings.Join(m.chatLines, "\n")
	if !strings.Contains(joined, "Smart compact: on") || !strings.Contains(joined, "all live sessions") {
		t.Fatalf("confirmation missing: %s", joined)
	}

	handled, _, _ = m.dispatchCommand("/smartcompact off")
	if !handled || m.agent.Context.SmartCompact() {
		t.Fatal("off did not clear smart compact")
	}

	m.dispatchCommand("/smartcompact")
	if !m.agent.Context.SmartCompact() {
		t.Fatal("bare command did not toggle on")
	}

	before := m.agent.Context.SmartCompact()
	m.dispatchCommand("/smartcompact maybe")
	if m.agent.Context.SmartCompact() != before {
		t.Fatal("invalid value changed the setting")
	}
	if !strings.Contains(strings.Join(m.chatLines, "\n"), "use on or off") {
		t.Fatalf("invalid-value line missing: %s", strings.Join(m.chatLines, "\n"))
	}
}
