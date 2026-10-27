package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// ── A1/A3: close semantics ──

func TestLiveSessionsClose(t *testing.T) {
	ls := newLiveSessions(&agent.Agent{})
	bg := ls.Add(&agent.Agent{}, "bg")

	t.Run("cannot close focused", func(t *testing.T) {
		if err := ls.Close(0); err == nil {
			t.Fatal("expected focused-close error")
		}
	})
	t.Run("cannot close while streaming", func(t *testing.T) {
		bg.streaming = true
		if err := ls.Close(1); err == nil {
			t.Fatal("expected streaming-close error")
		}
		bg.streaming = false
	})
	t.Run("close flushes, detaches, keeps active index", func(t *testing.T) {
		if err := ls.Close(1); err != nil {
			t.Fatalf("close: %v", err)
		}
		if len(ls.sessions) != 1 || ls.ByID("s2") != nil {
			t.Fatal("session not detached")
		}
		if ls.active != 0 || ls.Active().id != "s1" {
			t.Fatalf("active desynced: %d", ls.active)
		}
	})
}

// ── A4: close/quit must not reshuffle the saved-session list ──

// Regression for the sidebar-order shuffle on close/quit: Close and
// flushAndQuit used to force a full save (FlushSession) on every session
// they swept, re-stamping each one's store Updated timestamp with ~now.
// Store.List/LatestID order by that timestamp, so a merely-closed CLEAN
// session jumped to the top of the saved list and the focused session was
// demoted on quit — instead of the intended semantics: the session that
// received the last message is the newest. Both paths now flush through
// FlushPending, which writes only dirty sessions, so clean sessions keep
// their earned timestamp (the TUI twin of the web ShutdownSessions /
// evictRuntime fixes).
func TestCloseAndQuitPreserveSavedSessionOrdering(t *testing.T) {
	m := newSidebarFullModel(t)
	store := m.agent.SessionStore.(*session.Store)
	dir := m.agent.WorkingDir

	now := time.Now()
	saveAt := func(id string, ago time.Duration) {
		t.Helper()
		if err := store.Save(id, agent.SessionSnapshot{
			WorkingDir: dir,
			Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		if err := store.SetUpdatedAt(dir, id, now.Add(-ago).UTC()); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}
	// "newest" received the last message a minute ago; "old" is a
	// background pane restored from a 2 h-old save.
	saveAt("newest", time.Minute)
	saveAt("old", 2*time.Hour)
	topIsNewest := func(what string) {
		t.Helper()
		list, err := store.List(dir)
		if err != nil {
			t.Fatalf("list after %s: %v", what, err)
		}
		if len(list) == 0 || list[0].ID != "newest" {
			got := "(none)"
			if len(list) > 0 {
				got = list[0].ID
			}
			t.Fatalf("List[0] after %s = %s, want newest (close/quit must not reshuffle recency)", what, got)
		}
	}
	topIsNewest("setup")

	// Closing a CLEAN background pane must not write: it received nothing
	// since its last save, so its timestamp — and list position — must
	// survive untouched.
	bg := newSwitchTestAgent(t)
	bg.SessionStore = m.agent.SessionStore
	bg.WorkingDir = dir
	bg.SessionID = "old"
	m.lives.Add(bg, "old")
	if err := m.lives.Close(1); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, want := store.UpdatedAt(dir, "old"), now.Add(-2*time.Hour).UTC(); !got.Equal(want) {
		t.Fatalf("clean close re-stamped Updated = %v, want %v", got, want)
	}
	topIsNewest("clean close")

	// Quit must not reshuffle either: the fan-out used to stamp every
	// session in sweep order, demoting the focused one on restart. The
	// focused session here was never saved (clean) — FlushPending must
	// leave it unsaved, not create a fresh "just updated" entry.
	m.flushAndQuit()
	topIsNewest("quit")
	if got, want := store.UpdatedAt(dir, "old"), now.Add(-2*time.Hour).UTC(); !got.Equal(want) {
		t.Fatalf("quit re-stamped Updated = %v, want %v", got, want)
	}
	list, err := store.List(dir)
	if err != nil {
		t.Fatalf("list after quit: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("store has %d sessions after quit, want 2 (a clean never-saved session must not be written)", len(list))
	}
}

// ── A2: owner tagging drops switch-boundary stragglers ──

func TestStreamEventSidGuard(t *testing.T) {
	m := &Model{lives: newLiveSessions(&agent.Agent{})}
	m.lives.Add(&agent.Agent{}, "bg")

	t.Run("attribution extraction", func(t *testing.T) {
		sa, ok := any(streamTokenMsg{token: "x", sid: "s2", seq: 7}).(streamAttributed)
		if !ok {
			t.Fatal("stream token must implement streamAttributed")
		}
		if sid, seq := sa.attribution(); sid != "s2" || seq != 7 {
			t.Fatalf("got %q %d, want s2 7", sid, seq)
		}
		if _, ok := any(tea.WindowSizeMsg{}).(streamAttributed); ok {
			t.Fatal("non-stream msg must report ok=false")
		}
	})

	t.Run("straggler from backgrounded owner is dropped", func(t *testing.T) {
		m.lives.Switch(0) // s1 focused
		before := len(m.chatLines)
		// A chunk sent by s2 while it was focused, delivered after the switch.
		mod, _ := m.Update(streamTokenMsg{token: "leak", sid: "s2"})
		got := mod.(*Model)
		if len(got.chatLines) != before {
			t.Fatal("straggler leaked into focused transcript")
		}
	})

	t.Run("focused session events still render", func(t *testing.T) {
		m.resetStreamState(false)
		mod, _ := m.Update(streamStartMsg{sid: "s1"})
		got := mod.(*Model)
		_ = got // must not panic / must be processed without drop
		if _, ok := any(streamStartMsg{sid: "s1"}).(streamAttributed); !ok {
			t.Fatal("start should be tagged")
		}
	})
}

// The ownsStream guard precedes the routing switch in Update; terminal
// msgs are deliberately excluded from streamEventSid so background
// finalization still routes. This pins that exclusion.
func TestTerminalMsgsBypassStragglerGuard(t *testing.T) {
	m := dragModel(t)
	m.lives.Add(&agent.Agent{}, "bg")
	m.lives.ByID("s2").streaming = true

	mod, _ := m.Update(endOfStream("s2", 0))
	got := mod.(*Model)
	if got.lives.ByID("s2") == nil || got.lives.ByID("s2").streaming {
		t.Fatal("Update dropped/rerouted a background terminal message")
	}
}

// ── A5: prefs persistence round-trip ──

func TestUIPrefsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := uiPrefsPath(dir)

	t.Run("missing file yields defaults, not found", func(t *testing.T) {
		p, found := loadUIPrefs(filepath.Join(dir, "empty"))
		if found || p.SidebarVisible || p.SidebarWidth != defaultSidebarWidth {
			t.Fatalf("defaults wrong: found=%v %+v", found, p)
		}
	})

	t.Run("save then load preserves state", func(t *testing.T) {
		saveUIPrefs(dir, uiPrefs{SidebarVisible: true, SidebarWidth: 34})
		p, found := loadUIPrefs(dir)
		if !found || !p.SidebarVisible || p.SidebarWidth != 34 {
			t.Fatalf("round-trip lost state: found=%v %+v", found, p)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("prefs file missing: %v", err)
		}
	})

	t.Run("corrupt file falls back to defaults", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		p, found := loadUIPrefs(dir)
		if found || p.SidebarVisible || p.SidebarWidth != defaultSidebarWidth {
			t.Fatalf("corrupt handling wrong: found=%v %+v", found, p)
		}
	})
}

// Startup policy (web parity): ALWAYS open; only the width persists.
func TestResolveSidebarStart(t *testing.T) {
	dir := t.TempDir()

	t.Run("first run opens at default width", func(t *testing.T) {
		p := resolveSidebarStart(filepath.Join(dir, "fresh"))
		if !p.SidebarVisible || p.SidebarWidth != defaultSidebarWidth {
			t.Fatalf("first run = %+v, want open/default", p)
		}
	})

	t.Run("saved close is ignored, width kept", func(t *testing.T) {
		saveUIPrefs(dir, uiPrefs{SidebarVisible: false, SidebarWidth: 30})
		p := resolveSidebarStart(dir)
		if !p.SidebarVisible {
			t.Fatal("panel must open by default (web parity)")
		}
		if p.SidebarWidth != 30 {
			t.Fatalf("width preference lost: %+v", p)
		}
	})

	t.Run("explicit open width is honored", func(t *testing.T) {
		saveUIPrefs(dir, uiPrefs{SidebarVisible: true, SidebarWidth: 38})
		p := resolveSidebarStart(dir)
		if !p.SidebarVisible || p.SidebarWidth != 38 {
			t.Fatalf("open choice lost: %+v", p)
		}
	})
}

// ── Critical: m.streaming is the FOCUSED session's mirror ──

// Regression: switching away from a streaming session must unlock input
// (m.streaming=false on the idle target), and a background finish must
// never touch the focused mirror.
func TestSwitchSyncsStreamingMirror(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	m.lives.Add(a2, "second")

	t.Run("leave mid-turn session unlocks the idle target", func(t *testing.T) {
		s1 := m.lives.Active() // s1
		s1.streaming = true
		m.streaming = true // turn started while s1 was focused

		cmd := m.switchToLive(1)
		_ = cmd
		if m.streaming {
			t.Fatal("m.streaming stayed true after leaving a backgrounded stream — input would be bricked")
		}
		if !m.lives.ByID("s1").streaming {
			t.Fatal("background session must keep streaming")
		}
		if m.progressPhase != progressHidden {
			t.Fatalf("progress not cleared: %v", m.progressPhase)
		}
	})

	t.Run("join locks the mirror again", func(t *testing.T) {
		// Start from idle s1 focus, then join streaming s2.
		m.switchToLive(0)
		m.streaming = false
		m.clearProgress()
		m.lives.ByID("s2").streaming = true

		m.switchToLive(1)

		if !m.streaming {
			t.Fatal("joining a streaming session must set m.streaming")
		}
		if m.progressPhase == progressHidden {
			t.Fatal("joined session must show progress")
		}
	})

	t.Run("background finish leaves focused mirror alone", func(t *testing.T) {
		// s1's turn from the first subtest ended meanwhile; focus it while
		// s2 keeps streaming; then s2 finishes.
		m.lives.ByID("s1").streaming = false
		m.switchToLive(0)
		if m.streaming {
			t.Fatal("precondition: focus on idle s1")
		}

		mod, _ := m.handleTurnFinishedMsg("s2", 0, nil)
		got := mod.(*Model)
		if got.lives.ByID("s2").streaming {
			t.Fatal("background flags not cleared")
		}
		if got.streaming {
			t.Fatal("background finish corrupted the focused mirror")
		}
	})
}

// ── Per-session progress state: the indicator survives a focus switch ──

// Regression: switchToLive used to hardcode "thinking" when joining a
// streaming session, so a join mid tool-execution showed "thinking…" for
// the rest of the tool run. The phase the session recorded must be
// restored, and the tick decision must follow the OLD focused session's
// animation state.
func TestSwitchRestoresSessionProgressPhase(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	m.lives.Add(a2, "second")

	t.Run("join mid tool-execution keeps the tool phase", func(t *testing.T) {
		s2 := m.lives.ByID("s2")
		s2.streaming = true
		s2.progressPhase = progressTool
		s2.progressLabel = "running execute_command"
		s2.activeTool = "execute_command"

		cmd := m.switchToLive(1)
		if m.progressPhase != progressTool || m.progressLabel != "running execute_command" {
			t.Fatalf("phase = %v %q, want the session's tool phase", m.progressPhase, m.progressLabel)
		}
		if m.activeToolName != "execute_command" {
			t.Fatalf("activeToolName = %q, want execute_command", m.activeToolName)
		}
		// The old focused session was idle: the spinner must (re)start.
		// The batch carries tick + context probe (a two-cmd batch
		// delivers a BatchMsg of both; a lone probe would mean the tick
		// was dropped).
		batch, ok := cmd().(tea.BatchMsg)
		if !ok || len(batch) != 2 {
			t.Fatalf("joining an animating phase from idle must return [tick, probe], got %T", cmd)
		}
	})

	t.Run("leave and rejoin keeps the phase", func(t *testing.T) {
		m.switchToLive(0)
		if m.progressPhase != progressHidden {
			t.Fatalf("idle target must clear the mirror: %v", m.progressPhase)
		}
		m.switchToLive(1)
		if m.progressPhase != progressTool || m.progressLabel != "running execute_command" {
			t.Fatalf("rejoin lost the phase: %v %q", m.progressPhase, m.progressLabel)
		}
	})

	t.Run("no double tick when the old session was animating", func(t *testing.T) {
		// Both sessions streaming: the tick loop already runs, so the
		// switch must not schedule another start.
		m.lives.ByID("s1").streaming = true
		m.lives.ByID("s1").progressPhase = progressThinking
		m.switchToLive(0) // back to s1 (animating)
		// The batch must carry ONLY the context probe: a spinner tick
		// would double the running animation loop (the pre-fix
		// regression). A single-cmd batch is returned directly, so the
		// probe's message type is the discriminator.
		if cmd := m.switchToLive(1); cmd == nil {
			t.Fatal("switch must request the target's context stats")
		} else if _, ok := cmd().(contextStatsMsg); !ok {
			t.Fatalf("switch between two animating sessions returned %T, want the lone context probe (a tick would double the spinner)", cmd)
		}
		m.lives.ByID("s1").streaming = false
		m.lives.ByID("s1").progressPhase = progressHidden
	})

	t.Run("background end clears the recorded phase", func(t *testing.T) {
		// s1 focused (idle), s2 streaming; s2's turn ends in the background.
		m.switchToLive(0)
		s2 := m.lives.ByID("s2")
		s2.streaming = true
		s2.turnSeq = 1
		s2.progressPhase = progressTool
		s2.progressLabel = "running execute_command"

		m.handleTurnFinishedMsg("s2", 1, nil)
		if s2.progressPhase != progressHidden {
			t.Fatalf("background end must clear the session phase: %v", s2.progressPhase)
		}
		// Joining the now-idle session takes the idle path.
		m.switchToLive(1)
		if m.streaming || m.progressPhase != progressHidden {
			t.Fatalf("idle join must stay idle: streaming=%v phase=%v", m.streaming, m.progressPhase)
		}
	})
}

// Regression: the join restores the session's recorded indicator state
// captured BEFORE the rebind. The drained buffer is now discarded (its
// events are older than the join snapshot), so the indicator keeps the
// restored phase until the next LIVE event updates it.
func TestJoinRestoresRecordedPhase(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	s2 := m.lives.Add(a2, "second")
	s2.streaming = true
	s2.progressPhase = progressTool
	s2.progressLabel = "running execute_command"
	s2.activeTool = "execute_command"
	s2.enqueue(streamToolResultMsg{sid: s2.id, seq: 1})
	s2.enqueue(streamTokenMsg{token: "hi", sid: s2.id})

	m.switchToLive(1)
	// The buffered events are discarded (older than the join snapshot):
	// the indicator keeps the session's recorded phase, not a hardcoded
	// "thinking" and not a replay-driven phase.
	if m.progressPhase != progressTool || m.progressLabel != "running execute_command" {
		t.Fatalf("phase = %v %q, want the restored tool phase", m.progressPhase, m.progressLabel)
	}
	if m.activeToolName != "execute_command" {
		t.Fatalf("activeToolName = %q, want the restored tool name", m.activeToolName)
	}
	if len(s2.pending) != 0 {
		t.Fatalf("pending not drained: %d left", len(s2.pending))
	}
}

// ── Terminal attribution: successful turns MUST finalize ──

// Regression for the silent-brick bug: a success terminal built without
// its sid made ByID("") miss, so handleTurnFinishedMsg early-returned and
// the focused session never left streaming state.
func TestTerminalAttribution(t *testing.T) {
	t.Run("endOfStream attributes", func(t *testing.T) {
		msg := endOfStream("s7", 4)
		end, ok := msg.(streamEndMsg)
		if !ok || end.sid != "s7" || end.seq != 4 {
			t.Fatalf("endOfStream = %#v", msg)
		}
	})
	t.Run("failOfStream attributes", func(t *testing.T) {
		msg := failOfStream("s7", 4, errFake{})
		e, ok := msg.(streamErrorMsg)
		if !ok || e.sid != "s7" || e.seq != 4 || e.err == nil {
			t.Fatalf("failOfStream = %#v", msg)
		}
	})

	t.Run("attributed success finalizes the focused session", func(t *testing.T) {
		m := dragModel(t) // single live session s1, focused
		s1 := m.lives.Active()
		s1.streaming = true
		m.streaming = true

		mod, _ := m.Update(endOfStream("s1", 0))
		got := mod.(*Model)
		if got.streaming || got.lives.ByID("s1").streaming {
			t.Fatal("successful turn did not finalize streaming state")
		}
		if got.progressPhase != progressHidden {
			t.Fatalf("progress stuck: %v", got.progressPhase)
		}
	})
}

// ── Turn epoch: superseded turns' stragglers must not touch the new turn ──

// Regression: cancel + resubmit leaves the old turn's goroutine unwinding.
// Its stragglers (tokens AND the context-canceled terminal) carry the old
// turnSeq; without the epoch guard they clear the new turn's streaming
// flag, clobber its cancel func, wipe its stream buffers, and surface a
// spurious "context canceled" error line.
func TestStaleTurnStragglersDropped(t *testing.T) {
	m := dragModel(t)
	s1 := m.lives.Active()

	// Turn 1 in flight.
	s1.streaming = true
	m.streaming = true
	s1.turnSeq = 1
	var cancel1Called, cancel2Called bool
	cancel1 := func() { cancel1Called = true }
	s1.cancel = cancel1

	// User cancels (the key handler clears the mirror immediately) and
	// resubmits: turn 2 starts on the same session while turn 1 unwinds.
	m.streaming = false
	m.resetStreamState(false)
	s1.turnSeq = 2
	m.streaming = true
	cancel2 := func() { cancel2Called = true }
	s1.cancel = cancel2

	t.Run("stale token is dropped", func(t *testing.T) {
		before := len(m.chatLines)
		mod, _ := m.Update(streamTokenMsg{token: "stale", sid: "s1", seq: 1})
		got := mod.(*Model)
		if len(got.chatLines) != before {
			t.Fatal("stale token leaked into the new turn's transcript")
		}
	})

	t.Run("stale terminal does not clobber the new turn", func(t *testing.T) {
		mod, _ := m.Update(failOfStream("s1", 1, context.Canceled))
		got := mod.(*Model)
		if !got.streaming {
			t.Fatal("stale terminal cleared the new turn's streaming flag")
		}
		if got.lives.Active().streaming != true {
			t.Fatal("stale terminal cleared the session's streaming flag")
		}
		// Invoke the session's cancel func: it must still be turn 2's
		// (a stale terminal would have replaced it with nil).
		got.lives.Active().cancel()
		if !cancel2Called || cancel1Called {
			t.Fatal("stale terminal clobbered the new turn's cancel func")
		}
		if joined := strings.Join(got.chatLines, "\n"); strings.Contains(joined, "context canceled") {
			t.Fatal("stale cancel error surfaced as a turn error")
		}
	})

	t.Run("fresh terminal still finalizes", func(t *testing.T) {
		mod, _ := m.Update(endOfStream("s1", 2))
		got := mod.(*Model)
		if got.streaming || got.lives.Active().streaming {
			t.Fatal("fresh terminal must finalize the current turn")
		}
		if got.lives.Active().cancel != nil {
			t.Fatal("finalized turn must release its cancel func")
		}
	})
}

// ── B8: saved sessions render as rows in the unified list ──

func TestRenderSidebarSavedRows(t *testing.T) {
	m := newSidebarTestModel(100)
	m.height = 30
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	m.savedCache = []agent.SessionInfo{{ID: "abc123", Label: "old task"}}
	main := m.renderMainColumn()
	out := stripANSI(m.renderSidebar(strings.Count(main, "\n") + 1))
	// The saved row shows its label (id when unlabeled); its meta line
	// carries no live state (the live row above it does).
	lines := strings.Split(out, "\n")
	found := false
	for i, l := range lines {
		if strings.Contains(l, "old task") {
			found = true
			if i+1 < len(lines) {
				meta := lines[i+1]
				for _, st := range []string{"active", "open", "responding"} {
					if strings.Contains(meta, st) {
						t.Fatalf("saved row meta carries live state %q: %q", st, meta)
					}
				}
			}
			break
		}
	}
	if !found {
		t.Fatalf("saved row missing:\n%s", out)
	}
}

// ── Phase 3a: per-session /compact state (mirrors the streaming pattern) ──

// Regression: the compacting flags used to live on the Model and describe
// the FOCUSED session's /compact, but survived a focus switch — input on
// the NEW session was silently blocked, the spinner kept animating for the
// other session's work, and ctrl+c on the new session cancelled the OLD
// session's compaction. The flags now live on the owning liveSession;
// m.compacting is the focused-session mirror, synced in switchToLive.
func TestSwitchDuringCompact(t *testing.T) {
	m := dragModel(t)
	m.keys = DefaultKeyMap // dragModel leaves the keymap zeroed
	m.ctx = context.Background()
	a2 := newSwitchTestAgent(t)
	m.lives.Add(a2, "second")
	s1 := m.lives.ByID("s1")

	t.Run("switch away: B unlocks, A keeps compacting", func(t *testing.T) {
		s1.compacting = true
		m.compacting = true

		m.switchToLive(1)
		if m.compacting {
			t.Fatal("focused mirror must clear on the idle target — B's input would be bricked")
		}
		if !s1.compacting {
			t.Fatal("background session must keep compacting")
		}
		// B accepts input immediately: submit is not swallowed.
		m.textarea.SetValue("hello")
		_, cmd, ok := m.handleSubmitKey(keyMsg("enter"))
		if !ok || cmd == nil {
			t.Fatalf("B must accept input: ok=%v cmd=%v", ok, cmd != nil)
		}
		// Finalize the turn B just started (clean slate for the next case).
		m.handleTurnFinishedMsg("s2", m.lives.ByID("s2").turnSeq, nil)
	})

	t.Run("ctrl+c on idle B leaves A's compaction running", func(t *testing.T) {
		m.switchToLive(1)
		s1.compacting = true
		var cancelled bool
		s1.compactCancel = func() { cancelled = true }

		_, cmd, _ := m.handleCancelKey()
		if cmd == nil {
			t.Fatal("ctrl+c on idle B must quit (B has nothing to cancel)")
		}
		if cancelled {
			t.Fatal("ctrl+c on B cancelled A's compaction")
		}
		if !s1.compacting {
			t.Fatal("A's compaction flags must be untouched")
		}
		m.quitting = false
	})

	t.Run("A's compactResultMsg clears A's flags, not B's", func(t *testing.T) {
		m.switchToLive(1) // B focused, idle
		s1.compacting = true
		s1.compactCancel = func() {}

		mod, _ := m.Update(compactResultMsg{agent: s1.agent, err: nil})
		got := mod.(*Model)
		if got.lives.ByID("s1").compacting || got.lives.ByID("s1").compactCancel != nil {
			t.Fatal("owning session's flags not cleared")
		}
		if got.compacting {
			t.Fatal("idle B's mirror must not be set")
		}
		if got.statusMsg != "✓ History compacted" {
			t.Fatalf("statusMsg = %q, want the attributed notice", got.statusMsg)
		}
		if strings.Contains(strings.Join(got.chatLines, "\n"), "History compacted (") {
			t.Fatal("background result must not rebuild B's transcript")
		}
	})

	t.Run("switch onto a compacting session restores the mirror", func(t *testing.T) {
		m.switchToLive(1)
		s1.compacting = true
		s1.compactCancel = func() {}

		// The batch carries tick + context probe (a two-cmd batch
		// delivers a BatchMsg of both; a lone probe would mean the tick
		// was dropped).
		cmd := m.switchToLive(0)
		batch, ok := cmd().(tea.BatchMsg)
		if !ok || len(batch) != 2 {
			t.Fatalf("joining a compacting session from idle must return [tick, probe], got %T", cmd)
		}
		if !m.compacting {
			t.Fatal("focused mirror must restore on join")
		}
		// Steering: input is never blocked on the compacting session —
		// submit QUEUES (the drain waits for the compaction gate).
		m.textarea.SetValue("x")
		_, _, ok = m.handleSubmitKey(keyMsg("enter"))
		if !ok {
			t.Fatal("submit must be consumed while compacting (it queues)")
		}
		if q := m.lives.Active().steerQ.Items(); len(q) != 1 || q[0].Text != "x" {
			t.Fatalf("queue = %v, want the queued text", q)
		}
		m.lives.Active().steerQ = nil
		// ctrl+c cancels THIS session's compaction.
		var cancelled bool
		s1.compactCancel = func() { cancelled = true }
		_, cmd, _ = m.handleCancelKey()
		if cmd != nil {
			t.Fatal("cancel during compact must not quit")
		}
		if !cancelled || m.compacting || s1.compacting {
			t.Fatalf("compaction not cancelled: cancelled=%v mirror=%v session=%v", cancelled, m.compacting, s1.compacting)
		}
	})
}

// ── Keybinding regressions ──

// Regression: [ / ] were global hotkeys (sidebar resize) that shadowed
// the viewport's prompt-rail jump — the documented "[ / ] previous/next
// prompt" binding was dead code in handleViewportKey.
func TestViewportBracketKeysJumpPrompts(t *testing.T) {
	m := newSidebarFullModel(t)
	m.focus = FocusViewport
	// Two prompts separated by enough filler that the viewport scrolls
	// (the jump is a SetYOffset, which clamps to the content height).
	for i := 0; i < 25; i++ {
		m.appendChatLine(DimStyle.Render(fmt.Sprintf("filler %d", i)))
	}
	m.appendChatLine(UserStyle.Render(userLabel) + " prompt A")
	for i := 0; i < 25; i++ {
		m.appendChatLine(DimStyle.Render(fmt.Sprintf("filler %d", i)))
	}
	m.appendChatLine(UserStyle.Render(userLabel) + " prompt B")
	if len(m.tocAnchors) != 2 {
		t.Fatalf("tocAnchors = %d, want 2", len(m.tocAnchors))
	}
	// appendChatLine pins the viewport to the bottom; start at the top
	// so the first "]" has a prompt to jump to.
	m.viewport.SetYOffset(0)
	w := m.sidebarWidth

	// "]" in viewport focus jumps to the next prompt — it must not
	// resize the panel or leave the viewport.
	m.handleKeyMsg(tea.KeyPressMsg{Code: ']'})
	if m.sidebarWidth != w {
		t.Fatalf("] must not resize the sidebar in viewport focus: %d -> %d", w, m.sidebarWidth)
	}
	if m.focus != FocusViewport {
		t.Fatalf("] must not leave viewport focus: %v", m.focus)
	}
	if got, want := m.viewport.YOffset, m.tocAnchors[0].line; got != want {
		t.Fatalf("] must jump to the next prompt: YOffset = %d, want %d", got, want)
	}

	// In sidebar focus the same key resizes the panel (its own handler).
	m.focus = FocusSidebar
	m.handleKeyMsg(tea.KeyPressMsg{Code: ']'})
	if m.sidebarWidth != w+4 {
		t.Fatalf("] in sidebar focus must resize the panel: %d, want %d", m.sidebarWidth, w+4)
	}
}

// Regression: the viewport's printable-typing fall-through ran BEFORE
// the scroll switch, so j/k/g/G typed into the input instead of
// scrolling — the documented "j/k scroll line" binding was dead code.
func TestViewportScrollKeysNotTyped(t *testing.T) {
	m := newSidebarFullModel(t)
	m.focus = FocusViewport
	for i := 0; i < 30; i++ {
		m.appendChatLine(DimStyle.Render(fmt.Sprintf("line %d", i)))
	}
	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset
	if bottom == 0 {
		t.Fatal("test setup: viewport must be scrollable")
	}

	m.handleKeyMsg(tea.KeyPressMsg{Code: 'k'})
	if m.focus != FocusViewport {
		t.Fatalf("k must stay in viewport focus, got %v", m.focus)
	}
	if m.viewport.YOffset != bottom-1 {
		t.Fatalf("k must scroll up one line: %d, want %d", m.viewport.YOffset, bottom-1)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("k must not type into the input: %q", got)
	}

	m.handleKeyMsg(tea.KeyPressMsg{Code: 'j'})
	if m.viewport.YOffset != bottom {
		t.Fatalf("j must scroll back down: %d, want %d", m.viewport.YOffset, bottom)
	}

	// A non-scroll printable still switches to the input and is passed
	// through (Focus is synchronous in bubbles v2, so the character lands
	// in the same update).
	m.handleKeyMsg(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.focus != FocusInput {
		t.Fatalf("x must switch to input focus, got %v", m.focus)
	}
	if got := m.textarea.Value(); got != "x" {
		t.Fatalf("x must type into the input: %q", got)
	}
}

// Regression: the sessions modal's "d" used to delete a session
// permanently in one keystroke; the sidebar path confirms. Both must
// go through ModalConfirm, and the sessions list stays open behind the
// dialog (confirmRestore).
func TestSessionsModalDeleteConfirms(t *testing.T) {
	m := newSidebarFullModel(t)
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.refreshSavedSessions()
	m.sessionList = m.savedCache
	m.sessionCursor = 0
	m.modal = ModalSessions

	// "d" opens the confirm dialog — nothing is deleted yet.
	m.handleSessionsKey(keyMsg("d"))
	if m.modal != ModalConfirm {
		t.Fatalf("modal = %v, want ModalConfirm", m.modal)
	}
	m.refreshSavedSessions()
	if len(m.savedCache) == 0 {
		t.Fatal("opening the confirm dialog must not delete the session")
	}

	// Cancel: the sessions list comes back, session untouched.
	m.handleConfirmKey(keyMsg("n"))
	if m.modal != ModalSessions {
		t.Fatalf("cancel must restore the sessions modal, got %v", m.modal)
	}
	m.refreshSavedSessions()
	if len(m.savedCache) == 0 {
		t.Fatal("cancel must not delete the session")
	}

	// Confirm: the delete runs; the (now empty) list closes.
	m.handleSessionsKey(keyMsg("d"))
	if m.modal != ModalConfirm {
		t.Fatalf("modal = %v, want ModalConfirm", m.modal)
	}
	m.handleConfirmKey(keyMsg("y"))
	m.refreshSavedSessions()
	for _, si := range m.savedCache {
		if si.ID == "abc" {
			t.Fatal("confirmed delete did not remove the session")
		}
	}
	if m.modal != ModalNone {
		t.Fatalf("list empty after delete: modal = %v, want ModalNone", m.modal)
	}
}

// TestStreamAttributionCoverage pins the classification the Update dispatch
// relies on (C1): every stream-RENDER message implements streamAttributed, so
// ownership/staleness/the not-streaming gate are applied to it by construction;
// the two terminal messages deliberately do NOT, because handleTurnFinishedMsg
// resolves their session and generation itself and must still finalize a turn
// whose session lost focus.
func TestStreamAttributionCoverage(t *testing.T) {
	render := []tea.Msg{
		streamStartMsg{},
		streamRoundStartMsg{},
		streamTokenMsg{},
		streamThinkingMsg{},
		streamStatsMsg{},
		streamCompactingMsg{},
		streamStallMsg{},
		streamRetryMsg{},
		streamToolCallMsg{},
		streamToolCallArgsMsg{},
		streamToolCallFinalMsg{},
		streamToolExecuteMsg{},
		streamToolResultMsg{},
		streamRoundEndMsg{},
		condensedNoteMsg{},
	}
	for _, msg := range render {
		if _, ok := msg.(streamAttributed); !ok {
			t.Errorf("%T does not implement streamAttributed; a new stream message must embed streamMeta", msg)
		}
	}
	terminal := []tea.Msg{
		endOfStream("s1", 1),
		failOfStream("s1", 1, nil),
	}
	for _, msg := range terminal {
		if _, ok := msg.(streamAttributed); ok {
			t.Errorf("%T must NOT implement streamAttributed: handleTurnFinishedMsg owns its resolution", msg)
		}
	}
}
