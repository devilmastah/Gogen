package session

import (
	"time"

	"gogen/internal/llm"
)

// SessionSnapshot is persisted conversation state.
type SessionSnapshot struct {
	WorkingDir string
	Oneshot    bool
	// Model is the bare model ID the endpoint expects on the wire.
	Model string
	// ModelProvider is the registered provider profile that owns Model
	// ("" in legacy snapshots = the default profile). Restored together
	// with Model as the routing key; an empty value restores the model
	// under the default profile (legacy snapshots predate per-profile
	// routing).
	ModelProvider string
	Mode          string
	ThinkingLevel string // persisted thinking level; empty/absent means "off"
	Label         string
	// LabelRenamed is true when Label was set deliberately (RenameSession /
	// the session_rename tool) rather than derived from the first user
	// message. Persisted so the store never regenerates a deliberate rename
	// (see sessionLabel's legacy-50-char migration rule).
	LabelRenamed   bool
	ProjectProfile string
	Todos          *TodoList
	Messages       []llm.Message
	TokenCounts    []int // pre-computed token counts per message (nil if unavailable)
	ContextLimit   int   // resolved context window size (0 = unknown)
	// ParentID is non-empty for nested (subagent) sessions. Nested sessions
	// are excluded from the flat saved list, cascade-deleted with their
	// parent, and capped per parent (D2).
	ParentID string
	// SubagentStatus records the outcome of a nested (subagent) session:
	// "running" (admitted, no terminal state recorded yet), "success",
	// "failed", or "" (not a subagent, or a record written before admission
	// was stamped). Persisted so the sidebar can render the true outcome
	// after a reload/restart, when the subagent_started/finished events are
	// gone; "running" with no live runtime means the child was interrupted.
	SubagentStatus string
	// SubagentSummary is the report/error summary written alongside
	// SubagentStatus.
	SubagentSummary string
}

// SessionInfo describes a saved session entry.
type SessionInfo struct {
	ID      string
	Oneshot bool
	// UpdatedAt is the entry's update time, formatted RFC3339Nano (see
	// UpdatedTime for the time.Time form).
	UpdatedAt    string
	MessageCount int
	Label        string
	// ParentID is non-empty for nested (subagent) sessions; the sidebar
	// renders them under their parent and the flat saved-session list
	// excludes them.
	ParentID string
	// SubagentStatus is the persisted outcome of a nested (subagent)
	// session: "running" (no terminal state recorded), "success", "failed",
	// or "" (not a subagent / legacy record).
	SubagentStatus string
	// SubagentSummary is the report/error summary written alongside
	// SubagentStatus.
	SubagentSummary string
}

// UpdatedTime parses UpdatedAt (the RFC3339Nano form the store writes),
// returning the zero time when it is absent or malformed — callers sort by it
// and must tolerate a missing timestamp rather than dropping the entry.
func (s SessionInfo) UpdatedTime() time.Time {
	if s.UpdatedAt == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s.UpdatedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// TodoItem represents a single task.
type TodoItem struct {
	ID        int       `json:"id"`
	Text      string    `json:"text"`
	Status    string    `json:"status"` // pending, done
	CreatedAt time.Time `json:"created_at"`
	DoneAt    time.Time `json:"done_at,omitempty"`
}

// TodoList manages a persistent todo list.
type TodoList struct {
	Items  []TodoItem `json:"items"`
	NextID int        `json:"next_id"`
}

// ArchiveEntry is one line of a session's archive sidecar (Phase 5):
// content that was shadowed out of the live history. The sidecar is an
// append-only JSONL file written by the session store (see
// Store.AppendArchive); the agent only produces entries.
type ArchiveEntry struct {
	// TS is when the entry was archived (UTC).
	TS time.Time `json:"ts"`
	// Kind identifies the kind of shadowed content. "condensed_message"
	// is the Phase 0e last-resort condensation of a message that cannot
	// fit the context window.
	Kind string `json:"kind"`
	// Index is the message's index in the live history at the moment it
	// was shadowed.
	Index int `json:"index"`
	// Role is the shadowed message's role.
	Role string `json:"role"`
	// Tokens is the shadowed message's estimated token count.
	Tokens int `json:"tokens"`
	// Content is the shadowed message's full original content.
	Content string `json:"content"`
	// Model is the model that produced the shadowed message (assistant
	// messages only; empty otherwise).
	Model string `json:"model,omitempty"`
}
