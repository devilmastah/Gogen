package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const (
	gitLogDefaultLimit = 20
	gitLogMaxLimit     = 100
)

// GitLog returns recent commit history, optionally scoped to a path.
func (e *Executor) GitLog(ctx context.Context, path string, limit int) (string, error) {
	if limit <= 0 {
		limit = gitLogDefaultLimit
	}
	if limit > gitLogMaxLimit {
		limit = gitLogMaxLimit
	}

	args, err := e.gitArgsWithPath([]string{"log", "--oneline", "--no-decorate", "-n", strconv.Itoa(limit)}, path)
	if err != nil {
		return "", err
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("log", text, err)
	}
	if text == "" {
		return "No commits found", nil
	}
	return text, nil
}

// GitBlame returns per-line attribution for path: the commit, author, and
// date that last touched each line. An empty ref blames the working tree;
// a validated ref blames the file as of that commit. lineStart/lineEnd map
// to git blame -L and must be given together (0 = unset), so large files can
// be narrowed to the lines of interest.
func (e *Executor) GitBlame(ctx context.Context, path, ref string, lineStart, lineEnd int) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("file is required")
	}
	if _, err := e.SecurePath(path); err != nil {
		return "", err
	}
	ref = strings.TrimSpace(ref)
	if err := validateGitRef(ref); err != nil {
		return "", err
	}
	if lineStart < 0 || lineEnd < 0 {
		return "", fmt.Errorf("line_start and line_end must be positive")
	}
	if (lineStart == 0) != (lineEnd == 0) {
		return "", fmt.Errorf("line_start and line_end must be given together")
	}
	if lineStart > lineEnd {
		return "", fmt.Errorf("line_start must not exceed line_end")
	}

	args := []string{"blame"}
	if lineStart > 0 {
		args = append(args, "-L", fmt.Sprintf("%d,%d", lineStart, lineEnd))
	}
	if ref != "" {
		args = append(args, ref)
	}
	args = append(args, "--", path)

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("blame", text, err)
	}
	if text == "" {
		return "No blame information (file may be empty)", nil
	}
	return text, nil
}
