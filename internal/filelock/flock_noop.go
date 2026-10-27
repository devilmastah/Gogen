//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !solaris && !windows

package filelock

import "os"

// supported is false: this platform has no implementation, so there is NO
// cross-process exclusion here.
//
// LIMITATION (explicit, not papered over): on plan9, js/wasm, wasip1 and any
// other GOOS without a flock/LockFileEx port, Acquire returns immediately and
// Release does nothing. The automation store then falls back to what it had
// before the file lock existed: its in-process mutex keeps goroutines of one
// process consistent, while two processes writing the same store can still
// overwrite each other's rows and double-fire a due automation. Supported
// release targets (Linux, macOS, Windows) all take the real lock; this file is
// the honest remainder rather than a silent lie in the shared doc comment.
const supported = false

// tryLock claims success immediately (see the limitation above).
func tryLock(*os.File) (bool, error) { return true, nil }

// unlockFile is a no-op (see the limitation above).
func unlockFile(*os.File) error { return nil }
