//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// supported: LockFileEx excludes other processes on Windows.
const supported = true

// tryLock takes a non-blocking exclusive byte-range lock on the first byte of
// the lock file. LOCKFILE_FAIL_IMMEDIATELY is required: without it LockFileEx
// would block with no timeout, and the caller's bounded wait needs a
// "would block" answer it can retry.
//
// Byte-range locks are MANDATORY on Windows (unlike flock, which is advisory):
// while the lock is held, other processes are refused access to the locked
// range. That is harmless here because the range is one byte of a dedicated
// lock file, never of a JSON store file — the store files stay freely readable
// by `gogen automation ls`, editors, and the run-history readers.
func tryLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		// Another process (or another handle) holds the range.
		return false, nil
	default:
		return false, err
	}
}

// unlockFile releases the byte range. The OVERLAPPED only describes the range
// (offset 0, length 1); the lock itself belongs to the handle, so a fresh
// zeroed struct unlocks exactly what tryLock locked.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
