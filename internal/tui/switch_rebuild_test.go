package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/modelinfo"
)

// newSwitchTestAgent builds a fully wired agent (real provider/executor/
// context manager over a dead endpoint) so switchToLive's idle-rebuild
// path — renderMessages + the async context-stats probe — runs for real.
//
// The provider gets a models.dev resolver with NO disk cache, and that is
// load-bearing, not tidiness: Warm() fills <workingDir>/.gogen/models.json
// from a detached goroutine (modelinfo.Resolver.refreshAsync →
// writeDiskCache) that nothing can join. With the default
// CachePath(workingDir), the registry fetch — a real network round trip to
// models.dev, so hundreds of milliseconds — outlives the test body, and its
// MkdirAll+rename of .gogen then lands while t.TempDir's RemoveAll is
// walking that tree. On Windows the walk fails with "The directory is not
// empty" (ERROR_DIR_NOT_EMPTY) — the intermittent build-windows failure of
// TestRebindBlockedWhileFocusedBusy — and when the write wins the race
// instead, the recreated directory is simply left behind. Dropping the
// cache path removes the write target, so no late writer is left to race:
// no teardown wait (there is nothing to wait on) and no sleep is needed.
// The catalog fetch itself is unchanged.
func newSwitchTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	dir := t.TempDir()
	p := llm.NewOpenAIProviderWithResolver("", "", "http://127.0.0.1:9/v1", dir, modelinfo.NewResolver(""))
	exec := agent.NewExecutorWithGuard(dir, agent.NewCommandGuard("", nil))
	cm := contextmgr.NewManager(p, contextmgr.Settings{})
	return agent.NewAgent(p, exec, cm)
}

// The idle path must fully rebind and rebuild without panicking: this
// exercises renderMessages + the async context-stats request against a
// real agent, which the registry-only tests skipped.
func TestSwitchToLiveIdleRebuild(t *testing.T) {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle

	a1 := newSwitchTestAgent(t)
	a2 := newSwitchTestAgent(t)

	m := &Model{agent: a1, lives: newLiveSessions(a1), viewport: vp, sessionID: a1.SessionID}
	m.lives.Add(a2, "second")
	if m.agent != a1 {
		t.Fatal("startup agent not focused")
	}

	cmd := m.switchToLive(1)
	if m.agent != a2 || m.sessionID != a2.SessionID {
		t.Fatalf("rebind failed: agent=%v sessionID=%q", m.agent != nil, m.sessionID)
	}
	if !m.lives.ByID("s2").focused.Load() || m.lives.Active() != m.lives.ByID("s2") {
		t.Fatal("focus gates wrong after idle switch")
	}
	// An empty session legitimately renders nothing; the assertion is that
	// the full rebuild path (renderMessages + the async context-stats
	// request) ran against a real agent without panicking or desyncing
	// registry state.
	if cmd == nil {
		t.Fatal("idle switch must request the target's context stats")
	}
	if m.lives.active != 1 || len(m.lives.sessions) != 2 {
		t.Fatalf("registry desynced: active=%d n=%d", m.lives.active, len(m.lives.sessions))
	}

	m.switchToLive(0)
	if m.agent != a1 || m.lives.active != 0 {
		t.Fatal("switch back failed")
	}
	_ = filepath.Join // keep import stable if helpers change
}

// Switching to an idle session must probe context stats OFF the Update
// thread: the switch returns the probe as a cmd (no synchronous
// ContextStats call — a synchronous probe would tokenize the whole
// restored history on the render loop), the message is tagged with the
// target session, and the indicator updates only when it lands.
func TestSwitchToLiveContextStatsProbeAsync(t *testing.T) {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	a1 := newSwitchTestAgent(t)
	a2 := newSwitchTestAgent(t)
	a2.SessionID = "second-id" // NewAgent leaves it empty; the probe tags it
	m := &Model{agent: a1, lives: newLiveSessions(a1), viewport: vp, sessionID: a1.SessionID}
	m.lives.Add(a2, "second")

	// Distinct pre-switch indicator so a synchronous (wrong) refresh would
	// be visible.
	m.contextStats = agent.TurnContext{Snapshot: contextmgr.ContextSnapshot{Used: 111, Limit: 1000}}
	m.contextLine = agent.FormatContextBrief(m.contextStats)

	cmd := m.switchToLive(1)
	if cmd == nil {
		t.Fatal("idle switch must request the target's context stats")
	}
	// The probe has not run on the Update thread: the indicator is still
	// the old session's until the async message lands.
	if m.contextStats.Snapshot.Used != 111 {
		t.Fatalf("indicator changed synchronously on switch: used=%d", m.contextStats.Snapshot.Used)
	}
	msg := cmd()
	stats, ok := msg.(contextStatsMsg)
	if !ok {
		t.Fatalf("switch cmd delivered %T, want contextStatsMsg", msg)
	}
	if stats.sid != a2.SessionID {
		t.Fatalf("probe tagged with session %q, want %q", stats.sid, a2.SessionID)
	}
	m.handleContextStatsMsg(stats)
	if m.contextLine != agent.FormatContextBrief(stats.stats) {
		t.Fatal("indicator did not update when the async probe landed")
	}
}

// A context-stats probe that lands after focus moved to another session
// must be dropped: without the guard, rapidly switching between large
// restored sessions could leave the previous session's numbers in the
// indicator until the next boundary refresh.
func TestContextStatsMsgDropsStaleSession(t *testing.T) {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	a1 := newSwitchTestAgent(t)
	a2 := newSwitchTestAgent(t)
	a1.SessionID = "first-id"
	a2.SessionID = "second-id"
	m := &Model{agent: a1, lives: newLiveSessions(a1), viewport: vp, sessionID: a1.SessionID}
	m.lives.Add(a2, "second")

	stale := contextStatsMsg{
		stats: agent.TurnContext{Snapshot: contextmgr.ContextSnapshot{Used: 999, Limit: 1000}},
		sid:   a1.SessionID,
	}
	m.handleContextStatsMsg(stale)
	if m.contextStats.Snapshot.Used != 999 {
		t.Fatal("setup: current-session probe must apply")
	}

	m.switchToLive(1)
	// The slow probe for a1 lands after the switch: dropped.
	m.handleContextStatsMsg(stale)
	if m.contextStats.Snapshot.Used != 999 {
		t.Fatalf("stale probe overwrote the focused session's indicator: used=%d", m.contextStats.Snapshot.Used)
	}
	// The focused session's own probe still applies.
	fresh := contextStatsMsg{
		stats: agent.TurnContext{Snapshot: contextmgr.ContextSnapshot{Used: 42, Limit: 1000}},
		sid:   a2.SessionID,
	}
	m.handleContextStatsMsg(fresh)
	if m.contextStats.Snapshot.Used != 42 {
		t.Fatal("focused session's probe must apply")
	}
}

// failSaveStore is a SessionPersister whose writes fail, used to seed a
// persist error the way a disk-full background session would.
type failSaveStore struct{ err error }

func (s *failSaveStore) Save(string, agent.SessionSnapshot) error                { return s.err }
func (s *failSaveStore) AppendMessages(string, agent.SessionSnapshot, int) error { return s.err }
func (s *failSaveStore) LoadInWorkingDir(_, _ string) (agent.SessionSnapshot, error) {
	return agent.SessionSnapshot{}, os.ErrNotExist
}
func (s *failSaveStore) List(string) ([]agent.SessionInfo, error) { return nil, nil }
func (s *failSaveStore) LatestID(string) (string, error)          { return "", os.ErrNotExist }
func (s *failSaveStore) Delete(_, _ string) error                 { return nil }
func (s *failSaveStore) TouchSession(_, _ string) error           { return s.err }

// A background session whose save failed must surface the warning when it
// gains focus: switchToLive consumes the persist error, like the
// resume/delete paths.
func TestSwitchToLiveSurfacesPersistError(t *testing.T) {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	a1 := newSwitchTestAgent(t)
	a2 := newSwitchTestAgent(t)
	a2.SessionID = "bg"
	a2.SessionStore = &failSaveStore{err: errors.New("disk full")}
	a2.FlushSession()
	if err := a2.ConsumePersistError(); err == nil {
		t.Fatal("setup: failing store did not seed a persist error")
	}
	a2.FlushSession() // re-seed: the check above consumed it

	m := &Model{agent: a1, lives: newLiveSessions(a1), viewport: vp, sessionID: a1.SessionID}
	m.lives.Add(a2, "background")

	m.switchToLive(1)
	if !strings.Contains(m.statusMsg, "failed to save session") || !strings.Contains(m.statusMsg, "disk full") {
		t.Fatalf("statusMsg = %q, want the background session's persist error", m.statusMsg)
	}
}
