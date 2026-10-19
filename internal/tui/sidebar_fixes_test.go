package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/server"
	"gogen/internal/session"

	"charm.land/bubbles/v2/textarea"
)

// newSidebarRaceModel builds a model over a real agent with a fake streaming
// provider (turns and compactions actually rewrite Messages) and a real
// session store, so the sidebar's read paths can be exercised against the
// writers they race.
func newSidebarRaceModel(t *testing.T) *Model {
	t.Helper()
	dir := t.TempDir()
	p := fakeStreamProvider{}
	exec := agent.NewExecutorWithGuard(dir, agent.NewCommandGuard("", nil))
	cm := contextmgr.NewManager(p, contextmgr.Settings{ContextLimit: 100000})
	a := agent.NewAgent(p, exec, cm)
	a.WorkingDir = dir
	a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	a.SetSessionID("cur")
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	ta := textarea.New()
	ta.SetHeight(3)
	return &Model{
		agent:          a,
		lives:          newLiveSessions(a),
		viewport:       vp,
		textarea:       ta,
		ctx:            context.Background(),
		width:          100,
		height:         30,
		sidebarVisible: true,
		sidebarWidth:   defaultSidebarWidth,
		workspace:      server.NewWorkspaceForHost(a, nil),
	}
}

// Regression: a background /compact (compacting=true, streaming=false)
// rewrites agent.Messages off the Update thread, and buildSidebarRows read
// len(agent.Messages) raw — a data race that could print a torn message
// count. The locked MessageCount accessor is the contract; run with -race.
func TestSidebarMessageCountDuringCompaction(t *testing.T) {
	m := newSidebarRaceModel(t)
	a := m.agent
	seed := make([]llm.Message, 0, 160)
	for i := 0; i < 80; i++ {
		seed = append(seed,
			llm.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)})
	}
	a.Messages = seed
	sess := m.lives.Active()
	sess.compacting = true

	done := make(chan struct{})
	go func() {
		defer close(done)
		// The first compaction succeeds and replaces Messages; later ones
		// report "not enough history" — both are fine, the write is what
		// the sidebar races.
		for i := 0; i < 50; i++ {
			if err := a.CompactHistory(context.Background()); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			rows := m.buildSidebarRows()
			if len(rows) == 0 {
				t.Fatal("no rows built")
			}
		}
	}
}

// Regression: deleting a session that is still hosted in a BACKGROUND live
// slot used to leave the slot alive; its next flush re-created the file, so
// the deletion silently reverted. deleteSavedRow must refuse (the sidebar's
// ✕ path already did — the /resume modal path did not).
func TestDeleteLiveBackgroundSessionRefused(t *testing.T) {
	m := newSidebarRaceModel(t)
	probeSave := func(id string) {
		t.Helper()
		if err := m.agent.SessionStore.Save(id, agent.SessionSnapshot{
			WorkingDir: m.agent.WorkingDir,
			Messages:   []llm.Message{{Role: "user", Content: "hello " + id}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	probeSave("aaa")
	m.refreshSavedSessions()

	// Host aaa live, then focus another session so aaa is a BACKGROUND slot.
	m.openNewLiveSession("")
	m.resumeSavedRow("aaa")
	m.refreshSavedSessions()
	if i := m.lives.indexBySessionID("aaa"); i < 0 {
		t.Fatal("setup: aaa not hosted live")
	}
	m.switchToLive(0)
	m.refreshSavedSessions()

	if cmd := m.deleteSavedRow("aaa"); cmd != nil {
		t.Fatal("delete of a live background session must be refused")
	}
	if !strings.Contains(m.statusMsg, "close the session first") {
		t.Fatalf("statusMsg = %q, want the close-first hint", m.statusMsg)
	}
	// The store entry must still exist (nothing deleted).
	m.refreshSavedSessions()
	found := false
	for _, si := range m.savedCache {
		if si.ID == "aaa" {
			found = true
		}
	}
	if !found {
		t.Fatal("refused delete still removed the store entry")
	}

	// The sessions-modal path shares the guard and keeps its row.
	m.sessionList = append([]agent.SessionInfo(nil), m.savedCache...)
	for i, si := range m.sessionList {
		if si.ID == "aaa" {
			m.sessionCursor = i
		}
	}
	m.modal = ModalSessions
	m.deleteSelectedSession()
	if m.modal != ModalConfirm {
		t.Fatal("delete must ask for confirmation first")
	}
	m.handleConfirmKey(keyMsg("y"))
	if m.modal != ModalNone {
		t.Fatalf("modal = %v, want closed after the refusal so the status is visible", m.modal)
	}
	kept := false
	for _, si := range m.sessionList {
		if si.ID == "aaa" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("refused delete dropped the modal row")
	}
	if !strings.Contains(m.statusMsg, "close the session first") {
		t.Fatalf("statusMsg = %q, want the close-first hint", m.statusMsg)
	}
}

// The focused session still deletes through the agent (rebind) — the
// live-host guard must not touch that path.
func TestDeleteFocusedSessionStillRebinds(t *testing.T) {
	m := newSidebarRaceModel(t)
	if err := m.agent.SessionStore.Save("cur", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.refreshSavedSessions()

	cmd := m.deleteSavedRow("cur")
	if cmd == nil {
		t.Fatal("deleting the focused session must run")
	}
	if m.agent.SessionID == "cur" {
		t.Fatal("focused delete did not rebind to a fresh session")
	}
	m.refreshSavedSessions()
	for _, si := range m.savedCache {
		if si.ID == "cur" {
			t.Fatal("deleted focused session still in the store")
		}
	}
}

// A live row's message count must come from the locked accessor even while
// the agent is settled (the compaction write can land at any moment).
func TestSidebarLiveMsgCountUsesSnapshot(t *testing.T) {
	m := newSidebarRaceModel(t)
	m.agent.Messages = []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
	}
	rows := m.buildSidebarRows()
	if len(rows) != 1 || rows[0].msgCount != 3 {
		t.Fatalf("rows = %+v, want one row with msgCount 3", rows)
	}
	// Same via the saved-entry overlay path.
	if err := m.agent.SessionStore.Save("cur", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   m.agent.Messages,
	}); err != nil {
		t.Fatal(err)
	}
	m.refreshSavedSessions()
	rows = m.buildSidebarRows()
	if len(rows) != 1 || !rows[0].live || rows[0].msgCount != 3 {
		t.Fatalf("overlay row = %+v, want live with msgCount 3", rows)
	}
}

// Regression ("two color indicators under one session"): each sidebar row
// renders its own state dot, so a session that owns TWO rows shows two
// colored indicators. buildSidebarRows must therefore never emit a second
// row for a session id — not for a duplicated store entry, and not for a
// second live slot hosting the same SessionID (the overlay used to mark
// only the FIRST matching slot covered, so the second fell through to the
// fallback loop as a duplicate row).
func TestSidebarOneRowPerSession(t *testing.T) {
	// rowsOf counts the rows per session id and fails on a duplicate.
	rowsOf := func(t *testing.T, m *Model) map[string]sidebarRow {
		t.Helper()
		out := map[string]sidebarRow{}
		for _, r := range m.buildSidebarRows() {
			if prev, dup := out[r.id]; dup {
				t.Fatalf("duplicate row for %q: %+v and %+v", r.id, prev, r)
			}
			out[r.id] = r
		}
		return out
	}
	// dotLines counts the state-dot rows the panel actually paints.
	dotLines := func(m *Model) int {
		n := 0
		for _, l := range strings.Split(stripANSI(m.renderSidebar(30)), "\n") {
			if strings.Contains(l, "●") {
				n++
			}
		}
		return n
	}

	t.Run("duplicate store entry renders one row", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.savedCache = []agent.SessionInfo{
			{ID: "cur", Label: "root session"},
			{ID: "cur", Label: "root session"}, // re-written / doubled index entry
		}
		rows := rowsOf(t, m)
		if len(rows) != 1 || !rows["cur"].live {
			t.Fatalf("rows = %+v, want exactly one live row", rows)
		}
		if got := dotLines(m); got != 1 {
			t.Fatalf("panel paints %d state dots for one session, want 1", got)
		}
	})

	t.Run("duplicate live slot for one session renders one row", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.savedCache = []agent.SessionInfo{{ID: "cur", Label: "root session"}}
		// A second slot hosting the SAME SessionID (the state the defensive
		// dedupe exists for — two agents keyed to one session must not both
		// list it).
		dup := newSwitchTestAgent(t)
		dup.SetSessionID("cur")
		m.lives.Add(dup, "")
		m.lives.sessions[len(m.lives.sessions)-1].streaming = true

		rows := rowsOf(t, m)
		if len(rows) != 1 {
			t.Fatalf("rows = %+v, want one row", rows)
		}
		if got := dotLines(m); got != 1 {
			t.Fatalf("panel paints %d state dots for one session, want 1", got)
		}
		// The focused slot owns the row's state, not the last duplicate.
		if r := rows["cur"]; !r.live || !r.focused || r.streaming {
			t.Fatalf("row = %+v, want the FOCUSED slot's state (live, focused, not streaming)", r)
		}
	})

	t.Run("duplicate live slot without a store entry renders one row", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.savedCache = nil
		dup := newSwitchTestAgent(t)
		dup.SetSessionID(m.agent.SessionID)
		m.lives.Add(dup, "")
		if rows := rowsOf(t, m); len(rows) != 1 {
			t.Fatalf("rows = %+v, want one fallback row", rows)
		}
	})
}
