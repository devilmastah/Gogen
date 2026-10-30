package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/bubbletea/v2"

	"gogen/internal/agent"
	"gogen/internal/ioutil"
	"gogen/internal/steerq"
	"gogen/internal/streambuf"
)

// maxPendingStreamEvents bounds one background session's replay buffer
// (oldest dropped first, freshness wins — same policy as the delivery
// queue).
const maxPendingStreamEvents = 4096

// Sidebar geometry: clamp range for the live-sessions panel and the
// narrowest main column we tolerate before auto-hiding it.
//
// The live clamp range is TERMINAL-RELATIVE (web parity: the web sidebar
// scales from 12% of the viewport, floored at 120px, up to 50% of the
// viewport) — see sidebarMinWidth/sidebarMaxWidth. minSidebarWidth is the
// absolute floor (the web's 120px) and maxSidebarWidth the absolute
// ceiling (also the sanity bound for persisted widths).
const (
	defaultSidebarWidth = 38 // ≈ the web's 300px default at ~8px per cell
	minSidebarWidth     = 16
	maxSidebarWidth     = 80
	minMainWidth        = 40
)

// liveSession is one actively hosted conversation inside the TUI process.
// The focused session owns the transcript buffers and progress UI; other
// live sessions continue running turns in the background — their stream
// events are buffered in pending (see enqueue/popAll) and replayed when
// focus arrives, while their terminal state always surfaces attributed.
//
// Ownership contract: streaming/cancel state is mutated only from the
// Update thread (turn-start writes happen synchronously inside submit
// paths; finalization happens in handleTurnFinishedMsg), mirroring the
// existing single-session Messages-ownership rule.
type liveSession struct {
	id    string
	label string
	agent *agent.Agent

	// parentID is the owning session id for a NESTED (subagent) slot, empty
	// for a user session: it is what nests the sidebar row and what makes the
	// slot a child's pane. Immutable after admission.
	parentID string
	// child is the continuable-subagent handle for a child slot (nil for user
	// sessions). The handle mirrors the few fields the parent's tool goroutine
	// needs (turn state, queue depth, cancel), so nothing here is touched
	// off the Update thread.
	child *childHandle

	cancel    context.CancelFunc // cancels the in-flight turn; nil when idle
	streaming bool
	// turnSeq is the generation of the in-flight turn: incremented on every
	// submit, stamped on every stream message the turn's adapter emits.
	// After cancel + resubmit the superseded turn's goroutine may still be
	// unwinding; its stragglers (tokens AND the terminal) carry the old
	// seq and are dropped by the Update thread instead of clobbering the
	// new turn's state (cancel func, streaming flags, transcript).
	// Mutated only on the Update thread.
	turnSeq uint64

	// /compact runs off the Update thread (it makes an LLM summarization
	// call). compacting gates the FOCUSED session's input band + submit
	// (mirrored onto Model.compacting like streaming); compactCancel lets
	// ctrl+c abort the in-flight compaction instead of quitting;
	// compactUserCancelled marks a user-initiated abort so the late
	// compactResultMsg does not double-report its context error.
	// Mutated only on the Update thread (like streaming).
	compacting           bool
	compactCancel        context.CancelFunc
	compactUserCancelled bool
	// compactSeq is the generation of the in-flight /compact: incremented
	// by cmdCompact on every start and stamped on the result. After
	// cancel + immediate restart the superseded run's goroutine may still
	// be unwinding; its late compactResultMsg carries the old seq and is
	// dropped instead of clearing the new run's flags (which would unlock
	// input mid-compaction and orphan its cancel func) or reporting the
	// old cancellation as a fresh failure. Mutated only on the Update
	// thread.
	compactSeq uint64

	// lastActive is the last OUTPUT time (completed turn, session spawn) —
	// the sidebar's ordering key for live sessions (web: pane.lastActivity,
	// written on output only — never on focus, submit, or persistence).
	// Mutated only on the Update thread.
	lastActive time.Time

	// focused reports whether this session currently owns the transcript
	// UI. Read from stream goroutines (the adapter gate), hence atomic.
	focused atomic.Bool

	// progress is this session's wait-indicator state; the Model's
	// progressPhase/progressLabel/activeToolName are the FOCUSED session's
	// mirror of these fields. switchToLive restores them when focus returns
	// to a streaming session, so the indicator shows the session's actual
	// phase ("running execute_command…") instead of resetting to "thinking".
	// Mutated only on the Update thread.
	progressPhase progressPhase
	progressLabel string
	// progressRetry records that the label explains a stream recovery (see
	// Model.progressRetry): restored on a focus switch like the phase, and
	// cleared with the rest of the progress state.
	progressRetry bool
	activeTool    string
	// streamSpeedLine is this session's rendered token rate ("42 tok/s")
	// for the progress line, updated by handleStreamStatsMsg (the shared
	// streamutil.SpeedMeter) while the session is focused. Mirrored onto
	// the Model like the progress fields above; joinStreamingSession
	// restores it when focus returns mid-round.
	streamSpeedLine string

	// pending buffers display-only notes (condensedNoteMsg) emitted while
	// this session streamed in the background — see enqueue for why no
	// other event kind is kept. On focus the buffer is drained and
	// DISCARDED: a streaming target's join renders the full committed
	// history from SnapshotMessages (every drained event is older than
	// that snapshot),
	// and an idle target's rebuild is authoritative. The drain is still
	// mandatory — an undrained buffer would replay stale events on the
	// NEXT join (mu guards cross-thread).
	mu      sync.Mutex
	pending []tea.Msg

	// transcriptStale latches that the transcript shown for this session
	// was rebuilt from a mid-turn join snapshot (joinStreamingSession) and
	// lags the stream: the in-flight round's head was not in Messages at
	// the join, and a commit/event straddle can duplicate one line. The
	// turn-end convergence rebuild (handleTurnFinishedMsg) re-renders the
	// authoritative history and clears it; the idle switch path clears it
	// too (its rebuild is already authoritative). Mutated only on the
	// Update thread.
	transcriptStale bool

	// round is the current round's in-flight LLM output buffer (see
	// streambuf.RoundBuffer): self-synchronizing leaf state — the stream
	// goroutine appends, the Update thread snapshots on join.
	round streambuf.RoundBuffer

	// steerQ is this session's FIFO of turns waiting for the session
	// to go idle (steering.go): system deliveries (job notices, scheduled
	// reminders, subagent reports — the TUI counterpart of the web
	// server's delivery queue) and user steering messages typed while a
	// turn runs. Drained ONE item per turn at every terminal boundary
	// (handleTurnFinishedMsg, handleCompactResultMsg, drainDeliveries), in
	// strict arrival order across both kinds. Per-session — following the
	// session on focus switches — so a queued message runs on the session
	// it was typed into, never on whichever session happens to be focused
	// at drain time. Update-thread-only state, like streaming/turnSeq —
	// no lock.
	//
	// The queue itself is internal/steerq, shared with the web host: item
	// kinds, caps, the notice-batching rule and the "zero popped slots"
	// requirement live there (they used to be implemented twice). Reads are
	// nil-safe; writes go through queue(), which creates it.
	steerQ *steerq.Queue
}

// queue returns this session's steering queue, creating it on first use.
// Update-thread only (see steerQ).
func (s *liveSession) queue() *steerq.Queue {
	if s.steerQ == nil {
		// NewDefault applies the shared bounds (steerq.UserCap /
		// steerq.SystemCap): refuse user text at the cap, evict the oldest
		// notice at the system cap, never evict a targeted reply.
		s.steerQ = steerq.NewDefault()
	}
	return s.steerQ
}

// queueLen is the nil-safe read every render path uses.
func (s *liveSession) queueLen() int {
	if s == nil {
		return 0
	}
	return s.steerQ.Len()
}

// The session's round buffer (s.round, streambuf.RoundBuffer) accumulates
// the current round's in-flight LLM output so a mid-turn join can render
// the current reply from its first character. The web server's attach
// rewind is the SAME buffer's snapshot: WSMessage.Rewind is a
// streambuf.Snapshot (registry.go), and the join here renders that same
// struct — the payload shape cannot drift between the two hosts. The
// assistant message only reaches a.Messages when the round completes, so
// without this buffer a join mid-round would show the committed history
// with the in-flight reply missing until the turn-end rebuild.
//
// Fed by the stream adapter from round start REGARDLESS of focus (the
// adapter sees every event; the pending buffer only accumulates while
// unfocused — that is the gap this closes: watch A stream, switch away,
// come back mid-round). Cleared at turn start, round start, and round end
// via the shared streambuf.RoundSink (the adapter's OnStart/OnRoundStart/
// OnStreamEnd — the same timing the web server's wsStreamSink calls): the
// empty state IS the "between rounds" marker (Snapshot returns nil) —
// completed content lives in Messages already, so a join between rounds
// must not render it a second time.

// resetProgress clears the session's wait-indicator state (turn end,
// cancel, error).
func (s *liveSession) resetProgress() {
	s.progressPhase = progressHidden
	s.progressLabel = ""
	s.progressRetry = false
	s.activeTool = ""
	s.streamSpeedLine = ""
}

// enqueue buffers one event produced while this session streamed in the
// background. Only display-only notes survive a background span; every
// other rendering event is already reflected in Messages by the time focus
// returns (the join snapshot, or the idle rebuild), and both drains
// (switchToLive, joinStreamingSession) keep nothing but a condensedNoteMsg
// — so holding a tool result, a diff, a token batch or an image payload
// here would only pin that payload in memory until the drain threw it
// away. maxPendingStreamEvents stays as the safety net for note floods.
func (s *liveSession) enqueue(msg tea.Msg) {
	if _, ok := msg.(condensedNoteMsg); !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) >= maxPendingStreamEvents {
		s.pending = s.pending[1:]
	}
	s.pending = append(s.pending, msg)
}

// popAll detaches buffered events for replay on the Update thread once
// this session gains focus.
func (s *liveSession) popAll() []tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = nil
	return out
}

// jobNoticeHookFor returns the focus-aware job-completion hook for s:
// while s is focused the summary runs a normal delivery turn (deliver);
// while s is backgrounded it is buffered as an attributed condensed note
// and surfaced when focus returns (switchToLive replays an idle target's
// notes). deliver runs on a background goroutine and must be safe to call
// from one (e.g. tea.Program.Send).
func jobNoticeHookFor(s *liveSession, deliver func(summary string)) func(summary string) {
	return func(summary string) {
		if s.focused.Load() {
			deliver(summary)
			return
		}
		// seq stays 0: the note is not part of any turn, and its replay
		// path (switchToLive's manual filter) bypasses the staleTurn
		// guard, which would otherwise drop it once turnSeq > 0.
		s.enqueue(condensedNoteMsg{note: noticeLabel + " job finished: " + summary, sid: s.id})
	}
}

// liveSessions hosts every active session. Index 0 is the startup agent;
// additional entries are spawned through the shared web lifecycle core
// (server.NewWorkspaceForHost → NewSessionAgent).
//
// This is the TUI counterpart of the web server's session registry: the
// sidebar's ACTIVE section lists exactly these entries, while INACTIVE
// sessions are the persisted snapshots in the shared session store.
type liveSessions struct {
	sessions []*liveSession
	active   int
}

func newLiveSessions(root *agent.Agent) *liveSessions {
	root0 := &liveSession{id: "s1", label: "main", agent: root, lastActive: time.Now()}
	root0.focused.Store(true)
	return &liveSessions{sessions: []*liveSession{root0}}
}

// Active returns the focused session (never nil after construction).
func (ls *liveSessions) Active() *liveSession {
	return ls.sessions[ls.active]
}

// ByID resolves a session by id; nil when unknown (e.g. closed mid-turn).
func (ls *liveSessions) ByID(id string) *liveSession {
	for _, s := range ls.sessions {
		if s.id == id {
			return s
		}
	}
	return nil
}

// ByAgent resolves the slot hosting the given agent (pointer identity);
// nil when no live slot hosts it (e.g. the agent was rebound away or the
// session closed). compactResultMsg carries the compacting agent, not a
// slot id, so the result path needs this mapping. Read on the Update
// thread like ByID.
func (ls *liveSessions) ByAgent(a *agent.Agent) *liveSession {
	for _, s := range ls.sessions {
		if s.agent == a {
			return s
		}
	}
	return nil
}

// indexBySessionID returns the index of the slot hosting the given agent
// SessionID, or -1 when no live slot hosts it. The slot ids ("s1", "s2")
// are stream-routing ids, not session ids, so ByID does not apply — the
// sidebar overlay (buildSidebarRows) and the resume guard both need this
// mapping. Read on the Update thread like buildSidebarRows.
func (ls *liveSessions) indexBySessionID(id string) int {
	if id == "" {
		return -1
	}
	for i, s := range ls.sessions {
		if s.agent != nil && s.agent.SessionID == id {
			return i
		}
	}
	return -1
}

// Add registers a new live session WITHOUT switching focus. The caller
// owns building the agent (shared workspace factory) and flushing it on
// close.
func (ls *liveSessions) Add(a *agent.Agent, label string) *liveSession {
	s := &liveSession{id: nextLiveID(ls), label: label, agent: a, lastActive: time.Now()}
	ls.sessions = append(ls.sessions, s)
	return s
}

// AddChild registers a NESTED (subagent) child as a live session. The slot's
// routing id IS the child's session id: every stream event, terminal and
// notice for that child already carries it, so no extra id space is needed
// (and session.NewID cannot collide with the "sN" slot ids). Children are
// added unstreaming and unfocused; RunTurn starts their first turn.
func (ls *liveSessions) AddChild(a *agent.Agent, label string, h *childHandle) *liveSession {
	id := a.SessionID
	if id == "" {
		id = nextLiveID(ls)
	}
	s := &liveSession{
		id:         id,
		label:      label,
		agent:      a,
		parentID:   a.ParentID(),
		child:      h,
		lastActive: time.Now(),
	}
	ls.sessions = append(ls.sessions, s)
	return s
}

// indexOf returns the slot index of s, or -1.
func (ls *liveSessions) indexOf(s *liveSession) int {
	for i, cur := range ls.sessions {
		if cur == s {
			return i
		}
	}
	return -1
}

// remove drops the slot at index i, keeping the focused slot focused (a child
// leaving the registry must not steal focus from the user's session).
func (ls *liveSessions) remove(i int) {
	if i < 0 || i >= len(ls.sessions) {
		return
	}
	ls.sessions = append(ls.sessions[:i], ls.sessions[i+1:]...)
	if ls.active > i {
		ls.active--
	}
	if ls.active >= len(ls.sessions) && len(ls.sessions) > 0 {
		ls.active = len(ls.sessions) - 1
	}
}

// Switch focuses session i, flipping the adapter gates accordingly.
// Switching away from a streaming session leaves its turn running.
func (ls *liveSessions) Switch(i int) {
	if i < 0 || i >= len(ls.sessions) || i == ls.active {
		return
	}
	old := ls.sessions[ls.active]
	old.focused.Store(false)
	ls.active = i
	ls.sessions[i].focused.Store(true)
}

// nextLiveID derives the next stable routing id ("s2", "s3", …).
func nextLiveID(ls *liveSessions) string {
	n := len(ls.sessions) + 1
	for {
		id := "s" + strconv.Itoa(n)
		if ls.ByID(id) == nil {
			return id
		}
		n++
	}
}

// switchToLive focuses live session i and rebinds every piece of focused
// Model state to its agent — including the streaming mirror: m.streaming
// describes the FOCUSED session, so leaving a mid-turn session unlocks
// input on the newly focused one, and joining one locks it again. The
// transcript is rebuilt from Messages using the same converge-on-switch
// contract as /resume; the previous session keeps its turn running in the
// background either way.
func (m *Model) switchToLive(i int) tea.Cmd {
	if m.lives == nil {
		return nil
	}
	target := m.lives.ByIndex(i)
	if target == nil || target == m.lives.Active() {
		return nil
	}
	// Drain the replay buffer on BOTH sides of the focus flip (see
	// joinStreamingSession): before it, while the adapter gate is still
	// closed, and after it, to catch events emitted in between. A single
	// pre-flip drain would strand those straddlers in the buffer forever
	// (they would surface on the NEXT join); a single post-flip drain
	// would miss them entirely. The drained events are DISCARDED for a
	// streaming target (its join snapshot renders everything committed)
	// and only display-only notes are surfaced for an idle target — the
	// drain is unconditional because those notes are not part of Messages.
	pending := target.popAll()
	m.lives.Switch(i)
	pending = append(pending, target.popAll()...)
	// Was the OLD focused session animating? The tick decision below
	// compares against this — the mirror is rebound before it runs.
	wasAnimating := m.progressAnimating()
	m.agent = target.agent
	m.sessionID = target.agent.SessionID
	// A background session's failed save is surfaced when it gains focus
	// (the resume/delete paths already do this).
	m.checkPersistError()
	var cmds []tea.Cmd
	if target.streaming {
		// Joined mid-turn: render the full committed history from a
		// SnapshotMessages (web attach parity) and let the live tail flow
		// in below it; the authoritative convergence rebuild happens at
		// turn end (transcriptStale).
		m.joinStreamingSession(target, pending)
	} else {
		// Rebuild transcript (mirrors cmdSession's clear-chat resume path).
		m.rebuildTranscriptFrom(target.agent)
		// The rebuild is authoritative: heal any stale latch left by an
		// earlier mid-turn join whose turn ended while this session was in
		// the background (the focused turn-end path never ran for it).
		target.transcriptStale = false
		// An idle target's buffered STREAM events are already reflected in
		// Messages (the rebuild above is authoritative — the turn
		// finalized before streaming cleared), so replaying them would
		// duplicate the finished turn: drop them. Only display-only notes
		// (condensedNoteMsg, e.g. a background job notice) are not part of
		// Messages and must be surfaced.
		for _, msg := range pending {
			if note, ok := msg.(condensedNoteMsg); ok {
				m.appendChatLine(SystemStyle.Render(note.note))
			}
		}
	}
	// Sync the focused-session mirrors + progress UI to the target:
	// m.streaming/m.compacting describe the FOCUSED session, so leaving a
	// busy session unlocks input on the newly focused one, and joining a
	// busy one locks it again. The queued-messages count in the progress
	// strip is a focused-session mirror too, and the band height depends
	// on the busy state — re-derive both.
	m.streaming = target.streaming
	m.compacting = target.compacting
	m.applyComposerPlaceholder()
	m.relayout()
	if target.streaming {
		if m.progressAnimating() && !wasAnimating {
			cmds = append(cmds, m.spinner.Tick)
		}
	} else {
		if target.compacting && !wasAnimating {
			// The compacting indicator animates via the spinner loop
			// (handleSpinnerTick ticks while compacting); restart it when
			// the old focused session was not animating.
			cmds = append(cmds, m.spinner.Tick)
		}
		m.clearProgress()
	}
	m.setViewportContent()
	m.pinFollow() // fresh transcript (the focused session changed)
	// The target's token-count cache may be cold (a large restored
	// session): probe off the Update thread, like the resume path — a
	// synchronous probe would tokenize the whole history on the render
	// loop. The indicator updates when contextStatsMsg lands.
	cmds = append(cmds, m.requestContextStats())
	// Keep the panel's cursor on the focused row (web: the focused row is
	// the highlighted one).
	m.sidebarCursor = m.sidebarFocusedRow()
	return tea.Batch(cmds...)
}

// joinStreamingSession renders the full committed history, the in-flight
// round (when one is streaming), and — only when nothing is in flight — a
// join notice; subsequent events flow live because focus has already
// flipped.
//
// The history comes from SnapshotMessages — the documented, race-tested
// contract the web attachSession uses (server.go): a mid-turn join shows
// every prior message at once instead of a bare "joined" notice. The
// in-flight round comes from the session's round buffer (web Rewind
// parity): it accumulates the current round's output regardless of focus,
// so the join shows the current reply from its first character and the
// live tail continues seamlessly from the pre-seeded stream state. The
// pending buffer is DISCARDED, not replayed: every drained event is older
// than the snapshots, so its content is already rendered above and
// replaying it would duplicate finished rounds. Accepted residual, healed
// by the turn-end convergence rebuild (handleTurnFinishedMsg, latched via
// transcriptStale):
//   - a few tokens at the seam: tokens already program.Send-ed but not yet
//     processed when the join ran (the web's batcher lag) briefly
//     duplicate at the pre-seeded line; a Messages commit and its render
//     event can likewise straddle the snapshot.
//
// pending is drained by the caller AROUND the focus flip (see
// switchToLive): once before it (adapter gate still closed) and once after
// (catching events emitted between the two drains). Both drains must stay:
// an undrained buffer would replay stale events on the NEXT join.
func (m *Model) joinStreamingSession(target *liveSession, pending []tea.Msg) {
	// Capture the phase the session recorded BEFORE the rebind: the reset
	// below wipes the mirror (and, via setActiveTool, the session's
	// recorded tool name). Restoring first lets the indicator show the
	// session's actual phase ("running X…") instead of a hardcoded
	// "thinking".
	phase, label, tool := target.progressPhase, target.progressLabel, target.activeTool
	// Full committed history first (web attach parity): the snapshot is
	// taken AFTER the caller's double drain, so every drained event is
	// either already rendered here or is an accepted residual (see above).
	m.chatLines = renderMessages(
		target.agent.SnapshotMessages(),
		target.agent.WorkingDir,
		target.agent.CurrentModel(),
		target.agent.Mode.String(),
		m.verbose,
	)
	m.markTranscriptRendered()
	// Reset BEFORE the round render: renderRoundBuffer pre-seeds the
	// stream state this wipes (buffers, line indices, tool-call maps).
	m.resetStreamState(false)
	// In-flight round (web Rewind parity): render the current reply from
	// its first character and pre-seed the stream state so the next live
	// token appends seamlessly. Empty buffer == between rounds (or before
	// the first token): the completed content is in the snapshot above,
	// and the join notice marks the wait for the next round's output. The
	// turn-end convergence rebuild replaces the whole transcript either
	// way (notice included).
	if rw := target.round.Snapshot(); rw != nil {
		m.renderRoundBuffer(rw)
	} else {
		m.chatLines = append(m.chatLines, SystemStyle.Render("▍ joined \""+target.label+"\" mid-turn"))
	}
	// Latch the convergence: the transcript lags the stream until the
	// turn-end rebuild re-renders the authoritative history.
	target.transcriptStale = true
	m.progressPhase = phase
	m.progressLabel = label
	m.progressRetry = target.progressRetry
	m.activeToolName = tool
	m.streamSpeedLine = target.streamSpeedLine
	if phase == progressHidden {
		// A streaming session records its phase at submit; hidden means
		// the turn was just cancelled and its terminal has not landed yet
		// — show the default wait (the old hardcoded behavior).
		m.progressPhase, m.progressLabel = progressThinking, "thinking"
		m.progressRetry = false
	}
	// Discard the drained STREAM events (their committed content is in the
	// snapshot above). Display-only notes are not part of Messages and
	// must still surface (idle-path parity).
	for _, msg := range pending {
		if note, ok := msg.(condensedNoteMsg); ok {
			m.chatLines = append(m.chatLines, SystemStyle.Render(note.note))
		}
	}
}

// renderRoundBuffer renders the in-flight round's accumulated output
// (streambuf.Snapshot) below the committed history and pre-seeds the stream
// state so the next live event continues the round seamlessly (web Rewind
// parity: the client renders the rewind through the normal stream
// machinery and continues the live stream after it).
//
// The display mirrors what the focused live path would show at this
// moment: the thinking block is OPEN only while nothing has followed it
// (the first content token or tool call closes it exactly as in the live
// path); tool-call lines show args only once the raw JSON is parseable
// (the live path's handleStreamToolArgs rule) — the next args delta or
// final re-renders the line from the seeded accumulation.
//
// Boundary: the Update-thread serialization orders the join-instant
// snapshot strictly before the next delivered token EXCEPT for tokens
// already program.Send-ed but not yet processed (the web's batcher lag,
// a few tokens wide) — those briefly duplicate at the seam. The turn-end
// convergence rebuild (transcriptStale) heals it.
func (m *Model) renderRoundBuffer(rw *streambuf.Snapshot) {
	if rw.Thinking != "" {
		if rw.Content == "" && len(rw.ToolCalls) == 0 {
			// Thinking still open: seed it live. The next content token
			// (or tool call) closes the block via closeThinkingBlock,
			// exactly as in the focused path.
			displayBuf := strings.TrimRight(rw.Thinking, "\n")
			m.streamThinkingLine = len(m.chatLines)
			m.chatLines = append(m.chatLines, renderStyledBlock(ThinkingStyle, "<thinking>"+displayBuf))
			m.streamThinkingOpen = true
			m.streamThinkingBuf.WriteString(rw.Thinking)
			// Seed the incremental render cache from the snapshot so the
			// next live token advances it instead of starting from empty.
			m.rebuildStreamThinkingCache()
		} else {
			// Content or tool calls followed: the live path already
			// closed the block — render it closed.
			m.chatLines = append(m.chatLines, renderStyledBlock(ThinkingTagStyle, "<thinking>"+rw.Thinking+"</thinking>"))
		}
	}
	if rw.Content != "" {
		label := AssistantStyle.Render(assistantLabel)
		m.streamAssistantLine = len(m.chatLines)
		m.chatLines = append(m.chatLines, label+" "+rw.Content)
	}
	prefix := ToolCallStyle.Render("  →")
	for _, tc := range rw.ToolCalls {
		m.streamToolCallNames[tc.Index] = tc.Name
		m.streamToolCallIDs[tc.Index] = tc.ID
		m.streamToolCallArgs[tc.Index] = &streamToolArgs{buf: []byte(tc.Args)}
		line := prefix + " " + tc.Name
		if args, err := parseInlineJSONArgs([]byte(tc.Args)); err == nil && len(args) > 0 {
			line += " " + ToolCallArgsStyle.Render(formatToolArgs(args))
		}
		m.streamToolCallLines[tc.Index] = len(m.chatLines)
		m.chatLines = append(m.chatLines, line)
	}
}

// Close detaches an idle background session after flushing it; it stays
// resumable under /resume. The focused or still-streaming session cannot
// be closed.
func (ls *liveSessions) Close(i int) error {
	if i < 0 || i >= len(ls.sessions) {
		return fmt.Errorf("no such session")
	}
	s := ls.sessions[i]
	if i == ls.active {
		return fmt.Errorf("session %q is focused", s.label)
	}
	if s.child != nil {
		// A nested (subagent) slot is owned by the continuable lifecycle: it
		// leaves the registry through release (retention, parent teardown, or
		// the sidebar's close action), never by dropping the slot here.
		return fmt.Errorf("session %q is a subagent child — release it instead", s.label)
	}
	if s.streaming {
		return fmt.Errorf("session %q is streaming — cancel its turn first", s.label)
	}
	if s.agent != nil {
		// FlushPending, not FlushSession: a clean session must not be
		// re-stamped. FlushSession forces a full write even when nothing
		// changed, and every save stamps Updated=now — that pushed the
		// just-closed session to the top of the saved-session list
		// (recency is interaction-based: the session that received the
		// last message is the newest). A dirty session (unsaved turn tail)
		// still writes, so no state is lost. Mirrors the web registry's
		// evictRuntime and the shutdown sweep.
		s.agent.FlushPending()
	}
	ls.sessions = append(ls.sessions[:i], ls.sessions[i+1:]...)
	if ls.active > i {
		ls.active--
	}
	return nil
}

// ByIndex returns the session at index i, or nil when out of range.
func (ls *liveSessions) ByIndex(i int) *liveSession {
	if i < 0 || i >= len(ls.sessions) {
		return nil
	}
	return ls.sessions[i]
}

// uiPrefs persists sidebar panel state per working directory, so the
// panel reopens with the geometry the user left it at (the TUI analogue
// of the web UI's localStorage preferences).
type uiPrefs struct {
	SidebarVisible bool `json:"sidebarVisible"`
	SidebarWidth   int  `json:"sidebarWidth"`
}

func uiPrefsPath(workingDir string) string {
	return filepath.Join(workingDir, ".gogen", "tui.json")
}

// loadUIPrefs best-effort reads saved panel state. found=false means no
// usable file existed (missing or corrupt) — distinct from an explicit
// saved choice, because the two drive different defaults.
func loadUIPrefs(workingDir string) (uiPrefs, bool) {
	p := uiPrefs{SidebarWidth: defaultSidebarWidth}
	data, err := os.ReadFile(uiPrefsPath(workingDir))
	if err != nil {
		return p, false
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return uiPrefs{SidebarWidth: defaultSidebarWidth}, false
	}
	if p.SidebarWidth < minSidebarWidth || p.SidebarWidth > maxSidebarWidth {
		p.SidebarWidth = defaultSidebarWidth
	}
	return p, true
}

// resolveSidebarStart resolves the panel's startup state: ALWAYS visible
// (web parity — the web sidebar opens by default), with the width the user
// last chose. A hidden preference from older builds is ignored: the panel
// is the multi-session surface and must stay discoverable.
func resolveSidebarStart(workingDir string) uiPrefs {
	p, _ := loadUIPrefs(workingDir)
	return uiPrefs{SidebarVisible: true, SidebarWidth: p.SidebarWidth}
}

// saveUIPrefs writes panel state. Errors are silently ignored — losing a
// width preference must never surface as a user-facing failure. The write
// is atomic (a torn file would silently revert to defaults on next start,
// which is the one outcome a plain os.WriteFile could produce) and
// fsync-free: the file is tiny and written on every panel toggle and
// resize, where fsync would add SSD wear for no user-visible benefit.
func saveUIPrefs(workingDir string, p uiPrefs) {
	path := uiPrefsPath(workingDir)
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	_ = ioutil.WriteFileAtomicNoSync(path, data, 0o600)
}

// persistSidebarPrefs saves current panel state for the active session's
// working directory (no-op without an agent).
func (m *Model) persistSidebarPrefs() {
	if m.agent == nil || m.agent.WorkingDir == "" {
		return
	}
	saveUIPrefs(m.agent.WorkingDir, uiPrefs{
		SidebarVisible: m.sidebarVisible,
		SidebarWidth:   m.sidebarWidth,
	})
}

// staleTurn reports whether an attributed event belongs to a SUPERSEDED
// turn of a known session (its seq no longer matches the session's
// current turnSeq). seq 0 means UNATTRIBUTED — produced outside any turn,
// e.g. the job-notice hook enqueues condensed notes without a generation
// (their replay-buffer path bypasses this guard by construction); such
// events always pass through so normal ownership routing applies.
// Production stream events never carry 0: every submit increments turnSeq
// before constructing the adapter. Unknown sessions pass through: the
// existing ownsStream / ByID-nil handling applies to them.
func (m *Model) staleTurn(sid string, seq uint64) bool {
	if m.lives == nil {
		return false
	}
	s := m.lives.ByID(sid)
	return s != nil && seq != 0 && s.turnSeq != seq
}

// ownsStream reports whether sid belongs to the focused session.
func (m *Model) ownsStream(sid string) bool {
	return m.lives == nil || m.lives.Active().id == sid
}

// endOfStream / failOfStream are the ONLY constructors for a turn's
// terminal messages: they enforce that every terminal carries its owning
// session id AND turn generation, so handleTurnFinishedMsg can always
// route finalization and drop superseded turns. (A bare streamEndMsg{}
// would finalize nothing — ByID("") misses — and brick the focused
// session's input forever.)
func endOfStream(sid string, seq uint64) tea.Msg {
	return streamEndMsg{sid: sid, seq: seq}
}

func failOfStream(sid string, seq uint64, err error) tea.Msg {
	return streamErrorMsg{err: err, sid: sid, seq: seq}
}

// turnSession returns the session that owns a newly-started turn,
// lazily materializing the registry for Models built without one
// (unit tests, embedded hosts).
func (m *Model) turnSession() *liveSession {
	if m.lives == nil {
		if m.agent != nil {
			m.lives = newLiveSessions(m.agent)
		} else {
			s := &liveSession{id: "transient"}
			s.focused.Store(true)
			return s
		}
	}
	return m.lives.Active()
}

// sidebarOffsetX is the horizontal shift of the chat column caused by the
// visible sessions panel (0 when hidden or auto-hidden). Mouse hit-testing
// must apply it so drag-selection maps to content coordinates.
func (m *Model) sidebarOffsetX() int {
	if m.sidebarVisible && m.width-m.sidebarWidth >= minMainWidth {
		return m.sidebarWidth
	}
	return 0
}

// mainWidth is the horizontal space the chat column may use: the full
// terminal width, minus the sidebar when it is visible and fits.
func (m *Model) mainWidth() int {
	return m.width - m.sidebarOffsetX()
}

// sidebarMinWidth is the live lower clamp: 12% of the terminal (web
// parity), floored at minSidebarWidth (the web's 120px floor).
func (m *Model) sidebarMinWidth() int {
	if w := m.width * 12 / 100; w > minSidebarWidth {
		return w
	}
	return minSidebarWidth
}

// sidebarMaxWidth is the live upper clamp: 50% of the terminal (web
// parity), never squeezing the main column below minMainWidth, and capped
// at maxSidebarWidth.
func (m *Model) sidebarMaxWidth() int {
	w := m.width / 2
	if cap := m.width - minMainWidth; w > cap {
		w = cap
	}
	if w > maxSidebarWidth {
		w = maxSidebarWidth
	}
	if w < minSidebarWidth {
		w = minSidebarWidth
	}
	return w
}

// toggleSidebar shows/hides the panel and re-lays-out the chat column.
// Returns the async saved-sessions refresh cmd when the panel is shown.
func (m *Model) toggleSidebar() tea.Cmd {
	m.sidebarVisible = !m.sidebarVisible
	if m.sidebarVisible && m.sidebarWidth == 0 {
		m.sidebarWidth = defaultSidebarWidth
	}
	if m.width > 0 {
		m.SetSize(m.width, m.height)
	}
	var cmd tea.Cmd
	if m.sidebarVisible {
		cmd = m.requestSavedSessions()
	} else if m.focus == FocusSidebar {
		// Never strand keyboard focus on a hidden panel.
		m.focus = FocusInput
	}
	m.persistSidebarPrefs()
	return cmd
}

// resizeSidebar steps the panel width by delta (clamped to the live
// terminal-relative range) and re-lays-out. The upper clamp never exceeds
// width - minMainWidth, so keyboard resize can never auto-hide the panel.
func (m *Model) resizeSidebar(delta int) {
	if !m.sidebarVisible {
		return
	}
	w := m.sidebarWidth + delta
	if w < m.sidebarMinWidth() {
		w = m.sidebarMinWidth()
	}
	if w > m.sidebarMaxWidth() {
		w = m.sidebarMaxWidth()
	}
	if w == m.sidebarWidth {
		return
	}
	m.sidebarWidth = w
	if m.width > 0 {
		m.SetSize(m.width, m.height)
	}
	m.persistSidebarPrefs()
}

// onSidebarBorder reports whether a mouse event landed on the panel's
// right border — the drag handle. The press zone is the border column plus
// ONE column of tolerance to its left; the right side has no tolerance
// because the next column is the chat area (a press there must start a text
// selection, not a panel drag).
func (m *Model) onSidebarBorder(ev mouseEvent) bool {
	if !m.sidebarVisible || m.sidebarOffsetX() == 0 {
		return false
	}
	bx := m.sidebarWidth - 1
	return ev.x >= bx-1 && ev.x <= bx
}

// handleSidebarResizeMouse drives border-drag resizing. Returns true when
// the event was consumed (it must NOT start a text selection).
//
// Precedence: a left-press within the border tolerance begins the drag;
// motion with the button held maps the panel width to the cursor; release
// finalizes and persists. While dragging, every event is swallowed so the
// transcript neither selects nor scrolls.
func (m *Model) handleSidebarResizeMouse(ev mouseEvent) bool {
	switch {
	case ev.kind == mousePress && ev.button == tea.MouseLeft && m.onSidebarBorder(ev):
		m.sidebarDragging = true
		return true
	case m.sidebarDragging && ev.kind == mouseMotion:
		w := ev.x + 1
		if w < m.sidebarMinWidth() {
			w = m.sidebarMinWidth()
		}
		if w > m.sidebarMaxWidth() {
			w = m.sidebarMaxWidth()
		}
		if w != m.sidebarWidth {
			m.sidebarWidth = w
			if m.width > 0 {
				m.SetSize(m.width, m.height)
			}
		}
		return true
	case m.sidebarDragging && ev.kind == mouseRelease:
		m.sidebarDragging = false
		m.persistSidebarPrefs()
		return true
	case m.sidebarDragging:
		return true // button-less motion etc.: swallow mid-drag
	}
	// Wheel over the panel is handled by handleSidebarMouse (it scrolls
	// the session list); this handler only owns the border drag.
	return false
}

// cancelActiveStream cancels the focused session's in-flight turn, if any.
//
// Deliberately does NOT clear streaming/cancel here: those flags stay set
// until the attributed terminal message (context-cancel error) arrives,
// because Messages remain owned by the unwinding stream goroutine until
// then — treating the session as idle early would let an idle-path
// transcript rebuild race that teardown. The dot may show ● for a few ms
// after esc; that is the contract working, not a bug.
func (m *Model) cancelActiveStream() {
	if m.lives == nil {
		return
	}
	if s := m.lives.Active(); s.cancel != nil {
		s.cancel()
	}
}

// cancelCompactForQuit aborts an in-flight /compact on the FOCUSED
// session at the quit paths (force quit / ctrl+d) so the summarization
// call is cancelled with the process; the late compactResultMsg is
// harmless (it only clears flags and, on success, persists — both safe
// post-quit). Focused-only, matching cancelActiveStream's semantics: a
// background session's compaction is left to finish (it persists on its
// own).
func (m *Model) cancelCompactForQuit() {
	if !m.compacting {
		return
	}
	if s := m.focusedSession(); s != nil {
		s.compactUserCancelled = true
		if s.compactCancel != nil {
			s.compactCancel()
		}
		s.compacting = false
		s.compactCancel = nil
	}
	m.compacting = false
}

// handleTurnFinishedMsg finalizes the owning session's turn. The FOCUSED
// session runs the normal end-of-turn pipeline (transcript finalize,
// context refresh, persist-error surfacing); a BACKGROUND session only
// drops its runtime flags and reports completion via the status line —
// its transcript is rebuilt from Messages whenever it is next focused
// (the same converge-on-switch contract as the web UI's mid-turn attach).
func (m *Model) handleTurnFinishedMsg(sid string, seq uint64, err error) (tea.Model, tea.Cmd) {
	if m.lives == nil {
		// Degenerate single-session construction without a registry. The
		// reshape check still runs: the transcript is append-only either way.
		m.rebuildTranscriptIfReshaped()
		return m.finishFocusedTurn(err)
	}
	s := m.lives.ByID(sid)
	if s == nil {
		return m, nil
	}
	// Prune queued delete-approvals owned by the terminating turn — this
	// terminal may itself be superseded (see the seq check below), but
	// either way the named turn's tool executions have unwound, so its
	// queued requests can never be answered usefully.
	m.pruneQueuedApprovals(sid, seq)
	if s.turnSeq != seq {
		// Superseded turn (cancel + resubmit while the old goroutine was
		// still unwinding): its late terminal must not clear the new
		// turn's streaming flag, clobber its cancel func, or run the
		// end-of-turn pipeline over the new turn's transcript.
		return m, nil
	}
	s.streaming = false
	s.cancel = nil
	// Drop the session's recorded indicator state. For the FOCUSED session
	// the end-of-turn pipeline's clearProgress does the same (idempotent);
	// for a BACKGROUND session this is the only clear — without it a later
	// join would restore a dead turn's phase.
	s.resetProgress()
	// Only a COMPLETED turn produced output: cancelled/failed turns must
	// not bump the row (web parity — pane.lastActivity is written on
	// output events only, never on focus or turn start).
	if err == nil {
		s.lastActive = time.Now()
		if s.agent != nil {
			// Per-id memory (touchSessionActivity): the row keeps this
			// stamp after the session stops being focused.
			m.touchSessionActivity(s.agent.SessionID, s.lastActive)
		}
	}
	// The store index just changed (label derived from the first user
	// message, message count, updatedAt) — refresh the unified list off
	// the Update thread.
	refresh := m.requestSavedSessions()
	if m.lives.Active() != s {
		if err != nil {
			m.statusMsg = "✗ " + s.label + ": " + err.Error()
		} else {
			m.statusMsg = "✓ " + s.label + " finished"
		}
		m.bellIfBlurred() // web parity: background pane finished/failed
		// A BACKGROUND session's steering queue drains on its OWN turn
		// boundaries: the next queued item starts its turn right here (its
		// events buffer in the replay queue; its terminal routes back
		// through this handler for the next item).
		return m, tea.Batch(refresh, m.drainSessionQueue(s))
	}
	// Turn-end convergence (web attach parity): a transcript rebuilt from a
	// mid-turn join (transcriptStale) lags the stream — the in-flight
	// round's head was not in Messages at the join, and a commit/event
	// straddle can duplicate one line. The turn's goroutine has returned
	// (the terminal message comes from it), so Messages is final: re-render
	// the authoritative history. Normal focused turns never set the flag,
	// so their incremental transcript (and scroll position) is preserved.
	if s.transcriptStale {
		s.transcriptStale = false
		// The rebuilt transcript has no live assistant/thinking line: drop
		// the join's stream indices so finishFocusedTurn's finalizers
		// (handleStreamEnd's trailing trim) cannot touch a stale line.
		m.rebuildTranscriptFrom(s.agent)
	}
	// A mid-turn auto-compaction (or any other wholesale history reshape)
	// replaced Messages while the transcript kept its rendered lines: rebuild
	// around the compacted history, or the append-only transcript (with its
	// wrapped and styled copies) grows for the life of the process.
	m.rebuildTranscriptIfReshaped()
	finished, cmd := m.finishFocusedTurn(err)
	return finished, tea.Batch(refresh, cmd)
}

// finishFocusedTurn runs the normal end-of-turn pipeline (success or
// error variant) for the FOCUSED session. Internal helper: terminal msgs
// from the wire go through handleTurnFinishedMsg; this skips the envelope.
func (m *Model) finishFocusedTurn(err error) (tea.Model, tea.Cmd) {
	if err != nil {
		// handleStreamError clears m.streaming; the placeholder swap and
		// relayout must run AFTER it so the busy band's reserved
		// progress-strip row returns to the composer and the placeholder
		// reads the CLEARED state (SetSize reserves the row only while
		// streaming/compacting).
		m.handleStreamError(err)
		m.applyComposerPlaceholder()
		m.relayout()
		return m, tea.Batch(m.refocusInput(), m.drainDeliveries())
	}
	// Success: handleStreamEndMsg owns the end-of-turn pipeline (it clears
	// m.streaming, then the relayout hands the strip's row back to the
	// composer) AND the queue drain (one item per terminal — multiple
	// queued items drain across consecutive turns). The placeholder swap
	// runs after both: a drain re-enters busy via startTurn (which applies
	// the queued placeholder itself); an empty queue restores the idle one.
	finished, cmd := m.handleStreamEndMsg()
	m.applyComposerPlaceholder()
	m.relayout()
	return finished, cmd
}
