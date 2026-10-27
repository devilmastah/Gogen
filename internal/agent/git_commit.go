package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

const (
	// gitTailBytes is the trailing window of git output kept when the cap
	// binds. git prints the reason it failed on its LAST line ("fatal: …",
	// "error: …"), so a head-only cut would drop exactly the message the
	// caller acts on. The window is additionally clamped to the cap, so a
	// small max_tool_result_bytes cannot be doubled by the tail.
	gitTailBytes = 8 * 1024
	// gitOutputGap separates the kept head from the kept tail. It must NOT
	// open with the standard truncation marker prefix ("\n… truncated ("):
	// the notice appended after the tail has to be the only occurrence, so
	// the context manager's idempotency check (HasTruncationMarker) and the
	// spill seam's "already cut upstream" test (HasTruncatedTail) keep
	// reading the body's shape correctly.
	gitOutputGap = "\n… (middle omitted) …\n"
)

// gitOutput is the bounded merged stdout+stderr sink for git commands. The
// head runs through the same commandOutputWriter every other tool output
// uses (so max_tool_result_bytes bounds it, 0 meaning unbounded), and a
// small trailing window is kept alongside it: CombinedOutput buffered the
// whole of `git log`/`diff`/`show` — tens or hundreds of MB on a big
// repository — and git's failure reason lives at the END of the stream, so
// capping the head alone would lose it. Memory is bounded by
// max + gitTailBytes no matter how much git prints.
//
// Write is called concurrently by exec's two pipe-copy goroutines (stdout
// and stderr), so the fan-out is serialized by mu. Lock order is
// mu -> commandOutputWriter.mu (the inner writer never calls back out).
type gitOutput struct {
	mu   sync.Mutex
	head *commandOutputWriter
	// tail holds the last tailMax bytes of the merged stream.
	tail    []byte
	tailMax int
	// total counts every byte written, so head/tail overlap can be detected
	// when the stream is only slightly over the cap.
	total int64
}

// newGitOutput returns a git output sink capped at max bytes (0 = unbounded).
func newGitOutput(max int) *gitOutput {
	tailMax := gitTailBytes
	if max > 0 && max < tailMax {
		tailMax = max
	}
	return &gitOutput{
		head:    newCommandOutputWriter("git", nil, max, nil),
		tail:    make([]byte, 0, tailMax),
		tailMax: tailMax,
	}
}

// Write fans one chunk out to the capped head and the tail window. It always
// reports the full chunk as written: the cap is a memory bound, not a short
// write. Bytes are handled verbatim (git output legitimately contains NUL
// and other control bytes — a binary file's diff).
func (w *gitOutput) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.head.Write(p)
	w.total += int64(n)
	if len(p) >= w.tailMax {
		// The chunk alone fills (or overfills) the window: nothing kept
		// from earlier writes can survive it.
		w.tail = append(w.tail[:0], p[len(p)-w.tailMax:]...)
		return n, err
	}
	w.tail = append(w.tail, p...)
	if over := len(w.tail) - w.tailMax; over > 0 {
		w.tail = append(w.tail[:0], w.tail[over:]...)
	}
	return n, err
}

// String returns the text for the model: byte-identical to the old
// CombinedOutput result while nothing was dropped, and otherwise the kept
// head, a gap line, the kept tail, and the standard truncation notice LAST
// (the shape every other per-tool cap footer writes, so the context
// manager's idempotency pass and the spill seam both recognize it). Safe to
// call once the command has finished and both pipes are drained.
func (w *gitOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	head := w.head.String()
	if !w.head.overflowed {
		return head
	}
	tail := string(w.tail)
	// Measured against the whole stream: when it was only slightly over the
	// cap, the head already holds the beginning of the tail window, so the
	// duplicated prefix is dropped rather than printed twice.
	if overlap := int64(len(head)) + int64(len(tail)) - w.total; overlap > 0 {
		tail = tail[min(overlap, int64(len(tail))):]
	}
	notice := fmt.Sprintf("\n… truncated (output exceeds %d bytes)", w.head.max)
	if tail == "" {
		return head + notice
	}
	return head + gitOutputGap + tail + notice
}

// runGitCommand executes a git command with common error handling.
// It returns the trimmed text output. On error, it wraps the error with
// the git subcommand name. If ctx is cancelled, it returns the context error.
// Non-empty text is returned alongside the error when the command produces output
// but still fails (e.g. git prints a message then exits non-zero).
//
// Output is bounded by the executor's max_tool_output_bytes (0 = unbounded)
// through gitOutput: huge `git log`/`diff`/`show` results keep their head,
// their tail (where git's failure reason lives), and a truncation notice.
func (e *Executor) runGitCommand(ctx context.Context, args []string) (string, error) {
	cmd, err := e.NewGitCmd(ctx, args...)
	if err != nil {
		return "", err
	}
	// One writer for both streams keeps today's merged semantics: callers
	// rely on git's stderr text being part of the returned result.
	out := newGitOutput(e.maxToolOutputBytes())
	cmd.Stdout = out
	cmd.Stderr = out
	cmdErr := cmd.Run()
	text := strings.TrimSpace(out.String())
	if cmdErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return text, fmt.Errorf("git %s failed: %w", args[0], cmdErr)
	}
	return text, nil
}

// gitError wraps the error message with the git subcommand name when there is output.
func gitError(subcmd, text string, cause error) error {
	if text == "" {
		return fmt.Errorf("git %s failed: %w", subcmd, cause)
	}
	return fmt.Errorf("git %s failed: %s", subcmd, text)
}

// gitArgsWithPath scopes args to path as a git pathspec (the "--" separator
// keeps a path that looks like a revision or an option from being interpreted
// as one). The path is validated against the workspace first, so no read-only
// git command can be pointed outside it. An empty path leaves args untouched
// (the command then covers the whole repository).
func (e *Executor) gitArgsWithPath(args []string, path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return args, nil
	}
	if _, err := e.SecurePath(path); err != nil {
		return nil, err
	}
	return append(args, "--", path), nil
}

// GitStage stages files for commit.
// When paths is empty, it uses `git add -A` to stage everything.
func (e *Executor) GitStage(ctx context.Context, paths []string) (string, error) {
	args := []string{"add"}
	if len(paths) == 0 {
		args = append(args, "-A")
	} else {
		for _, p := range paths {
			if _, err := e.SecurePath(p); err != nil {
				return "", err
			}
		}
		args = append(args, "--")
		args = append(args, paths...)
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("add", text, err)
	}
	invalidateTrackedCache(e.GetWorkingDir())
	return "Staged successfully", nil
}

// GitUnstage removes paths from the index (reverts them to unstaged).
// When paths is empty, it unstages everything with `git restore --staged -- .`
// (git restore rejects a bare invocation with no paths).
func (e *Executor) GitUnstage(ctx context.Context, paths []string) (string, error) {
	args := []string{"restore", "--staged"}
	if len(paths) > 0 {
		for _, p := range paths {
			if _, err := e.SecurePath(p); err != nil {
				return "", err
			}
		}
		args = append(args, "--")
		args = append(args, paths...)
	} else {
		args = append(args, "--", ".")
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("restore", text, err)
	}
	invalidateTrackedCache(e.GetWorkingDir())
	return "Unstaged successfully", nil
}

// GitPush pushes the current branch to origin. When branch is empty it runs
// the fixed argv `git push` (git resolves the upstream itself); otherwise
// branch is validated as a git ref and `git push -u origin <branch>` runs.
// No user input besides the validated branch ref ever reaches argv.
func (e *Executor) GitPush(ctx context.Context, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	var args []string
	if branch == "" {
		args = []string{"push"}
	} else {
		if err := validateGitRef(branch); err != nil {
			return "", err
		}
		args = []string{"push", "-u", "origin", branch}
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return text, gitError("push", text, err)
	}
	return text, nil
}

// GitCommit creates a commit with the given message.
func (e *Executor) GitCommit(ctx context.Context, message string) (string, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	text, err := e.runGitCommand(ctx, []string{"commit", "-m", message})
	if err != nil {
		// Exit code 1 with "nothing to commit" is not an error.
		if strings.Contains(text, "nothing to commit") {
			return text, nil
		}
		return text, gitError("commit", text, err)
	}
	invalidateTrackedCache(e.GetWorkingDir())
	return text, nil
}

// GitDiffShow returns a unified diff for a specific commit or range.
// When range is empty, shows the diff for HEAD.
func (e *Executor) GitDiffShow(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if err := validateGitRef(ref); err != nil {
		return "", err
	}

	args := []string{"show", "--no-color", "--no-ext-diff"}
	if ref != "" {
		args = append(args, ref)
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("show", text, err)
	}
	if text == "" {
		return "No diff found", nil
	}
	return text, nil
}
