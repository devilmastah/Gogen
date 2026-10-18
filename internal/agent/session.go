package agent

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// validateModelTimeout bounds the model validation call after session restore.
const validateModelTimeout = 12 * time.Second

// SessionSnapshot is the persisted conversation state. The canonical
// definition lives in internal/session (the storage layer owns the
// on-disk format); this alias keeps existing references compiling.
type SessionSnapshot = session.SessionSnapshot

// SessionPersister stores and loads agent sessions.
type SessionPersister interface {
	Save(id string, snap SessionSnapshot) error
	// AppendMessages writes a cumulative delta (all messages since the last
	// full snapshot); totalMsgCount is the agent's total message count.
	AppendMessages(id string, snap SessionSnapshot, totalMsgCount int) error
	LoadInWorkingDir(workingDir, id string) (SessionSnapshot, error)
	List(workingDir string) ([]SessionInfo, error)
	LatestID(workingDir string) (string, error)
	Delete(workingDir, id string) error
	// TouchSession updates only the timestamp metadata without rewriting messages.
	TouchSession(workingDir, id string) error
}

// SessionInfo describes a saved session entry. Canonical definition in
// internal/session; see SessionSnapshot above.
type SessionInfo = session.SessionInfo

// RestoreSessionLocal loads messages, mode, and model from a snapshot without
// contacting the provider. Prefer this at process startup so the UI can come
// up before model validation / context-limit lookup.
//
// newSessionID is the id being resumed into (may be empty at process startup).
// Debug builds with GOGEN_DEBUG_COMPARE_MESSAGES compare the previous session's
// wire view against the restored view (compiled out of production binaries).
func (a *Agent) RestoreSessionLocal(snap SessionSnapshot, newSessionID string) {
	prevSessionID := a.SessionID

	// Publish messages, their pre-computed token counts, and the stabilized
	// flag atomically (no defensive copy of the message slice). A snapshot
	// written by FlushSession (skipTokenCounts=true) carries no counts, and a
	// delta merge can leave them short; compute the missing counts once here
	// with one batched pass so the restored session takes the cached fast
	// paths instead of re-tokenizing the whole view every round until a
	// ContextStats / full doPersist backfills it.
	counts := snap.TokenCounts
	if a.Context != nil && len(counts) != len(snap.Messages) {
		counts = contextmgr.CountTokens(snap.Messages)
	}
	a.restoreMessages(snap.Messages, counts)
	// Token counts are keyed by content fingerprint, so entries from the
	// previous session remain valid as long as the content hasn't changed.
	// Keep the sticky project profile when resuming in the same working
	// directory so the system-prompt prefix stays byte-stable for provider
	// prompt caching. Re-detect only when the directory changed (or the
	// snapshot has no profile).
	if snap.ProjectProfile != "" && sameWorkingDir(snap.WorkingDir, a.WorkingDir) {
		a.statsMu.Lock()
		a.projectProfile = snap.ProjectProfile
		a.statsMu.Unlock()
	} else {
		a.statsMu.Lock()
		a.projectProfile = ""
		a.statsMu.Unlock()
	}
	// Pins are not persisted; drop any in-process indices from the previous
	// session so they cannot apply to the restored history.
	if a.PinManager != nil {
		a.PinManager.ClearPins()
	}
	// Todos are session-scoped. Older snapshots without a Todos field restore
	// to an empty list so project-global todos cannot leak across sessions.
	if a.TodoManager != nil {
		a.TodoManager.Replace(snap.Todos)
	}
	// Nested (subagent) sessions keep their parent link across restore so
	// the store's flat-list exclusion / cascade / per-parent cap stay
	// consistent when a child pane is reopened. The recorded outcome is
	// restored too: without it, the first persist of a reopened child
	// would write an empty status over the file's stored one, and the
	// sidebar would lose the true result after the next restart.
	a.SetParentID(snap.ParentID)
	a.SetSubagentOutcome(snap.SubagentStatus, snap.SubagentSummary)
	a.clearTurnUsage()
	// The restored conversation replaces the in-memory one wholesale, so the
	// previous conversation's compaction backoff/progress guards must not
	// carry over (they are keyed on the OLD message count and would suppress
	// emergency compaction / condensation on the restored history).
	a.resetCompactionState()
	a.statsMu.Lock()
	a.UsageAccum = UsageAccumulator{}
	a.statsMu.Unlock()
	// Reset save tracking — next doPersist will write a full snapshot that
	// merges any existing delta file from a previous process lifetime.
	a.resetSaveTracking()
	a.setSessionLabel(snap.Label)
	// Mode/thinking are read by config snapshots under statsMu (see
	// ModeAndThinkingLevel), so the restore publishes them under the same
	// lock — a concurrent attach's config snapshot would otherwise tear the
	// plain field reads. SessionOneshot is read by doPersist under statsMu,
	// so it is published under the same lock.
	a.statsMu.Lock()
	a.SessionOneshot = snap.Oneshot
	// Restore the rename marker alongside the label: a label that was a
	// deliberate rename before persistence must stay authoritative after a
	// restart (sessionLabel's legacy-50-char migration must not clobber it).
	a.labelRenamed = snap.LabelRenamed
	if m, ok := ParseMode(snap.Mode); ok {
		a.Mode = m
	}
	if l := NormalizeThinkingLevel(snap.ThinkingLevel); l != "" {
		a.ThinkingLevel = l
	}
	a.statsMu.Unlock()
	// The provider owns a separate reasoning-effort state (in web mode every
	// session gets a fresh provider seeded with the workspace default). Publish
	// the restored level to it so non-turn calls (e.g. /compact summarization
	// via GenerateResponse) and the first turn agree with the restored session
	// instead of the provider's stale seed. StreamProcessInput re-syncs every
	// turn as a safety net.
	if a.Provider != nil {
		a.Provider.SetThinkingLevel(string(a.ThinkingLevel))
	}
	// Seed the metadata fingerprint from the restored snapshot so the first
	// post-restore save compares against what was actually restored (the
	// save is a full one regardless — resetSaveTracking above — but this
	// keeps lastMeta truthful for the saves after that). lastMeta is
	// read/written by doPersist under persistMu, so the seed publishes under
	// the same lock. Seeded after the field applies above so a concurrent
	// doPersist never compares new metadata against a stale seed.
	a.persistMu.Lock()
	// Normalize the seed exactly as the fields above were normalized, so the
	// first post-restore doPersist compares like for like: an old snapshot
	// persisted "max" restores the field as "high", and TodoManager.Replace
	// (run earlier in this restore) normalizes NextID — seeding the raw
	// values would make lastMeta differ from the restored state until the
	// first full save rewrites it. (Cosmetic: that save is full anyway via
	// resetSaveTracking, but the seed should be truthful for the saves
	// after it.)
	seedMode, seedThinking := snap.Mode, snap.ThinkingLevel
	if m, ok := ParseMode(snap.Mode); ok {
		seedMode = m.String()
	}
	if l := NormalizeThinkingLevel(snap.ThinkingLevel); l != "" {
		seedThinking = string(l)
	}
	a.lastMeta = persistMeta{
		label:         snap.Label,
		mode:          seedMode,
		model:         snap.Model,
		modelProvider: snap.ModelProvider,
		thinking:      seedThinking,
		oneshot:       snap.Oneshot,
		profile:       snap.ProjectProfile,
		todos:         todoSnapshot(a.TodoManager),
	}
	a.persistMu.Unlock()
	if snap.Model != "" {
		// The routing key (owning profile) and the wire ID are restored
		// together; a legacy snapshot (empty ModelProvider) restores as a
		// default-profile model — ValidateRestoredModel re-resolves the
		// owner against the live catalog.
		ref := llm.ModelRef{Profile: snap.ModelProvider, ID: snap.Model}
		if p, ok := a.Provider.(llm.ModelRefProvider); ok {
			_ = p.SetModelRef(ref)
		} else {
			_ = a.Provider.SetModel(ref.WireID())
		}
		// The restored model is unverified until ValidateRestoredModel (or
		// the first turn's re-check) confirms it still exists at the
		// provider; until then it must not be blindly sent to the endpoint.
		a.setModelUnverified(true)
		// A legacy snapshot (no ModelProvider) lost the owning profile in
		// the migration: mark it so the first catalog validation
		// re-resolves the owner (pre-refactor rule) instead of leaving the
		// model on the default endpoint.
		a.setModelOwnerLegacy(snap.ModelProvider == "")
	}

	// Pre-warm the context limit from the snapshot so the first ContextStats
	// call after restore sees the correct max context size immediately,
	// without waiting for the async ValidateRestoredModel refresh.
	if a.Context != nil && snap.ContextLimit > 0 {
		a.Context.SetContextLimit(snap.ContextLimit)
	}

	a.compareViewOnRestore(prevSessionID, newSessionID)
}

// ValidateRestoredModel checks that the restored model still exists at the provider and
// refreshes the context limit. Safe to run in the background after startup.
// Bounded so a hung provider cannot run ListModels + ModelContextLimit
// back-to-back for an unbounded wall time.
func (a *Agent) ValidateRestoredModel(ctx context.Context, model string) {
	ctx, cancel := context.WithTimeout(ctx, validateModelTimeout)
	defer cancel()

	// Validate the restored model FIRST: it may no longer exist, in which
	// case it is cleared and the provider's sole model may be auto-selected
	// in its place. The context-limit refresh below must resolve for that
	// effective model, not the stale one.
	if model != "" {
		models, err := a.Provider.ListModels(ctx)
		if err != nil {
			// Cannot verify the restored model right now (endpoint down,
			// timeout). Fail open — keep the model — but leave it marked
			// unverified so requireModelSelected re-checks on the first
			// turn instead of sending a possibly-stale model to the
			// endpoint.
			models = nil
		} else {
			found := false
			owner := ""
			for _, m := range models {
				if m.ID == model {
					found = true
					// First match in catalog order (= profile order,
					// default first): the pre-refactor owner, used to
					// re-home a legacy restore (see
					// adoptLegacyModelOwner).
					owner = m.Provider
					break
				}
			}
			if found {
				// The restored model is still served by this provider.
				a.setModelUnverified(false)
				a.adoptLegacyModelOwner(model, owner)
			} else {
				// The restored model no longer exists at the provider.
				// Clear it — unless a concurrent /models selection
				// replaced it in the meantime.
				if a.Provider.ModelName() == model {
					_ = a.Provider.SetModel("")
				}
				a.setModelUnverified(false)
				a.setModelOwnerLegacy(false)
			}
		}
	}

	// Refresh the context limit UNCONDITIONALLY — never trust the snapshot's
	// pre-warmed value (RestoreSessionLocal) just because it is positive: it
	// was resolved by an earlier run/build, possibly to the 128k display
	// fallback (a cold models.dev registry, the pre-fix OpenCode URL
	// spelling, an endpoint that was down at save time). A positive value
	// used to short-circuit this refresh for the life of the restored
	// session, which kept the context bar at the fallback while /models
	// already resolved the real window. The probe also performs sole-model
	// auto-select, so a single-model provider still gets its model chosen.
	if a.Context != nil {
		a.Context.RefreshAfterModelChange(ctx)
	}
	// Let hosts refresh their UI: the restored model may have been cleared
	// or replaced by sole-model auto-select. Read under onModelChangedMu so
	// an overlapping ValidateRestoredModelAsync install cannot race it.
	if h := a.modelChangedHook(); h != nil {
		h()
	}
}

// modelChangedHook returns the installed OnModelChanged callback under
// onModelChangedMu. The caller invokes it OUTSIDE the lock (the hook must
// not block and may re-enter agent methods).
func (a *Agent) modelChangedHook() func() {
	a.onModelChangedMu.Lock()
	defer a.onModelChangedMu.Unlock()
	return a.OnModelChanged
}

// ValidateRestoredModelAsync is the async form of ValidateRestoredModel and
// the single wiring point for the host contract shared by every spawn site
// (the web runtime wrapper Server.validateModelAsync, the TUI root and /open
// sessions, and the CLI /resume and single-prompt paths): install the
// OnModelChanged host hook — when onChanged is non-nil, otherwise the agent
// keeps whatever hook a host already installed — and run the validation in a
// background goroutine so no caller blocks on provider ListModels. The hook
// fires from the validation goroutine after the model is confirmed, cleared,
// or auto-selected; it must not block. The goroutine is spawned
// unconditionally: an empty model still drives the context-limit refresh /
// sole-model auto-select probe, so whether a model warrants validation at
// all stays with the caller (snap.Model != "", modelInherited, the
// registration winner).
func (a *Agent) ValidateRestoredModelAsync(model string, onChanged func()) {
	if onChanged != nil {
		// Serialized with the validation goroutine's read: two validations
		// for one agent (e.g. a resume racing an earlier adopt) must not
		// race the hook field.
		a.onModelChangedMu.Lock()
		a.OnModelChanged = onChanged
		a.onModelChangedMu.Unlock()
	}
	go a.ValidateRestoredModel(context.Background(), model)
}

// SetSessionID makes id the agent's current session and syncs it to the
// provider: providers that implement llm.SessionIDSetter (OpenAIProvider)
// tag their OpenCode requests with a per-session header derived from it.
// Every session transition — startup, restore, /new, /resume, /fork,
// subagent spawn — goes through here so the provider's session view can
// never drift from a.SessionID.
func (a *Agent) SetSessionID(id string) {
	a.SessionID = id
	if s, ok := a.Provider.(llm.SessionIDSetter); ok {
		s.SetSessionID(id)
	}
}

// RestoreSession loads messages, mode, and model from a snapshot and makes id
// the agent's current session. It is the shared restore core used by
// main.go's startup restore, resumeSessionByID, and the session agent
// factory; model validation stays with the caller (async, so the UI is not
// blocked on provider ListModels).
func (a *Agent) RestoreSession(snap SessionSnapshot, id string) {
	a.RestoreSessionLocal(snap, id)
	a.SetSessionID(id)
}

// sameWorkingDir reports whether two working-directory paths refer to the same location.
// An empty snapshot dir is treated as matching (older sessions / same-store loads).
func sameWorkingDir(snapDir, currentDir string) bool {
	if snapDir == "" {
		return true
	}
	a, errA := filepath.Abs(snapDir)
	b, errB := filepath.Abs(currentDir)
	if errA != nil || errB != nil {
		return filepath.Clean(snapDir) == filepath.Clean(currentDir)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// setSessionLabel stores the session label under statsMu: web probes read it
// without turnMu (agentConfigMsgBasic, contextMsg) while the turn
// goroutine may derive it from the first user message. Clears the rename
// marker: derived, restored, and cleared labels are not renames by
// themselves (restore re-establishes the marker from the persisted snapshot
// when it was one).
func (a *Agent) setSessionLabel(label string) {
	a.statsMu.Lock()
	a.SessionLabel = label
	a.labelRenamed = false
	a.statsMu.Unlock()
}

// setSessionLabelRenamed is setSessionLabel for deliberate renames
// (RenameSession / the session_rename tool): the marker is persisted so the
// store never regenerates the label from the conversation, even when it
// happens to look like a legacy 50-char truncation of the first user message.
func (a *Agent) setSessionLabelRenamed(label string) {
	a.statsMu.Lock()
	a.SessionLabel = label
	a.labelRenamed = true
	a.statsMu.Unlock()
}

// SessionLabelSnapshot returns the current session label. Thread-safe.
func (a *Agent) SessionLabelSnapshot() string {
	a.statsMu.RLock()
	l := a.SessionLabel
	a.statsMu.RUnlock()
	return l
}

// SetSessionOneshot publishes the single-prompt (-p) session marker under
// statsMu: doPersist reads SessionOneshot under the same lock (shutdown and
// eviction flush paths run without the turn lock), so hosts must write it
// through here rather than assigning the field directly.
func (a *Agent) SetSessionOneshot(v bool) {
	a.statsMu.Lock()
	a.SessionOneshot = v
	a.statsMu.Unlock()
}

// RenameSession sets a user-visible label for the current session and persists it.
func (a *Agent) RenameSession(label string) (string, error) {
	a.setSessionLabelRenamed(label)
	a.FlushSession()
	return "Session label set to " + label, nil
}

// persistSession marks the session dirty and writes to disk only if the
// minimum interval since the last write has elapsed.  This coalesces
// rapid-fire saves during multi-tool turns into at most one write per
// persistMinInterval.  For final boundaries (turn complete, errors,
// context cancellation) use flushSession instead.
func (a *Agent) persistSession() {
	a.sessionDirty.Store(true)
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	// Skip if debounced — no point computing hash or doing I/O.
	// The debounce read must not race a concurrent doPersist's write of
	// lastPersistTime, so it runs under persistMu (best-effort timing).
	a.persistMu.Lock()
	debounced := !a.lastPersistTime.IsZero() && time.Since(a.lastPersistTime) < persistMinInterval
	a.persistMu.Unlock()
	if debounced {
		return
	}
	a.doPersist(false)
}

// FlushSession forces an immediate disk write regardless of debounce timing.
// Use at final boundaries: turn complete, errors, context cancellation, and quit.
// Skips full re-tokenization so Ctrl+C / --web shutdown stays snappy on large
// sessions; restored counts are reused when still valid.
func (a *Agent) FlushSession() {
	a.sessionDirty.Store(true)
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	a.doPersist(true)
}

// FlushPending writes any unsaved state to disk but, unlike FlushSession,
// does NOT mark the session dirty — a clean session is left untouched
// (doPersist's dirty-flag check makes it a no-op). The shutdown sweep uses
// this instead of FlushSession: forcing a write on every clean session
// re-stamped each one's Updated timestamp with ~now in sweep order (the
// focused session first, so it received the OLDEST stamp), which destroyed
// the recency ordering List/LatestID rely on after a restart — the
// saved-session list reshuffled and the session active at shutdown was
// demoted instead of restored as current. Dirty sessions (an unsaved turn,
// pending metadata change) still write exactly as before, so no state is at
// risk.
func (a *Agent) FlushPending() {
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	a.doPersist(true)
}

// doPersist is the actual write — called by persistSession/flushSession.
// Callers (persistSession, FlushSession) already validate SessionStore/SessionID;
// this method only checks the dirty flag.
//
// It uses incremental delta saves when only a few messages have been added
// since the last full snapshot, avoiding full JSON serialization on every
// 5-second debounce tick.  Importantly, lastSavedMsgCount is NOT advanced on
// incremental saves, so each delta always contains ALL messages since the last
// full snapshot — making the delta file self-contained and crash-safe.
// When skipTokenCounts is true (FlushSession), avoid cl100k re-tokenization.
func (a *Agent) doPersist(skipTokenCounts bool) {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	// Consume the dirty flag up front instead of checking it here and
	// clearing it at the end. Two flushes can be pending concurrently — the
	// turn's persistSession (holding turnMu) and the shutdown/delete/eviction
	// flush paths (no turnMu, e.g. ShutdownSessions after the 2s drain times
	// out) — and a write that snapshots EARLIER state must not clear the flag
	// set by a caller whose state it did not include. With Load-then-Store,
	// the earlier writer's trailing Store(false) wiped the later caller's
	// mark, so the later (possibly final, pre-exit) doPersist saw
	// dirty==false and returned without writing — the "quit during a running
	// turn still loses the last messages" bug. Swapping the flag at the start
	// (under persistMu) makes the writer consume exactly the mutations
	// published before it snapshotted; anything marked during the write
	// stays set and is picked up by the next flush.
	if !a.sessionDirty.Swap(false) {
		return
	}
	if a.SessionStore == nil || a.SessionID == "" {
		return
	}
	// Extend cached token counts to cover any new messages so the full
	// snapshot path can reuse them instead of re-tokenizing everything.
	a.extendTokenCounts()

	st := a.capturePersistState()
	count := len(st.msgs)
	st.profile = a.ensureProjectProfile()
	// The routing key and the wire ID are read as one ref so they cannot
	// drift apart in the persisted snapshot.
	ref := a.CurrentModelRef()
	st.model = ref.WireID()
	st.modelProvider = ref.ProfileName()
	subagentStatus, subagentSummary := a.SubagentOutcome()
	st.meta = persistMeta{
		label:           st.label,
		mode:            st.mode,
		model:           st.model,
		modelProvider:   st.modelProvider,
		thinking:        st.thinking,
		oneshot:         st.oneshot,
		profile:         st.profile,
		todos:           todoSnapshot(a.TodoManager),
		subagentStatus:  subagentStatus,
		subagentSummary: subagentSummary,
	}

	// Safety: if the message list was truncated since last save (e.g.
	// compaction, error rollback), force a full snapshot.
	if a.lastSavedMsgCount > count {
		a.lastSavedMsgCount = 0
	}

	// Decide: full snapshot or incremental delta?
	// Full snapshot on first save, when more than 5 new messages have
	// arrived, or when it's been >30s since the last full snapshot.
	// Also when non-message metadata (label, mode, model, thinking level,
	// oneshot, project profile, todos) changed since the last full snapshot:
	// incremental deltas do not carry those fields, so a quit before the
	// next full save would silently drop the change.
	needsFullSave := a.lastSavedMsgCount == 0 ||
		count-a.lastSavedMsgCount > 5 ||
		time.Since(a.lastFullSaveTime) > 30*time.Second ||
		!reflect.DeepEqual(st.meta, a.lastMeta)

	var ok bool
	if needsFullSave {
		ok = a.persistFullSnapshot(st, count, skipTokenCounts)
	} else {
		ok = a.persistDeltaSnapshot(st, count, skipTokenCounts)
	}
	if !ok {
		// The write failed; the in-memory state is still unsaved. Restore
		// the dirty flag so the next flush retries — a later turn boundary,
		// a registry eviction, or the exit sweep (ShutdownSessions /
		// flushAndQuit / the ctx-cancel sweep). Consuming the flag up front
		// is required to avoid the lost-update race documented above; the
		// bug was leaving it consumed on FAILURE, which made the failed
		// state look clean, so every later flush — including the exit sweep
		// — skipped it and the data was silently dropped. The helpers leave
		// lastSavedMsgCount/lastMeta/lastPersistTime unchanged, so the retry
		// re-attempts exactly the content that failed.
		a.sessionDirty.Store(true)
		return // the helper recorded the error; lastPersistTime stays stale
	}
	a.lastPersistErr = nil
	a.lastPersistTime = time.Now()
}

// persistState is the data doPersist captured under statsMu before
// serializing. It is immutable after capture: the write path never touches
// the live fields outside the lock.
type persistState struct {
	msgs         []llm.Message
	tokenCounts  []int
	countsEpoch  uint64
	label        string
	labelRenamed bool
	mode         string
	thinking     string
	oneshot      bool
	workingDir   string
	profile      string
	model        string
	// modelProvider is the provider profile owning model (the ModelRef
	// routing key persisted alongside it).
	modelProvider string
	meta          persistMeta
}

// capturePersistState snapshots the conversation and metadata under statsMu:
// web probes read them without turnMu, so doPersist must not touch the live
// fields outside the lock. The clone is deep (ToolCalls included) so the
// snapshot cannot race a concurrent in-place stabilization, and it is safe
// to tokenize and serialize after releasing the lock. Mode/thinking/oneshot
// are written under statsMu (SetMode, SetThinkingLevel, RestoreSessionLocal),
// so they are read under the same lock: doPersist also runs on the shutdown
// flush path with no turnMu, and an unlocked read there would race a
// concurrent SetMode or SetThinkingLevel from a still-running turn's command
// handler. Caller must hold persistMu.
func (a *Agent) capturePersistState() persistState {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return persistState{
		msgs:         cloneMessagesShallow(a.Messages),
		label:        a.SessionLabel,
		labelRenamed: a.labelRenamed,
		countsEpoch:  a.countsEpoch,
		tokenCounts:  append([]int(nil), a.tokenCounts...),
		mode:         a.Mode.String(),
		thinking:     string(a.ThinkingLevel),
		oneshot:      a.SessionOneshot,
		workingDir:   a.WorkingDir,
	}
}

// persistFullSnapshot writes a complete session snapshot, reusing the
// in-memory token counts when they cover the conversation (or tokenizing and
// backfilling the cache otherwise), then advances the incremental-save
// counters. Returns false after recording the error; the caller then skips
// the success bookkeeping.
func (a *Agent) persistFullSnapshot(st persistState, count int, skipTokenCounts bool) bool {
	subagentStatus, subagentSummary := a.SubagentOutcome()
	snap := SessionSnapshot{
		WorkingDir:     st.workingDir,
		Model:          st.model,
		ModelProvider:  st.modelProvider,
		Mode:           st.mode,
		ThinkingLevel:  st.thinking,
		Oneshot:        st.oneshot,
		Label:          st.label,
		LabelRenamed:   st.labelRenamed,
		ProjectProfile: st.profile,
		Todos:          st.meta.todos,
		Messages:       st.msgs,
		// Persist only a genuinely resolved window: an unresolved display
		// fallback must not be saved, or the next restore pre-warms the
		// guess as a resolved value.
		ContextLimit:    a.ResolvedContextLimit(),
		ParentID:        a.ParentID(),
		SubagentStatus:  subagentStatus,
		SubagentSummary: subagentSummary,
	}
	if len(st.tokenCounts) == len(st.msgs) {
		snap.TokenCounts = st.tokenCounts
	} else if a.Context != nil && !skipTokenCounts {
		snap.TokenCounts = a.Context.TokenCounts(st.msgs)
		// Backfill the in-memory cache so the next save or context probe
		// reuses these counts instead of re-tokenizing. The epoch guard
		// drops the result if the message list changed underneath us.
		a.statsMu.Lock()
		if a.countsEpoch == st.countsEpoch && len(a.tokenCounts) < len(st.msgs) {
			a.tokenCounts = append(a.tokenCounts, snap.TokenCounts[len(a.tokenCounts):]...)
		}
		a.statsMu.Unlock()
	}
	if err := a.SessionStore.Save(a.SessionID, snap); err != nil {
		log.Printf("session save failed (id=%s): %v", a.SessionID, err)
		a.lastPersistErr = err
		return false
	}
	a.lastSavedMsgCount = count
	a.lastFullSaveTime = time.Now()
	a.lastMeta = st.meta
	return true
}

// persistDeltaSnapshot appends only the messages since the last full save.
// lastSavedMsgCount is deliberately NOT advanced here: the delta file is
// overwritten on each incremental save and must always contain ALL messages
// since the last full snapshot. Advancing it would make the next delta save
// include only the newest messages, and a crash between increments would
// permanently lose the earlier batches. The full-save thresholds (5 new
// messages or 30 s) trigger a full snapshot soon enough, at which point
// lastSavedMsgCount is updated.
func (a *Agent) persistDeltaSnapshot(st persistState, count int, skipTokenCounts bool) bool {
	newMsgs := st.msgs[a.lastSavedMsgCount:]
	var newCounts []int
	if a.Context != nil && !skipTokenCounts {
		if len(st.tokenCounts) >= count {
			// extendTokenCounts ran above, so the in-memory cache already
			// covers these messages; reuse it instead of re-tokenizing
			// the same content a second time.
			newCounts = append([]int(nil), st.tokenCounts[a.lastSavedMsgCount:count]...)
		} else {
			newCounts = contextmgr.CountTokens(newMsgs)
		}
	}
	deltaSnap := SessionSnapshot{
		WorkingDir:  st.workingDir,
		Oneshot:     st.oneshot,
		Label:       st.label,
		Messages:    newMsgs,
		TokenCounts: newCounts,
	}
	if err := a.SessionStore.AppendMessages(a.SessionID, deltaSnap, count); err != nil {
		log.Printf("session delta save failed (id=%s): %v", a.SessionID, err)
		a.lastPersistErr = err
		return false
	}
	return true
}

// resetSaveTracking resets the incremental-save counters so the next
// doPersist writes a full snapshot. Call after any operation that
// truncates or replaces a.Messages (compaction, session restore, etc.).
func (a *Agent) resetSaveTracking() {
	// Serialize against a concurrent doPersist so the counters cannot change
	// mid-write: without this a shutdown flush could clone messages, then the
	// turn's truncate+reset lands, and the flush writes the pre-truncate
	// state as the final one.
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.lastSavedMsgCount = 0
	a.lastFullSaveTime = time.Time{}
}

// appendMessage appends one message to the conversation and, when the token
// count cache is complete (covers every previous message), extends it in the
// same critical section so the message list and the counts cache stay
// consistent for concurrent readers. This is the only way Messages grows
// during a turn. Thread-safe: ContextStats and SnapshotMessages snapshot
// Messages + counts under statsMu while the turn goroutine appends. Leaf
// lock: never acquire turnMu under it.
//
// The append and the cache extension share one critical section so a reader
// that sees the new message also sees its count: tokenCounts is a
// PREFIX-aligned cache that stays complete at publish time, which is what
// lets the web probes and compaction sizing take their no-tokenization fast
// paths (short prefixes are tolerated everywhere, but a lagging cache would
// push the missing suffix onto the next reader). The encode runs inside the
// lock only because the completeness check runs inside the lock first, so a
// count is never computed for a message that will not be cached. Moving the
// encode BEFORE the lock while keeping the check+append inside is equally
// correct — it just wastes the encode while the cache is incomplete, and it
// shortens the critical section. The current shape was kept because a cold
// cache is backfilled in one batched pass (ensureTokenCounts / ContextStats),
// so the per-append encode cost is paid only in the common complete-cache
// case.
func (a *Agent) appendMessage(m llm.Message) {
	a.statsMu.Lock()
	a.Messages = append(a.Messages, m)
	if a.tokenCounts != nil && len(a.tokenCounts) == len(a.Messages)-1 {
		a.tokenCounts = append(a.tokenCounts,
			contextmgr.ComputeMessageTokens(m))
	}
	a.statsMu.Unlock()
}

// publishMessages is the single critical section that publishes a conversation
// reshape under statsMu. prepare runs inside the lock and returns the new
// Messages and token counts (nil counts clears the cache); it may also mutate
// the messages in place (e.g. restore stabilization) and may read the current
// state (e.g. truncate reslicing). After prepare returns, publishMessages
// assigns the state, bumps countsEpoch, and refreshes the session image count
// atomically, so a concurrent ContextStats never pairs new messages with stale
// counts. Leaf lock: never acquire turnMu under it.
func (a *Agent) publishMessages(prepare func() (msgs []llm.Message, counts []int)) {
	a.statsMu.Lock()
	msgs, counts := prepare()
	a.Messages = msgs
	a.tokenCounts = counts
	a.countsEpoch++
	a.sessionImages.Store(countImages(msgs))
	a.statsMu.Unlock()
}

// replaceMessages swaps the conversation wholesale and invalidates the cached
// token counts (compaction, session restore, fork, reset). Publishing the new
// slice and clearing the counts in one critical section means a concurrent
// ContextStats never pairs new messages with stale counts. Leaf lock.
func (a *Agent) replaceMessages(msgs []llm.Message) {
	a.publishMessages(func() ([]llm.Message, []int) { return msgs, nil })
}

// replaceMessagesWithCounts swaps the conversation wholesale and publishes
// pre-computed per-message token counts in the same critical section. Unlike
// replaceMessages (which clears the cache), this keeps the fast
// shouldCompactUsingCounts path valid immediately — the caller computes the
// counts before publishing, which is cheap for a compaction because the
// conversation just shrank. Leaf lock.
func (a *Agent) replaceMessagesWithCounts(msgs []llm.Message, counts []int) {
	a.publishMessages(func() ([]llm.Message, []int) { return msgs, counts })
}

// restoreMessages publishes a restored session's messages together with their
// pre-computed token counts and marks every message as already-stabilized
// (persisted ArgsStr). One atomic publish so concurrent readers never observe
// partially-initialized messages. Takes ownership of msgs (no defensive copy).
// Leaf lock.
func (a *Agent) restoreMessages(msgs []llm.Message, counts []int) {
	a.publishMessages(func() ([]llm.Message, []int) {
		for i := range msgs {
			m := &msgs[i]
			m.ArgsStabilized = true
			// Recompute the ArgsJSONValid memo for restored tool calls: the
			// persisted ArgsStr is replayed byte-identically (never pinned or
			// trimmed here), so the flag is set only when ArgsStr is already the
			// exact trimmed valid wire bytes. A single validation per load beats
			// re-validating every restored tool call on every request.
			for j := range m.ToolCalls {
				tc := &m.ToolCalls[j]
				s := strings.TrimSpace(tc.ArgsStr)
				tc.ArgsJSONValid = tc.ArgsStr != "" && tc.ArgsStr == s && json.Valid([]byte(s))
			}
		}
		return msgs, counts
	})
}

// truncateMessages removes the last n messages (rollback paths) and trims the
// cached token counts to match, keeping the fast SnapshotWithCounts path valid
// after a rollback. Caller must guarantee n <= len(a.Messages). Leaf lock.
func (a *Agent) truncateMessages(n int) {
	a.publishMessages(func() ([]llm.Message, []int) {
		msgs := a.Messages[:len(a.Messages)-n]
		var counts []int
		if a.tokenCounts != nil && len(a.tokenCounts) > len(msgs) {
			counts = a.tokenCounts[:len(msgs)]
		}
		return msgs, counts
	})
}

// SnapshotMessages returns a copy of the current conversation messages that
// is safe to read after the lock is released: unstabilized ToolCalls are
// deep-copied, stabilized ones are shared (see cloneMessagesShallow). Used by the
// web server for history snapshots without holding the turn lock.
func (a *Agent) SnapshotMessages() []llm.Message {
	a.statsMu.RLock()
	msgs := cloneMessagesShallow(a.Messages)
	a.statsMu.RUnlock()
	return msgs
}

// MessageCount returns the current conversation message count. Thread-safe.
func (a *Agent) MessageCount() int {
	a.statsMu.RLock()
	n := len(a.Messages)
	a.statsMu.RUnlock()
	return n
}

// HistoryEpoch returns a counter bumped whenever the conversation is replaced
// wholesale (compaction, session restore, rollback, fork). History snapshots
// stamp it so clients can tell a snapshot that predates a reshape (e.g. a
// compaction that reset message indexes) from one that is merely older than
// the transcript they already rendered. Thread-safe.
func (a *Agent) HistoryEpoch() uint64 {
	a.statsMu.RLock()
	e := a.countsEpoch
	a.statsMu.RUnlock()
	return e
}

// HistoryShips reports whether a history snapshot includes m: snapshots skip
// empty placeholder messages so clients render only meaningful entries. The
// server's historyEntries wire builder and HistoryFingerprint share this
// predicate, so the skip decision can never disagree with what the builder
// would have shipped.
func HistoryShips(m llm.Message) bool {
	switch m.Role {
	case "user":
		// Pure-image messages (no text) are still valid history: they carry
		// their images. Only skip when there is nothing at all.
		return m.Content != "" || len(m.Images) > 0
	case "assistant":
		return m.Content != "" || len(m.ToolCalls) > 0 || m.Reasoning != "" || m.Refusal != ""
	case "tool":
		return m.Content != "" || m.ToolCallID != ""
	}
	return false
}

// HistoryFingerprint returns the history epoch together with the highest
// message index a history snapshot would ship (-1 when no message is
// shippable), read under one statsMu lock so callers compare a client's
// rendered transcript against ONE consistent conversation state. Snapshots
// skip placeholder messages (HistoryShips), so the last shipped index can
// trail len(Messages)-1. The web server's conditional session_attach uses
// the pair to decide whether a client's transcript is already current —
// without paying for the SnapshotMessages deep clone. Thread-safe.
func (a *Agent) HistoryFingerprint() (epoch uint64, lastShipped int) {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	epoch = a.countsEpoch
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if HistoryShips(a.Messages[i]) {
			return epoch, i
		}
	}
	return epoch, -1
}

// persistMinInterval is the minimum time between debounced session writes.
// Final boundaries (turn complete, errors) bypass this via flushSession().
const persistMinInterval = 5 * time.Second

// persistMeta is the set of session fields that only the full snapshot path
// persists (incremental deltas carry messages plus a label that loads
// ignore). Comparing the current values against the last full save detects
// changes that must force a full save — see lastMeta.
type persistMeta struct {
	label string
	mode  string
	model string
	// modelProvider is the provider profile owning model; changes to it
	// force a full save like changes to model.
	modelProvider string
	thinking      string
	oneshot       bool
	profile       string
	todos         *TodoList
	// subagentStatus/subagentSummary mirror the recorded subagent outcome:
	// writing it right after a turn's full save must force ANOTHER full
	// save (deltas do not carry the outcome to the index).
	subagentStatus  string
	subagentSummary string
}

// extendTokenCounts extends tokenCounts to cover any messages appended since
// the last extension. With appendMessage maintaining the counts inline this is
// normally a no-op; it is kept as a safety net for doPersist so a full
// snapshot can reuse the fast counts path.
func (a *Agent) extendTokenCounts() {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if a.tokenCounts == nil {
		return
	}
	if len(a.tokenCounts) < len(a.Messages) {
		// One batched pass with a shared memo for the missing suffix.
		a.tokenCounts = append(a.tokenCounts,
			contextmgr.CountTokens(a.Messages[len(a.tokenCounts):])...)
	}
}

// ConsumePersistError returns and clears the last session save failure, if any.
// Serialized with doPersist via persistMu: doPersist writes lastPersistErr
// under persistMu, and in web mode a no-turnMu flush path (ShutdownSessions,
// sessionDelete, registry eviction) can run concurrently with the turn end
// that consumes the error here.
func (a *Agent) ConsumePersistError() error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	err := a.lastPersistErr
	a.lastPersistErr = nil
	return err
}

// sessionPersist groups the session-persistence bookkeeping: the dirty flag,
// the debounced-write timestamps, and the last full-snapshot metadata
// (persistMeta). The persistMu comment carries the flush-serialization
// contract.
type sessionPersist struct {
	lastPersistErr error
	// sessionDirty tracks whether in-memory state differs from disk.
	// TUI: single owner goroutine. Web server: the per-session turnMu
	// serializes access across WebSocket clients (see internal/server), but the quit
	// flush (ShutdownSessions) and a running turn's flushes can both mark and
	// clear it concurrently — hence atomic (benign races on a plain bool were
	// still data races under -race).
	sessionDirty    atomic.Bool
	lastPersistTime time.Time // timestamp of last actual disk write
	// lastSavedMsgCount tracks how many messages were included in the last
	// full snapshot save. Used by doPersist to decide between full and
	// incremental delta saves, avoiding full JSON serialization every 5s.
	lastSavedMsgCount int
	lastFullSaveTime  time.Time // when the last full snapshot was written

	// persistMu serializes doPersist executions. The turn goroutine (holding
	// turnMu) and the shutdown/delete/eviction flush paths (no turnMu) can
	// call FlushSession concurrently — e.g. ShutdownSessions flushes after
	// the 2s drain times out while the turn is still running. Without this
	// lock two doPersists would interleave their message snapshots, counter
	// reads, and store writes, leaving a torn (snapshot, delta, index)
	// combination on disk that LoadInWorkingDir then drops or double-merges
	// — the "quit during a running turn loses history" bug. Serializing the
	// whole write keeps every pair consistent: the later writer re-reads the
	// counters the earlier one updated. Leaf lock: never held while
	// acquiring turnMu, and no code calls FlushSession/persistSession
	// while holding it (doPersist takes statsMu inside it, so statsMu holders
	// must not flush — they don't).
	persistMu sync.Mutex

	// lastMeta is the session metadata written by the last full snapshot
	// save. doPersist forces a full save (instead of an incremental delta)
	// when any of these changed since the last full snapshot: incremental
	// deltas only carry messages, so label/mode/model/thinking/oneshot/
	// profile/todo changes would otherwise be silently lost if the process
	// quit (or crashed) before the next full save.
	lastMeta persistMeta
}

// sessionMeta groups the small session-identity flags guarded by statsMu
// (see the statsMu comment in sessionStats) plus the model-change hook's
// install/read mutex (the exported OnModelChanged field itself stays on
// Agent).
type sessionMeta struct {
	// labelRenamed is true when SessionLabel was set deliberately
	// (RenameSession / session_rename tool). Guarded by statsMu; persisted
	// via SessionSnapshot.LabelRenamed so the store never regenerates the
	// label from the conversation (see sessionLabel).
	labelRenamed bool
	// modelUnverified is true when the selected model came from a session
	// restore or an external adoption (AdoptModelRef — the web /new
	// pane-model inheritance) and has not yet been confirmed to exist at
	// the provider. RestoreSessionLocal and AdoptModelRef set it;
	// ValidateRestoredModel clears it once the model is resolved (present,
	// absent, or auto-selected); SelectModel clears it (a selection comes
	// from the provider's own list). requireModelSelected re-checks a
	// still-unverified model on the first turn so a stale model from a
	// previous provider is never sent to the endpoint. Guarded by statsMu.
	modelUnverified bool
	// modelOwnerLegacy is true when the selected model was restored from a
	// LEGACY snapshot (saved before per-profile routing: no ModelProvider
	// field). Its owning profile was lost in the migration and is
	// re-resolved by the first catalog validation (ValidateRestoredModel /
	// recheckRestoredModel) with the pre-refactor ownership rule — the
	// first profile in profile order that lists the model — so a legacy
	// session routes exactly as the old code did: a model only one
	// non-default provider lists goes there, not to the default endpoint.
	// RestoreSessionLocal sets it; SelectModel/AdoptModelRef clear it (a
	// deliberate choice is never re-homed by a late validation); the
	// validation consumes it (adoptLegacyModelOwner). Guarded by statsMu.
	modelOwnerLegacy bool

	// onModelChangedMu serializes install (ValidateRestoredModelAsync) and
	// read (ValidateRestoredModel) of the exported OnModelChanged hook.
	// Hosts' direct construction-time assignments do not take it: they
	// happen before any validation goroutine is spawned for the agent, so
	// the spawn itself provides the happens-before edge. The lock only
	// closes the window between overlapping validations on one agent.
	// Leaf lock: never held while calling the hook.
	onModelChangedMu sync.Mutex
}
