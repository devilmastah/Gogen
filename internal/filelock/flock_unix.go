//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// supported: flock(2) excludes other processes on these platforms.
const supported = true

// tryLock attempts the exclusive lock without blocking (LOCK_NB). A blocking
// flock cannot honor a timeout, and a wedged holder would hang the caller.
//
// flock locks belong to the open file description, so two independent opens of
// the same path contend even inside one process. The store's in-process mutex
// is what serializes its own goroutines; this also covers a second handle.
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK): // == EAGAIN on Linux and the BSDs
		return false, nil
	case errors.Is(err, unix.EACCES):
		// What Solaris and NFS-backed mounts report for a held flock.
		return false, nil
	default:
		return false, err
	}
}

// unlockFile drops the lock. Closing the descriptor would too; unlocking first
// keeps the release symmetric with tryLock and reports a failure instead of
// hiding it.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
