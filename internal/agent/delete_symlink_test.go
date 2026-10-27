package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowDelete is a delete approver that always says yes, for the symlink tests.
func allowDelete(ctx context.Context) context.Context {
	return ContextWithDeleteApprover(ctx, func(context.Context, DeleteRequest) (bool, error) {
		return true, nil
	})
}

// TestDeleteFileUnlinksSymlinkNotTarget pins S5: deleting a symlink removes the
// LINK. Resolving the path first (the old behaviour) deleted the file it
// pointed at and left the link dangling over a hole.
func TestDeleteFileUnlinksSymlinkNotTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("real.txt", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	exec := NewExecutor(dir)
	if _, err := exec.DeleteFile(allowDelete(context.Background()), "link.txt"); err != nil {
		t.Fatalf("delete link: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("link still present: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "keep me" {
		t.Fatalf("target damaged by deleting the link: %q, %v", body, err)
	}
}

// TestSecurePathNoFollowKeepsLinkName: the boundary still applies to the link's
// directory, but the name is not resolved.
func TestSecurePathNoFollowKeepsLinkName(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "target.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "out.txt")
	if err := os.Symlink(filepath.Join("sub", "target.txt"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	exec := NewExecutor(dir)
	got, err := exec.SecurePathNoFollow("out.txt")
	if err != nil {
		t.Fatalf("SecurePathNoFollow: %v", err)
	}
	if filepath.Base(got) != "out.txt" {
		t.Fatalf("no-follow path = %q, want the link name", got)
	}
	// The resolved variant still reports the target (that is its job).
	resolved, err := exec.SecurePath("out.txt")
	if err != nil {
		t.Fatalf("SecurePath: %v", err)
	}
	if !strings.HasSuffix(resolved, filepath.Join("sub", "target.txt")) {
		t.Fatalf("resolved path = %q, want the target", resolved)
	}
	// Escapes are still refused.
	if _, err := exec.SecurePathNoFollow("../outside.txt"); err == nil {
		t.Fatal("traversal outside the boundary must be refused")
	}
	// A path whose parent escapes the workspace is refused (the traversal is
	// resolved against the working dir, so only real escapes count).
	if _, err := exec.SecurePathNoFollow("sub/../../../outside.txt"); err == nil {
		t.Fatal("traversal outside the boundary must be refused")
	}
}

// TestPatchDeleteUnlinksSymlink covers the patch_file delete path.
func TestPatchDeleteUnlinksSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	diff := "--- a/link.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-keep me\n"
	exec := NewExecutor(dir)
	if _, err := exec.PatchFile(allowDelete(context.Background()), diff, false, false); err != nil {
		t.Fatalf("patch delete of a symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link.txt")); !os.IsNotExist(err) {
		t.Fatalf("link still present after patch delete: %v", err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "keep me\n" {
		t.Fatalf("target damaged by patch delete of the link: %q, %v", body, err)
	}
}
