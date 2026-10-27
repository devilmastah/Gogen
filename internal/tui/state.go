package tui

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/server"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// FocusTarget indicates which component has keyboard focus.
type FocusTarget int

const (
	FocusInput FocusTarget = iota
	FocusViewport
	FocusSidebar // sessions panel (tab from viewport; web-parity mouse clicks land here too)
)

// maxPendingDeliveries caps queued system deliveries (job notices,
// subagent reports), mirroring the web delivery queue (32): overflow drops
// the OLDEST delivery — a stale job notice is worse than none — and the
// queue drains as ONE batched turn, so the cap bounds the notice backstop
// rather than the number of drained turns.
const maxPendingDeliveries = 32

// ModalKind identifies the active modal overlay.
type ModalKind int

const (
	ModalNone ModalKind = iota
	ModalApproval
	ModalSessions
	ModalModels
	ModalHelp
	ModalCompletion
	ModalSubagents
	ModalLiveSessions
	ModalConfirm // generic yes/no confirmation (sidebar session delete)
	ModalQueue   // queued steering messages (/queue — steering.go)
)

// modelChangedMsg is sent by the TUI runner when a background agent state
// change (e.g. restore model validation) may have altered what the status
// bar shows. Handling it re-renders (the provider model is read live) and
// re-probes the context indicator, whose line is cached state and would
// otherwise keep the restored snapshot's pre-warmed limit.
type modelChangedMsg struct{}

// Model is the top-level Bubble Tea model for the TUI.
type Model struct {
	// Core references
	agent *agent.Agent
	cfg   *config.Config

	// Runtime
	ctx     context.Context
	program *tea.Program
	// terminalBlurred tracks the terminal window's focus (tea.BlurMsg /
	// FocusMsg, requested via View's ReportFocus). The completion bell
	// rings only while blurred — the TUI equivalent of the web's desktop
	// notifications (turn end, turn error, approval request). Terminals
	// that don't report focus (or tmux without focus-events on) never
	// blur, so they never bell: the conservative default.
	terminalBlurred bool
	// bellsRung counts completion bells fired (test hook; the tty write
	// itself is not observable in tests).
	bellsRung int

	// Components
	viewport Viewport
	textarea textarea.Model

	// viewDetached suspends viewport auto-follow while the user reads
	// history: manual scrolls away from the live edge set it, returning to
	// the bottom (or an explicit pin: submit, session switch, rebuild)
	// clears it. Content growth only re-pins while it is false — the TUI's
	// stickToBottom (see follow.go).
	viewDetached bool
	// viewDetachLines is the viewport's total line count at the moment of
	// detach (the web's contentHeightAtDetach): growth past it while
	// detached means content landed below the fold, which latches
	// viewNewContent and flips the jump button's label to "New messages".
	viewDetachLines int
	viewNewContent  bool
	// viewJumpHover is true while the pointer is over the jump button (the
	// divider label), for its hover highlight.
	viewJumpHover bool

	// State
	focus     FocusTarget
	modal     ModalKind
	streaming bool // mirror of lives.Active().streaming (input guards)
	lives     *liveSessions
	// spawner is the host's single nested-session runner (see
	// installSubagentSpawner); nil when subagents are disabled. The Model
	// only reads it to attach the /subagents list.
	spawner *tuiSubagentSpawner
	// inUpdate marks the Update goroutine while it executes a message. Host
	// messages are posted INLINE from any other goroutine, but a host path
	// that runs ON the loop (the quit sweep, the sidebar's close action) must
	// post asynchronously: tea.Program.Send is an unbuffered handoff to the
	// loop itself, so an inline send there would deadlock the program.
	inUpdate atomic.Bool
	// programStopped latches when the event loop exits, so host posts made by
	// the teardown sweep are dropped instead of blocking on a dead channel.
	programStopped atomic.Bool

	// Sidebar panel (unified session list — web parity). Resize keys live
	// in viewport/sidebar focus so they never collide with text entry.
	sidebarVisible bool
	sidebarWidth   int
	// sidebarDragging is true while the user drags the panel's right
	// border to resize it (mouse press on the border column ±1).
	sidebarDragging bool
	// savedCache snapshots the persisted-session index; the unified list
	// overlays live sessions onto it (refreshed on turn end, session
	// changes, and the 30 s tick). Written only on the Update thread
	// (savedSessionsMsg).
	savedCache []agent.SessionInfo
	// savedTimes memoizes each saved session's parsed UpdatedAt, keyed by
	// session id (value: the raw string that was parsed, so a refresh that
	// moves the timestamp re-parses). buildSidebarRows runs once per View
	// and again per mouse/key event, so without this every frame reparsed
	// every row's RFC3339Nano timestamp. Same-thread as savedCache.
	savedTimes map[string]savedStamp
	// savedReqSeq guards against a stale savedSessionsMsg (an older
	// request resolving after a newer one) clobbering the fresh snapshot.
	savedReqSeq uint64
	// Sidebar navigation state (keyboard + mouse share the cursor).
	// sidebarCursor is the cursor's CURRENT list position; sidebarCursorID
	// is its identity (the selected row's session id). The unified list
	// reorders at runtime (background completion, /open, store refresh),
	// so the id is the source of truth and the position is re-resolved
	// from it (resolveSidebarCursor) — otherwise the highlight would jump
	// to a different session and x/d would act on the wrong one. Empty id
	// = positional cursor (fresh models, tests).
	sidebarCursor   int
	sidebarCursorID string
	sidebarScroll   int // first visible row (list overflow)
	// sidebarLastCursor is the cursor of the last render: the
	// keep-in-window logic follows the cursor only when it actually moved
	// (a wheel scroll moves the window, not the cursor, and must not be
	// snapped back to it).
	sidebarLastCursor int
	sidebarMainLines  int // main-column row count of the last render (hit-testing)
	// sidebarSeq is the persistent first-seen order per row id — the
	// deterministic tie-break for equal activity (see sidebarSeqOf).
	sidebarSeq     map[string]int
	sidebarSeqNext int
	// sidebarHover is the footer button under the mouse (sidebarFooterNew /
	// sidebarFooterClose; sidebarFooterNone otherwise) for the hover
	// highlight. Cell-motion mouse reporting is already on.
	sidebarHover int
	// sidebarHovering is true while the mouse is over the panel area —
	// the border renders highlighted (tracked in handleMouseMsg).
	sidebarHovering bool
	// Prompt rail (web TOC parity): one anchor per user prompt — the
	// wrapped line where it starts plus the raw text for the hover
	// preview. Rebuilt on full transcript rebuilds, extended by the
	// append funnels (see toc.go).
	tocAnchors []tocAnchor
	// tocHover is true while the pointer is in the rail's trigger strip
	// (the chat column's rightmost four cells, above the input row) —
	// the rail renders only then, so transcript text is never covered
	// at rest.
	tocHover bool
	// tocPreview is the anchor index under the pointer (-1 = none); its
	// dot gets the hover preview box.
	tocPreview int
	// sessionActivity is the per-id recency map (web pane.lastActivity):
	// session id → last in-process OUTPUT time (completed turn, session
	// spawn, resume seed). Keyed by session id — not by live slot — so a
	// session keeps its earned list position after it stops being focused
	// (resume rebind, pane close) instead of falling back to the store's
	// persist timestamp, which a flush rewrites to "now".
	sessionActivity map[string]time.Time
	// confirmText/confirmAction back ModalConfirm (sidebar session delete).
	// confirmRestore is the modal to return to when the dialog closes
	// (ModalNone for the sidebar path; ModalSessions when the dialog was
	// opened from the sessions list, which stays open behind it).
	confirmText    string
	confirmAction  func() (tea.Model, tea.Cmd)
	confirmRestore ModalKind
	// workspace spawns additional live sessions (/open) through the shared
	// web lifecycle core; nil keeps single-session hosting.
	workspace *server.Workspace
	verbose   bool
	width     int
	height    int
	quitting  bool

	// Wait indicator (input area) while a turn is in flight.
	// progressPhase/progressLabel/activeToolName are the FOCUSED session's
	// mirror of its liveSession progress fields (see liveSession.progress*);
	// switchToLive rebinds them to the target session on a focus change.
	spinner       spinner.Model
	progressPhase progressPhase
	progressLabel string
	// progressRetry marks the label as a stream-recovery reason ("retrying
	// stream (…)"): the regeneration is silent, so the stall signal must not
	// overwrite it with the generic "still waiting" label — the retry reason
	// IS the explanation for that silence. Cleared by every other progress
	// update (setProgress) and with the progress itself; mirrored onto the
	// focused session like the phase/label pair.
	progressRetry bool
	// streamSpeedLine is the FOCUSED session's rendered token rate
	// ("42 tok/s") for the progress line — mirror of
	// liveSession.streamSpeedLine, updated by handleStreamStatsMsg from
	// the shared streamutil.SpeedMeter and cleared with the progress.
	streamSpeedLine string

	// /compact runs off the Update thread (it makes an LLM summarization
	// call — running it inline froze the whole UI for the request's
	// duration). compacting is the FOCUSED session's mirror of its
	// liveSession compacting flag (see liveSession.compacting); switchToLive
	// rebinds it on a focus change, like m.streaming. The cancel func and
	// user-cancelled flag live on the owning session, not here.
	compacting bool

	// Chat content buffer (lines to render). This is the RAW source; the
	// WRAPPED form lives only in the viewport (viewport.Lines(), spliced from
	// prefixLines + the last line's wrap), which is the single rendered copy.
	// The old design also kept a joined wrappedContent string, a second join in
	// wrappedPrefix and an ANSI-stripped split (wrappedLines) + a styled split
	// (styledLines) of the same rows, all for text selection — four more
	// transcript copies, with an O(transcript) copy on every appended line.
	// Selection now reads the viewport rows and strips ANSI per visible row.
	chatLines       []string
	maxWrappedWidth int // cached max line width (incremental, avoids O(N) scan)
	// Incremental wrapping state: prefixLines holds the viewport lines for
	// chatLines[:len-1] so streaming updates splice only the last line's wrap
	// instead of re-splitting the whole conversation.
	prefixLines []string
	lastWrapped string
	// lastRewrapWidth is wrapWidth() at the last FULL re-wrap
	// (setViewportContent). SetSize runs at every round and turn boundary via
	// relayout, where the terminal size has not changed: re-wrapping the whole
	// transcript there is pure cost, so SetSize skips the re-wrap when the wrap
	// width still matches. A real resize (or any width change) clears it.
	lastRewrapWidth int
	// historyEpoch is the agent conversation epoch (agent.HistoryEpoch) the
	// current transcript was rendered from. The transcript is append-only
	// between full rebuilds, so a wholesale history reshape — a mid-turn
	// AUTO-compaction most notably — must be detected and the transcript
	// re-rendered from Messages; otherwise chatLines (plus its wrapped and
	// styled copies) keeps every line ever rendered for the life of the
	// process even though the agent's own history stays compacted, which is
	// unbounded growth over a long-lived session. See
	// rebuildTranscriptIfReshaped.
	historyEpoch uint64

	// Streaming accumulation. streamAssistantLine is the chatLines index of the
	// live assistant line (-1 when none) and doubles as the "the line exists"
	// marker; streamWrap is that line's incremental wrap (see streamWrapCache).
	streamAssistantLine int
	streamWrap          streamWrapCache
	streamThinkingBuf   strings.Builder
	streamThinkingOpen  bool
	streamThinkingLine  int                     // chatLines index for the open thinking line (-1 if none)
	streamThinkingCache streamThinkingCache     // incremental render cache for the open thinking block (see streaming.go)
	streamToolCallNames map[int]string          // index -> name
	streamToolCallArgs  map[int]*streamToolArgs // index -> accumulated args + derived display state
	streamToolCallIDs   map[int]string          // index -> call ID (for correlating results)
	streamToolCallLines map[int]int             // index -> chatLines index where the tool call line was added
	toolCallDiffs       map[string]string       // call ID -> diff text (for patch_file/show_diff)
	activeToolName      string                  // tool being prepared or executed; names the progress indicator

	streamToolDiffCount  map[int]int                   // index -> number of diff lines already rendered progressively
	streamToolDiffStart  map[int]int                   // index -> chatLines index where the first diff line is (after top border)
	streamToolDiffRender map[int]*streamToolDiffRender // index -> incremental extract/render state for the streaming "diff" arg
	toolDiffShown        map[string]bool               // call ID -> true if diff was shown progressively (skip in result)
	// Input history
	inputHistory []string
	historyIdx   int
	historyDraft string

	// Nested (subagent) sessions finished in this process (TUI children are
	// ephemeral; the /subagents modal lists them). Guarded because the
	// spawner records from the agent's turn goroutine while the modal
	// renders on the tea loop.
	subagentMu sync.Mutex
	subagents  []subagentRecord

	// Context stats
	contextStats agent.TurnContext
	contextLine  string
	// contextEst supports a live, approximate context-indicator update while
	// a turn is streaming. We snapshot the authoritative Used count at
	// stream start and at every round boundary (ContextStats is safe to call
	// concurrently with a running turn — it snapshots shared state under
	// statsMu) and add a cheap local estimate for tokens arriving via
	// already-safe (Update-thread) stream messages. The exact count is
	// restored via refreshContextStats() once streaming ends. Driven only
	// from the Update thread.
	contextEst contextmgr.LiveEstimate

	// Session
	sessionID string

	// Completion state
	completions    []string
	completionIdx  int
	completionLine string // the full line at time of tab press

	// Approval state. Each request owns its reply channel
	// (approvalRequest), so the approver — which runs on the turn's stream
	// goroutine during tool execution — never touches these fields; they
	// are Update-thread-only.
	approvalUI *approvalUIState
	// pendingApprovals queues concurrent delete requests behind the one on
	// screen (focused + background turns can both request deletes); the
	// head is promoted on dismiss. Requests whose requesting turn has
	// terminated are pruned there (pruneQueuedApprovals) — promoting one
	// would show a modal whose answer goes nowhere.
	pendingApprovals []*approvalRequest
	// modalBeforeApproval is the modal an approval took over; restored
	// when the approval queue drains.
	modalBeforeApproval ModalKind

	// deliveryDrops counts system deliveries dropped on queue overflow
	// (the per-session steering queue, steering.go); rendered as a system
	// line at the next drain (never mid-stream).
	deliveryDrops int

	// Modal data
	sessionList   []agent.SessionInfo
	modelList     []llm.ModelInfo
	sessionCursor int
	modelCursor   int
	liveCursor    int // ModalLiveSessions selection over m.lives.sessions
	// queueCursor is the /queue modal's selected row (steering.go).
	queueCursor int
	// thinkingSel is the models modal's staged thinking level (web
	// subagent-picker parity): moved with ←/→ or a chip click, applied
	// together with the model on enter. Off = "no reasoning_effort sent".
	thinkingSel agent.ThinkingLevel

	// Keymap
	keys KeyMap

	// Screen dimensions tracking
	ready bool

	// Text selection (mouse drag-to-select in viewport). Coordinates are rows
	// of the viewport's wrapped content; the plain text of a row is derived on
	// demand (see Model.plainLine) instead of being cached per row.
	selectionYOff int // viewport YOffset at selection start (for stable coordinates)
	selection     *SelectionState
	// Render caches — recomputed only when inputs change.
	dividerCache       string
	dividerCacheWidth  int
	dividerCacheFocus  FocusTarget
	dividerCacheStream bool
	// preferredInputLines is the textarea height the layout was first
	// sized with (captured on the first SetSize). Short terminals shrink
	// the input band to keep the frame within the terminal; this field is
	// the value to restore when the terminal grows again — reading the
	// live textarea height instead would make a once-shrunk band permanent.
	preferredInputLines int
	statusMsg           string // transient message (e.g. "Copied N chars")
}

// NewModel creates a new TUI model. Returns a pointer: Model contains a
// mutex (the subagent list guard), so values must not be copied.
func NewModel(a *agent.Agent, cfg *config.Config) *Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message or command..."
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.SetHeight(3)
	ta.MaxHeight = 8
	ta.CharLimit = 0 // no limit
	// v2: styles are immutable-by-convention; replace the whole Styles struct.
	ts := textarea.DefaultStyles(true)
	ts.Focused.CursorLine = lipgloss.NewStyle()
	ts.Blurred.CursorLine = lipgloss.NewStyle()
	ts.Focused.Prompt = InputPromptStyle
	ts.Blurred.Prompt = InputPromptStyle
	ta.SetStyles(ts)
	ta.KeyMap.InsertNewline.SetEnabled(false) // we handle Enter ourselves

	// Remap textarea keybindings to match our CLI conventions
	ta.KeyMap.WordBackward = key.NewBinding(key.WithKeys("ctrl+left"))
	ta.KeyMap.WordForward = key.NewBinding(key.WithKeys("ctrl+right"))
	ta.KeyMap.DeleteWordBackward = key.NewBinding(key.WithKeys("ctrl+w"))
	ta.KeyMap.DeleteAfterCursor = key.NewBinding(key.WithKeys("ctrl+k"))
	ta.KeyMap.DeleteBeforeCursor = key.NewBinding(key.WithKeys("ctrl+u"))
	ta.KeyMap.LineStart = key.NewBinding(key.WithKeys("ctrl+a", "home"))
	ta.KeyMap.LineEnd = key.NewBinding(key.WithKeys("ctrl+e", "end"))
	ta.KeyMap.DeleteCharacterForward = key.NewBinding(key.WithKeys("ctrl+d", "delete"))
	ta.KeyMap.DeleteCharacterBackward = key.NewBinding(key.WithKeys("backspace"))

	vp := NewViewport(80, 20)
	// Scroll 1 line per wheel event (default is 3; lower for smoother
	// scrolling when the mouse/terminal sends multiple events per notch).
	vp.MouseWheelDelta = 1
	vp.Style = ViewportStyle

	verbose := cfg != nil && cfg.CLIVerbose

	modelLine := ""
	if a != nil {
		modelLine = a.CurrentModel()
	}

	m := Model{
		agent:                a,
		cfg:                  cfg,
		viewport:             vp,
		textarea:             ta,
		focus:                FocusInput,
		modal:                ModalNone,
		streaming:            false,
		verbose:              verbose,
		spinner:              newProgressSpinner(),
		progressPhase:        progressHidden,
		chatLines:            make([]string, 0, 64), // pre-allocate to reduce GC during streaming
		streamAssistantLine:  -1,
		streamThinkingLine:   -1,
		streamToolCallNames:  make(map[int]string),
		streamToolCallArgs:   make(map[int]*streamToolArgs),
		streamToolCallIDs:    make(map[int]string),
		streamToolCallLines:  make(map[int]int),
		toolCallDiffs:        make(map[string]string),
		streamToolDiffCount:  make(map[int]int),
		streamToolDiffStart:  make(map[int]int),
		streamToolDiffRender: make(map[int]*streamToolDiffRender),
		toolDiffShown:        make(map[string]bool),
		keys:                 DefaultKeyMap,
		sessionID:            "",
		selectionYOff:        -1,
		selection:            nil,
		statusMsg:            "",
	}

	if a != nil {
		m.lives = newLiveSessions(a)
		m.sessionID = a.SessionID
		// Creation is a fresh session's first recency event; a resumed
		// session's stamp is re-pinned to its store timestamp by
		// seedRootLastActive below.
		m.touchSessionActivity(a.SessionID, time.Now())
		// Web parity: the panel opens by default; only the width persists
		// across restarts (the web sidebar is open by default too).
		m.sidebarVisible = true
		m.sidebarWidth = resolveSidebarStart(a.WorkingDir).SidebarWidth
		if m.sidebarVisible {
			m.refreshSavedSessions()
		}
		// A resumed session keeps its earned list position (its last
		// OUTPUT time), not the process start.
		m.seedRootLastActive()
		// Build initial history
		m.chatLines = renderMessages(a.Messages, a.WorkingDir, modelLine, a.Mode.String(), m.verbose)
		m.setViewportContent()
		m.pinFollow()
		m.markTranscriptRendered()
		if stats := a.ContextStats(context.Background()); stats.Snapshot.Limit > 0 || stats.Snapshot.Used > 0 {
			m.contextStats = stats
			m.contextLine = agent.FormatContextBrief(stats)
		}
		// Nested sessions: reuse the runner's host-level spawner when it
		// exists (production), else build one for this Model (tests,
		// embedded single-session hosts). Feature-gated by the subagent
		// flag; the spawner itself is harmless when the tool is not exposed.
		m.installSubagentSpawner()
	}

	return &m
}

// installSubagentSpawner wires nested sessions for the root agent and attaches
// this Model's /subagents list to the spawner. One spawner per host: when the
// runner already built it (installed on the workspace, so every session the
// workspace creates inherits it), the Model only attaches its list — the child
// registry must not be duplicated per Model.
func (m *Model) installSubagentSpawner() {
	if m.agent == nil || m.cfg == nil || !m.cfg.SubagentEnabled() {
		return
	}
	if m.spawner == nil {
		if sp, ok := m.agent.SubagentSpawner().(*tuiSubagentSpawner); ok && sp != nil {
			m.spawner = sp
		}
	}
	if m.spawner == nil {
		m.spawner = newTUISubagentSpawner(m.cfg, m.workspace)
		m.agent.SetSubagentSpawner(m.spawner)
	}
	m.spawner.setRecorder(m.recordSubagent)
	// The sender is installed by the runner once the program exists (see
	// TUI.Run); a Model constructed directly (tests) leaves it nil, so a
	// background spawn reports "the terminal UI is not running" instead of
	// blocking forever.
}

// maxRecordedSubagents bounds the /subagents list. Each record keeps the
// child's final report (up to agent.MaxSubagentReportBytes), so an unbounded
// list would grow for the life of a long-lived TUI process; the newest
// entries are the ones the modal is opened for. The shared lifecycle applies
// the same cap per parent (subagents.DefaultMaxFinished).
const maxRecordedSubagents = 32

// recordSubagent appends a finished nested session to the /subagents list
// (called from the spawner on the agent's turn goroutine). The list is
// bounded (maxRecordedSubagents): the oldest records are dropped.
func (m *Model) recordSubagent(r subagentRecord) {
	m.subagentMu.Lock()
	m.subagents = append(m.subagents, r)
	if over := len(m.subagents) - maxRecordedSubagents; over > 0 {
		// In-place shift, oldest first (dst index is always below src).
		m.subagents = append(m.subagents[:0], m.subagents[over:]...)
	}
	m.subagentMu.Unlock()
}

func (m *Model) SetSize(width, height int) {
	m.width = width
	m.height = height
	m.ready = true

	// Layout: status bar (1 line) at bottom, textarea above it, viewport fills rest.
	//
	// The composed frame must never exceed the terminal: the renderer runs
	// inline (alt-screen off) and an over-tall frame scrolls the screen and
	// desyncs the incremental cell diff. On short terminals the input band
	// gives way FIRST, then the viewport floors at one row — the old
	// unconditional viewport floor of 3 made the minimum frame 8 lines no
	// matter the terminal height.
	statusBarHeight := 1
	if m.preferredInputLines == 0 {
		m.preferredInputLines = m.textarea.Height()
	}
	inputLines := m.preferredInputLines
	if inputLines > 8 {
		inputLines = 8
	}
	if inputLines < 1 {
		inputLines = 1
	}
	// Busy band: while a turn (or compaction) runs, the progress strip
	// renders as one row INSIDE the input band and the composer gives it a
	// row (busyStripRows) — the band's total height is unchanged, so a turn
	// boundary never shifts the viewport (the zero-layout-jump invariant
	// padInputBand served). The composer stays visible and editable: Enter
	// queues (steering), ctrl+c interrupts. bandLines is the band's TOTAL
	// height and what the viewport budget is computed from; the textarea
	// gets the band minus the strip's row. (Budgeting the viewport from the
	// textarea height instead would hand the strip's row back to the
	// viewport, growing the frame one row past the terminal at every turn
	// start — the inline renderer would scroll and desync.)
	stripRows := m.busyStripRows()
	bandLines := inputLines
	textareaHeight := inputLines - stripRows
	if textareaHeight < 1 {
		// A one-row composer cannot give up its row: the strip adds a band
		// row instead (the viewport budget absorbs it; the frame stays
		// exact).
		textareaHeight = 1
		bandLines++
	}

	vpHeight := height - statusBarHeight - bandLines - 1 // -1 for divider
	if vpHeight < 3 {
		// Short terminal: shrink the input band before taking rows from
		// the chat viewport, then floor the viewport at a single row. The
		// band floors at strip+composer while busy (2) and at the composer
		// alone when idle (1).
		bandLines -= 3 - vpHeight
		if minBand := 1 + stripRows; bandLines < minBand {
			bandLines = minBand
		}
		vpHeight = height - statusBarHeight - bandLines - 1
		if vpHeight < 1 {
			vpHeight = 1 // under ~4 rows nothing fits; best effort
		}
		textareaHeight = bandLines - stripRows
		if textareaHeight < 1 {
			textareaHeight = 1
		}
	}

	main := m.mainWidth()
	m.viewport.Width = main - 2 // padding
	m.viewport.Height = vpHeight
	m.textarea.SetWidth(main - 2)
	m.textarea.SetHeight(textareaHeight)

	// Re-wrap only when the wrap width actually changed. SetSize runs at every
	// round and turn boundary (relayout) and on every modal/queue change, where
	// the terminal size is the same — re-wrapping the whole transcript there is
	// pure cost (the transcript is already wrapped for this width; every
	// chatLines mutation goes through buildFromPrefix or a full re-wrap).
	// Content-only paths keep calling setViewportContent directly.
	if m.wrapWidth() != m.lastRewrapWidth {
		m.setViewportContent()
	}
	if m.viewDetached {
		// Re-wrapping redefines the "below the fold" baseline: without
		// this, a width change alone would read as new messages.
		m.viewDetachLines = m.viewport.TotalLines()
	}
	// A resize re-wraps the whole transcript; while following, re-anchor
	// at the live edge. relayout() routes through here at every turn
	// boundary too, so this must respect a detached reader (followBottom,
	// not pinFollow) — otherwise every turn end would yank them down.
	m.followBottom()
}

// busyStripRows reports how many rows the busy progress strip contributes
// to the input band: 1 while a turn (or compaction) runs and the terminal
// is tall enough to hold the strip NEXT TO the composer (status bar +
// divider + strip + one-row composer + one-row viewport = 5), 0 otherwise.
// A terminal shorter than that keeps the pre-steering composer-only band —
// the alternative (rendering the strip anyway) would grow the frame past
// the terminal and desync the inline renderer. SetSize budgets the band
// from this and renderMainColumn renders from it: one predicate, so the
// rendered band can never disagree with the budgeted one and overflow.
func (m *Model) busyStripRows() int {
	if m.height < 5 {
		return 0
	}
	if m.streaming || m.compacting {
		return 1
	}
	return 0
}
