package spill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"gogen/internal/contextmgr"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return NewStore(dir), dir
}

// TestDirKeepsSessionIDFindable pins the findability contract: a generated
// session id (session.NewID() = 32 lowercase hex chars) must land in a dir
// named after itself — .gogen/spill/session-<id> mirrors
// .gogen/sessions/<id>.json so a user can map spilled output to its session
// by eye.
func TestDirKeepsSessionIDFindable(t *testing.T) {
	s, dir := newTestStore(t)
	id := "3f9a1c2d5e6b7a8c9d0e1f2a3b4c5d6e"
	want := filepath.Join(dir, ".gogen", "spill", "session-"+id)
	if got := s.Dir(id); got != want {
		t.Fatalf("Dir = %q, want %q (raw id must stay in the name)", got, want)
	}
	if again := s.Dir(id); again != want {
		t.Fatalf("Dir not deterministic: %q vs %q", want, again)
	}
	if s.Dir("other-id") == want {
		t.Fatal("different session ids mapped to the same dir")
	}
}

// TestDirSanitizesUnsafeSessionID pins the defense-in-depth: ids reaching
// the agent through SetSessionID are arbitrary strings (hand-edited
// snapshots, tests, hosts); they must sanitize to a single safe element one
// level under the spill root, while staying recognizable.
func TestDirSanitizesUnsafeSessionID(t *testing.T) {
	s, dir := newTestStore(t)
	root := filepath.Join(dir, ".gogen", "spill")
	tests := []struct {
		name     string
		id       string
		wantBase string
	}{
		{"path traversal", "../../etc", "session-.._.._etc"},
		{"separators", "a/b\\c", "session-a_b_c"},
		{"spaces", "my session", "session-my_session"},
		{"empty", "", "session-unnamed"},
		{"dot-dot", "..", "session-unnamed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Dir(tc.id)
			if filepath.Dir(got) != root {
				t.Fatalf("Dir = %q, want exactly one level under %q", got, root)
			}
			if filepath.Base(got) != tc.wantBase {
				t.Fatalf("base = %q, want %q", filepath.Base(got), tc.wantBase)
			}
		})
	}
	// Very long host-supplied ids are capped so they cannot blow up path
	// length limits.
	long := strings.Repeat("x", 500)
	if got := filepath.Base(s.Dir(long)); len(got) > len("session-")+120 {
		t.Fatalf("dir base = %d chars, want capped at %d", len(got), len("session-")+120)
	}
}

func TestNewStoreEmptyWorkingDir(t *testing.T) {
	if NewStore("") != nil {
		t.Fatal("NewStore(\"\") must return nil (spilling unavailable)")
	}
}

func TestSaveWritesFullContentWithPrivatePerms(t *testing.T) {
	s, _ := newTestStore(t)
	want := strings.Repeat("out\n", 1000)
	path, err := s.Save("sess-1", "execute_command", []byte(want))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("path = %q, want absolute", path)
	}
	if base := filepath.Base(path); !strings.HasPrefix(base, "execute_command-") || !strings.HasSuffix(base, ".log") {
		t.Fatalf("file name = %q, want <label>-<rand>.log", base)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("spilled content differs: got %d bytes, want %d", len(data), len(want))
	}
	// 0600 file / 0700 dir: no group or other access can ever be granted
	// (the modes carry no bits for umask to clear either). Windows does not
	// honor POSIX mode bits (Stat reports 0666/0777), so this is POSIX-only.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file perm = %o, want 600", perm)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Fatalf("session dir perm = %o, want 700", perm)
		}
	}
}

func TestSaveSanitizesLabel(t *testing.T) {
	s, _ := newTestStore(t)
	path, err := s.Save("sess-1", "../../etc/pass wd", []byte("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if base := filepath.Base(path); strings.ContainsAny(base, "/\\ ") {
		t.Fatalf("label not sanitized: %q", base)
	}
	// The file must be inside the session dir, not escaped from it.
	if filepath.Dir(path) != s.Dir("sess-1") {
		t.Fatalf("file escaped the session dir: %q", path)
	}
}

func TestSaveRemovesPartialFileOnError(t *testing.T) {
	s, dir := newTestStore(t)
	// Plant a file that MkdirAll cannot traverse: make the spill root a
	// regular file so creating the session dir fails.
	root := filepath.Join(dir, ".gogen", "spill")
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("sess-1", "tool", []byte("x")); err == nil {
		t.Fatal("expected Save to fail when the spill root is a file")
	}
	// No partial session dir may appear next to the blocking file (stat
	// may legitimately fail with ENOTDIR because the parent is a file —
	// only an existing entry is a failure).
	if info, err := os.Stat(s.Dir("sess-1")); err == nil {
		t.Fatalf("partial session dir left behind: %v", info)
	}
}

func TestTargetRefusesPlantedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	s, _ := newTestStore(t)
	// Plant a symlink at the exact target name: the O_EXCL open must fail
	// (never follow the link), the sticky error must disable the target,
	// and the planted file must keep its original content.
	tgt := s.NewTarget("sess-1", "fixed")
	tgt.name = "fixed.log" // deterministic name for the plant
	sessionDir := s.Dir("sess-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(sessionDir, "fixed.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.Write([]byte("spilled")); err == nil {
		t.Fatal("expected the write over a planted symlink to fail")
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "original" {
		t.Fatalf("planted symlink was followed: data=%q err=%v", data, err)
	}
	// Sticky failure: later writes must not silently open a new file.
	if _, err := tgt.Write([]byte("more")); err == nil {
		t.Fatal("expected the sticky spill error to persist")
	}
	if path, _, ok := tgt.Finish(); ok {
		t.Fatalf("Finish reported a spill file at %q despite the failed open", path)
	}
}

func TestTargetLazyOpenAndFinish(t *testing.T) {
	s, _ := newTestStore(t)
	tgt := s.NewTarget("sess-1", "tool")
	if tgt.Path() != "" {
		t.Fatalf("Path = %q before any write, want empty", tgt.Path())
	}
	if _, _, ok := tgt.Finish(); ok {
		t.Fatal("Finish must report ok=false when nothing was written")
	}
	// Nothing on disk: a producer that never overflows creates no files.
	if entries, _ := os.ReadDir(s.Dir("sess-1")); len(entries) != 0 {
		t.Fatalf("lazy target created files: %v", entries)
	}
	// A used target: two writes, then Finish; Finish is idempotent.
	tgt2 := s.NewTarget("sess-1", "tool")
	if _, err := tgt2.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt2.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	path, total, ok := tgt2.Finish()
	if !ok || total != 2 {
		t.Fatalf("Finish = (%q, %d, %v), want ok with 2 bytes", path, total, ok)
	}
	// Finish is idempotent by value: the same (path, total, ok) again.
	if path2, total2, ok2 := tgt2.Finish(); !ok2 || path2 != path || total2 != total {
		t.Fatalf("Finish not idempotent: (%q, %d, %v), want (%q, %d, true)", path2, total2, ok2, path, total)
	}
}

func TestPreviewBudgetAndMarkerContract(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		max         int
		wantLocator bool // false: cap too small to hold the full locator
	}{
		{"typical cap", strings.Repeat("0123456789\n", 2000), 262144, true},
		{"small cap", strings.Repeat("x", 1000), 200, true},
		{"tiny cap", strings.Repeat("x", 500), 60, false},
		{"multi-byte runes", strings.Repeat("日本語のテキスト", 300), 1024, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Preview(tc.content, tc.max, "/tmp/spill/session/f.log", int64(len(tc.content)))
			if len(got) > tc.max {
				t.Fatalf("preview is %d bytes, exceeds cap %d", len(got), tc.max)
			}
			if !contextmgr.HasTruncationMarker(got) {
				t.Fatalf("preview lost the standard marker prefix: %q", head(got, 80))
			}
			if !utf8.ValidString(got) {
				t.Fatal("preview is not valid UTF-8")
			}
			if !tc.wantLocator {
				return
			}
			if !strings.Contains(got, "/tmp/spill/session/f.log") {
				t.Fatal("preview lost the spill path locator")
			}
			if !strings.Contains(got, "read_file") || !strings.Contains(got, "search_code") {
				t.Fatal("preview lost the retrieval hint")
			}
		})
	}
}

func TestPreviewKeepsHeadAndTail(t *testing.T) {
	content := "HEAD-MARKER\n" + strings.Repeat("m\n", 500) + "TAIL-MARKER"
	// 400 bytes: comfortably above the locator, so head and tail budgets
	// are non-zero and both ends must appear around the locator.
	got := Preview(content, 400, "/s/f.log", int64(len(content)))
	if !strings.Contains(got, "HEAD-MARKER") {
		t.Fatalf("preview dropped the head: %q", got)
	}
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Fatalf("preview dropped the tail: %q", got)
	}
}

// TestPreviewEscapesPercentInPath pins the format-pass escape: the locator
// embeds the spill path, and TruncateHeadTail Sprintf-formats any marker
// containing '%' (its dropped-bytes contract) — so a '%' in the working
// dir's path must be escaped to a literal, or the format pass mangles the
// locator path ("proj%s" becomes "%!s(int=N)") and the model's read_file
// retrieval points at a file that does not exist.
func TestPreviewEscapesPercentInPath(t *testing.T) {
	content := "HEAD-MARKER\n" + strings.Repeat("m\n", 500) + "TAIL-MARKER"
	path := filepath.Join(t.TempDir(), "proj%s", "50%d", ".gogen", "spill",
		"session-x", "exec_command-abc.log")
	got := Preview(content, 512, path, int64(len(content)))
	if !strings.Contains(got, path) {
		t.Fatalf("locator lost the literal path: %q", got)
	}
	if strings.Contains(got, "%!") {
		t.Fatalf("locator path was format-mangled: %q", got)
	}
	if len(got) > 512 {
		t.Fatalf("preview is %d bytes, exceeds cap 512", len(got))
	}
	// The locator must stay machine-parseable: fork-time repointing scans
	// previews with LocatorPaths.
	if parsed := LocatorPaths(got); len(parsed) != 1 || parsed[0] != path {
		t.Fatalf("LocatorPaths = %q, want [%s]", parsed, path)
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestFinishDiscardsPartialFileOnError pins the mid-stream failure
// contract: a target that wrote some bytes and then hit an error must not
// advertise a locator to a partial file — Finish removes the file and
// reports ok=false, so the caller falls back to the plain truncation
// marker (which tells the truth: the tail is gone).
func TestFinishDiscardsPartialFileOnError(t *testing.T) {
	s, _ := newTestStore(t)
	tgt := s.NewTarget("sess-1", "tool")
	if _, err := tgt.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	tgt.mu.Lock()
	tgt.err = fmt.Errorf("simulated ENOSPC")
	tgt.mu.Unlock()
	path, _, ok := tgt.Finish()
	if ok {
		t.Fatal("failed target must report ok=false")
	}
	if path != "" {
		t.Fatal("failed target must not report a path")
	}
	entries, err := os.ReadDir(s.Dir("sess-1"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("partial spill file left behind: %v", entries)
	}
}

func TestPreviewStreamedReadsTailFromDisk(t *testing.T) {
	s, _ := newTestStore(t)
	headText := strings.Repeat("H", 8000)
	tailText := "FINAL-LINE-42"
	full := headText + strings.Repeat("m", 100000) + "\n" + tailText
	path, err := s.Save("sess-1", "cmd", []byte(full))
	if err != nil {
		t.Fatal(err)
	}
	got := PreviewStreamed(headText, 4096, path, int64(len(full)))
	if len(got) > 4096 {
		t.Fatalf("streamed preview is %d bytes, exceeds cap 4096", len(got))
	}
	if !strings.Contains(got, tailText) {
		t.Fatal("streamed preview did not retrieve the tail from disk")
	}
	if !strings.Contains(got, "H") {
		t.Fatal("streamed preview lost the head")
	}
	if int64(len(full)) == 0 || !strings.Contains(got, "full output saved to") {
		t.Fatal("streamed preview lost the locator")
	}
}

func TestPreviewStreamedUnreadableTailFallsBackToHead(t *testing.T) {
	got := PreviewStreamed(strings.Repeat("H", 5000), 1000, filepath.Join(t.TempDir(), "missing.log"), 99999)
	if len(got) > 1000 {
		t.Fatalf("preview is %d bytes, exceeds cap 1000", len(got))
	}
	if !strings.Contains(got, "full output saved to") {
		t.Fatal("locator lost when the tail read fails")
	}
	if !strings.Contains(got, "H") {
		t.Fatal("head lost when the tail read fails")
	}
}

// TestGlobalRootOverrideAlignsEveryPath pins the global-mode contract:
// with a process-wide root set, the store, the session-dir cleanup and the
// fork repoint all resolve the SAME directory (a mismatch would orphan spill
// trees on delete and dangle child locators). Clearing it restores the
// project-local layout.
func TestGlobalRootOverrideAlignsEveryPath(t *testing.T) {
	proj := t.TempDir()
	global := t.TempDir()
	t.Cleanup(func() { SetGlobalRoot("") })

	SetGlobalRoot(global)
	s := NewStore(proj)
	want := filepath.Join(global, "session-sess-1")
	if got := s.Dir("sess-1"); got != want {
		t.Fatalf("Dir in global mode = %q, want %q", got, want)
	}
	path, err := s.Save("sess-1", "tool", []byte("global spill"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(path)) != global {
		t.Fatalf("spill file %q is not under the global root %q", path, global)
	}
	if _, err := os.Stat(filepath.Join(proj, ".gogen")); !os.IsNotExist(err) {
		t.Fatalf("global mode wrote into the project dir (stat err = %v)", err)
	}
	// The cleanup hook and the fork repoint use the same root.
	if err := RemoveSessionDir(proj, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("RemoveSessionDir missed the global session dir (stat err = %v)", err)
	}
	src, err := s.Save("sess-2", "tool", []byte("repoint me"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := RepointFile(proj, src, "sess-3")
	if err != nil {
		t.Fatalf("RepointFile in global mode: %v", err)
	}
	if filepath.Dir(dst) != filepath.Join(global, "session-sess-3") {
		t.Fatalf("repointed path = %q, want the global target dir", dst)
	}
	// A path under the PROJECT root is rejected while global mode is on.
	SetGlobalRoot("")
	if _, err := RepointFile(proj, src, "sess-4"); err == nil {
		t.Fatal("project-root path must be rejected once the global root is cleared")
	}
	if got := NewStore(proj).Dir("sess-1"); got != filepath.Join(proj, ".gogen", "spill", "session-sess-1") {
		t.Fatalf("project-mode Dir after clearing the global root = %q", got)
	}
}

// TestRemoveSessionDir pins the session-store lifecycle hook: the whole
// spill tree of one session goes, other sessions' dirs stay, and removing
// a missing dir (delete of a never-spilled session) is a no-op. An empty
// id is a no-op too — it must never sweep the shared "unnamed" dir.
func TestRemoveSessionDir(t *testing.T) {
	s, dir := newTestStore(t)
	for _, sid := range []string{"sess-1", "sess-2"} {
		if _, err := s.Save(sid, "tool", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveSessionDir(dir, "sess-1"); err != nil {
		t.Fatalf("RemoveSessionDir: %v", err)
	}
	if _, err := os.Stat(s.Dir("sess-1")); !os.IsNotExist(err) {
		t.Fatalf("spill dir survived removal (stat err = %v)", err)
	}
	if _, err := os.Stat(s.Dir("sess-2")); err != nil {
		t.Fatalf("unrelated session's spill dir was removed: %v", err)
	}
	// Idempotent + empty-id no-op.
	if err := RemoveSessionDir(dir, "sess-1"); err != nil {
		t.Fatalf("re-remove of missing dir = %v, want nil", err)
	}
	if err := RemoveSessionDir(dir, ""); err != nil {
		t.Fatalf("RemoveSessionDir(\"\") = %v, want nil", err)
	}
}

func TestLocatorPaths(t *testing.T) {
	loc := func(p string) string { return locatorLine(p, 1234) }
	s := "before\n" + loc("/a/b.log") + "\nmiddle\n" + loc("/c/d.log") +
		"\nunterminated full output saved to /no-tail-here"
	got := LocatorPaths(s)
	if len(got) != 2 || got[0] != "/a/b.log" || got[1] != "/c/d.log" {
		t.Fatalf("LocatorPaths = %q, want [/a/b.log /c/d.log]", got)
	}
	if got := LocatorPaths("no locators here"); len(got) != 0 {
		t.Fatalf("LocatorPaths(locator-free) = %q, want none", got)
	}
}

// TestLocatorPathsRequiresTheRealShape pins the strictness the spill
// salvage decision depends on: only a line locatorLine actually writes
// counts as a locator. Text that merely quotes the marker phrase — this
// package's own sources, docs, a model echoing the format, a log — must not
// make an oversized result look like an already-spilled preview (that would
// silently downgrade it to a head-only cut).
func TestLocatorPathsRequiresTheRealShape(t *testing.T) {
	real := locatorLine("/tmp/spill/session-x/tool-abcd.log", 4096)
	if got := LocatorPaths(real); len(got) != 1 {
		t.Fatalf("real locator not parsed: %q -> %q", real, got)
	}
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"no prologue count", "\n… truncated (bytes total; full output saved to /tmp/a.log — use read_file (offset/limit)"},
		{"non-numeric count", "\n… truncated (many bytes total; full output saved to /tmp/a.log — use read_file (offset/limit)"},
		{"no marker prefix", "… total; full output saved to /tmp/a.log — use read_file …"},
		{"bare phrase", "full output saved to /tmp/a.log — use read_file"},
		{"quoted source line", `marker := "\n… truncated (%d bytes total; %s%s%s (offset/limit) or search_code"`},
		{"missing tail", "\n… truncated (12 bytes total; full output saved to /tmp/a.log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LocatorPaths(tc.in); len(got) != 0 {
				t.Fatalf("non-locator text parsed as a locator: %q -> %q", tc.in, got)
			}
		})
	}
}

// TestRewriteLocatorsTargetsParsedSpans pins the fork-repoint contract: the
// rewrite replaces the locator's OWN bytes, even when the same path text
// appears earlier in the result (a first-match replace would have rewritten
// that copy and left the real locator dangling after the parent's spill dir
// was deleted).
func TestRewriteLocatorsTargetsParsedSpans(t *testing.T) {
	oldPath := "/w/.gogen/spill/session-a/tool-1.log"
	newPath := "/w/.gogen/spill/session-b/tool-1.log"
	content := "cmd printed " + oldPath + " in passing\n" + locatorLine(oldPath, 10) + "\ntail"
	got := RewriteLocators(content, func(p string) (string, bool) {
		if p != oldPath {
			t.Fatalf("resolve got %q, want the parsed path", p)
		}
		return newPath, true
	})
	// The prose copy stays; the locator's copy moves.
	if !strings.HasPrefix(got, "cmd printed "+oldPath+" in passing\n") {
		t.Fatalf("prose copy of the path was rewritten: %q", got)
	}
	if !strings.Contains(got, "full output saved to "+newPath) {
		t.Fatalf("locator was not repointed: %q", got)
	}
	if strings.Contains(got, "full output saved to "+oldPath) {
		t.Fatalf("old locator survived: %q", got)
	}
	if paths := LocatorPaths(got); len(paths) != 1 || paths[0] != newPath {
		t.Fatalf("LocatorPaths after rewrite = %q, want [%s]", paths, newPath)
	}
	// A declined resolve keeps the original text; no locators is a no-op.
	if got := RewriteLocators(content, func(string) (string, bool) { return "", false }); got != content {
		t.Fatalf("declined rewrite changed the content: %q", got)
	}
	if got := RewriteLocators("plain text", func(string) (string, bool) { return "x", true }); got != "plain text" {
		t.Fatalf("locator-free rewrite = %q, want unchanged", got)
	}
}

// TestTargetDiscardRemovesFile pins the discard contract used when nobody
// consumes a spill target (a write failure, or the cap changing mid-command):
// the file — partial or complete — is closed and removed, and the target can
// never advertise a path afterwards. A dropped reference (the pre-fix
// behavior) left the file on disk and its descriptor open until the GC.
func TestTargetDiscardRemovesFile(t *testing.T) {
	s, _ := newTestStore(t)
	tgt := s.NewTarget("sess-discard", "tool")
	if _, err := tgt.Write([]byte("partial output")); err != nil {
		t.Fatal(err)
	}
	path := tgt.Path()
	if path == "" {
		t.Fatal("target did not open its file")
	}
	tgt.Discard()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("discarded spill file still on disk (stat err = %v)", err)
	}
	if tgt.Path() != "" {
		t.Fatalf("Path = %q after Discard, want empty", tgt.Path())
	}
	if p, _, ok := tgt.Finish(); ok || p != "" {
		t.Fatalf("Finish after Discard = (%q, ok=%v), want no file", p, ok)
	}
	// Idempotent, and a discard of a never-written target is a no-op.
	tgt.Discard()
	unused := s.NewTarget("sess-discard", "tool")
	unused.Discard()
	if entries, err := os.ReadDir(s.Dir("sess-discard")); err != nil || len(entries) != 0 {
		t.Fatalf("discard left files behind: %v err=%v", entries, err)
	}
	// A write after Discard is refused (the target is closed for good).
	if _, err := tgt.Write([]byte("more")); err == nil {
		t.Fatal("write after Discard must fail")
	}
}

// TestRepointFile pins the fork-repoint primitive: the file gains a second
// directory entry in the target session's dir (same content), repointing
// is idempotent, and the data survives deleting the SOURCE entry — the
// fork-then-delete-original scenario at the file level. Paths outside the
// spill root are rejected so locator text in message content can never
// trick a fork into linking arbitrary files.
func TestRepointFile(t *testing.T) {
	s, dir := newTestStore(t)
	src, err := s.Save("sess-a", "execute_command", []byte("shared output"))
	if err != nil {
		t.Fatal(err)
	}
	dst, err := RepointFile(dir, src, "sess-b")
	if err != nil {
		t.Fatalf("RepointFile: %v", err)
	}
	if filepath.Dir(dst) != s.Dir("sess-b") {
		t.Fatalf("dst = %q, want inside %q", dst, s.Dir("sess-b"))
	}
	if filepath.Base(dst) != filepath.Base(src) {
		t.Fatalf("dst base = %q, want the same file name", filepath.Base(dst))
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "shared output" {
		t.Fatalf("dst content = %q err %v", data, err)
	}
	if runtime.GOOS != "windows" { // Windows does not honor POSIX mode bits
		if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("dst perm = %v, want 600", info)
		}
	}
	// Idempotent: repointing again returns the same path.
	again, err := RepointFile(dir, src, "sess-b")
	if err != nil || again != dst {
		t.Fatalf("re-repoint = (%q, %v), want (%q, nil)", again, err, dst)
	}
	// THE SCENARIO: removing the source entry leaves the target's data.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(dst)
	if err != nil || string(data) != "shared output" {
		t.Fatalf("dst broke after source removal: %q err %v", data, err)
	}
	// Outside the spill root: rejected.
	if _, err := RepointFile(dir, filepath.Join(dir, "elsewhere.txt"), "sess-b"); err == nil {
		t.Fatal("repoint of a path outside the spill root must fail")
	}
	// Missing source under the root: fails, keeps the caller's original.
	if _, err := RepointFile(dir, filepath.Join(s.Dir("sess-a"), "nope.log"), "sess-b"); err == nil {
		t.Fatal("repoint of a missing file must fail")
	}
	// Degenerate args.
	if _, err := RepointFile("", src, "sess-b"); err == nil {
		t.Fatal("empty working dir must fail")
	}
}

func TestContextTargetRoundTrip(t *testing.T) {
	ctx := context.Background()
	if TargetFromContext(ctx) != nil {
		t.Fatal("empty context must carry no target")
	}
	tgt := (&Store{root: "/x"}).NewTarget("s", "t")
	if got := TargetFromContext(ContextWithTarget(ctx, tgt)); got != tgt {
		t.Fatalf("TargetFromContext = %v, want the attached target", got)
	}
}

// TestTargetCapBoundsSpillGrowth pins the size cap: a command that streams
// forever must not fill the disk, since the spill exists precisely because the
// output does not fit in memory.
func TestTargetCapBoundsSpillGrowth(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	tgt := st.NewTarget("sess", "cap")
	chunk := make([]byte, 1<<20)
	// Write past the cap in 1 MiB chunks; the write that crosses it must fail
	// instead of silently growing.
	var err error
	for i := 0; i < int(MaxTargetBytes/(1<<20))+2; i++ {
		if _, werr := tgt.Write(chunk); werr != nil {
			err = werr
			break
		}
	}
	if err == nil {
		t.Fatal("writes past MaxTargetBytes were accepted")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("error = %v, want a size-cap message", err)
	}
	if _, total, ok := tgt.Finish(); ok && total > MaxTargetBytes {
		t.Fatalf("spilled %d bytes, cap is %d", total, MaxTargetBytes)
	}
}
