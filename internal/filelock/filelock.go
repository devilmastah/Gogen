// Package filelock provides a small advisory cross-process file lock.
//
// It exists for the automation store, which more than one process writes: the
// `gogen automation …` CLI while a TUI or `--web` host is running, and two
// hosts side by side. Every read-modify-write of a store file runs while
// holding an exclusive lock on a sibling lock file, so a process always
// applies its change to the state that is actually on disk instead of
// overwriting a row another process just wrote.
//
// The lock is ADVISORY: it only excludes processes that ask for it, so every
// writer must go through Acquire. The lock file is a separate file that is
// never renamed, truncated or removed — removing it would let a third process
// lock a fresh inode while the previous holder still holds the old one. It
// must not be one of the JSON store files either: those are published by
// temp-file rename, and a lock on the replaced inode protects nothing.
//
// Waiting is bounded (DefaultTimeout) and non-blocking per attempt, so a
// wedged holder surfaces as ErrTimeout instead of hanging a CLI invocation or
// a host's sweep goroutine forever.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// DefaultTimeout bounds the wait for the lock. A store mutation is short (read
// a small JSON file, apply, write), so a wait this long means a wedged or
// pathologically slow holder.
const DefaultTimeout = 10 * time.Second

// Retry parameters for the non-blocking attempts: the common contention window
// is one read-modify-write (a few milliseconds), so the first retry almost
// always wins and the backoff only matters under real contention.
const (
	retryDelay = 2 * time.Millisecond
	retryMax   = 50 * time.Millisecond
)

// ErrTimeout reports that the lock was still held when the wait budget ran
// out.
var ErrTimeout = errors.New("filelock: timed out waiting for the lock")

// Lock is a held advisory lock on a lock file. Release it when the
// read-modify-write is done; the zero value and a nil *Lock are safe to
// release.
type Lock struct {
	f *os.File
}

// Acquire takes the exclusive lock on path, waiting up to DefaultTimeout. The
// parent directory must exist: callers own their store directory's creation,
// because a lock file appearing under a mistyped path would be worse than an
// error.
func Acquire(path string) (*Lock, error) {
	return AcquireTimeout(path, DefaultTimeout)
}

// AcquireTimeout is Acquire with an explicit wait budget (tests use a short
// one). A timeout returns an error wrapping ErrTimeout.
func AcquireTimeout(path string, timeout time.Duration) (*Lock, error) {
	if !supported {
		// No cross-process lock on this platform (see flock_noop.go): the
		// caller keeps its in-process mutex, and this returns an empty Lock so
		// the Release path stays symmetric.
		return &Lock{}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	delay := retryDelay
	for {
		ok, err := tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if ok {
			return &Lock{f: f}, nil
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("%s is still locked by another process: %w", path, ErrTimeout)
		}
		time.Sleep(delay)
		if delay < retryMax {
			delay *= 2
		}
	}
}

// Release unlocks and closes the lock file. It is idempotent and safe on a nil
// or empty Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	unlockErr := unlockFile(f)
	closeErr := f.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock %s: %w", f.Name(), unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock file %s: %w", f.Name(), closeErr)
	}
	return nil
}

// Supported reports whether this platform really excludes other processes.
// It is false on the platforms listed in flock_noop.go, where the lock
// degrades to the caller's in-process mutex.
func Supported() bool { return supported }
