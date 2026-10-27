package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/session"
	"gogen/internal/textutil"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Sidebar panel layout constants (row indices inside the panel):
//
//	0  ┌──┐ top border
//	1  "SESSIONS" header
//	2  working directory
//	3  blank
//	4+ session rows, three lines each (title + meta + blank separator;
//	     the last rendered session has no trailing separator)
//	-3 blank
//	-2 footer hints
//	-1 └──┘ bottom border
const (
	sidebarHeaderLines = 3
	sidebarFooterLines = 2
	// sidebarBorderLines is the top + bottom border rows of the panel box.
	sidebarBorderLines = 2
)

// Footer button ids double as the hover state; sidebarFooterNone is the
// zero value so literal-built models (tests) start unhovered.
const (
	sidebarFooterNone   = 0
	sidebarFooterNew    = 1 // "n new" — same action as the n key
	sidebarFooterClose  = 2 // "x close" — same action as the x key (cursor row)
	sidebarFooterDelete = 3 // "d del" — same action as the d key (cursor row)
)

// Footer labels: the clickable button segments plus the plain hint tail.
// The buttons render brighter than the hints (cyan; the hovered one gets
// the row-highlight style). "d del" (not "d delete") keeps all three
// buttons inside the default panel width.
const (
	sidebarFooterNewLabel    = "n new"
	sidebarFooterCloseLabel  = "x close"
	sidebarFooterDeleteLabel = "d del"
	// "size" (not "resize") keeps the full footer (39 cells) inside any
	// panel ≥ 42 wide; the help modal carries the full wording.
	sidebarFooterRest = "[/] size  ^b hide"
)

// sidebarFooterZone is one clickable footer button: its id and the
// inclusive terminal-column range it occupies (the "│ " prefix is 2
// columns, same convention as the row ✕ zone in sidebarRowAt).
type sidebarFooterZone struct {
	button int
	x0, x1 int
}

// sidebarFooterZones returns the hit-test ranges of the footer buttons.
// Layout: "n new"(cols 2-6) + "  " + "x close"(cols 9-15) + "  " +
// "d del"(cols 18-22).
func sidebarFooterZones() []sidebarFooterZone {
	newX := 2
	closeX := newX + len(sidebarFooterNewLabel) + 2
	delX := closeX + len(sidebarFooterCloseLabel) + 2
	return []sidebarFooterZone{
		{button: sidebarFooterNew, x0: newX, x1: newX + len(sidebarFooterNewLabel) - 1},
		{button: sidebarFooterClose, x0: closeX, x1: closeX + len(sidebarFooterCloseLabel) - 1},
		{button: sidebarFooterDelete, x0: delX, x1: delX + len(sidebarFooterDeleteLabel) - 1},
	}
}

// sidebarRow is one entry of the unified session list — the TUI port of the
// web sidebar's row model (components/sessions.js): ONE list of sessions in
// which a live (hosted) session overlays onto its saved entry by id.
// Focus/streaming are ATTRIBUTES of the row, never its position: ordering is
// by last output time (the in-process output stamp when known, else the
// store's updatedAt), so focusing a session never reorders the list.
type sidebarRow struct {
	id         string
	label      string
	live       bool // hosted in this TUI process
	focused    bool // live and currently focused
	streaming  bool // live and a turn is in flight
	liveIdx    int  // index into m.lives.sessions; -1 for saved-only rows
	msgCount   int
	updatedAt  time.Time // store timestamp (zero when unknown)
	lastActive time.Time // last in-process output (zero when the session never ran here)
	seq        int       // persistent first-seen order (tie-break)

	// Nested (subagent) rows. parentID is the owning session id (empty for a
	// top-level row) and depth is the indent level (0 for top-level): nested
	// rows render under their parent, recursively, carrying its children with
	// it when the parent reorders.
	parentID string
	depth    int
	// childStatus is the child's persisted outcome ("", "success", "failed")
	// and childSummary its summary — the dot state for a live child, the
	// muted text for a settled one. Read off the child agent (or the store
	// index) at build time, so the renderer never touches host state.
	childStatus  string
	childSummary string
}

// activity is the row's sort key: last OUTPUT time.
//
// Rows with an in-process output stamp (live rows, or saved rows for
// sessions that ran in this process) use ONLY that stamp: the store's
// Updated is a PERSIST timestamp (rewritten by every flush/debounce/touch),
// so mixing it in would bump a row on bookkeeping writes, not on output —
// including the resume flush that rewrites the LEFT-BEHIND session's
// timestamp. Rows without in-process history fall back to the store
// timestamp.
func (r sidebarRow) activity() time.Time {
	if !r.lastActive.IsZero() {
		return r.lastActive
	}
	return r.updatedAt
}

// touchSessionActivity records a session's last in-process OUTPUT time in
// the per-id recency map (web pane.lastActivity). Keyed by session id —
// not by live slot — so a session keeps its earned list position after it
// stops being focused (resume rebind, pane close). Mutated only on the
// Update thread.
func (m *Model) touchSessionActivity(id string, t time.Time) {
	if id == "" {
		return
	}
	if m.sessionActivity == nil { // Models built as literals (tests) skip NewModel
		m.sessionActivity = make(map[string]time.Time)
	}
	m.sessionActivity[id] = t
}

// storeSessionTime returns a session's persisted timestamp from the
// saved-session index (ok=false when the index has no usable entry).
func (m *Model) storeSessionTime(id string) (time.Time, bool) {
	for _, si := range m.savedCache {
		if si.ID != id {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, si.UpdatedAt); err == nil {
			return ts, true
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// effectiveLastActive is a live session's in-process output stamp: the max
// of the slot's stamp and the per-id memory. The two are kept in sync (turn
// end, rebind re-stamp); the max is the safety net so a rebound slot can
// never strand the previous session's stamp on the new row.
func (m *Model) effectiveLastActive(s *liveSession) time.Time {
	t := s.lastActive
	if s.agent != nil {
		if v := m.sessionActivity[s.agent.SessionID]; v.After(t) {
			t = v
		}
	}
	return t
}

// rebindActivity re-stamps the focused live session after the agent was
// rebound to a different session id (resume, /new, delete-current — web:
// makePane's initialActivity seed). A session that produced output in this
// process keeps its in-process stamp; a resumed session with no in-process
// history is pinned to its store timestamp so it keeps its earned list
// position instead of jumping to the top; a brand-new session (no store
// entry yet) starts fresh at the top.
func (m *Model) rebindActivity() {
	if m.lives == nil || m.sessionID == "" {
		return
	}
	if _, ok := m.sessionActivity[m.sessionID]; !ok {
		if ts, ok := m.storeSessionTime(m.sessionID); ok {
			m.touchSessionActivity(m.sessionID, ts)
		} else {
			m.touchSessionActivity(m.sessionID, time.Now())
		}
	}
	m.lives.Active().lastActive = m.sessionActivity[m.sessionID]
}

// sidebarSeqOf returns the row's persistent first-seen sequence: the
// tie-break for equal activity. Assigned once per id and never reset, so
// the order is immune to group reassignment (a session gaining its first
// store entry) and to the store's unstable equal-timestamp ordering —
// equal-activity rows keep their relative position forever.
func (m *Model) sidebarSeqOf(id string) int {
	if n, ok := m.sidebarSeq[id]; ok {
		return n
	}
	if m.sidebarSeq == nil { // Models built as literals (tests) skip NewModel
		m.sidebarSeq = make(map[string]int)
	}
	m.sidebarSeqNext++
	m.sidebarSeq[id] = m.sidebarSeqNext
	return m.sidebarSeqNext
}

// savedStamp memoizes one saved session's parsed UpdatedAt. raw is the
// timestamp string the parse came from, so a refresh that rewrites a row's
// timestamp invalidates the entry.
type savedStamp struct {
	raw string
	ts  time.Time
}

// savedTime returns si's parsed UpdatedAt, parsing at most once per
// (id, raw) pair. buildSidebarRows runs on every View and again on every
// mouse/key event that clamps or re-resolves the list, so without this memo
// each frame re-parsed every saved row's RFC3339Nano timestamp.
func (m *Model) savedTime(si agent.SessionInfo) time.Time {
	if si.ID == "" { // no id → nothing to key on; parse without caching
		ts, err := time.Parse(time.RFC3339Nano, si.UpdatedAt)
		if err != nil {
			return time.Time{}
		}
		return ts
	}
	if st, ok := m.savedTimes[si.ID]; ok && st.raw == si.UpdatedAt {
		return st.ts
	}
	var ts time.Time
	if parsed, err := time.Parse(time.RFC3339Nano, si.UpdatedAt); err == nil {
		ts = parsed
	}
	if m.savedTimes == nil {
		m.savedTimes = make(map[string]savedStamp, len(m.savedCache))
	}
	m.savedTimes[si.ID] = savedStamp{raw: si.UpdatedAt, ts: ts}
	return ts
}

// setSavedCache replaces the persisted-session snapshot and drops memoized
// timestamps for sessions that are no longer listed (see savedTimes).
func (m *Model) setSavedCache(list []agent.SessionInfo) {
	m.savedCache = list
	if len(m.savedTimes) == 0 {
		return
	}
	live := make(map[string]bool, len(list))
	for _, si := range list {
		live[si.ID] = true
	}
	for id := range m.savedTimes {
		if !live[id] {
			delete(m.savedTimes, id)
		}
	}
}

// buildSidebarRows merges the persisted-session index (m.savedCache) with
// the live registry into the unified list. Saved entries come first (store
// order, recency-sorted); live sessions overlay onto their saved row by
// agent SessionID, and live sessions with no store entry yet get fallback
// rows so they never disappear (the web's "creating…" pane fallback).
//
// Invariant: exactly ONE row per session id. Every row renders its own
// state dot, so a second row for the same session shows as two colored
// indicators under one session — the symptom a duplicated store entry or a
// duplicated live slot must never produce (see the seen map below).
func (m *Model) buildSidebarRows() []sidebarRow {
	if m.lives == nil {
		return nil
	}
	rows := make([]sidebarRow, 0, len(m.savedCache))
	covered := make(map[int]bool, len(m.lives.sessions)) // live sessions overlaid below
	// seen holds every session id that already owns a row. One session is
	// ONE row: a duplicated store entry (a re-written index, an id listed
	// twice) or a second live slot hosting the same SessionID must never
	// render a second row — each duplicate carries its own state dot, so
	// the panel would show two colored indicators for a single session.
	seen := make(map[string]bool, len(m.savedCache)+len(m.lives.sessions))
	for _, si := range m.savedCache {
		if si.ParentID != "" {
			continue // nested (subagent) rows are not part of the flat list
		}
		if si.ID != "" {
			if seen[si.ID] {
				continue // duplicate store entry for a session already listed
			}
			seen[si.ID] = true
		}
		r := sidebarRow{id: si.ID, label: si.Label, msgCount: si.MessageCount, liveIdx: -1}
		r.updatedAt = m.savedTime(si)
		// Match EVERY live slot hosting this session (marking all of them
		// covered, so none of them falls through to the fallback loop
		// below), but render the row from ONE of them: the focused slot
		// when several share the id, so the row's state dot reports
		// "active"/"responding" for the session the user is looking at.
		match := -1
		for i, s := range m.lives.sessions {
			if s.agent == nil || s.agent.SessionID != si.ID {
				continue
			}
			covered[i] = true
			if match < 0 || i == m.lives.active {
				match = i
			}
		}
		if match >= 0 {
			s := m.lives.sessions[match]
			i := match
			r.live, r.liveIdx = true, i
			r.focused = i == m.lives.active
			r.streaming = s.streaming
			// Live label wins: the agent derives it from the first user
			// message (SessionLabelSnapshot — the same source the web
			// pushes as sessionLabel), so a fresh session shows
			// "New session…" until its first turn names it.
			if l := s.agent.SessionLabelSnapshot(); l != "" {
				r.label = l
			}
			// Message count: the live count is authoritative, but only
			// read at a settled boundary (not mid-turn — the stream
			// goroutine owns Messages while streaming, and a background
			// /compact rewrites it off-thread even with streaming=false).
			// MessageCount is the locked accessor: a raw len() here raced
			// both writers and could print a torn count.
			if !s.streaming {
				r.msgCount = s.agent.MessageCount()
			}
			r.lastActive = m.effectiveLastActive(s)
		}
		// In-process memory for a session that is no longer live (resume
		// rebind, pane close): its row keeps the output stamp instead of
		// the store's persist timestamp (which a flush rewrites to now).
		if !r.live {
			r.lastActive = m.sessionActivity[si.ID]
		}
		if r.label == "" {
			r.label = si.ID
		}
		r.seq = m.sidebarSeqOf(r.id)
		rows = append(rows, r)
	}
	for i, s := range m.lives.sessions {
		if covered[i] {
			continue
		}
		// No store entry yet: the row id is the agent's session id, or
		// the live registry id when the agent is not keyed yet.
		id := s.id
		if s.agent != nil && s.agent.SessionID != "" {
			id = s.agent.SessionID
		}
		// A live CHILD slot renders as a nested row under its parent (see
		// nestedRows), never as a flat row — otherwise an open child would
		// appear twice and break the one-row-per-session invariant below.
		if s.child != nil {
			continue
		}
		// A second slot for a session that already owns a row is dropped —
		// see the seen map above.
		if seen[id] {
			continue
		}
		seen[id] = true
		r := sidebarRow{id: id, live: true, liveIdx: i, focused: i == m.lives.active,
			streaming: s.streaming, lastActive: m.effectiveLastActive(s)}
		if s.agent != nil {
			r.label = s.agent.SessionLabelSnapshot()
			if !s.streaming {
				// Locked accessor — see the overlay path above.
				r.msgCount = s.agent.MessageCount()
			}
		}
		if r.label == "" {
			r.label = "New session…"
		}
		r.seq = m.sidebarSeqOf(r.id)
		rows = append(rows, r)
	}
	// Sort by last output, newest first; ties by persistent first-seen
	// order (deterministic regardless of group membership or store order).
	sortSidebarRows(rows)
	return m.appendNestedRows(rows)
}

// maxNestedDepth bounds the nested-row recursion: the subagent nesting limit's
// own ceiling is 10, so anything deeper is a corrupt ParentID chain.
const maxNestedDepth = 16

// sortSidebarRows orders rows by last output (newest first), ties by the
// persistent first-seen order.
func sortSidebarRows(rows []sidebarRow) {
	slices.SortFunc(rows, func(a, b sidebarRow) int {
		if c := b.activity().Compare(a.activity()); c != 0 {
			return c
		}
		return a.seq - b.seq
	})
}

// nestedRows collects the nested (subagent) rows by parent session id. Two
// sources, exactly like the web sidebar's collectNestedRows: the LIVE child
// slots (authoritative for live status and the pane) first, then the children
// still in the store — a child whose live runtime was released (retention,
// restart) keeps its row from the persisted snapshot. Children open as panes
// render in the same place, under their parent.
func (m *Model) nestedRows() map[string][]sidebarRow {
	// Fast path: buildSidebarRows runs on every render (including per token
	// batch while streaming), and the overwhelming majority of frames have no
	// child slot and no stored child at all — do not allocate for them.
	hasChild := false
	for _, s := range m.lives.sessions {
		if s.child != nil {
			hasChild = true
			break
		}
	}
	if !hasChild {
		for _, si := range m.savedCache {
			if si.ParentID != "" {
				hasChild = true
				break
			}
		}
	}
	if !hasChild {
		return nil
	}
	out := make(map[string][]sidebarRow)
	live := make(map[string]bool, len(m.lives.sessions))
	for i, s := range m.lives.sessions {
		if s.child == nil {
			continue
		}
		parent := s.parentID
		if parent == "" && s.agent != nil {
			parent = s.agent.ParentID()
		}
		if parent == "" {
			continue
		}
		status, summary := "", ""
		if s.agent != nil {
			status, summary = s.agent.SubagentOutcome()
		}
		r := sidebarRow{
			id:           s.id,
			label:        s.label,
			live:         true,
			focused:      i == m.lives.active,
			streaming:    s.streaming,
			liveIdx:      i,
			parentID:     parent,
			childStatus:  status,
			childSummary: summary,
			lastActive:   m.effectiveLastActive(s),
		}
		if s.agent != nil {
			if l := s.agent.SessionLabelSnapshot(); l != "" {
				r.label = l
			}
			if !s.streaming {
				// Locked accessor: the stream goroutine owns Messages while
				// streaming (see the flat overlay above).
				r.msgCount = s.agent.MessageCount()
			}
		}
		if r.label == "" {
			r.label = "subagent"
		}
		r.seq = m.sidebarSeqOf(r.id)
		live[s.id] = true
		out[parent] = append(out[parent], r)
	}
	for _, si := range m.savedCache {
		if si.ParentID == "" || live[si.ID] {
			continue
		}
		r := sidebarRow{
			id:           si.ID,
			label:        si.Label,
			liveIdx:      -1,
			parentID:     si.ParentID,
			msgCount:     si.MessageCount,
			childStatus:  si.SubagentStatus,
			childSummary: si.SubagentSummary,
		}
		if ts, err := time.Parse(time.RFC3339Nano, si.UpdatedAt); err == nil {
			r.updatedAt = ts
		}
		if r.label == "" {
			r.label = si.ID
		}
		r.seq = m.sidebarSeqOf(r.id)
		out[si.ParentID] = append(out[si.ParentID], r)
	}
	for _, kids := range out {
		sortSidebarRows(kids)
	}
	return out
}

// appendNestedRows interleaves each flat row's nested children directly behind
// it, recursively (depth >= 2 renders as grandchildren). The recursion lives
// here so a parent that reorders carries its children with it. A child whose
// parent row is missing (a parent pruned from the store, or not listed yet)
// still renders — at the end, top-level — so a running child can never vanish
// from the panel.
func (m *Model) appendNestedRows(rows []sidebarRow) []sidebarRow {
	if m.lives == nil {
		return rows
	}
	nested := m.nestedRows()
	if len(nested) == 0 {
		return rows
	}
	out := make([]sidebarRow, 0, len(rows)+len(nested))
	placed := make(map[string]bool, len(rows))
	var addChildren func(parentID string, depth int)
	addChildren = func(parentID string, depth int) {
		for _, kid := range nested[parentID] {
			if depth > maxNestedDepth || placed[kid.id] {
				continue // cycle guard / one row per session id
			}
			placed[kid.id] = true
			kid.depth = depth
			out = append(out, kid)
			addChildren(kid.id, depth+1)
		}
	}
	for _, r := range rows {
		if placed[r.id] {
			continue
		}
		placed[r.id] = true
		out = append(out, r)
		addChildren(r.id, 1)
	}
	// Orphans: sorted parent ids keep the output deterministic (a map walk
	// would reorder the panel between renders).
	orphans := make([]string, 0, len(nested))
	for parentID := range nested {
		if !placed[parentID] {
			orphans = append(orphans, parentID)
		}
	}
	slices.Sort(orphans)
	for _, parentID := range orphans {
		addChildren(parentID, 1)
	}
	return out
}

// seedRootLastActive pins the startup session's output time to its store
// timestamp so a resumed OLD session keeps its earned position instead of
// starting at the top (web: a restored pane is not bumped; only genuinely
// new sessions start fresh). A session with no store entry keeps the
// process-start time set by newLiveSessions.
func (m *Model) seedRootLastActive() {
	if m.lives == nil || m.agent == nil {
		return
	}
	for _, si := range m.savedCache {
		if si.ID != m.agent.SessionID {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, si.UpdatedAt); err == nil {
			m.lives.sessions[0].lastActive = ts
			m.touchSessionActivity(m.agent.SessionID, ts)
		}
		return
	}
}

// relativeTime formats a timestamp the way the web sidebar's "3m ago"
// labels do.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 45*time.Second:
		return "now"
	case d < 90*time.Second:
		return "1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// sidebarVisibleRows is how many sessions fit in a panel of mainLines rows
// (border + header + footer reserved). Each session takes two lines (title
// + meta) plus a blank separator after it — except the last rendered one —
// so k sessions occupy 3k-1 lines. Zero when the panel is too short for
// even one session — the footer still pins to the bottom.
func (m *Model) sidebarVisibleRows(mainLines int) int {
	avail := mainLines - sidebarBorderLines - sidebarHeaderLines - sidebarFooterLines
	v := (avail + 1) / 3
	if v < 0 {
		v = 0
	}
	return v
}

// clampSidebarScrollTo keeps m.sidebarScroll within the bounds of a list
// of n rows.
func (m *Model) clampSidebarScrollTo(n int) {
	if m.sidebarMainLines <= 0 {
		m.sidebarScroll = 0
		return
	}
	maxScroll := n - m.sidebarVisibleRows(m.sidebarMainLines)
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.sidebarScroll < 0 {
		m.sidebarScroll = 0
	}
	if m.sidebarScroll > maxScroll {
		m.sidebarScroll = maxScroll
	}
}

// clampSidebarScroll keeps m.sidebarScroll within the current list bounds.
func (m *Model) clampSidebarScroll() {
	m.clampSidebarScrollTo(len(m.buildSidebarRows()))
}

// resolveSidebarCursor maps the identity-based cursor (sidebarCursorID) to
// its current position in rows, keeping m.sidebarCursor in sync. The list
// reorders at runtime (background completion, /open, store refresh), so
// the id — not a stored index — decides where the cursor points: the
// highlight follows the selected session and x/d act on it even after a
// reorder. When the id no longer exists (row closed/deleted) the stale id
// is dropped and the last positional cursor is kept (clamped).
func (m *Model) resolveSidebarCursor(rows []sidebarRow) int {
	if m.sidebarCursorID != "" {
		for i, r := range rows {
			if r.id == m.sidebarCursorID {
				m.sidebarCursor = i
				return i
			}
		}
		m.sidebarCursorID = ""
	}
	if m.sidebarCursor < 0 {
		m.sidebarCursor = 0
	}
	if m.sidebarCursor >= len(rows) {
		m.sidebarCursor = max(0, len(rows)-1)
	}
	return m.sidebarCursor
}

// setSidebarCursor moves the panel cursor to rows[idx], recording the
// row's identity so runtime reorders keep it on the same session (see
// resolveSidebarCursor).
func (m *Model) setSidebarCursor(rows []sidebarRow, idx int) {
	if idx < 0 || idx >= len(rows) {
		return
	}
	m.sidebarCursor = idx
	m.sidebarCursorID = rows[idx].id
}

// sidebarFocusedRow is the list index of the focused session (for cursor
// initialization when the panel gains keyboard focus). It also records the
// row's identity so the cursor survives later reorders.
func (m *Model) sidebarFocusedRow() int {
	rows := m.buildSidebarRows()
	for i, r := range rows {
		if r.focused {
			m.setSidebarCursor(rows, i)
			return i
		}
	}
	return 0
}

// renderSidebar renders the unified sessions panel: EXACTLY mainLines rows
// tall (no trailing newline), so JoinHorizontal can never grow the combined
// frame past the terminal. Layout: box border (top row, right column,
// bottom row) around the header, working dir, and session rows (title:
// label + ✕ action; meta: state dot + state · msgs · relative time — the
// web row's structure), with a blank separator line between consecutive
// sessions. The border is dim, highlighted while the mouse is over the
// panel.
func (m *Model) renderSidebar(mainLines int) string {
	w := m.sidebarWidth
	if w < minSidebarWidth {
		w = minSidebarWidth
	}
	inner := w - 3 // "│ " prefix + content cells (the right "│" is appended)
	m.sidebarMainLines = mainLines

	rows := m.buildSidebarRows()
	visible := m.sidebarVisibleRows(mainLines)
	// Resolve the identity-based cursor to its current position: the list
	// may have reordered since the last render (background completion,
	// /open, store refresh), and the cursor must follow its session, not
	// its old slot. A resolved position change counts as a cursor move for
	// the window-follow logic below, so a reorder can never strand the
	// cursor outside the visible window.
	cursor := m.resolveSidebarCursor(rows)
	// Keep the cursor inside the visible window — but only when the
	// cursor itself moved since the last render (keyboard/click nav, or a
	// reorder that relocated it). A wheel scroll moves the window away
	// from the cursor and must not be snapped back to it on the next
	// frame.
	if cursor != m.sidebarLastCursor {
		if cursor < m.sidebarScroll {
			m.sidebarScroll = cursor
		}
		if cursor >= m.sidebarScroll+visible {
			m.sidebarScroll = cursor - visible + 1
		}
	}
	m.sidebarLastCursor = cursor
	m.clampSidebarScrollTo(len(rows))

	// dim pads a (possibly pre-styled) content string to the panel width.
	// Measurement is ANSI-aware so styled meta lines truncate and pad by
	// their VISIBLE width (raw-rune counting would break on escape codes).
	// The pad is measured on the CUT result, not the input: a wide rune
	// straddling the boundary makes ansi.Cut return one cell less than
	// requested (it never splits a double-width rune), and padding by the
	// input's width then left that row a cell short — the right border
	// stepped left on exactly those lines.
	dim := func(s string) string {
		t := cutCells(s, 0, inner)
		return ansiDimOn + "│ " + t +
			strings.Repeat(" ", max(0, inner-cellWidth(t))) + ansiReset
	}
	out := make([]string, 0, mainLines)
	add := func(s string) {
		if len(out) < mainLines-sidebarBorderLines {
			out = append(out, s)
		}
	}

	add(ansiHighlightOn + "│ " + "SESSIONS" +
		strings.Repeat(" ", max(0, inner-8)) + ansiReset)
	wd := ""
	if m.agent != nil {
		wd = m.agent.WorkingDir
	}
	add(dim(wd))
	add(dim(""))

	for i := 0; i < visible && m.sidebarScroll+i < len(rows); i++ {
		r := rows[m.sidebarScroll+i]
		add(m.renderSidebarTitle(r, i+m.sidebarScroll == cursor, inner))
		add(dim(m.sidebarMeta(r)))
		// Blank separator between consecutive sessions (none after the
		// last rendered one).
		if i+1 < visible && m.sidebarScroll+i+1 < len(rows) {
			add(dim(""))
		}
	}

	// Pad, then pin the footer to the bottom two rows (above the bottom
	// border).
	for len(out) < mainLines-sidebarBorderLines-sidebarFooterLines {
		add(dim(""))
	}
	add(dim(""))
	// A modal overlay parks the mouse handlers, so never render a stale
	// hover highlight under it.
	if m.modal != ModalNone {
		m.sidebarHover = sidebarFooterNone
	}
	add(m.renderSidebarFooter(inner))
	if len(out) > mainLines-sidebarBorderLines {
		out = out[:mainLines-sidebarBorderLines]
	}

	// Box border: dim normally, highlighted while the mouse is over the
	// panel (sidebarHovering, tracked in handleMouseMsg). Every inner row
	// is exactly w-1 cells wide, so the appended right border brings each
	// line to exactly w — the panel's column budget.
	borderStyle := ansiDimOn
	if m.sidebarHovering {
		borderStyle = ansiCyanOn
	}
	for i := range out {
		out[i] += borderStyle + "│" + ansiReset
	}
	top := borderStyle + "┌" + strings.Repeat("─", w-2) + "┐" + ansiReset
	bottom := borderStyle + "└" + strings.Repeat("─", w-2) + "┘" + ansiReset
	return strings.Join(append(append([]string{top}, out...), bottom), "\n")
}

// sidebarFooterPlain is the footer's unstyled text (the measure for the
// narrow-panel fallback).
const sidebarFooterPlain = sidebarFooterNewLabel + "  " + sidebarFooterCloseLabel + "  " +
	sidebarFooterDeleteLabel + "  " + sidebarFooterRest

// sidebarFooterButtonsLen is the content width of the three button
// segments plus their gaps — the minimum inner width at which they render
// (and are hit-testable).
const sidebarFooterButtonsLen = len(sidebarFooterNewLabel) + 2 + len(sidebarFooterCloseLabel) +
	2 + len(sidebarFooterDeleteLabel)

// renderSidebarFooter renders the panel's footer hint line with the
// "n new", "x close", and "d del" segments styled as clickable buttons
// (cyan; the hovered button gets the row-highlight style). The buttons
// always render first; only the hint tail truncates when the panel is
// narrow. The line is padded to the panel width like the other rows, with
// one cell reserved before the right border so the hint never runs flush
// against it; a panel too narrow for the buttons falls back to the plain
// truncated hint (and sidebarFooterAt misses too).
func (m *Model) renderSidebarFooter(inner int) string {
	if inner < sidebarFooterButtonsLen {
		// Reserve one cell before the right border like the button form.
		return ansiDimOn + "│ " + textutil.TruncateRunes(sidebarFooterPlain, max(0, inner-1)) +
			" " + ansiReset
	}
	btn := func(b int) string {
		if m.sidebarHover == b {
			return ansiHighlightOn
		}
		return ansiCyanOn
	}
	tail := "  " + sidebarFooterRest
	if inner < len(sidebarFooterPlain)+1 {
		tail = textutil.TruncateRunes(tail, inner-sidebarFooterButtonsLen-1)
	}
	line := btn(sidebarFooterNew) + sidebarFooterNewLabel + ansiReset + "  " +
		btn(sidebarFooterClose) + sidebarFooterCloseLabel + ansiReset + "  " +
		btn(sidebarFooterDelete) + sidebarFooterDeleteLabel + ansiReset +
		ansiDimOn + tail
	// line carries ANSI codes — measure the VISIBLE width (sliceRuneLen
	// counts the escape runes too, which collapsed the padding and left
	// the right border off the panel edge on wide panels).
	pad := inner - cellWidth(line)
	if pad < 0 {
		pad = 0
	}
	return ansiDimOn + "│ " + line + strings.Repeat(" ", pad) + ansiReset
}

// sidebarFooterAt reports which footer button a position lands on. The
// zones are EXACT columns: the buttons are separated by two-column gaps,
// so any tolerance wider than the button itself would let a click in a
// gap (or in the "│ " prefix) trigger the nearest button. ok=false for the
// non-interactive footer parts, every other row, and a panel too narrow to
// render the buttons.
func (m *Model) sidebarFooterAt(x, y int) (button int, ok bool) {
	if m.sidebarMainLines <= 0 || y != m.sidebarMainLines-sidebarBorderLines {
		return sidebarFooterNone, false
	}
	if m.sidebarWidth-3 < sidebarFooterButtonsLen {
		return sidebarFooterNone, false // buttons not rendered at this width
	}
	for _, z := range sidebarFooterZones() {
		if x >= z.x0 && x <= z.x1 {
			return z.button, true
		}
	}
	return sidebarFooterNone, false
}

// depthIndent is the tree indent for a nested (subagent) row at depth: two
// cells per level behind a "└ " glyph, capped at three levels so a long chain
// cannot widen the panel past its column budget. Empty for a top-level row.
// Shared by the title and meta lines so a row's two lines line up.
func depthIndent(depth int) string {
	if depth <= 0 {
		return ""
	}
	return strings.Repeat("  ", min(depth-1, 3)) + "└ "
}

// renderSidebarTitle renders one row's title line the way the web row's
// title does: a plain full-fg label (the cursor row gets the highlight
// style — the web "current" row's emphasis) with the right-aligned ✕
// action. The state dot lives on the meta line (web parity — see
// sidebarMeta). The ✕ sits two cells inside the drag handle so click zones
// never overlap (see sidebarRowAt). The label is truncated to the row's
// visible-cell budget (cutCells), so wide-rune labels cannot widen the
// panel past its column budget.
func (m *Model) renderSidebarTitle(r sidebarRow, cursor bool, inner int) string {
	prefix := "  "
	if cursor {
		prefix = "▸ "
	}
	// Content cells: prefix(2) + label + pad + "✕ "(2). The label is
	// truncated and the pad computed in VISIBLE cells (cutCells /
	// cellWidth), like the panel's other rows: rune counting
	// under-pads wide runes (CJK, emoji) by half their width, which pushed
	// the ✕ — and the right border — past the panel edge and widened the
	// whole joined frame past the terminal (the inline renderer would
	// soft-wrap and desync).
	// Nested (subagent) rows indent under their parent behind a tree glyph.
	// The indent eats into the label budget in VISIBLE cells (like the pad
	// below), so a deep chain cannot widen the panel past its column budget;
	// the glyph is capped at 3 steps to keep the label readable.
	indent := depthIndent(r.depth)
	labelMax := inner - 5 - cellWidth(indent)
	if labelMax < 2 {
		labelMax = 2
	}
	label := cutCells(r.label, 0, labelMax)
	pad := inner - 4 - cellWidth(label) - cellWidth(indent)
	if pad < 1 {
		pad = 1
	}

	var b strings.Builder
	b.WriteString(ansiDimOn)
	b.WriteString("│ " + prefix + indent)
	if cursor {
		b.WriteString(ansiHighlightOn)
	} else {
		b.WriteString(ansiReset) // web title is full fg, not dim
	}
	b.WriteString(label)
	b.WriteString(ansiReset)
	b.WriteString(strings.Repeat(" ", pad))
	b.WriteString(ansiDimOn)
	b.WriteString("✕ ")
	b.WriteString(ansiReset)
	return b.String()
}

// sidebarMeta renders one row's meta line the way the web row's meta row
// does: a colored state dot (amber ● responding / green ● active / cyan ●
// open) ahead of the muted state label, then message count and relative
// time, " · "-separated. Saved rows carry no state — the web shows no dot
// for them either.
func (m *Model) sidebarMeta(r sidebarRow) string {
	dotStyle, state := "", ""
	dot := true
	switch {
	case r.depth > 0 && r.live:
		// A live child's dot carries its state (web parity): amber while the
		// child responds, red when it failed, green when it finished, cyan
		// while it is idle-but-continuable.
		switch {
		case r.streaming:
			dotStyle, state = ansiPromptOn, "responding"
		case r.childStatus == "failed":
			dotStyle, state = ansiRedOn, "failed"
		case r.childStatus == "success":
			dotStyle, state = ansiGreenOn, "done"
		default:
			dotStyle, state = ansiCyanOn, "idle"
		}
	case r.depth > 0:
		// A settled child (released, or restored from the store) renders a
		// MUTED TEXT status instead of a dot, so an archived outcome never
		// reads as an active session.
		dot = false
		state = "done"
		if r.childStatus == "failed" {
			state = "failed"
		}
	case r.streaming:
		dotStyle, state = ansiPromptOn, "responding"
	case r.live && r.focused:
		dotStyle, state = ansiGreenOn, "active"
	case r.live:
		dotStyle, state = ansiCyanOn, "open"
	}
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(depthIndent(r.depth))
	if state != "" {
		if dot {
			b.WriteString(dotStyle)
			b.WriteString("● ")
		}
		b.WriteString(ansiDimOn)
		b.WriteString(state)
	}
	if r.msgCount > 0 {
		if state != "" {
			b.WriteString(" · ")
		}
		b.WriteString(ansiDimOn)
		b.WriteString(fmt.Sprintf("%d msgs", r.msgCount))
	}
	if rel := relativeTime(r.activity()); rel != "" {
		if state != "" || r.msgCount > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(ansiDimOn)
		b.WriteString(rel)
	}
	b.WriteString(ansiReset)
	return b.String()
}

// sidebarRowAt maps a mouse position to (row index, action-zone). ok=false
// when the position is outside the row area (border/header/footer rows).
// Each session occupies a three-line group (title + meta + blank
// separator); the separator line maps to the session above it. The action
// zone is the ✕ glyph at the right edge of a TITLE line: it closes a live
// row (stays saved) or deletes a saved row (confirm). Row groups start at
// sidebarHeaderLines+1 (the top border row is 0).
func (m *Model) sidebarRowAt(x, y int) (row int, actionZone bool, ok bool) {
	if m.sidebarOffsetX() == 0 || x < 0 || x >= m.sidebarOffsetX() {
		return 0, false, false
	}
	start := sidebarHeaderLines + sidebarBorderLines - 1
	if y < start || m.sidebarMainLines <= 0 {
		return 0, false, false
	}
	visible := m.sidebarVisibleRows(m.sidebarMainLines)
	// The window is 3*visible-1 lines tall (no trailing separator).
	if y >= start+3*visible-1 {
		return 0, false, false
	}
	rows := m.buildSidebarRows()
	i := (y-start)/3 + m.sidebarScroll
	if i < 0 || i >= len(rows) {
		return 0, false, false
	}
	// ✕ sits at terminal column sidebarWidth-3; the zone is the glyph plus
	// two columns to its LEFT (sidebarWidth-5..sidebarWidth-3). The column
	// right of the glyph (sidebarWidth-2) and the border (sidebarWidth-1)
	// belong to the drag handle, which handleSidebarResizeMouse consumes
	// earlier — the zones are disjoint. The title line is the first line
	// of each 3-line group.
	actionZone = (y-start)%3 == 0 &&
		x >= m.sidebarWidth-5 && x <= m.sidebarWidth-3
	return i, actionZone, true
}

// handleSidebarMouse processes mouse events over the panel: wheel scrolls
// the list, left-press selects the row and acts on it (row body →
// focus/resume, ✕ → close/delete, footer "n new"/"x close" → the n/x key
// actions), and button-less motion tracks the footer-button hover
// highlight. Returns true when the event was consumed (it must NOT start a
// text selection or scroll the chat).
func (m *Model) handleSidebarMouse(ev mouseEvent) (bool, tea.Cmd) {
	if m.modal != ModalNone || m.sidebarOffsetX() == 0 {
		return false, nil
	}
	switch {
	case ev.kind == mouseWheelEvent && ev.x < m.sidebarOffsetX():
		// Scroll the session list (the web list scrolls the same way);
		// never let the wheel move the chat under the panel.
		if ev.button == tea.MouseWheelUp {
			m.sidebarScroll--
		} else {
			m.sidebarScroll++
		}
		m.clampSidebarScroll()
		return true, nil
	case ev.kind == mouseMotion && ev.button == tea.MouseNone && ev.x < m.sidebarOffsetX():
		// Hover highlight over the footer buttons (cell motion is on).
		// Button-less motion never drives text selection, so consuming it
		// over the panel is safe; held-button motion falls through.
		hover := sidebarFooterNone
		if b, ok := m.sidebarFooterAt(ev.x, ev.y); ok {
			hover = b
		}
		if hover != m.sidebarHover {
			m.sidebarHover = hover
		}
		return true, nil
	case ev.kind == mousePress && ev.button == tea.MouseLeft && ev.x < m.sidebarOffsetX():
		m.sidebarHover = sidebarFooterNone
		// Footer buttons act like their keys: "n new" spawns a live
		// session, "x close" closes the cursor row's pane (stays saved),
		// "d del" deletes the cursor row with confirmation. The returned
		// cmd (e.g. the progress spinner tick when a switch lands on a
		// streaming session) must reach Update — dropping it here left
		// the spinner frozen after mouse-driven switches.
		if b, ok := m.sidebarFooterAt(ev.x, ev.y); ok {
			switch b {
			case sidebarFooterNew:
				return true, m.openNewLiveSession("")
			case sidebarFooterClose:
				if rows := m.buildSidebarRows(); len(rows) > 0 {
					return true, m.sidebarCloseRow(m.resolveSidebarCursor(rows))
				}
				return true, nil
			case sidebarFooterDelete:
				if rows := m.buildSidebarRows(); len(rows) > 0 {
					return true, m.sidebarDeleteRow(m.resolveSidebarCursor(rows))
				}
				return true, nil
			}
		}
		row, actionZone, ok := m.sidebarRowAt(ev.x, ev.y)
		if !ok {
			return true, nil // header/footer: consume, no selection
		}
		// Record the row's identity: a reorder before the next action must
		// keep the cursor (and the ✕/x/d targets) on this session.
		if rows := m.buildSidebarRows(); row < len(rows) {
			m.setSidebarCursor(rows, row)
		}
		m.focus = FocusSidebar
		if actionZone {
			// Web parity: ✕ on a live row closes the pane (stays
			// saved); ✕ on a saved row deletes it (with confirmation).
			if rows := m.buildSidebarRows(); rows[row].live {
				return true, m.sidebarCloseRow(row)
			}
			return true, m.sidebarDeleteRow(row)
		}
		return true, m.sidebarOpenRow(row)
	}
	return false, nil
}

// handleSidebarKey dispatches keys when the sessions panel has focus.
// Mouse is the primary interaction (web parity); this is the keyboard
// equivalent: ↑↓/jk move, enter open, x close (stays saved), d delete
// (confirm), n new, i/esc back.
func (m *Model) handleSidebarKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Help) {
		m.modal = ModalHelp
		return m, nil
	}
	// Cancel turn / compact or quit (shared handler; stays on sidebar
	// focus — refocusInput is a no-op here).
	if key.Matches(msg, m.keys.CancelTurn) {
		_, cmd, _ := m.handleCancelKey()
		return m, cmd
	}
	rows := m.buildSidebarRows()
	n := len(rows)
	// Identity-based cursor: nav moves the cursor by position but records
	// the target row's id, so a reorder between keypresses can never make
	// enter/x/d act on a different session than the highlighted one.
	cursor := m.resolveSidebarCursor(rows)
	switch msg.String() {
	case "up", "k":
		if cursor > 0 {
			m.setSidebarCursor(rows, cursor-1)
		}
	case "down", "j":
		if n > 0 && cursor < n-1 {
			m.setSidebarCursor(rows, cursor+1)
		}
	case "pgup":
		if cursor > 10 {
			m.setSidebarCursor(rows, cursor-10)
		} else {
			m.setSidebarCursor(rows, 0)
		}
	case "pgdown":
		if n > 0 && cursor < n-10 {
			m.setSidebarCursor(rows, cursor+10)
		} else if n > 0 {
			m.setSidebarCursor(rows, n-1)
		}
	case "home", "g":
		m.setSidebarCursor(rows, 0)
	case "end", "G":
		if n > 0 {
			m.setSidebarCursor(rows, n-1)
		}
	case "enter":
		return m, m.sidebarOpenRow(cursor)
	case "x":
		if n > 0 {
			return m, m.sidebarCloseRow(cursor)
		}
	case "d":
		if n > 0 {
			return m, m.sidebarDeleteRow(cursor)
		}
	case "n":
		return m, m.openNewLiveSession("")
	case "i", "esc":
		m.focus = FocusInput
		return m, m.textarea.Focus()
	case "[":
		m.resizeSidebar(-4)
	case "]":
		m.resizeSidebar(4)
	}
	// Any other printable character jumps to the input and types (same as
	// viewport focus) — the panel is a browser, not a text field.
	if c := msg.Code; c >= 32 && c < 127 && msg.Mod == 0 {
		m.focus = FocusInput
		focusCmd := m.textarea.Focus()
		var updateCmd tea.Cmd
		m.textarea, updateCmd = m.textarea.Update(msg)
		return m, tea.Batch(focusCmd, updateCmd)
	}
	return m, nil
}

// sidebarOpenRow focuses a live row (switchToLive) or opens a saved row as
// its own live session (web openSessionPane parity — safe even mid-turn).
// No-op when the row is already the focused session.
func (m *Model) sidebarOpenRow(row int) tea.Cmd {
	rows := m.buildSidebarRows()
	if row < 0 || row >= len(rows) {
		return nil
	}
	r := rows[row]
	if r.live {
		if r.liveIdx == m.lives.active {
			return nil
		}
		cmd := m.switchToLive(r.liveIdx)
		// Same feedback line as /switch and the active-sessions modal, so
		// every focus entry point announces the switch (the new-session
		// path prints its own "Opened live session" line instead).
		m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Switched to session: %s", r.label)))
		return cmd
	}
	return m.resumeSavedRow(r.id)
}

// closeLiveSession closes one live slot, taking its continuable children with
// it: a child whose parent is gone cannot deliver its report or wait for a
// reply (the web host's eviction cascade, at slot scope). Child slots
// themselves are released through the lifecycle — liveSessions.Close refuses
// them, because dropping the slot alone would leave the lifecycle holding a
// dead pane.
func (m *Model) closeLiveSession(i int) error {
	if m.lives == nil {
		return nil
	}
	if s := m.lives.ByIndex(i); s != nil && s.child == nil && s.agent != nil && m.spawner != nil {
		m.spawner.CancelAll(s.agent.SessionID)
	}
	return m.lives.Close(i)
}

// sidebarCloseRow closes the row's live pane — the session stays SAVED and
// its row reappears as a saved row (web closePane). Close and delete are
// distinct actions: this never deletes. A saved row has nothing to close;
// a responding session must be cancelled first; the FOCUSED session closes
// by moving focus to the newest-output remaining live session (web
// closePane parity) — the last open session cannot be closed.
func (m *Model) sidebarCloseRow(row int) tea.Cmd {
	rows := m.buildSidebarRows()
	if row < 0 || row >= len(rows) {
		return nil
	}
	r := rows[row]
	if !r.live {
		m.statusMsg = "Close: session is saved, not open in this TUI (d deletes it)"
		return nil
	}
	if s := m.lives.sessions[r.liveIdx]; s.child != nil {
		// A child's pane is owned by the continuable-subagent lifecycle, not
		// by the session list: closing it RELEASES the child (its runtime is
		// torn down, the saved session stays resumable) through the same path
		// the retention window uses. Removing the slot here would leave the
		// lifecycle holding a dead pane.
		if c := s.child.childOf(); c != nil && m.spawner != nil {
			m.spawner.ReleaseChild(c)
			m.statusMsg = "Released " + r.label + " (still saved)"
			m.sidebarCursor = max(0, min(m.sidebarCursor, len(m.buildSidebarRows())-1))
			return nil
		}
	}
	if s := m.lives.sessions[r.liveIdx]; s.streaming {
		m.statusMsg = "Close: " + r.label + " is responding — cancel its turn first"
		return nil
	}
	if r.focused {
		if len(m.lives.sessions) == 1 {
			m.statusMsg = "Close: the only open session cannot be closed (d deletes it)"
			return nil
		}
		// Web parity (closePane): focus moves to the newest-output
		// remaining live session.
		target := -1
		var best time.Time
		for i, s := range m.lives.sessions {
			if i == r.liveIdx {
				continue
			}
			if at := m.effectiveLastActive(s); target == -1 || at.After(best) {
				target, best = i, at
			}
		}
		cmd := m.switchToLive(target)
		if err := m.closeLiveSession(r.liveIdx); err != nil {
			m.statusMsg = "Close: " + err.Error()
			return nil
		}
		m.statusMsg = "Closed " + r.label + " (still saved)"
		m.sidebarCursor = max(0, min(m.sidebarCursor, len(m.buildSidebarRows())-1))
		return tea.Batch(cmd, m.requestSavedSessions())
	}
	if err := m.closeLiveSession(r.liveIdx); err != nil {
		m.statusMsg = "Close: " + err.Error()
		return nil
	}
	m.statusMsg = "Closed " + r.label + " (still saved)"
	m.sidebarCursor = max(0, min(m.sidebarCursor, len(m.buildSidebarRows())-1))
	return m.requestSavedSessions()
}

// sidebarDeleteRow asks for confirmation and PERMANENTLY deletes the row's
// session (web deleteSession). A background LIVE row must be closed first
// (x): deleting behind a hosted agent would leave it running on a deleted
// id. The focused live row deletes through the agent, which starts a fresh
// session in its place.
func (m *Model) sidebarDeleteRow(row int) tea.Cmd {
	rows := m.buildSidebarRows()
	if row < 0 || row >= len(rows) {
		return nil
	}
	r := rows[row]
	if r.live && !r.focused {
		m.statusMsg = "Delete: close the session first (x), then delete it"
		return nil
	}
	// The focused row deletes through the agent (rebind) — refuse up front
	// while it is busy so the user is not asked to confirm a delete that
	// cannot run yet. deleteSavedRow re-checks at execution time: the
	// confirm modal can stay open while a turn starts in the meantime.
	if r.focused {
		if reason := m.rebindBlockReason("Delete"); reason != "" {
			m.statusMsg = reason
			return nil
		}
	}
	m.confirmText = fmt.Sprintf("Delete session %q?\nIts message history will be permanently deleted.", r.label)
	m.confirmAction = func() (tea.Model, tea.Cmd) {
		m.modal = ModalNone
		return m, m.deleteSavedRow(r.id)
	}
	m.modal = ModalConfirm
	return nil
}

// rebindBlockReason reports why the focused agent must not be rebound or
// reset right now ("" = safe). Rebinding mid-turn swaps Messages out from
// under the running stream goroutine: the left-behind session's file
// freezes at the rebind (its in-flight reply is never saved) and the old
// turn's tail persists under the NEW session's id. Rebinding
// mid-compaction lets the in-flight CompactHistory rewrite the session
// that replaces the current one (it runs on the same agent object).
// handleSubmitKey gates the TYPED entry points, but sidebar mouse/keyboard
// actions and the confirm-modal callback run outside it — every rebind
// entry point must check this (resumeSavedRow, deleteSavedRow).
func (m *Model) rebindBlockReason(verb string) string {
	switch {
	case m.streaming:
		return verb + ": wait for the current turn to finish (or cancel it first)."
	case m.compacting:
		return verb + ": wait for compaction to finish (or cancel it with ctrl+c)."
	}
	return ""
}

// resumeSavedRow opens a saved session (the web's openSessionPane). Shared
// by the sidebar, the /resume sessions modal, and typed /resume (cmdSession
// routes through here so every entry point gets the same guards).
//
// Web parity (loadOrCreateRuntime): a saved session gets its OWN live agent
// loaded from the store snapshot — the focused agent is never rebound, so
// opening a session is allowed at ANY time, even while the focused session
// is mid-turn or mid-compaction (its turn keeps running in the background
// under its own SessionID). Without a workspace (single-session host) the
// only option is rebinding the focused agent, which stays gated on
// rebindBlockReason.
func (m *Model) resumeSavedRow(id string) tea.Cmd {
	if id == "" || id == m.sessionID {
		return nil
	}
	// A session still hosted in a background slot is FOCUSED, not opened
	// again: a second agent with the same SessionID would both persist to
	// the same file — the new agent's first save is a full snapshot that
	// wipes the other's pending delta, and the load-time merge drops the
	// mismatched writer's tail (the partial-restore bug). The web dedupes
	// in loadOrCreateRuntime for exactly this reason. switchToLive is safe
	// even mid-turn (it joins the running turn instead of rebinding).
	if m.lives != nil {
		if i := m.lives.indexBySessionID(id); i >= 0 {
			return m.switchToLive(i)
		}
	}
	if m.workspace != nil && m.lives != nil {
		return m.spawnSavedSession(id)
	}
	// Single-session host (no workspace): the only option is rebinding the
	// focused agent, which is unsafe mid-turn or mid-compaction (see
	// rebindBlockReason).
	if reason := m.rebindBlockReason("Resume"); reason != "" {
		m.appendChatLine(ErrorStyle.Render(reason))
		return nil
	}
	result, _, err := m.agent.HandleSessionCommand(m.ctx, "resume "+id, session.NewID())
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
		// The resume flushes the left-behind session before loading; a
		// failed load can still leave a persist error behind.
		m.checkPersistError()
		return nil
	}
	if result.Action == agent.SessionActionClearChat {
		cmd := m.applySessionSwitch(result)
		// The resume flushed the left-behind session; surface a failed
		// save instead of losing it silently (cmdSession's old contract).
		m.checkPersistError()
		return cmd
	}
	return nil
}

// spawnSavedSession opens a saved session as a NEW live session — the TUI's
// openSessionPane (web parity): the store snapshot is loaded into a fresh
// agent through the shared workspace factory (the web's
// loadOrCreateRuntime), the session is registered in the live registry, and
// focus moves to it. The focused agent is NEVER rebound, so this is safe
// even while the focused session is mid-turn or mid-compaction: its turn
// keeps running in the background under its own SessionID and its
// persistence is untouched (a rebind would swap Messages out from under
// the running stream goroutine — see rebindBlockReason).
func (m *Model) spawnSavedSession(id string) tea.Cmd {
	if m.workspace.Store == nil {
		m.appendChatLine(ErrorStyle.Render("Resume: session persistence disabled."))
		return nil
	}
	snap, err := m.workspace.Store.LoadInWorkingDir(m.workspace.GetWorkingDir(), id)
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
		return nil
	}
	a := m.workspace.NewSessionAgent(&snap, id)
	// Background validation (ValidateRestoredModelAsync) may clear or
	// auto-select the restored model after the UI opened; re-render when it
	// lands (the TUI runner hooks the same event for the root agent).
	if snap.Model != "" {
		// Capture the sender HERE (Update thread): the hook fires from
		// the validation goroutine, and m.program is written once in
		// TUI.Run without synchronization — reading it inside the closure
		// would race that write (the same hazard spawnLiveSession
		// documents for its job-notice sender). Run assigns m.program
		// before the event loop starts, so the captured value is final.
		sender := m.program
		a.ValidateRestoredModelAsync(snap.Model, func() {
			if sender != nil {
				sender.Send(modelChangedMsg{})
			}
		})
	}
	m.spawnLiveSession(a)
	i := len(m.lives.sessions) - 1
	// switchToLive rebuilds the transcript from the restored Messages and
	// rebinds every focused mirror; it is safe even mid-turn.
	cmd := m.switchToLive(i)
	m.appendChatLine(SystemStyle.Render(agent.ResumeOutputMessage(id, snap.Messages)))
	// The resumed session keeps its earned position (web: makePane seeds
	// initialActivity from the saved session's updatedAt); the left-behind
	// session keeps its in-process output stamp.
	m.rebindActivity()
	return tea.Batch(cmd, m.requestSavedSessions())
}

// deleteSavedRow deletes a saved session by id (the web's deleteSession).
// Shared by the sidebar ✕/x action and the /resume sessions modal. Refuses
// (nil cmd, status message) when the id is still hosted in a background
// live slot — see the guard below.
func (m *Model) deleteSavedRow(id string) tea.Cmd {
	// Deleting the CURRENT session rebinds the focused agent (a fresh
	// session starts in its place) — the same mid-turn / mid-compaction
	// hazard as resume. Checked HERE, not only in sidebarDeleteRow,
	// because the confirm modal can stay open while a delivery turn starts
	// on the focused session in the meantime. Deleting a NON-current
	// session rebinds nothing and is always safe. m.agent.SessionID is
	// the source of truth (m.sessionID is its mirror).
	if id == m.agent.SessionID {
		if reason := m.rebindBlockReason("Delete"); reason != "" {
			m.appendChatLine(ErrorStyle.Render(reason))
			return nil
		}
	} else if m.lives != nil && m.lives.indexBySessionID(id) >= 0 {
		// A session still hosted in a BACKGROUND live slot must be closed
		// (x) before it can be deleted: deleting the file behind the live
		// agent leaves the slot running, and the agent's next flush
		// (turn end, close, quit sweep) re-creates the file — the deletion
		// silently reverts. The sidebar's ✕/d path refuses these rows up
		// front (sidebarDeleteRow); this guard covers the /resume modal,
		// which lists saved-but-live sessions too. The focused session is
		// exempt: it deletes through the agent, which rebinds to a fresh
		// session in its place (the branch above).
		m.statusMsg = "Delete: close the session first (x), then delete it"
		return nil
	}
	result, _, err := m.agent.HandleSessionCommand(m.ctx, "resume del "+id, session.NewID())
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
		return nil
	}
	if result.Action == agent.SessionActionClearChat {
		// The deleted session was the current one: the agent started a
		// fresh session — rebuild the transcript around the notice.
		m.chatLines = nil
		m.chatLines = append(m.chatLines, SystemStyle.Render(result.Output))
		m.setViewportContent()
		m.pinFollow() // fresh transcript (the deleted session was current)
		m.sessionID = m.agent.SessionID
		// The fresh session has no store entry yet: creation is its first
		// recency event, so it starts at the top (web: makePane without
		// initialActivity).
		m.rebindActivity()
		m.refreshContextStats()
	} else {
		m.appendChatLine(SystemStyle.Render(result.Output))
	}
	// No cursor reset: the identity-based cursor keeps its session when the
	// list changes, and falls back to the (clamped) old position when the
	// deleted row was the cursor's — the row that slid into the slot.
	return m.requestSavedSessions()
}

// spawnLiveSession registers a workspace-built agent as a new live session
// and wires the per-session TUI extras both spawn paths (openNewLiveSession,
// spawnSavedSession) need:
//   - the parent-generic subagent spawner (the TUI spawner builds each
//     child from the calling agent + shared cfg, so opened sessions get
//     working subagents instead of "spawner not installed");
//   - the focus-aware job-notice hook (the workspace deliverer is nil on
//     the TUI host: focused → normal delivery turn; background → attributed
//     system line via the replay buffer — a global delivery turn would run
//     on whichever session is focused, not this one).
//
// Returns the new slot (always the last one).
func (m *Model) spawnLiveSession(a *agent.Agent) *liveSession {
	// The workspace seeds its spawner into every agent it creates, so a
	// session built by the shared factory already has one. A session built
	// without it (the nil-workspace path) gets the host's single spawner
	// here — one registry per host, never one per session.
	if m.spawner != nil && a.SubagentSpawner() == nil {
		a.SetSubagentSpawner(m.spawner)
	}
	s := m.lives.Add(a, "")
	if m.cfg != nil && m.cfg.JobNoticesEnabled() {
		// Capture the sender HERE (Update thread): the hook fires from a
		// background goroutine and must not read m.program, which is
		// written once in TUI.Run without synchronization.
		sender := m.program
		a.SetJobNoticeHook(jobNoticeHookFor(s, func(summary string) {
			if sender != nil {
				// The notice belongs to THIS slot: name it, so a focus
				// switch before tea delivers the message cannot re-attribute
				// the notice to whichever session is focused by then.
				sender.Send(deliveryRequestMsg{sid: s.id, text: summary})
			}
		}))
	}
	return s
}

// openNewLiveSession spawns an additional live session through the shared
// web lifecycle core and focuses it immediately (the web sidebar's "New"
// button). Shared by /open and the panel's n key.
func (m *Model) openNewLiveSession(label string) tea.Cmd {
	if m.workspace == nil {
		m.appendChatLine(ErrorStyle.Render("Open: no workspace attached (single-session host)."))
		return nil
	}
	// Inherit the FOCUSED session's model + thinking level (web
	// createNewSession parity). The workspace default is the startup-era
	// seed and set_model never advances it (model state is per-session, D1),
	// so without this every spawned session would start on whatever model
	// the workspace was built with instead of the one the user last picked.
	// The typed /new inherits implicitly (it reuses the same provider); the
	// spawned session must do it explicitly. The inheritance carries the
	// FULL routing key (owning profile + ID), never the bare ID: a bare ID
	// resets the owner to the default profile and silently re-homes a model
	// the focused session runs on a different provider (a same-ID model on
	// the OpenCode catalog would steal it).
	var inheritRef llm.ModelRef
	var inheritLevel agent.ThinkingLevel
	if m.agent != nil {
		inheritRef = m.agent.CurrentModelRef()
		_, inheritLevel = m.agent.ModeAndThinkingLevel()
	}
	a := m.workspace.NewSessionAgent(nil, session.NewID())
	// Adopt only when it differs from the fresh provider's seed (the
	// workspace default — verified by construction). An equal routing key
	// needs no adoption; the shared-provider path (single-session test/embed
	// hosts) has a.CurrentModelRef() == the focused ref, so it is a no-op
	// and the shared provider is never mutated. An adopted model is marked
	// unverified and re-checked against the provider on the first turn
	// (requireModelSelected), the same safety net a restored model gets.
	if inheritRef.Valid() && inheritRef != a.CurrentModelRef() {
		a.AdoptModelRef(inheritRef)
	}
	if inheritLevel != "" {
		// Seeds the fresh session's agent + provider and persists the level
		// into the new session file. Runs AFTER the model adoption: the
		// setter flushes, and the flush must persist the inherited model,
		// not the pre-adoption default seed.
		a.SetThinkingLevel(inheritLevel)
	}
	s := m.spawnLiveSession(a)
	// Creation is the fresh session's first recency event (web: makePane
	// without initialActivity) — it starts at the top of the list.
	m.touchSessionActivity(a.SessionID, time.Now())
	if label == "" {
		// Derive from the unique registry id so labels never collide after
		// closes ("session-2", "session-4", …).
		label = "session-" + strings.TrimPrefix(s.id, "s")
	}
	s.label = label
	i := len(m.lives.sessions) - 1
	// switchToLive already moves the panel cursor (with identity) to the
	// newly focused session's row, wherever it sorts.
	cmd := m.switchToLive(i)
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Opened live session %q (%s).", label, s.id)))
	return tea.Batch(cmd, m.requestSavedSessions())
}

// sidebarTickMsg re-renders the panel's relative-time labels and picks up
// store changes (the web's 30 s sidebar tick). One loop runs for the whole
// program lifetime (started in Init); it is a no-op while the panel is
// hidden, so toggling can never fork a second loop.
type sidebarTickMsg struct{}

// sidebarTickCmd schedules the next 30 s panel refresh.
func sidebarTickCmd() tea.Cmd {
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg { return sidebarTickMsg{} })
}
