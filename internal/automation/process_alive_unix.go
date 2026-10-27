//go:build !windows

package automation

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid belongs to a live process. It answers one
// question for MarkInterruptedRuns: is the co-host that owns a `running` row
// still there? Signal 0 is the portable probe — it performs the permission and
// existence checks without delivering anything.
//
// EPERM means the process exists but belongs to another user, which still
// counts as alive (the conservative answer: leave that run alone). Any probe
// error other than ESRCH is also treated as alive, so an unreadable /proc or an
// exotic sandbox can only make this sweep skip a row, never close a live run.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// FindProcess cannot fail on Unix; the signal is the actual probe. Note the
	// process is NOT waited on: it is not our child.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false
	}
	return true
}
