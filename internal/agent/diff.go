package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// emptyTreeHash is the well-known SHA-1 of the empty git tree, used as the
// diff base on an unborn HEAD (a repo with no commits yet), where
// `git diff --cached` alone fails with "bad revision 'HEAD'".
const emptyTreeHash = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// GitStagedDiff returns the staged (index vs HEAD) diff, or an empty string
// when nothing is staged. On an unborn HEAD it diffs against the empty tree
// so the first commit's staged content is still visible.
func (e *Executor) GitStagedDiff(ctx context.Context) (string, error) {
	args := []string{"diff", "--no-color", "--cached"}
	if !e.hasHead(ctx) {
		args = append(args, emptyTreeHash)
	}
	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		// Preserve git's stderr (e.g. "fatal: not a git repository") when
		// the command produced output; otherwise the wrapped error already
		// names the failing subcommand.
		if text != "" {
			return "", fmt.Errorf("git diff --cached failed: %s", text)
		}
		return "", err
	}
	return text, nil
}

// hasHead reports whether the repository has at least one commit. A probe
// failure (not a git repo, git missing) is reported as "no HEAD" so the
// caller surfaces the diff-command error instead of a misleading one.
func (e *Executor) hasHead(ctx context.Context) bool {
	cmd, err := e.NewGitCmd(ctx, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return false
	}
	_, err = cmd.Output()
	return err == nil
}

// ShowDiff returns a unified diff for the working tree using git when available.
func (e *Executor) ShowDiff(ctx context.Context, path string, staged bool) (string, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff"}
	if staged {
		args = append(args, "--cached")
	}
	args, err := e.gitArgsWithPath(args, path)
	if err != nil {
		return "", err
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		// git diff exits 1 when there are no differences (or --check
		// whitespace findings) — that is not a failure.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			if text == "" {
				return "No differences found", nil
			}
			return text, nil
		}
		// Preserve git's stderr (e.g. "fatal: not a git repository") when
		// the command produced output; otherwise the wrapped error already
		// names the failing subcommand.
		if text != "" {
			return "", fmt.Errorf("git diff failed: %s", text)
		}
		return "", err
	}
	if text == "" {
		return "No differences found", nil
	}
	return text, nil
}
