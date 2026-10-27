package server

import (
	"context"
	"errors"
	"strings"
	"time"
)

// respondOp runs one FS/Git request-response op and writes the standardized
// reply envelope. Callers preset resp (Type, RequestID and any request echo
// fields); fn executes the op and populates the result fields — the
// executors return zero values on failure, so nothing stale leaks into an
// error reply. A non-nil error is stamped into resp.Error instead of
// setting Success.
func respondOp(ws *wsConn, resp WSMessage, fn func(resp *WSMessage) error) {
	if err := fn(&resp); err != nil {
		resp.Error = err.Error()
	} else {
		resp.Success = true
	}
	_ = ws.writeJSON(resp)
}

// fsOp is one entry of the FS/Git dispatch tables (fsReadOps, fsWriteOps):
// resultType is the "…_result" reply envelope and run executes the op,
// populating the result and request-echo fields on resp. The executors
// return zero values on failure, so fields assigned from their results
// cannot leak into an error reply.
type fsOp struct {
	resultType string
	// async runs the op off the read loop (a long LLM call must not
	// serialize the connection's other messages). The reply is safe to
	// write from a goroutine (writeJSON only enqueues onto the conn's
	// send queue, drained by a single writer) and the client correlates
	// by RequestID, so out-of-order delivery is fine. Only meaningful on
	// read ops: write ops run under the workspace filesystem lock and
	// must stay on the caller's goroutine — unless noFSLock is set.
	async bool
	// noFSLock marks a WRITE op that does not touch the working tree: it is
	// network-only (git_push). Such an op skips the workspace filesystem lock
	// AND runs off the read loop, because fsMu is global — a slow push held it
	// for the whole transfer, stalling every session's file edits and the
	// connection's other messages. The op must not read or write working-tree
	// files, or it would race the locked tool handlers.
	noFSLock bool
	run      func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error
}

// fsReadOps dispatches read-only FS/Git requests. Unknown types are ignored.
var fsReadOps = map[string]fsOp{
	"fs_list": {resultType: "fs_list_result", run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
		resp.Path = msg.Path
		var err error
		resp.Entries, err = s.fsList(msg.Path)
		return err
	}},
	"fs_read": {resultType: "fs_read_result", run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
		resp.Path = msg.Path
		var err error
		resp.Content, resp.Language, err = s.fsRead(msg.Path)
		return err
	}},
	"fs_search": {resultType: "fs_search_result", run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
		resp.Path, resp.Pattern = msg.Path, msg.Pattern
		var err error
		resp.Matches, resp.Truncated, err = s.fsSearch(ctx, msg.Pattern, msg.Path, msg.Glob, msg.IgnoreCase)
		return err
	}},
	"git_status": {resultType: "git_status_result", run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
		status, err := s.gitStatusEntries(ctx)
		if err != nil {
			return err
		}
		resp.GitStatus = &status
		// Legacy flat list (Unstaged+Untracked) kept for one release
		// cycle so a stale browser tab (pre-v2 panel) keeps working.
		legacy := make([]GitStatusEntry, 0, len(status.Unstaged)+len(status.Untracked))
		legacy = append(legacy, status.Unstaged...)
		legacy = append(legacy, status.Untracked...)
		resp.GitEntries = legacy
		return nil
	}},
	"git_file_diff": {resultType: "git_file_diff_result", run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
		resp.Path = msg.Path
		var err error
		resp.Original, resp.Modified, resp.Language, err = s.gitFileDiff(ctx, msg.Path)
		return err
	}},
	"git_commit_message": {
		// One-shot LLM call over the staged diff — no chat session is
		// created or touched (see generateCommitMessage). Run it OFF the
		// read loop: the call can take up to gitCommitMessageTimeout (60s),
		// and the read loop serializes every message on the connection
		// (cancel, FS reads/writes, editor saves).
		resultType: "git_commit_message_result",
		async:      true,
		run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
			var err error
			resp.Content, err = s.generateCommitMessage(ctx)
			return err
		},
	},
}

// runFSOp dispatches msg through op: it builds the typed result envelope and
// hands it to respondOp, asynchronously when op.async.
func (s *Server) runFSOp(ws *wsConn, ctx context.Context, msg WSMessage, op fsOp) {
	run := func(resp *WSMessage) error { return op.run(s, ctx, msg, resp) }
	envelope := WSMessage{Type: op.resultType, RequestID: msg.RequestID}
	if op.async {
		go respondOp(ws, envelope, run)
		return
	}
	respondOp(ws, envelope, run)
}

// handleFSReadMessage serves read-only FS/Git requests (listing, reading,
// search, status, file diff, one-shot commit-message generation). Unknown
// types are ignored.
func (s *Server) handleFSReadMessage(ws *wsConn, ctx context.Context, msg WSMessage) {
	if op, ok := fsReadOps[msg.Type]; ok {
		s.runFSOp(ws, ctx, msg, op)
	}
}

// fsWriteOps dispatches mutating FS/Git requests. Entries run on the
// caller's goroutine under the workspace filesystem lock held by
// handleFSWriteMessage.
var fsWriteOps = map[string]fsOp{
	"fs_write": {resultType: "fs_write_result", run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
		resp.Path = msg.Path
		return s.fsWrite(msg.Path, msg.Content)
	}},
	"fs_replace": {resultType: "fs_replace_result", run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
		resp.Path, resp.Pattern = msg.Path, msg.Pattern
		var err error
		resp.Replaced, resp.FileCount, resp.Files, err = s.fsReplace(ctx, msg.Pattern, msg.Replacement, msg.Path, msg.Glob)
		return err
	}},
	"fs_apply_patch": {
		// Applies a unified diff to files under the working directory using
		// the agent's patch engine (exact-context match, no fuzzy relocation).
		// Delete-only patches require approval and are rejected here; use the
		// agent for those.
		resultType: "fs_apply_patch_result",
		run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
			report, err := s.ws.Exec.PatchFile(ctx, msg.Diff, false, false)
			if report != "" {
				resp.Result = report
			}
			return err
		},
	},
	"git_commit": {
		// Commit composer (ticket #53). The message is passed as a single
		// argv element via GitCommit — never through a shell.
		resultType: "git_commit_result",
		run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
			if strings.TrimSpace(msg.Content) == "" {
				return errors.New("commit message is required")
			}
			var err error
			resp.Result, err = s.ws.Exec.GitCommit(ctx, msg.Content)
			return err
		},
	},
	"git_stage": {
		// Stage the given paths (empty = all changes, i.e. `git add -A`).
		// GitStage SecurePath-validates every path and builds argv-only.
		resultType: "git_stage_result",
		run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
			var err error
			resp.Result, err = s.ws.Exec.GitStage(ctx, msg.Paths)
			return err
		},
	},
	"git_unstage": {
		// Unstage the given paths (empty = all staged changes) via
		// `git restore --staged -- <paths>` — SecurePath-validated,
		// argv-only.
		resultType: "git_unstage_result",
		run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
			var err error
			resp.Result, err = s.ws.Exec.GitUnstage(ctx, msg.Paths)
			return err
		},
	},
	"git_push": {
		// Push to origin with a fixed argv; the only user-controlled input
		// is the branch ref, validated inside GitPush (validateGitRef).
		//
		// noFSLock: the transfer is network I/O and mutates no working-tree
		// file. Holding the workspace lock (and the read loop) for it stalled
		// every session's edits during a slow push; a bounded timeout keeps a
		// hung remote from pinning the goroutine until the tab closes.
		resultType: "git_push_result",
		noFSLock:   true,
		run: func(s *Server, ctx context.Context, msg WSMessage, resp *WSMessage) error {
			pushCtx, cancel := context.WithTimeout(ctx, gitPushTimeout)
			defer cancel()
			var err error
			resp.Result, err = s.ws.Exec.GitPush(pushCtx, msg.Branch)
			return err
		},
	},
}

// handleFSWriteMessage serves mutating FS/Git requests, serialized on the
// workspace filesystem lock (not the session turn lock): they wait only for
// the actual mutation window of a running tool, never for the whole
// streaming turn. Unknown types are ignored.
func (s *Server) handleFSWriteMessage(ws *wsConn, ctx context.Context, msg WSMessage) {
	op, ok := fsWriteOps[msg.Type]
	if !ok {
		return
	}
	if op.noFSLock {
		// Network-only op: no lock (fsMu is global, so holding it for a push
		// blocked every session's file edits) and no read-loop stall.
		go s.runFSOp(ws, ctx, msg, op)
		return
	}
	s.ws.fsMu.Lock()
	defer s.ws.fsMu.Unlock()
	s.runFSOp(ws, ctx, msg, op)
}

// gitPushTimeout bounds one git_push. Long enough for a big push over a slow
// link, short enough that a hung remote cannot pin the goroutine.
const gitPushTimeout = 2 * time.Minute
