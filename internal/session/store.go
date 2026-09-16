package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/randhex"
)

const version = 1

// legacyLabelMaxLen is the label length older versions truncated to. Stored
// labels of exactly this length are the only ones that may need migration to
// the full untruncated label, so List can skip re-reading session files for
// every other entry.
const legacyLabelMaxLen = 50

type file struct {
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	WorkingDir    string    `json:"workingDir"`
	Model         string    `json:"model"`
	Mode          string    `json:"mode"`
	ThinkingLevel string    `json:"thinkingLevel,omitempty"`
	Label         string    `json:"label,omitempty"`
	// LabelRenamed records that Label was set deliberately (RenameSession /
	// session_rename tool) rather than derived from the first user message.
	// The store must never regenerate a renamed label — not even one that
	// looks like a legacy 50-char truncation (see sessionLabel).
	LabelRenamed   bool          `json:"labelRenamed,omitempty"`
	ProjectProfile string        `json:"projectProfile,omitempty"`
	Todos          *TodoList     `json:"todos,omitempty"`
	Messages       []llm.Message `json:"messages"`
	Oneshot        bool          `json:"oneshot,omitempty"`
	TokenCounts    []int         `json:"tokenCounts,omitempty"`
	ContextLimit   int           `json:"contextLimit,omitempty"`
	// ParentID marks nested (subagent) sessions; the flat session list
	// excludes them and deleting the parent cascades.
	ParentID string `json:"parentID,omitempty"`
	// SubagentStatus records the final outcome of a nested (subagent)
	// session: "" (unknown / not finished), "success", or "failed".
	// Mirrored into the index so the sidebar can render the true outcome
	// after a reload/restart, when the subagent events are not replayed.
	SubagentStatus  string `json:"subagentStatus,omitempty"`
	SubagentSummary string `json:"subagentSummary,omitempty"`
}

type Store struct {
	mu           sync.Mutex
	enabled      bool
	maxCount     int
	maxAgeDays   int
	createdCache map[string]time.Time // sessionID → Created timestamp (avoids re-read)
	saveCount    int                  // counter for periodic pruning
	autoPrune    bool                 // internal prune on Save; disabled when a registry owns pruning
	globalDir    string               // non-empty: override per-project .gogen/sessions/ dir
}

// StoreOptions configures retention for persisted sessions.
type StoreOptions struct {
	MaxCount   int // keep at most this many sessions (0 = config.DefaultSessionMaxCount)
	MaxAgeDays int // drop sessions older than this many days (0 = config.DefaultSessionMaxAgeDays, negative = keep forever)
}

// SetGlobalDir configures the store to use a fixed directory for session
// storage instead of the per-project .gogen/sessions/. Used in global mode.
func (s *Store) SetGlobalDir(dir string) {
	s.mu.Lock()
	s.globalDir = dir
	s.mu.Unlock()
}

// SetAutoPrune toggles the internal prune that Save performs every few
// writes. The TUI keeps it enabled (single active session — pruning around
// the current session is correct). The multi-session web server disables it
// and becomes the sole pruner via Prune, so a Save from one active session
// can never delete another live session's file. Default is enabled.
func (s *Store) SetAutoPrune(enabled bool) {
	s.mu.Lock()
	s.autoPrune = enabled
	s.mu.Unlock()
}

// SetRetention updates the session retention options at runtime (the web
// settings modal session_max_count / session_max_age_days). The same
// normalization as NewStoreWithOptions applies: 0 = the config default,
// negative maxAgeDays = keep sessions forever. Callers must re-prune after
// a change that reduces capacity (the web handler uses its registry-aware
// pruneSessions).
func (s *Store) SetRetention(maxCount, maxAgeDays int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxAgeDays < 0 {
		// Negative disables age-based retention ("keep sessions forever").
		s.maxAgeDays = maxAgeDays
	} else {
		s.maxAgeDays = config.Effective(maxAgeDays, config.DefaultSessionMaxAgeDays)
	}
	s.maxCount = config.Effective(maxCount, config.DefaultSessionMaxCount)
}

// MaxCount returns the current max-session count (runtime-safe read).
func (s *Store) MaxCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxCount
}

// MaxAgeDays returns the current max session age in days (-1 = keep
// sessions forever; runtime-safe read).
func (s *Store) MaxAgeDays() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxAgeDays
}

// NewStoreWithOptions creates a session store with custom retention.
func NewStoreWithOptions(enabled bool, opts StoreOptions) *Store {
	maxCount := config.Effective(opts.MaxCount, config.DefaultSessionMaxCount)
	maxAge := config.Effective(opts.MaxAgeDays, config.DefaultSessionMaxAgeDays)
	if opts.MaxAgeDays < 0 {
		// Negative disables age-based retention ("keep sessions forever");
		// 0 still means the default via config.Effective.
		maxAge = -1
	}
	return &Store{enabled: enabled, maxCount: maxCount, maxAgeDays: maxAge, createdCache: make(map[string]time.Time), autoPrune: true}
}

func (s *Store) dir(workingDir string) string {
	if s.globalDir != "" {
		return s.globalDir
	}
	return filepath.Join(workingDir, ".gogen", "sessions")
}

func (s *Store) path(workingDir, id string) string {
	if s.globalDir != "" {
		// In global mode, sessions are stored flat in the global dir.
		return filepath.Join(s.globalDir, id+".json")
	}
	return filepath.Join(s.dir(workingDir), id+".json")
}

func validateSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("session id is required")
	}
	// "index" is the metadata index file's name (index.json): a session with
	// that id would overwrite (Save) or remove (Delete) the index, and it is
	// skipped by the file scans, so it can never round-trip. Reject it
	// case-insensitively: on case-insensitive filesystems (Windows, macOS)
	// "INDEX" collides with index.json too.
	if strings.EqualFold(id, "index") {
		return fmt.Errorf("invalid session id: %q is reserved", id)
	}
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid session id")
	}
	if id != filepath.Base(id) {
		return fmt.Errorf("invalid session id")
	}
	return nil
}

func ensureUnderSessionsDir(workingDir, path string, globalDirs ...string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Check against global dir first if provided.
	for _, gd := range globalDirs {
		if gd == "" {
			continue
		}
		gdAbs, err := filepath.Abs(gd)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(gdAbs, absPath)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	// Fall back to project-local sessions dir.
	sessionsDir, err := filepath.Abs(filepath.Join(workingDir, ".gogen", "sessions"))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(sessionsDir, absPath)
	if err != nil {
		return fmt.Errorf("invalid session path")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid session path")
	}
	return nil
}

// NewID generates a new session id.
func NewID() string {
	return randhex.ID(16, "")
}
