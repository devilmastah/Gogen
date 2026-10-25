package agent

import "context"

// GitStatus returns a summary of working tree status.
func (e *Executor) GitStatus(ctx context.Context, path string) (string, error) {
	args, err := e.gitArgsWithPath([]string{"status", "--short"}, path)
	if err != nil {
		return "", err
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("status", text, err)
	}
	if text == "" {
		return "Working tree clean (no changes)", nil
	}
	return text, nil
}
