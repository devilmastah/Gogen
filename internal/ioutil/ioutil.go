package ioutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gogen/internal/debuglog"
)

// WriteFileAtomic writes content to a file atomically using a temp file + rename.
// It creates parent directories as needed, preserves existing file permissions
// when overwriting, and handles unsupported chmod gracefully on some filesystems.
// The temp file is fsynced before the rename and the containing directory is
// fsynced after it, so the new content survives a crash or power loss.
func WriteFileAtomic(path string, content []byte, perm os.FileMode) error {
	return writeFileSync(path, content, perm, false)
}

// WriteFileAtomicNoSync is like WriteFileAtomic but skips fsync to reduce
// SSD wear.  Use for high-frequency internal state files (session
// snapshots and deltas, the session index, board state) where write
// volume matters more than last-write durability: temp+rename still
// guarantees readers never observe a torn file, and the worst case after
// a power loss is losing the most recent write. Neither the temp file nor
// the containing directory is fsynced, so the rename itself may also be
// lost, reverting to the previous file. Keep fsync (plain
// WriteFileAtomic) for user-requested file edits and configuration.
func WriteFileAtomicNoSync(path string, content []byte, perm os.FileMode) error {
	return writeFileSync(path, content, perm, true)
}

// NewAtomicWriterNoSync is the streaming counterpart of WriteFileAtomicNoSync:
// the same temp-file-plus-rename contract and permission handling, without the
// fsyncs (see WriteFileAtomicNoSync for when that is acceptable). Callers write
// straight to the returned writer and then Commit (or Abort).
func NewAtomicWriterNoSync(path string, perm os.FileMode) (*AtomicWriter, error) {
	return newAtomicWriter(path, perm, true)
}

// writeFileSync is the shared implementation. When skipFSync is true neither
// the temp file nor the containing directory is fsynced before/after rename
// (trades durability for less SSD wear).
func writeFileSync(path string, content []byte, perm os.FileMode, skipFSync bool) error {
	w, err := newAtomicWriter(path, perm, skipFSync)
	if err != nil {
		return err
	}
	if _, err := w.Write(content); err != nil {
		w.Abort()
		return err
	}
	return w.Commit()
}

// AtomicWriter writes content to a temporary file and atomically moves it into
// place on Commit. It is the streaming counterpart of WriteFileAtomic: a
// caller whose content is too large to buffer in memory writes straight to the
// writer (it implements io.Writer) instead of materializing the whole body as
// a []byte, so the file never has to exist on the heap.
//
// Parent directories are created when the writer is constructed and an
// existing target file's mode is preserved. Commit fsyncs the temp file before
// the rename and the containing directory after it (the durability contract
// documented on WriteFileAtomic). Abort — or letting the writer go without
// Commit — closes and removes the temp file, leaving any existing target
// untouched.
type AtomicWriter struct {
	path      string
	tmp       *os.File
	tmpName   string
	skipFSync bool
	done      bool
}

// NewAtomicWriter creates a temp file in path's directory (creating parent
// directories as needed) ready to receive the atomic write. Call Commit to
// publish it or Abort to discard it.
func NewAtomicWriter(path string, perm os.FileMode) (*AtomicWriter, error) {
	return newAtomicWriter(path, perm, false)
}

func newAtomicWriter(path string, perm os.FileMode, skipFSync bool) (*AtomicWriter, error) {
	// Resolve a symlinked target first: writing through the temp+rename would
	// otherwise replace the LINK with a regular file, so a dotfile manager's
	// ~/.config/gogen/config.yaml -> ~/dotfiles/... link silently became a
	// private copy that stopped tracking the original.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	// Preserve the existing file mode when overwriting, so execute bits
	// on scripts are not destroyed.
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode()
	}
	tmp, err := os.CreateTemp(dir, ".gogen-write-*")
	if err != nil {
		return nil, err
	}
	w := &AtomicWriter{path: path, tmp: tmp, tmpName: tmp.Name(), skipFSync: skipFSync}

	// Chmod may be unsupported on some filesystems (Windows, FUSE, 9p, some
	// network mounts). When that happens, don't fail the whole write — log
	// a debug entry and continue. The temp file's default mode (typically
	// 0600) will be inherited by the renamed final file, but the content
	// write itself still succeeds.
	if err := tmp.Chmod(perm); err != nil {
		if !isChmodUnsupported(err) {
			_ = tmp.Close()
			_ = os.Remove(w.tmpName)
			return nil, err
		}
		debuglog.Write("ioutil/write", "Chmod unsupported; file written with default mode", "fs-chmod-unsupported", map[string]any{
			"path": path,
			"err":  err.Error(),
		})
	}
	return w, nil
}

// Write implements io.Writer, appending to the temp file.
func (w *AtomicWriter) Write(p []byte) (int, error) {
	return w.tmp.Write(p)
}

// Commit fsyncs (unless disabled), closes the temp file, renames it into place,
// and fsyncs the containing directory. It is a no-op after the writer has
// already been committed or aborted.
func (w *AtomicWriter) Commit() error {
	if w.done {
		return nil
	}
	w.done = true
	if !w.skipFSync {
		if err := w.tmp.Sync(); err != nil {
			_ = w.tmp.Close()
			_ = os.Remove(w.tmpName)
			return err
		}
	}
	if err := w.tmp.Close(); err != nil {
		_ = os.Remove(w.tmpName)
		return err
	}
	if err := os.Rename(w.tmpName, w.path); err != nil {
		_ = os.Remove(w.tmpName)
		return err
	}
	if !w.skipFSync {
		// Fsyncing the temp file guarantees its content is durable, but the
		// directory entry that makes path point at it is not. Fsync the
		// containing directory so a power loss cannot revert path to the
		// previous file.
		if err := syncDir(filepath.Dir(w.path)); err != nil {
			return err
		}
	}
	return nil
}

// Abort closes and removes the temp file, leaving the target untouched. It is a
// no-op after Commit and safe to call more than once.
func (w *AtomicWriter) Abort() {
	if w.done {
		return
	}
	w.done = true
	_ = w.tmp.Close()
	_ = os.Remove(w.tmpName)
}

// syncDir fsyncs a directory so that a preceding rename or create within it
// is durable. Some platforms (Windows) and filesystems (FUSE, 9p, network
// mounts) do not support directory fsync; such an error is logged and treated
// as success since there is nothing more the caller can do.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if isDirSyncUnsupported(err) {
			debuglog.Write("ioutil/write", "directory fsync unsupported; rename durability not guaranteed", "fs-dirsync-unsupported", map[string]any{
				"dir": dir,
				"err": err.Error(),
			})
			return nil
		}
		return err
	}
	return nil
}

// isDirSyncUnsupported reports whether a directory fsync failure is due to the
// platform or filesystem not supporting the operation rather than a real I/O
// error. Windows returns ERROR_ACCESS_DENIED (FlushFileBuffers on a directory
// handle) and some filesystems (e.g. overlayfs) return EINVAL.
func isDirSyncUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EINVAL) {
		return true
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		if containsAny(strings.ToLower(pe.Err.Error()), "not supported", "not implemented", "operation not supported", "access is denied", "incorrect function", "invalid argument") {
			return true
		}
	}
	return containsAny(strings.ToLower(err.Error()), "not supported", "not implemented", "operation not supported", "access is denied", "incorrect function", "invalid argument")
}

// isChmodUnsupported reports whether a chmod failure is a "not supported"
// error we should ignore rather than propagate. Chmod can return ENOTSUP,
// ENOSYS, EOPNOTSUPP, or Windows ERROR_INVALID_FUNCTION on some filesystems
// (FUSE, 9p, network mounts) where mode bits aren't tracked.
func isChmodUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
		return true
	}
	// Fallback: Windows ERROR_INVALID_FUNCTION or other platform-specific
	// errors that don't map to standard POSIX codes.
	var pe *os.PathError
	if errors.As(err, &pe) {
		s := pe.Err.Error()
		if containsAny(s, "not supported", "not implemented", "operation not supported") {
			return true
		}
	}
	s := err.Error()
	return containsAny(s, "not supported", "not implemented", "operation not supported")
}

// containsAny reports whether s contains any of the given substrings.
// Empty substrings never match (strings.Contains("", "") is true, so the
// empty case is skipped explicitly to preserve the historical behavior).
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
