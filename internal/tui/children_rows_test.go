package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
	"gogen/internal/subagents"
)

// syncSender is a programSender that dispatches the host's Update-thread work
// INLINE, on the calling goroutine: child admission, targeted deliveries and
// releases are handled immediately and a returned turn command is executed and
// fed back. Bubble Tea's real loop does the same asynchronously; the tests use
// this to stay deterministic (the Model is only touched on one goroutine at a
// time, and the test reads it after Spawner.Wait).
type syncSender struct {
	mu   sync.Mutex
	m    *Model
	msgs []tea.Msg // non-Update traffic (delivery requests), recorded for assertions
}

func (s *syncSender) Send(msg tea.Msg) {
	// Bound the inline re-entry: a terminal can drain a queued item, which
	// starts another turn, whose terminal lands here again.
	for depth := 0; msg != nil && depth < 32; depth++ {
		switch v := msg.(type) {
		case tea.BatchMsg:
			// The real runtime runs batch members concurrently; run them in
			// order and feed each result back (the tests need determinism, not
			// concurrency). Members that block — the spinner/cursor tick loops
			// never return — are dropped: their messages would only restart
			// themselves.
			for _, cmd := range v {
				if out := runCmdBounded(cmd); out != nil {
					s.Send(out)
				}
			}
			msg = nil
		case childSpawnMsg:
			_, cmd := s.m.handleChildSpawnMsg(v)
			msg = runCmd(cmd)
		case childMessageMsg:
			_, cmd := s.m.handleChildMessageMsg(v)
			msg = runCmd(cmd)
		case childReleaseMsg:
			s.m.handleChildReleaseMsg(v)
			msg = nil
		default:
			_, cmd := s.m.handleMsg(msg)
			s.mu.Lock()
			s.msgs = append(s.msgs, msg)
			s.mu.Unlock()
			msg = runCmd(cmd)
		}
	}
}

// runCmd executes one tea command (if any) and returns its message.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// runCmdBounded executes one tea command and gives up when it blocks: the
// spinner/cursor tick commands wait on their own timer channel forever, which
// an inline loop must not inherit. A dropped tick only means "don't animate".
func runCmdBounded(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}

// take drains the recorded traffic.
func (s *syncSender) take() []tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.msgs
	s.msgs = nil
	return out
}

// spawnChild spawns a background child through the REAL lifecycle path (agent
// build, admission handoff, turn, terminal) and returns its id: the synchronous
// sender plays the event loop, and Spawner.Wait quiesces the spawn goroutine so
// the test can read the Model without a race.
func (hh *childHarness) spawnChild(t *testing.T, job string) string {
	t.Helper()
	id, err := hh.sp.SpawnBackground(context.Background(), hh.m.agent, job, "", 0)
	if err != nil {
		t.Fatalf("SpawnBackground(%q): %v", job, err)
	}
	hh.sp.Wait()
	return id
}

// spawnChildFrom spawns a child of a CHILD (a grandchild), exercising the
// nesting chain.
func (hh *childHarness) spawnChildFrom(t *testing.T, parent *subagents.Child[*childHandle], job string) string {
	t.Helper()
	id, err := hh.sp.SpawnBackground(context.Background(), parent.Agent, job, "", parent.Depth)
	if err != nil {
		t.Fatalf("SpawnBackground(%q) from %s: %v", job, parent.Label, err)
	}
	hh.sp.Wait()
	return id
}

// rowsFor finds the rows with the given session id.
func rowsFor(rows []sidebarRow, id string) []sidebarRow {
	var out []sidebarRow
	for _, r := range rows {
		if r.id == id {
			out = append(out, r)
		}
	}
	return out
}

// TestSidebarNestedRowsUnderParent pins the R1 layout: a live child slot is NOT
// a flat row — it renders once, nested under its parent, carrying the depth and
// the parent link (and clicking it focuses the child pane).
func TestSidebarNestedRowsUnderParent(t *testing.T) {
	hh := newChildHarness(t, "child report")
	parentID := hh.m.agent.SessionID
	childID := hh.spawnChild(t, "explore the parser")

	rows := hh.m.buildSidebarRows()
	if got := rowsFor(rows, parentID); len(got) != 1 {
		t.Fatalf("parent rows = %d, want 1", len(got))
	}
	kids := rowsFor(rows, childID)
	if len(kids) != 1 {
		t.Fatalf("child rows = %d, want exactly 1 (nested, not also flat)", len(kids))
	}
	kid := kids[0]
	if kid.depth != 1 || kid.parentID != parentID {
		t.Fatalf("child row depth/parent = %d/%q, want 1/%q", kid.depth, kid.parentID, parentID)
	}
	if !kid.live || kid.liveIdx < 0 {
		t.Fatal("the child row must point at its live slot (the pane)")
	}
	pi, ci := -1, -1
	for i, r := range rows {
		if r.id == parentID {
			pi = i
		}
		if r.id == childID {
			ci = i
		}
	}
	if pi < 0 || ci != pi+1 {
		t.Fatalf("child row at %d, parent at %d — want the child directly after its parent", ci, pi)
	}
}

// TestSidebarNestedRowsRecursive pins depth >= 2: a child that spawns its own
// child (subagent_max_depth 2) keeps the chain's shape in the panel.
func TestSidebarNestedRowsRecursive(t *testing.T) {
	hh := newChildHarness(t, "report")
	hh.cfg.SubagentMaxDepth = 2
	childID := hh.spawnChild(t, "child job")
	child := hh.sp.Get(childID)
	if child == nil {
		t.Fatal("child not registered")
	}

	grandID := hh.spawnChildFrom(t, child, "grandchild job")
	rows := hh.m.buildSidebarRows()
	kid := rowsFor(rows, childID)
	if len(kid) != 1 || kid[0].depth != 1 {
		t.Fatalf("child rows = %d (depth %v), want one at depth 1", len(kid), kid)
	}
	gk := rowsFor(rows, grandID)
	if len(gk) != 1 || gk[0].depth != 2 {
		t.Fatalf("grandchild rows = %d (depth %v), want one at depth 2", len(gk), gk)
	}
}

// TestSidebarSettledChildFromStore pins the second source of nested rows: a
// child whose live runtime is gone (released by retention, or restored after a
// restart) still renders under its parent, as a settled row with a muted text
// status instead of a dot.
func TestSidebarSettledChildFromStore(t *testing.T) {
	m := dragModel(t)
	m.agent.SetSessionID("parent-session")
	parent := m.agent.SessionID
	m.savedCache = []agent.SessionInfo{
		{ID: "child-1", Label: "subagent: failed job", ParentID: parent, SubagentStatus: "failed", SubagentSummary: "boom", MessageCount: 4, UpdatedAt: "2026-01-01T00:00:00Z"},
		{ID: "child-2", Label: "subagent: ok job", ParentID: parent, SubagentStatus: "success", MessageCount: 2, UpdatedAt: "2026-01-02T00:00:00Z"},
	}
	rows := m.buildSidebarRows()
	for _, id := range []string{"child-1", "child-2"} {
		got := rowsFor(rows, id)
		if len(got) != 1 {
			t.Fatalf("%s rows = %d, want 1", id, len(got))
		}
		if got[0].depth != 1 || got[0].parentID != parent {
			t.Fatalf("%s depth/parent = %d/%q", id, got[0].depth, got[0].parentID)
		}
		if got[0].live {
			t.Fatalf("%s must read settled (not live)", id)
		}
	}
	if got := rowsFor(rows, "child-1")[0].childStatus; got != "failed" {
		t.Fatalf("child-1 status = %q, want failed", got)
	}
}

// TestSidebarOrphanChildRendered pins that a child whose parent row is missing
// (parent pruned from the store) still renders — a running child must never
// vanish from the panel.
func TestSidebarOrphanChildRendered(t *testing.T) {
	m := dragModel(t)
	m.agent.SetSessionID("parent-session")
	m.savedCache = []agent.SessionInfo{
		{ID: "orphan", Label: "subagent: orphan", ParentID: "gone-parent", UpdatedAt: "2026-01-01T00:00:00Z"},
	}
	got := rowsFor(m.buildSidebarRows(), "orphan")
	if len(got) != 1 || got[0].depth != 1 {
		t.Fatalf("orphan rows = %v, want one nested row", got)
	}
}

// TestSidebarChildRowRendering pins the child presentation: the indent + tree
// glyph, and the state dot mapping (amber responding / red failed / green done /
// cyan idle) with the muted text form for a settled child.
func TestSidebarChildRowRendering(t *testing.T) {
	m := dragModel(t)
	m.SetSize(120, 40)

	states := []struct {
		name    string
		row     sidebarRow
		wantDot bool
		state   string
	}{
		{name: "responding", row: sidebarRow{depth: 1, live: true, streaming: true}, wantDot: true, state: "responding"},
		{name: "failed", row: sidebarRow{depth: 1, live: true, childStatus: "failed"}, wantDot: true, state: "failed"},
		{name: "done", row: sidebarRow{depth: 1, live: true, childStatus: "success"}, wantDot: true, state: "done"},
		{name: "idle", row: sidebarRow{depth: 1, live: true}, wantDot: true, state: "idle"},
		{name: "settled done", row: sidebarRow{depth: 1, childStatus: "success"}, wantDot: false, state: "done"},
		{name: "settled failed", row: sidebarRow{depth: 1, childStatus: "failed"}, wantDot: false, state: "failed"},
		// A child whose runtime is gone and whose recorded outcome never
		// reached a terminal state: interrupted, never done.
		{name: "settled interrupted", row: sidebarRow{depth: 1, childStatus: "running"}, wantDot: false, state: "interrupted"},
		{name: "settled legacy", row: sidebarRow{depth: 1}, wantDot: false, state: "interrupted"},
	}
	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			meta := stripANSI(m.sidebarMeta(tc.row))
			if strings.Contains(meta, "●") != tc.wantDot {
				t.Fatalf("dot presence in %q = %v, want %v", meta, !tc.wantDot, tc.wantDot)
			}
			if !strings.Contains(meta, tc.state) {
				t.Fatalf("meta %q missing state %q", meta, tc.state)
			}
			if !strings.Contains(meta, "└") {
				t.Fatalf("nested meta %q missing the tree glyph", meta)
			}
		})
	}

	// A long nested label must not widen the panel: every rendered line stays
	// exactly the panel width (the frame invariant the renderer relies on).
	m.savedCache = []agent.SessionInfo{{
		ID: "child", Label: "subagent: " + strings.Repeat("wide", 40),
		ParentID: m.agent.SessionID, UpdatedAt: "2026-01-01T00:00:00Z",
	}}
	m.sidebarVisible = true
	m.sidebarWidth = 30
	panel := m.renderSidebar(24)
	for _, line := range strings.Split(stripANSI(panel), "\n") {
		if w := cellWidth(line); w != m.sidebarWidth {
			t.Fatalf("panel line %q is %d cells, want %d", line, w, m.sidebarWidth)
		}
	}
	// The child's title indents under its parent.
	if !strings.Contains(stripANSI(panel), "└") {
		t.Fatalf("rendered panel missing the nested glyph:\n%s", stripANSI(panel))
	}
}

// TestCloseParentCancelsChildren pins the parent-scope cascade: closing a
// session's pane cancels and releases its continuable children (they have
// nobody left to deliver to).
func TestCloseParentCancelsChildren(t *testing.T) {
	hh := newChildHarness(t, "report")
	childID := hh.spawnChild(t, "job")
	if hh.sp.Get(childID) == nil {
		t.Fatal("child not registered")
	}
	// A second live session must exist so the root row is closable.
	hh.m.spawnLiveSession(newSwitchTestAgent(t))
	rows := hh.m.buildSidebarRows()
	idx := -1
	for i, r := range rows {
		if r.id == hh.m.agent.SessionID {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("root session row not found")
	}
	runCmd(hh.m.sidebarCloseRow(idx))
	if hh.sp.Get(childID) != nil {
		t.Fatal("closing the parent must release its children")
	}
}

// TestFlushAllSessionsCancelsChildren pins the quit path: the shutdown sweep
// cancels and releases every child before flushing, so no child outlives the
// host.
func TestFlushAllSessionsCancelsChildren(t *testing.T) {
	hh := newChildHarness(t, "report")
	childID := hh.spawnChild(t, "job")
	hh.m.flushAllSessions()
	if hh.sp.Get(childID) != nil {
		t.Fatal("the quit sweep must release every child")
	}
}

// TestReleaseCascadesToGrandchildren pins the recursive teardown: releasing a
// child releases the children it spawned.
func TestReleaseCascadesToGrandchildren(t *testing.T) {
	hh := newChildHarness(t, "report")
	hh.cfg.SubagentMaxDepth = 2
	childID := hh.spawnChild(t, "child job")
	child := hh.sp.Get(childID)
	if child == nil {
		t.Fatal("child not registered")
	}
	grandID := hh.spawnChildFrom(t, child, "grandchild job")
	if hh.sp.Get(grandID) == nil {
		t.Fatal("grandchild not registered")
	}
	hh.sp.ReleaseChild(child)
	if hh.sp.Get(grandID) != nil {
		t.Fatal("releasing a child must release its grandchildren")
	}
}

// TestSubagentsModalShowsLiveChildren pins the modal's live half: running
// children are listed with their status, label and progress, next to the
// finished records (the tool-visible surface, list_agents parity).
func TestSubagentsModalShowsLiveChildren(t *testing.T) {
	hh := newChildHarness(t, "child report")
	hh.spawnChild(t, "inspect the parser")
	hh.m.SetSize(120, 40)

	out := stripANSI(hh.m.renderSubagentsModal())
	if !strings.Contains(out, "subagent: inspect the parser") {
		t.Fatalf("modal missing the live child:\n%s", out)
	}
	if !strings.Contains(out, "finished") && !strings.Contains(out, "running") && !strings.Contains(out, "idle") {
		t.Fatalf("modal missing the child status:\n%s", out)
	}
	if !strings.Contains(out, "depth 1") {
		t.Fatalf("modal missing the nesting depth:\n%s", out)
	}
}
