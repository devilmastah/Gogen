package tui

import (
	"strings"
	"testing"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/server"
)

func TestCmdContextMode(t *testing.T) {
	m := dragModel(t)
	cfg := config.Defaults()
	m.cfg = &cfg
	m.workspace = server.NewWorkspaceForHost(m.agent, &cfg)
	setThreshold := func(v float64) {
		m.agent.Context.UpdateSettingsFunc(func(s contextmgr.Settings) contextmgr.Settings {
			s.CompactThreshold = v
			return s
		})
	}
	setThreshold(0.85)
	other := newSwitchTestAgent(t)
	m.lives.sessions = append(m.lives.sessions, &liveSession{id: "s2", label: "other", agent: other})
	if m.agent.Context.DeepDive() {
		t.Fatal("deep dive default is on")
	}

	m.dispatchCommand("/contextmode")
	if !strings.Contains(strings.Join(m.chatLines, "\n"), "Context mode: default") {
		t.Fatalf("bare command did not show the mode: %s", strings.Join(m.chatLines, "\n"))
	}
	if m.agent.Context.DeepDive() {
		t.Fatal("bare command changed the mode")
	}

	handled, quit, cmd := m.dispatchCommand("/contextmode deep_dive")
	if !handled || quit || cmd != nil {
		t.Fatalf("deep_dive: handled=%v quit=%v cmd=%v", handled, quit, cmd != nil)
	}
	if !m.agent.Context.DeepDive() || !other.Context.DeepDive() {
		t.Fatalf("deep dive not applied: focused=%v other=%v", m.agent.Context.DeepDive(), other.Context.DeepDive())
	}
	if m.cfg.ContextMode != config.ContextModeDeepDive {
		t.Fatalf("config context_mode = %q, want deep_dive for /save-config", m.cfg.ContextMode)
	}
	if got := m.workspace.GetRuntimeConfig().ContextMode; got != config.ContextModeDeepDive {
		t.Fatalf("runtime config context_mode = %q, want deep_dive for new sessions", got)
	}
	if strings.Contains(strings.Join(m.chatLines, "\n"), "auto-compaction is off") {
		t.Fatal("auto-compaction note shown while auto-compaction is on")
	}
	if !strings.Contains(strings.Join(m.chatLines, "\n"), "Context mode: deep_dive (all live sessions") {
		t.Fatalf("confirmation missing: %s", strings.Join(m.chatLines, "\n"))
	}

	m.dispatchCommand("/contextmode maybe")
	if !m.agent.Context.DeepDive() || m.cfg.ContextMode != config.ContextModeDeepDive {
		t.Fatal("invalid value changed the setting")
	}
	if !strings.Contains(strings.Join(m.chatLines, "\n"), "use default or deep_dive") {
		t.Fatalf("invalid-value line missing: %s", strings.Join(m.chatLines, "\n"))
	}

	m.dispatchCommand("/contextmode default")
	if m.agent.Context.DeepDive() || other.Context.DeepDive() || m.cfg.ContextMode != config.DefaultContextMode {
		t.Fatal("default did not clear deep dive")
	}

	setThreshold(0)
	m.dispatchCommand("/contextmode deep_dive")
	if !strings.Contains(strings.Join(m.chatLines, "\n"), "auto-compaction is off") {
		t.Fatalf("compact_threshold 0 note missing: %s", strings.Join(m.chatLines, "\n"))
	}
}
