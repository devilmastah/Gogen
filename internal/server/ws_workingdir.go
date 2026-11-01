package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// handleWSWorkingDir handles the working-dir branch of the config message.
func (s *Server) handleWSWorkingDir(ws *wsConn, ctx context.Context, pane **sessionRuntime, msg WSMessage) {
	// Changing the working directory is only allowed in global mode: in
	// project mode the server is scoped to one project directory and
	// sessions persist under it, so re-pointing the workspace would orphan
	// sessions and escape the project boundary. The TUI's /dir command is a
	// separate path (not web mode) and is unaffected.
	if !s.ws.GlobalMode {
		writeNoticeError(ws, "workspace", "Error: changing the working directory is only allowed in global mode (start gogen with --global)")
		return
	}
	absDir, err := filepath.Abs(msg.WorkingDir)
	if err != nil {
		writeNoticeError(ws, "workspace", fmt.Sprintf("Error: invalid path: %v", err))
		return
	}
	info, err := os.Stat(absDir)
	if err != nil || !info.IsDir() {
		writeNoticeError(ws, "workspace", fmt.Sprintf("Error: directory does not exist: %s", absDir))
		return
	}
	// The working dir is workspace-global. Interrupt the pane's own turn
	// (cancel-then-lock), then sync the change to EVERY session agent under
	// its own turn lock in a fixed (sorted) order — SetWorkingDir /
	// AfterWorkingDirChange mutate persist fields that a mid-turn doPersist
	// would race, so each must run under that session's turn lock.
	rt := *pane
	if rt.ownsTurn(ws) {
		rt.stream.cancelInFlight()
	}
	// Deliberately do NOT mirror the change into s.config.WorkingDir: the
	// server never re-reads that field after construction (SaveConfig is
	// reachable only from the --save-config CLI flag and the TUI, both
	// outside web mode), and writing it here would be an unsynchronized
	// write to a struct other goroutines read for unrelated fields. The
	// authoritative runtime value is ws.WorkingDir (set below) and the
	// per-session agents' WorkingDir (applyWorkingDirToAll).
	s.ws.SetWorkingDir(absDir)
	// Apply to every session agent OFF the read loop: a running turn
	// holds its session's turnMu for its ENTIRE duration, so waiting for all
	// of them here would freeze this connection's messages — pane switches,
	// sends, cancels — for as long as the longest running turn. The dir is
	// workspace-global; each agent's SetWorkingDir is atomic under its own
	// turnMu, so messages issued while the change is in flight simply see
	// the pre-change dir until their session is updated.
	go func(paneRT *sessionRuntime) {
		skipped := s.applyWorkingDirToAll(absDir)
		a := paneRT.agent
		if !paneRT.turnMu.TryRLock() {
			// The pane's own turn is still stuck (the sweep skipped it): the
			// config echo would hang on the lock. Send the skip report and
			// let the next config request or the turn end re-sync the client.
			if len(skipped) > 0 {
				writeNoticeError(ws, "workspace", workingDirSkipMessage(absDir, skipped))
			}
			return
		}
		cfg := agentConfigMsgBasic(a)
		paneRT.turnMu.RUnlock()
		// This reader briefly held the turn lock; a handler parked in
		// tryAcquireTurn waits on the release broadcast, so nudge it. The
		// reader is not the write holder, so there is no releaseTurn here.
		paneRT.notifyTurnReleased()
		accum := a.SnapshotUsageAccum()
		applyContextStats(&cfg, a.ContextStats(ctx), &accum)
		fillModelPricing(a, &cfg) // pure cache lookup, like the other echoes
		// The echo IS the standard config frame (agentConfigMsgBasic + stats)
		// with the new directory stamped on it: the hand-copied literal that
		// used to sit here listed 18 of the frame's fields, so every field
		// added since (pricing, reasoning efforts, session identity, the
		// runtime config registry's projection) silently missed this echo and
		// left the client half-stale after a workspace change.
		cfg.WorkingDir = absDir
		s.decorateConfig(&cfg)
		_ = ws.writeJSON(cfg)
		if len(skipped) > 0 {
			writeNoticeError(ws, "workspace", workingDirSkipMessage(absDir, skipped))
		}
	}(*pane)
}

// applyWorkingDirToAll syncs a workspace working-dir change to every session
// agent. Each agent's SetWorkingDir + AfterWorkingDirChange run under its own
// turn lock, acquired one at a time in sorted id order (never nested, so no
// lock-order deadlock; a running turn must finish or be cancelled before its
// session's lock is taken). Sessions that cannot be quiesced within the
// standard drain budget are skipped and returned so the caller can report
// them; they keep the pre-change directory until their turn finishes and the
// change is re-issued.
func (s *Server) applyWorkingDirToAll(absDir string) (skipped []string) {
	// Move the shared index before any session turn lock. SetWorkingDir
	// waits out a refresh, and that wait must not sit under a session's
	// turn lock. A later per-session SetWorkingDir sees the same folder
	// and returns.
	if ix := s.ws.CodeIndex(); ix != nil {
		ix.SetWorkingDir(absDir)
	}
	ids := s.registry.activeIDs()
	slices.Sort(ids)
	// A working-dir change relocates every session's persisted state into the
	// new directory: each agent's SetWorkingDir + AfterWorkingDirChange below
	// forces a full save there, which would stamp Updated=now on every open
	// session — the saved-session list would rank them all as "just updated"
	// even though none was interacted with. Capture each session's current
	// persisted Updated (from its pre-change directory — each agent's
	// WorkingDir still points there until the sweep) and restore it into the
	// new directory's index right after the flush. Best-effort: sessions with
	// no persisted state (never-saved /new panes) or a failed restore keep
	// the fresh stamp, matching the pre-fix behavior.
	for _, id := range ids {
		rt, ok := s.registry.get(id)
		if !ok {
			continue
		}
		// Bounded wait instead of a blocking Lock: a running (possibly
		// stuck) turn holds its session's turnMu for its ENTIRE duration,
		// and a blocking Lock here would hang the goroutine — and the
		// working-dir change — on that one session forever. Only the
		// requesting pane's turn was cancelled above; other sessions' turns
		// are not interrupted.
		if !rt.tryAcquireTurn(wsTurnAcquireWait) {
			if !rt.tryAcquireTurn(wsStreamDrainWait) {
				skipped = append(skipped, id)
				continue
			}
		}
		var prevUpdated time.Time
		if s.ws.Store != nil {
			prevUpdated = s.ws.Store.UpdatedAt(rt.agent.WorkingDir, id)
		}
		rt.agent.SetWorkingDir(absDir)
		rt.agent.AfterWorkingDirChange()
		if s.ws.Store != nil && !prevUpdated.IsZero() {
			_ = s.ws.Store.SetUpdatedAt(absDir, id, prevUpdated)
		}
		rt.releaseTurn()
	}
	return skipped
}

// workingDirSkipMessage reports a partially-applied working-dir change: the
// listed sessions were busy (running turns that were not interrupted) and
// still use the old directory.
func workingDirSkipMessage(absDir string, skipped []string) string {
	return fmt.Sprintf("Working directory set to %s; %d session(s) were busy and still use the old directory (re-issue the change once their turns finish): %s",
		absDir, len(skipped), strings.Join(skipped, ", "))
}
