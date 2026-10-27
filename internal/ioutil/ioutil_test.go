package ioutil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeErr is a test-only error type for exercising the string-matching fallback.
type fakeErr struct{ msg string }

func (e fakeErr) Error() string { return e.msg }

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	content := []byte("hello world")

	if err := WriteFileAtomic(path, content, 0644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch: got %q, want %q", got, content)
	}
}

func TestWriteFileAtomicCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "test.txt")
	content := []byte("nested")

	if err := WriteFileAtomic(path, content, 0644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch: got %q, want %q", got, content)
	}
}

func TestWriteFileAtomicPreservesPermissions(t *testing.T) {
	// Skip on Windows where chmod is best-effort.
	if os.PathSeparator == '\\' {
		t.Skip("chmod not reliable on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "script.sh")

	// Write initial file with execute permission.
	if err := WriteFileAtomic(path, []byte("#!/bin/sh"), 0755); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Overwrite — should preserve 0755.
	if err := WriteFileAtomic(path, []byte("#!/bin/sh\necho hi"), 0644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := info.Mode().Perm()
	if got != 0755 {
		t.Fatalf("permissions not preserved: got %o, want 0755", got)
	}
}

func TestAtomicWriterCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	w, err := NewAtomicWriter(path, 0644)
	if err != nil {
		t.Fatal(err)
	}
	var total int
	for _, chunk := range []string{"hello ", "streaming ", "world"} {
		n, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
		total += n
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// A second Commit must be a harmless no-op.
	if err := w.Commit(); err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello streaming world" {
		t.Fatalf("content mismatch: got %q", got)
	}
	if len(got) != total {
		t.Fatalf("size mismatch: got %d, want %d", len(got), total)
	}
}

func TestAtomicWriterAbortLeavesTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	w, err := NewAtomicWriter(path, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	w.Abort()
	w.Abort() // idempotent

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("Abort modified the target: got %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gogen-write-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestAtomicWriterPreservesPermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("chmod not reliable on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh"), 0755); err != nil {
		t.Fatal(err)
	}
	w, err := NewAtomicWriter(path, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("#!/bin/sh\necho hi")); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0755 {
		t.Fatalf("permissions not preserved: got %o, want 0755", got)
	}
}

func TestWriteFileAtomicNoSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nosync.txt")
	content := []byte("no sync content")

	if err := WriteFileAtomicNoSync(path, content, 0644); err != nil {
		t.Fatalf("WriteFileAtomicNoSync: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch: got %q, want %q", got, content)
	}
}

func TestWriteFileAtomicEmptyContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	if err := WriteFileAtomic(path, []byte{}, 0644); err != nil {
		t.Fatalf("WriteFileAtomic empty: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty file, got %q", got)
	}
}

func TestWriteFileAtomicLargeContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.bin")
	content := make([]byte, 10*1024*1024) // 10 MB
	content[len(content)-1] = 0xFF

	if err := WriteFileAtomic(path, content, 0644); err != nil {
		t.Fatalf("WriteFileAtomic large: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != len(content) {
		t.Fatalf("size mismatch: got %d, want %d", len(got), len(content))
	}
	if got[len(got)-1] != 0xFF {
		t.Fatal("content corrupted at end")
	}
}

func TestWriteFileAtomicCleansUpOnWriteFailure(t *testing.T) {
	// Directory write bits are not enforced the same way on Windows, and root
	// can bypass DAC on Linux — skip when the probe write still succeeds.
	if os.PathSeparator == '\\' {
		t.Skip("directory write bits not enforced on Windows")
	}
	dir := t.TempDir()
	roDir := filepath.Join(dir, "readonly")
	if err := os.MkdirAll(roDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0755) })
	path := filepath.Join(roDir, "test.txt")

	err := WriteFileAtomic(path, []byte("fail"), 0644)
	if err == nil {
		t.Skip("read-only directory still writable (e.g. running as root)")
	}
	entries, readErr := os.ReadDir(roDir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gogen-write-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteFileAtomicInvalidDir(t *testing.T) {
	// Parent path is a regular file, so MkdirAll must fail on every OS.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "child.txt")
	err := WriteFileAtomic(path, []byte("fail"), 0644)
	if err == nil {
		t.Fatal("expected error for invalid dir")
	}
}

func TestIsChmodUnsupported(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "permission denied",
			err:  os.ErrPermission,
			want: false,
		},
		{
			name: "not found",
			err:  os.ErrNotExist,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isChmodUnsupported(tt.err)
			if got != tt.want {
				t.Errorf("isChmodUnsupported(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsChmodUnsupportedStringFallback(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"not supported", true},
		{"not implemented", true},
		{"operation not supported", true},
		{"permission denied", false},
		{"file not found", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			err := &os.PathError{Op: "chmod", Path: "x", Err: fakeErr{msg: tt.msg}}
			got := isChmodUnsupported(err)
			if got != tt.want {
				t.Errorf("isChmodUnsupported(PathError{msg=%q}) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestIsDirSyncUnsupported(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "permission denied",
			err:  os.ErrPermission,
			want: false,
		},
		{
			name: "not supported syscall",
			err:  syscall.ENOTSUP,
			want: true,
		},
		{
			name: "invalid argument syscall",
			err:  syscall.EINVAL,
			want: true,
		},
		{
			name: "windows access denied string fallback",
			err:  &os.PathError{Op: "sync", Path: "x", Err: fakeErr{"Access is denied."}},
			want: true,
		},
		{
			name: "wrong category of error",
			err:  os.ErrNotExist,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDirSyncUnsupported(tt.err)
			if got != tt.want {
				t.Errorf("isDirSyncUnsupported(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestContainsAny(t *testing.T) {
	tests := []struct {
		name    string
		s       string
		substrs []string
		want    bool
	}{
		{
			name:    "exact match",
			s:       "hello world",
			substrs: []string{"world"},
			want:    true,
		},
		{
			name:    "partial match",
			s:       "hello world",
			substrs: []string{"wor"},
			want:    true,
		},
		{
			name:    "no match",
			s:       "hello world",
			substrs: []string{"foo"},
			want:    false,
		},
		{
			name:    "empty substr",
			s:       "hello",
			substrs: []string{""},
			want:    false,
		},
		{
			name:    "empty string",
			s:       "",
			substrs: []string{"a"},
			want:    false,
		},
		{
			name:    "multiple substrings first match",
			s:       "hello world",
			substrs: []string{"foo", "world"},
			want:    true,
		},
		{
			name:    "multiple substrings no match",
			s:       "hello world",
			substrs: []string{"foo", "bar"},
			want:    false,
		},
		{
			name:    "substring longer than string",
			s:       "hi",
			substrs: []string{"hello"},
			want:    false,
		},
		{
			name:    "case sensitive",
			s:       "Hello",
			substrs: []string{"hello"},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsAny(tt.s, tt.substrs...)
			if got != tt.want {
				t.Errorf("containsAny(%q, %v) = %v, want %v", tt.s, tt.substrs, got, tt.want)
			}
		})
	}
}

// TestAtomicWriterFollowsSymlink pins the symlinked-config fix: writing through
// a symlink must update the TARGET and keep the link, not replace the link with
// a regular file (which silently forked a dotfile-managed config).
func TestAtomicWriterFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles-config.yaml")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteFileAtomic(link, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic through a symlink: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a regular file (mode %v, err %v)", fi.Mode(), err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "new" {
		t.Fatalf("target content = %q (%v), want the new bytes", body, err)
	}
}
