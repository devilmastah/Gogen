package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gogen/internal/ioutil"
	"gogen/internal/projectfile"
)

// Project-wide kanban board (one file per ticket, D1).
//
// Storage layout under the board directory:
//
//	index.json   — version, column definitions, per-column item order, next id
//	<id>.json    — one ticket per file (content + status + activity)
//
// The directory is <workingDir>/.gogen/board in project mode and the global
// board directory in global mode (D3). Status always comes from the ticket
// file; the index holds column membership/order only, so a corrupt index can
// be rebuilt from the ticket files (single source of truth).

const (
	boardVersion     = 1
	boardMaxItems    = 200 // D4: total ticket cap
	boardMaxActivity = 50  // D4: activity entries per ticket (oldest dropped)
)

// MaxReviewRounds caps auto-review starts per ticket: past it a move into
// in_review no longer spawns a reviewer (a worker/reviewer ping-pong would
// otherwise loop forever). The cap is surfaced as a comment on the ticket
// so the stopped auto-review is visible.
const MaxReviewRounds = 5

// BoardColumns is the fixed column set (D10 — column configuration is a
// follow-up).
var BoardColumns = []string{"backlog", "ready", "in_progress", "in_review", "blocked", "done"}

// BoardActivity is one entry in a ticket's activity log.
type BoardActivity struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`
	Text string    `json:"text"`
}

// BoardItem is one ticket.
type BoardItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Context is free-text background for the started agent, merged into the
	// prompt's {context} placeholder ahead of the activity-derived log.
	Context  string `json:"context,omitempty"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	// AgentSessionID is the session started for this ticket by the web
	// board's "Start agent" button ("" = none). The "Open agent" button
	// targets it; a stale id (session deleted) is reset by ResetAgent.
	AgentSessionID string `json:"agentSession,omitempty"`
	// Model is the per-ticket model chosen in the web board's "Start
	// agent" popover ("" = the workspace default model).
	Model string `json:"model,omitempty"`
	// Prompt is the per-ticket prompt template for the agent started from
	// this ticket ("" = the configured board_start_prompt template). The
	// popover pre-fills from it; the start op is authoritative.
	Prompt string `json:"prompt,omitempty"`
	// ThinkingLevel is the per-ticket reasoning-effort level chosen in the
	// web board's "Start agent" popover ("" = inherit the active pane's
	// live level, the pre-existing behavior; "off" = never send
	// reasoning_effort). The popover pre-fills from it; the start op is
	// authoritative.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	// ReviewModel / ReviewThinkingLevel are the per-ticket review-agent
	// options chosen in the card popover's review section ("" = inherit:
	// the configured review_agent_model, then the workspace default model;
	// the workspace thinking level). SetReviewOptions persists them; the
	// auto-review trigger resolves the cascade and validates the level
	// against the FINAL model at review-start time.
	ReviewModel         string `json:"reviewModel,omitempty"`
	ReviewThinkingLevel string `json:"reviewThinkingLevel,omitempty"`
	// ReviewSessionID is the review session currently or last started for
	// this ticket by the auto-review trigger ("" = none). It doubles as
	// the one-reviewer-per-ticket guard: while the linked session is live,
	// a new move into in_review does not spawn a second reviewer. Cleared
	// by moveLocked whenever the ticket leaves in_review.
	ReviewSessionID string `json:"reviewSession,omitempty"`
	// ReviewRounds counts started review sessions for this ticket. The
	// trigger stops auto-reviewing past MaxReviewRounds.
	ReviewRounds int `json:"reviewRounds,omitempty"`

	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	DoneAt    time.Time       `json:"done_at,omitempty"`
	Activity  []BoardActivity `json:"activity,omitempty"`
}

// BoardUpdate is a partial edit of a ticket's human-authored fields: a nil
// field is left unchanged, so a rename does not wipe the description. The
// web UI's edit form sends all three fields (full replace); the agent tool
// sends only what it wants to change.
type BoardUpdate struct {
	Title       *string
	Description *string
	Priority    *string
	Context     *string
}

// BoardSnapshot is the full board state for rendering (web UI / list).
type BoardSnapshot struct {
	Columns []string    `json:"columns"`
	Items   []BoardItem `json:"items"`
}

type boardIndex struct {
	Version int                 `json:"version"`
	NextID  int                 `json:"next_id"`
	Columns []string            `json:"columns"`
	Order   map[string][]string `json:"order"` // column → item ids (creation order)
}

func defaultBoardIndex() *boardIndex {
	return &boardIndex{
		Version: boardVersion,
		NextID:  1,
		Columns: append([]string(nil), BoardColumns...),
		Order:   map[string][]string{},
	}
}

// BoardManager is the project-wide kanban board. Thread-safe. The web server
// shares ONE manager across all session agents (like MCPRegistry), so claims
// and moves serialize in-process; the TUI/CLI agent owns its own. Different
// tickets never interfere on disk (one file per ticket); a same-ticket
// write from two separate gogen processes is last-write-wins.
type BoardManager struct {
	mu         sync.Mutex
	dir        string
	globalMode bool
	idx        *boardIndex

	// reviewHookMu guards reviewNeeded (its own mutex, NOT m.mu: the
	// callback fires after m.mu is released, and its handler calls back
	// into the manager and spawns sessions — holding m.mu would deadlock).
	// The web workspace installs it once at startup (nil = no auto-review;
	// TUI/CLI). reviewNeeded receives a SNAPSHOT of the ticket and the
	// actor label of the move that entered in_review.
	reviewHookMu sync.RWMutex
	reviewNeeded func(item BoardItem, by string)
}

// NewBoardManager creates a board manager rooted at the board directory for
// workingDir (global mode → the global board directory).
func NewBoardManager(workingDir string, globalMode bool) *BoardManager {
	return &BoardManager{
		dir:        boardDirFor(workingDir, globalMode),
		globalMode: globalMode,
		idx:        defaultBoardIndex(),
	}
}

// SetWorkingDir re-points the manager at the board directory for a new
// working dir (global mode keeps the global board).
func (m *BoardManager) SetWorkingDir(workingDir string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dir = boardDirFor(workingDir, m.globalMode)
	m.idx = defaultBoardIndex()
}

// boardDirFor resolves the board directory (D3).
func boardDirFor(workingDir string, globalMode bool) string {
	if globalMode {
		return projectfile.GlobalBoardDir()
	}
	return filepath.Join(workingDir, ".gogen", "board")
}

// --- persistence ---

func (m *BoardManager) indexPath() string         { return filepath.Join(m.dir, "index.json") }
func (m *BoardManager) itemPath(id string) string { return filepath.Join(m.dir, id+".json") }

// loadIndexLocked reads the index, falling back to a fresh index when the
// file is absent and to a rebuild from ticket files when it is corrupt.
func (m *BoardManager) loadIndexLocked() error {
	data, err := os.ReadFile(m.indexPath())
	if os.IsNotExist(err) {
		// Ticket files may still exist (the index was deleted, or an older
		// version never wrote one). Falling back to a pristine index reset
		// NextID to 1, so the next Add overwrote ticket 1.json and its
		// column/order vanished from the board. Rebuild from disk instead.
		return m.rebuildIndexLocked()
	}
	if err != nil {
		return err
	}
	var idx boardIndex
	if err := json.Unmarshal(data, &idx); err != nil || idx.Version != boardVersion || idx.Columns == nil {
		return m.rebuildIndexLocked()
	}
	m.idx = &idx
	return nil
}

// rebuildIndexLocked reconstructs the index from the ticket files (status is
// authoritative on the tickets; column order falls back to ticket creation
// time).
func (m *BoardManager) rebuildIndexLocked() error {
	idx := defaultBoardIndex()
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			m.idx = idx
			return nil
		}
		return err
	}
	type itemMeta struct {
		id        string
		status    string
		createdAt time.Time
	}
	var metas []itemMeta
	maxID := 0
	for _, e := range entries {
		if e.IsDir() || e.Name() == "index.json" || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		data, err := os.ReadFile(m.itemPath(id))
		if err != nil {
			continue
		}
		var item BoardItem
		if err := json.Unmarshal(data, &item); err != nil {
			continue
		}
		if n, err := strconv.Atoi(id); err == nil && n > maxID {
			maxID = n
		}
		metas = append(metas, itemMeta{id: id, status: item.Status, createdAt: item.CreatedAt})
	}
	slices.SortFunc(metas, func(a, b itemMeta) int { return a.createdAt.Compare(b.createdAt) })
	for _, meta := range metas {
		col := meta.status
		if !slices.Contains(idx.Columns, col) {
			col = idx.Columns[0]
		}
		idx.Order[col] = append(idx.Order[col], meta.id)
	}
	idx.NextID = maxID + 1
	m.idx = idx
	return nil
}

// saveIndexLocked rewrites the board index. Board ops are agent- and
// user-driven actions that touch BOTH files (index + item), so the
// fsync-free write is deliberate (SSD wear policy, matching session
// files): temp+rename still guarantees readers never see a torn file, and
// the worst case after a power loss is losing the most recent board
// operation — the same tradeoff Store.Save already makes for sessions.
// Caller must hold m.mu.
func (m *BoardManager) saveIndexLocked() error {
	data, err := json.MarshalIndent(m.idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	return ioutil.WriteFileAtomicNoSync(m.indexPath(), data, 0o644)
}

func (m *BoardManager) loadItemLocked(id string) (*BoardItem, error) {
	data, err := os.ReadFile(m.itemPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("board item #%s not found", id)
		}
		return nil, err
	}
	var item BoardItem
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, fmt.Errorf("board item #%s is corrupt: %v", id, err)
	}
	return &item, nil
}

// saveItemLocked rewrites one board item; see saveIndexLocked for the
// fsync-free wear rationale. Caller must hold m.mu.
func (m *BoardManager) saveItemLocked(item *BoardItem) error {
	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	return ioutil.WriteFileAtomicNoSync(m.itemPath(item.ID), data, 0o644)
}

// appendActivityLocked records an activity entry, capping the log at
// boardMaxActivity entries (oldest dropped).
func (m *BoardManager) appendActivityLocked(item *BoardItem, by, text string) {
	item.Activity = append(item.Activity, BoardActivity{At: time.Now().UTC(), By: by, Text: text})
	if len(item.Activity) > boardMaxActivity {
		item.Activity = append([]BoardActivity(nil), item.Activity[len(item.Activity)-boardMaxActivity:]...)
	}
	item.UpdatedAt = time.Now().UTC()
}

// --- operations (web UI and agent tool share these) ---

// Snapshot returns the full board state for rendering.
//
// Deliberately uncached: an in-memory cache needs an invalidation key, and
// index.json is NOT one — a mutation that only touches a ticket (title, status,
// comment, assignee) rewrites that ticket file alone. A cache keyed on the
// index mtime therefore served stale cards, so the snapshot re-reads the index
// and the tickets every time (the cost is bounded: at most one small JSON file
// per ticket).
func (m *BoardManager) Snapshot() (*BoardSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return nil, err
	}
	snap := &BoardSnapshot{
		Columns: append([]string(nil), m.idx.Columns...),
		Items:   make([]BoardItem, 0),
	}
	for _, col := range m.idx.Columns {
		for _, id := range m.idx.Order[col] {
			item, err := m.loadItemLocked(id)
			if err != nil {
				continue // missing ticket — skip (treated as deleted)
			}
			snap.Items = append(snap.Items, *item)
		}
	}
	return snap, nil
}

// List returns a formatted board summary (agent-facing).
func (m *BoardManager) List() (string, error) {
	snap, err := m.Snapshot()
	if err != nil {
		return "", err
	}
	if len(snap.Items) == 0 {
		return "Board is empty. Add a card with the board tool (action=add).", nil
	}
	var b strings.Builder
	pending, done := 0, 0
	for _, item := range snap.Items {
		if item.Status == "done" {
			done++
		} else {
			pending++
		}
		fmt.Fprintf(&b, "[%-10s] #%s %s", item.Status, item.ID, item.Title)
		if item.Priority != "" {
			fmt.Fprintf(&b, " (priority: %s)", item.Priority)
		}
		if item.Assignee != "" {
			fmt.Fprintf(&b, " (assignee: %s)", item.Assignee)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n%d pending, %d done (columns: %s)", pending, done, strings.Join(snap.Columns, ", "))
	return b.String(), nil
}

// Show returns a formatted ticket with its activity log (agent-facing).
func (m *BoardManager) Show(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#%s %s\n", item.ID, item.Title)
	fmt.Fprintf(&b, "Status: %s", item.Status)
	if item.Assignee != "" {
		fmt.Fprintf(&b, " | Assignee: %s", item.Assignee)
	}
	if item.Priority != "" {
		fmt.Fprintf(&b, " | Priority: %s", item.Priority)
	}
	if item.AgentSessionID != "" {
		fmt.Fprintf(&b, " | Agent session: %s", item.AgentSessionID)
	}
	b.WriteString("\nCreated: " + item.CreatedAt.Format(time.RFC3339))
	if !item.DoneAt.IsZero() {
		b.WriteString("\nDone: " + item.DoneAt.Format(time.RFC3339))
	}
	if item.Description != "" {
		b.WriteString("\n\n" + item.Description)
	}
	if item.Context != "" {
		b.WriteString("\n\nContext:\n" + item.Context)
	}
	if len(item.Activity) > 0 {
		b.WriteString("\n\nActivity:")
		for _, act := range item.Activity {
			fmt.Fprintf(&b, "\n  %s %s: %s", act.At.Format(time.RFC3339), act.By, act.Text)
		}
	}
	return b.String(), nil
}

// Item returns the structured ticket for id (server-side accessor; Show is
// the formatted, agent-facing form). Thread-safe like every other op.
func (m *BoardManager) Item(id string) (*BoardItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return nil, err
	}
	return m.loadItemLocked(id)
}

// AttachAgent links a ticket to the agent session working on it (the web
// "Open agent" button's target) and records the start in the activity log.
// The ticket must already be claimed (Claim sets the assignee); the session
// id is the machine-readable link, the assignee stays human-readable.
func (m *BoardManager) AttachAgent(id, sessionID, by string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("agent session id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	item.AgentSessionID = sessionID
	m.appendActivityLocked(item, by, "agent session "+sessionID+" started")
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	return fmt.Sprintf("Agent session %s is working on board item #%s", sessionID, item.ID), nil
}

// ResetAgent clears a stale agent link (assignee + agent session id) and
// moves the ticket back to the backlog so it can be started again. Used
// when the agent session a ticket points at no longer exists (deleted).
func (m *BoardManager) ResetAgent(id, by string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	if item.AgentSessionID == "" && item.Assignee == "" {
		return fmt.Sprintf("Board item #%s has no agent link to reset", item.ID), nil
	}
	if item.Status != "backlog" {
		if err := m.moveLocked(item, "backlog"); err != nil {
			return "", err
		}
	}
	old := item.AgentSessionID
	item.Assignee = ""
	item.AgentSessionID = ""
	note := "agent link cleared"
	if old != "" {
		note = "agent session " + old + " no longer exists — link cleared"
	}
	m.appendActivityLocked(item, by, note)
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	if err := m.saveIndexLocked(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Reset board item #%s for a fresh start", item.ID), nil
}

// SetStartOptions persists the model, prompt template, and reasoning-effort
// level chosen in the web board's "Start agent" popover ("" clears back to
// the defaults: workspace model / configured board_start_prompt / inherit
// the active pane's level). The level is canonicalized via
// NormalizeThinkingLevel so the stored value matches what the start handler
// applies and what the popover compares against (lowercase chips). Silent by
// design — the claim already logs the start, and the stored options only
// pre-fill the popover on the next start.
func (m *BoardManager) SetStartOptions(id, model, prompt, thinkingLevel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return err
	}
	item.Model = strings.TrimSpace(model)
	item.Prompt = strings.TrimSpace(prompt)
	item.ThinkingLevel = string(NormalizeThinkingLevel(thinkingLevel))
	item.UpdatedAt = time.Now().UTC()
	return m.saveItemLocked(item)
}

// SetOnReviewNeeded installs the auto-review trigger, fired after a
// successful Move into in_review — the single choke point both the agent
// board tool and the web UI (move op / drag-drop) go through. The callback
// runs OUTSIDE the manager lock with a snapshot of the ticket plus the
// actor label of the move; moves can race, so the callback must be safe to
// run concurrently. nil detaches (TUI/CLI: no auto-review).
func (m *BoardManager) SetOnReviewNeeded(fn func(item BoardItem, by string)) {
	m.reviewHookMu.Lock()
	m.reviewNeeded = fn
	m.reviewHookMu.Unlock()
}

// onReviewNeeded returns the installed auto-review trigger (nil when the
// feature is not wired).
func (m *BoardManager) onReviewNeeded() func(item BoardItem, by string) {
	m.reviewHookMu.RLock()
	defer m.reviewHookMu.RUnlock()
	return m.reviewNeeded
}

// SetReviewOptions persists the per-ticket review-agent model and
// reasoning-effort level chosen in the card popover's review section (""
// clears back to the inherited defaults: the configured review_agent_model,
// then the workspace default model; the workspace thinking level). The
// level is canonicalized via NormalizeThinkingLevel so the stored value
// matches what the auto-review trigger applies and what the popover
// pre-fills.
func (m *BoardManager) SetReviewOptions(id, model, thinkingLevel, by string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	item.ReviewModel = strings.TrimSpace(model)
	item.ReviewThinkingLevel = string(NormalizeThinkingLevel(thinkingLevel))
	m.appendActivityLocked(item, by, "review options updated")
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	return fmt.Sprintf("Review options set on board item #%s", item.ID), nil
}

// AttachReviewAgent records the auto-started review session on the ticket:
// the link (ReviewSessionID) doubles as the one-reviewer guard, and the
// rounds counter feeds the MaxReviewRounds cap. Called by the review
// trigger after the session is registered, before its first turn.
func (m *BoardManager) AttachReviewAgent(id, sessionID, by string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("review session id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	item.ReviewSessionID = sessionID
	item.ReviewRounds++
	m.appendActivityLocked(item, by, "review started (round "+strconv.Itoa(item.ReviewRounds)+", session "+sessionID+")")
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	return fmt.Sprintf("Review session %s started for board item #%s (round %d)", sessionID, item.ID, item.ReviewRounds), nil
}

// Add creates a new ticket in the backlog column.
func (m *BoardManager) Add(title, description, priority, by string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("board item title is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	if m.itemCountLocked() >= boardMaxItems {
		return "", fmt.Errorf("too many board items (max %d); complete or remove some first", boardMaxItems)
	}
	item := &BoardItem{
		ID:          fmt.Sprintf("%d", m.idx.NextID),
		Title:       title,
		Description: strings.TrimSpace(description),
		Status:      "backlog",
		Priority:    normalizePriority(priority),
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	m.idx.NextID++
	m.appendActivityLocked(item, by, "created")
	m.idx.Order[item.Status] = append(m.idx.Order[item.Status], item.ID)
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	if err := m.saveIndexLocked(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Added board item #%s: %s", item.ID, item.Title), nil
}

// Update applies a partial edit to a ticket's title, description, and
// priority (nil fields are left unchanged). A provided title must be
// non-empty. An edit that changes nothing is a no-op (no activity entry);
// otherwise an "edited …" entry listing the changed fields is appended.
func (m *BoardManager) Update(id string, upd BoardUpdate, by string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	var changed []string
	if upd.Title != nil {
		title := strings.TrimSpace(*upd.Title)
		if title == "" {
			return "", fmt.Errorf("board item title is required")
		}
		if title != item.Title {
			item.Title = title
			changed = append(changed, "title")
		}
	}
	if upd.Description != nil {
		desc := strings.TrimSpace(*upd.Description)
		if desc != item.Description {
			item.Description = desc
			changed = append(changed, "description")
		}
	}
	if upd.Priority != nil {
		prio := normalizePriority(*upd.Priority)
		if prio != item.Priority {
			item.Priority = prio
			changed = append(changed, "priority")
		}
	}
	if upd.Context != nil {
		ctx := strings.TrimSpace(*upd.Context)
		if ctx != item.Context {
			item.Context = ctx
			changed = append(changed, "context")
		}
	}
	if len(changed) == 0 {
		return fmt.Sprintf("Board item #%s is unchanged", item.ID), nil
	}
	m.appendActivityLocked(item, by, "edited "+strings.Join(changed, ", "))
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	return fmt.Sprintf("Updated board item #%s: %s", item.ID, item.Title), nil
}

// Claim assigns the ticket to by and moves it to in_progress.
func (m *BoardManager) Claim(id, by string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	if item.Status == "in_progress" {
		if item.Assignee != "" && item.Assignee != by {
			return "", fmt.Errorf("board item #%s is already claimed by %s", id, item.Assignee)
		}
		// A card moved to in_progress without an assignee (drag-drop, Move)
		// is claimable: take the assignment so the claim is recorded even
		// though the column did not change.
		if item.Assignee == "" {
			item.Assignee = by
			m.appendActivityLocked(item, by, "claimed")
			if err := m.saveItemLocked(item); err != nil {
				return "", err
			}
			return fmt.Sprintf("Claimed board item #%s: %s", item.ID, item.Title), nil
		}
		return fmt.Sprintf("Board item #%s is already in progress", id), nil
	}
	if err := m.moveLocked(item, "in_progress"); err != nil {
		return "", err
	}
	item.Assignee = by
	m.appendActivityLocked(item, by, "claimed")
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	if err := m.saveIndexLocked(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Claimed board item #%s: %s", item.ID, item.Title), nil
}

// Move changes a ticket's column. Moving into in_review additionally fires
// the auto-review hook (SetOnReviewNeeded) after the mutation is persisted
// and the manager lock released: the hook's handler calls back into the
// manager and spawns a review session, so it must never run under m.mu.
// Defers run LIFO — the hook defer is registered BEFORE the unlock defer,
// so the unlock executes first.
func (m *BoardManager) Move(id, column, by string) (string, error) {
	column = strings.TrimSpace(strings.ToLower(column))
	if !slices.Contains(BoardColumns, column) {
		return "", fmt.Errorf("unknown board column %q (want one of: %s)", column, strings.Join(BoardColumns, ", "))
	}
	var fireReviewHook func()
	defer func() {
		if fireReviewHook != nil {
			fireReviewHook()
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	if item.Status == column {
		return fmt.Sprintf("Board item #%s is already in %s", id, column), nil
	}
	if err := m.moveLocked(item, column); err != nil {
		return "", err
	}
	m.appendActivityLocked(item, by, "moved to "+column)
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	if err := m.saveIndexLocked(); err != nil {
		return "", err
	}
	if column == "in_review" {
		// Snapshot for the hook: the ticket may keep mutating after the
		// unlock, and the callback must not observe live state.
		snapshot := *item
		if hook := m.onReviewNeeded(); hook != nil {
			fireReviewHook = func() { hook(snapshot, by) }
		}
	}
	return fmt.Sprintf("Moved board item #%s to %s: %s", item.ID, column, item.Title), nil
}

// Block moves a ticket to blocked with a reason.
func (m *BoardManager) Block(id, reason, by string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", fmt.Errorf("block reason is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	if item.Status != "blocked" {
		if err := m.moveLocked(item, "blocked"); err != nil {
			return "", err
		}
	}
	m.appendActivityLocked(item, by, "blocked: "+reason)
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	if err := m.saveIndexLocked(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Blocked board item #%s: %s", item.ID, item.Title), nil
}

// Comment appends a note to a ticket.
func (m *BoardManager) Comment(id, text, by string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("comment text is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	m.appendActivityLocked(item, by, text)
	if err := m.saveItemLocked(item); err != nil {
		return "", err
	}
	return fmt.Sprintf("Commented on board item #%s", item.ID), nil
}

// Done marks a ticket done (done_at set; moving out of done clears it).
func (m *BoardManager) Done(id, by string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	if item.Status != "done" {
		if err := m.moveLocked(item, "done"); err != nil {
			return "", err
		}
		item.DoneAt = time.Now().UTC()
		m.appendActivityLocked(item, by, "marked done")
		if err := m.saveItemLocked(item); err != nil {
			return "", err
		}
		if err := m.saveIndexLocked(); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Board item #%s is done: %s", item.ID, item.Title), nil
}

// Delete removes a ticket entirely: the ticket file and its index entry
// (like TodoManager.RemoveTodo). The activity log goes with it. Removing a
// claimed/in-progress card is allowed — the user is the boss; a subagent
// mid-work on the card simply gets "item not found" on its next operation.
func (m *BoardManager) Delete(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadIndexLocked(); err != nil {
		return "", err
	}
	item, err := m.loadItemLocked(id)
	if err != nil {
		return "", err
	}
	title := item.Title
	// Drop the id from its column's order.
	m.idx.Order[item.Status] = removeFromOrder(m.idx.Order[item.Status], id)
	if err := os.Remove(m.itemPath(id)); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := m.saveIndexLocked(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Removed board item #%s: %s", id, title), nil
}

// moveLocked updates item.Status, the index order, and the done marker.
// Callers must hold m.mu and save both files afterwards. Any move OUT of
// in_review clears the review link: the ticket no longer awaits review, so
// the next move into in_review may start a fresh reviewer.
func (m *BoardManager) moveLocked(item *BoardItem, column string) error {
	if item.Status != "" {
		m.idx.Order[item.Status] = removeFromOrder(m.idx.Order[item.Status], item.ID)
	}
	item.Status = column
	if column == "done" {
		item.DoneAt = time.Now().UTC()
	} else {
		item.DoneAt = time.Time{}
	}
	if column != "in_review" {
		item.ReviewSessionID = ""
	}
	m.idx.Order[column] = append(m.idx.Order[column], item.ID)
	return nil
}

func (m *BoardManager) itemCountLocked() int {
	n := 0
	for _, ids := range m.idx.Order {
		n += len(ids)
	}
	return n
}

// removeFromOrder returns order with id removed, or order unchanged if id is
// not present.
func removeFromOrder(order []string, id string) []string {
	if i := slices.Index(order, id); i >= 0 {
		return append(order[:i], order[i+1:]...)
	}
	return order
}

func normalizePriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "low", "medium", "high", "urgent":
		return strings.ToLower(strings.TrimSpace(p))
	}
	return ""
}

// SetBoardManager attaches the shared project board manager. The web server
// sets the same manager on every session agent (so claims serialize);
// TUI/CLI sets a per-agent manager. nil detaches.
func (a *Agent) SetBoardManager(m *BoardManager) {
	a.boardManager.Store(m)
	a.noteToolsChanged()
}

// BoardManager returns the attached board manager (nil when the board
// feature is disabled or not wired).
func (a *Agent) BoardManager() *BoardManager {
	return a.boardManager.Load()
}

// SetOnBoardChanged installs a callback invoked after every successful board
// mutation made through this agent's board tool; it receives the mutation's
// output message. The web server uses it to broadcast a fresh board_state and
// a success notice (toast) to all clients. nil detaches.
func (a *Agent) SetOnBoardChanged(h func(msg string)) {
	a.boardHookMu.Lock()
	a.boardHook = h
	a.boardHookMu.Unlock()
}

// notifyBoardChanged fires the installed board-change hook with the
// mutation's output message, if any hook is installed.
func (a *Agent) notifyBoardChanged(msg string) {
	a.boardHookMu.RLock()
	h := a.boardHook
	a.boardHookMu.RUnlock()
	if h != nil {
		h(msg)
	}
}
