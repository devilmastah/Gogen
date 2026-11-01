package agent

import (
	"gogen/internal/llm"
	"gogen/internal/projectfile"
)

// SetWorkingDir updates in-memory working directory fields (agent, executor,
// todos). It does not touch disk or the models.dev cache — call
// AfterWorkingDirChange for that.
func (a *Agent) SetWorkingDir(dir string) {
	// WorkingDir is read by ContextStats and doPersist without the turn lock
	// (web probes, shutdown/eviction flushes), so the publish goes under
	// statsMu together with the projectProfile reset — a plain field write
	// here raced those unlocked readers (data race on a.WorkingDir).
	a.statsMu.Lock()
	a.WorkingDir = dir
	a.projectProfile = ""
	a.statsMu.Unlock()
	if a.Executor != nil {
		a.Executor.SetWorkingDir(dir)
	}
	if a.TodoManager != nil {
		a.TodoManager.SetWorkingDir(dir)
	}
	if bm := a.BoardManager(); bm != nil {
		// Global mode keeps the global board; project mode re-points to the
		// new working dir's .gogen/board (D3).
		bm.SetWorkingDir(dir)
	}
	if sm := a.SkillsManager(); sm != nil {
		// Re-point the project skills root to the new working dir (the user
		// root is unchanged).
		sm.SetWorkingDir(dir)
	}
	if ix := a.CodeIndex(); ix != nil {
		ix.SetWorkingDir(dir)
	}
	if a.instructionsEnabled.Load() {
		// Re-render the AGENTS.md/CLAUDE.md section for the new dir: the
		// project guidelines body is unchanged, but the workspace
		// instructions must never come from the previous project.
		a.RefreshWorkspaceInstructions(dir)
	}
}

// WorkingDirSnapshot returns the agent's current working directory.
// Thread-safe: SetWorkingDir publishes the field under statsMu, so callers
// that run OUTSIDE the turn goroutine (the web settings sweeps, context
// stats) must read it here rather than touching the field.
func (a *Agent) WorkingDirSnapshot() string {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return a.WorkingDir
}

// AfterWorkingDirChange persists the session and retargets the models.dev
// cache for the new project dir. Both steps do disk (and possibly background
// network) I/O. The web server calls it under the session's turnMu so it is
// serialized with a concurrent doPersist from a running turn.
func (a *Agent) AfterWorkingDirChange() {
	cacheDir := a.WorkingDir
	if a.GlobalMode {
		cacheDir = projectfile.GlobalDataDir()
	}
	if p, ok := a.Provider.(*llm.OpenAIProvider); ok {
		p.SetModelInfoCacheDir(cacheDir)
	}
	// The session's persisted location follows the working dir
	// (Store.Save/AppendMessages key by snap.WorkingDir). After a dir
	// change the last full snapshot lives in the OLD directory, so an
	// incremental delta here would be written to the NEW directory without
	// its base snapshot — the session becomes unloadable there until the
	// next full save (which may never come if the process quits first).
	// Force a full snapshot into the new directory instead.
	a.resetSaveTracking()
	a.FlushSession()
}
