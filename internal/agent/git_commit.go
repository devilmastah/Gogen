package agent

import (
	"context"
	"fmt"
	"strings"
)

// runGitCommand executes a git command with common error handling.
// It returns the trimmed text output. On error, it wraps the error with
// the git subcommand name. If ctx is cancelled, it returns the context error.
// Non-empty text is returned alongside the error when the command produces output
// but still fails (e.g. git prints a message then exits non-zero).
func (e *Executor) runGitCommand(ctx context.Context, args []string) (string, error) {
	cmd, err := e.NewGitCmd(ctx, args...)
	if err != nil {
		return "", err
	}
	out, cmdErr := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
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
