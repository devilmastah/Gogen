package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireReleaseRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Release is idempotent and safe on a nil Lock.
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil Release: %v", err)
	}
	// The lock is available again.
	l2, err := AcquireTimeout(path, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	defer l2.Release()

	if Supported() {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("lock file not created: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("lock file mode = %o, want 600", perm)
		}
	}
}

// TestContention confirms a second holder is refused while the first holds the
// lock. Two independent opens in ONE process contend too (a flock belongs to
// the open file description), which is what makes this testable in-process.
func TestContention(t *testing.T) {
	if !Supported() {
		t.Skip("no cross-process lock on this platform")
	}
	path := filepath.Join(t.TempDir(), "store.lock")
	held, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// A short budget: the second acquire must give up rather than block.
	start := time.Now()
	_, err = AcquireTimeout(path, 60*time.Millisecond)
	if err == nil {
		t.Fatal("a second Acquire succeeded while the lock was held")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error %v should wrap ErrTimeout", err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("gave up after %v, before the wait budget elapsed", waited)
	}

	// Releasing makes it available again.
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	again, err := AcquireTimeout(path, time.Second)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestConcurrentHoldersExclude pins mutual exclusion under real contention: at
// most one goroutine may be inside the critical section at a time.
func TestConcurrentHoldersExclude(t *testing.T) {
	if !Supported() {
		t.Skip("no cross-process lock on this platform")
	}
	path := filepath.Join(t.TempDir(), "store.lock")
	const workers = 8
	inside := make(chan struct{}, 1)
	done := make(chan struct{}, workers)
	violations := make(chan string, workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			l, err := AcquireTimeout(path, 10*time.Second)
			if err != nil {
				violations <- "Acquire: " + err.Error()
				return
			}
			defer l.Release()
			select {
			case inside <- struct{}{}:
			default:
				violations <- "two holders inside the critical section"
			}
			time.Sleep(time.Millisecond)
			<-inside
		}()
	}

	for i := 0; i < workers; i++ {
		<-done
	}
	close(violations)
	for v := range violations {
		t.Fatal(v)
	}
}

func TestAcquireMissingDir(t *testing.T) {
	if !Supported() {
		t.Skip("no lock file is opened on this platform")
	}
	path := filepath.Join(t.TempDir(), "no-such-dir", "store.lock")
	if l, err := Acquire(path); err == nil {
		l.Release()
		t.Fatal("Acquire created a lock file under a missing directory")
	}
}
