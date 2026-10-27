//go:build windows

package automation

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is STILL_ACTIVE (winbase.h 259): GetExitCodeProcess returns this
// while the process has not exited. x/sys/windows does not export it.
const stillActive = 259

// processAlive reports whether pid belongs to a live process, answering the one
// question MarkInterruptedRuns asks: is the co-host that owns a `running` row
// still there?
//
// Windows has no signal-0 probe, so this opens the process with
// PROCESS_QUERY_LIMITED_INFORMATION and asks for its exit code. OpenProcess is
// also the existence test: a nonexistent pid fails with ERROR_INVALID_PARAMETER,
// so every other failure (notably ERROR_ACCESS_DENIED — the process exists but
// belongs to another user) counts as alive, matching the store's rule of never
// closing a run it cannot rule out. A pid recycled by an unrelated process reads
// as alive too; that costs a stale history row, never a live run.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return true
	}
	return code == stillActive
}
