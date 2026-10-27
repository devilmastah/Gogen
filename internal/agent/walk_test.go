package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWalkReadableFilesSkipsSymlinks pins the symlink containment in the walk
// policy. filepath.WalkDir lstats, so a symlink entry looks tiny and passed the
// old size check; the readers then followed it via os.ReadFile, which let a
// link inside the workspace read (and, via rename/replace, rewrite) a file
// outside the boundary.
func TestWalkReadableFilesSkipsSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("real content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "leakdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	e := NewExecutor(dir)
	var visited []string
	err := e.walkReadableFiles(context.Background(), dir, "", "", nil, false,
		func(path, rel string, content []byte) error {
			visited = append(visited, rel)
			if strings.Contains(string(content), "TOPSECRET") {
				t.Fatalf("walker read through a symlink: %s", rel)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(visited) != 1 || visited[0] != "real.txt" {
		t.Fatalf("visited = %v, want [real.txt]", visited)
	}
}

// TestWalkRejectsNonRegularFiles pins the other half of the same predicate:
// devices (and FIFOs, sockets) are not read, so a link/entry pointing at one
// cannot be streamed until memory runs out.
func TestWalkRejectsNonRegularFiles(t *testing.T) {
	info, err := os.Stat(os.DevNull)
	if err != nil {
		t.Skipf("no %s on this platform: %v", os.DevNull, err)
	}
	if info.Mode().IsRegular() {
		t.Skipf("%s is a regular file here", os.DevNull)
	}
	if searchableWalkFile(os.DevNull, info, nil) {
		t.Fatalf("%s accepted by the walker", os.DevNull)
	}
}

// TestReadFileBoundedEnforcesCap pins the read-side bound: the stat check can
// go stale (or not apply to the content at all), so the read itself stops at
// the search cap instead of loading the file.
func TestReadFileBoundedEnforcesCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, make([]byte, searchMaxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileBounded(path); err == nil {
		t.Fatal("a file over the cap must not be read")
	}
}

// TestReadFileBoundedRejectsBinary pins the NUL probe: the walkers feed regex
// and tree-sitter scans, so a binary blob (no symlink needed) is skipped
// instead of being scanned as text.
func TestReadFileBoundedRejectsBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob.bin")
	if err := os.WriteFile(path, []byte("text\x00more"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileBounded(path); err == nil {
		t.Fatal("a file containing a NUL byte must be skipped")
	}
}
