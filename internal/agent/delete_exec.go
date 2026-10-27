package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var (
	ErrDeleteDenied           = errors.New("delete denied by user")
	ErrDeleteApprovalRequired = errors.New("delete blocked: approval is required")
)

// DeleteFile removes a file, or an EMPTY directory, after user approval
// when required. Non-empty directories are refused: recursive deletion is
// deliberately out of scope — delete the contents first, or use
// execute_command's guarded shell for a bulk removal.
func (e *Executor) DeleteFile(ctx context.Context, path string) (string, error) {
	// No-follow: removing "config.yml" when it is a symlink unlinks the LINK.
	// Resolving first deleted the target and left the link dangling.
	secure, err := e.SecurePathNoFollow(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(secure)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(secure)
		if err != nil {
			return "", err
		}
		if len(entries) > 0 {
			return "", fmt.Errorf("directory %s is not empty (%d entries); delete only removes files and empty directories", path, len(entries))
		}
	}
	if err := e.requireDeleteApproval(ctx, []string{path}, "delete"); err != nil {
		return "", err
	}
	if err := os.Remove(secure); err != nil {
		return "", err
	}
	return fmt.Sprintf("Deleted %s", path), nil
}
