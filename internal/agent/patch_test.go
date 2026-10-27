package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gogen/internal/llm"
)

func TestParseDiffLineCountRejectsNegative(t *testing.T) {
	for _, part := range []string{"-1", "-1,2"} {
		if _, err := parseDiffLineCount(part); err == nil {
			t.Fatalf("expected error for %q", part)
		}
	}
}

func TestParseDiffLineCountAllowsZeroForNewFiles(t *testing.T) {
	got, err := parseDiffLineCount("0,0")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("got %d want 0", got)
	}
}

func TestParseDiffLineCountAcceptsPositive(t *testing.T) {
	got, err := parseDiffLineCount("5,3")
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("got %d want 5", got)
	}
}

func TestParseUnifiedDiffKeepsBlankContextLines(t *testing.T) {
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n" +
		" package main\n" +
		"\n" + // bare blank line (LLM style) should become empty context
		"+// comment\n" +
		" func main() {\n" +
		" }\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d", len(files), len(files[0].hunks))
	}
	h := files[0].hunks[0]
	if len(h.oldLines) != 4 {
		t.Fatalf("oldLines=%d want 4 (blank context kept): %#v", len(h.oldLines), h.oldLines)
	}
	if h.oldLines[1] != "" {
		t.Fatalf("expected empty context line, got %q", h.oldLines[1])
	}
	if len(h.newLines) != 5 {
		t.Fatalf("newLines=%d want 5: %#v", len(h.newLines), h.newLines)
	}
}

// TestPureInsertionHunkLandsAfterTheNamedLine pins git's zero-old-count
// convention end to end: "@@ -2,0 +3,1 @@" inserts AFTER line 2, so the new
// line becomes the third. Parsing plus applying must agree with patch(1);
// the old start = oldStart-1 arithmetic inserted one line early.
func TestPureInsertionHunkLandsAfterTheNamedLine(t *testing.T) {
	diff := "--- a/file.txt\n+++ b/file.txt\n@@ -2,0 +3,1 @@\n+inserted\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%v", len(files), files)
	}
	got, _, _, err := applyPatchHunks([]string{"a", "b", "c"}, files[0].hunks, false, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "inserted", "c"}
	if !linesEqual(got, want) {
		t.Fatalf("lines = %#v, want %#v", got, want)
	}
}

func TestParseUnifiedDiffAcceptsCompactHunkHeader(t *testing.T) {
	diff := "--- a/main.go\n+++ b/main.go\n@@-1,2 +1,3@@\n package main\n+// x\n func main() {\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%v", len(files), files)
	}
	if files[0].hunks[0].oldStart != 1 {
		t.Fatalf("oldStart=%d", files[0].hunks[0].oldStart)
	}
}

func TestParseUnifiedDiffGitStyleMultiFile(t *testing.T) {
	diff := "" +
		"diff --git a/a.txt b/a.txt\n" +
		"index 111..222 100644\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"\n" +
		"diff --git a/b.txt b/b.txt\n" +
		"--- a/b.txt\n" +
		"+++ b/b.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" alpha\n" +
		"+beta\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d want 2: %+v", len(files), files)
	}
	if normalizePatchPath(files[0].newName) != "a.txt" || normalizePatchPath(files[1].newName) != "b.txt" {
		t.Fatalf("paths=%q %q", files[0].newName, files[1].newName)
	}
	if len(files[0].hunks) != 1 || len(files[0].hunks[0].oldLines) != 1 {
		t.Fatalf("first hunk corrupted: %#v", files[0].hunks)
	}
}

func TestNormalizePatchPathStripsTimestampsAndQuotes(t *testing.T) {
	cases := map[string]string{
		"a/foo.txt\t2024-01-01 12:00:00.000000000 +0000": "foo.txt",
		"b/foo.txt 2024-01-01 12:00:00":                  "foo.txt",
		`"b/foo bar.txt"`:                                "foo bar.txt",
		"a/foo.txt":                                      "foo.txt",
		// Exactly one prefix is stripped: a file literally named
		// "b/foo.txt" appears as "a/b/foo.txt" and must resolve to
		// "b/foo.txt", not "foo.txt".
		"a/b/foo.txt": "b/foo.txt",
	}
	for in, want := range cases {
		got := normalizePatchPath(in)
		if got != want {
			t.Fatalf("normalizePatchPath(%q)=%q want %q", in, got, want)
		}
	}
}

// TestPlanPatchDevNullTimestampDelete pins the /dev/null detection fix:
// git appends a tab-timestamp to delete headers ("+++ /dev/null\t2024-…"),
// which must still classify the section as a delete.
func TestPlanPatchDevNullTimestampDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	plan, label, err := exec.planPatch(patchFile{
		oldName: "a/old.txt",
		newName: "/dev/null\t2024-01-01 12:00:00.000000000 +0000",
	}, false)
	if err != nil {
		t.Fatalf("planPatch: %v", err)
	}
	if !plan.delete || plan.target != "old.txt" {
		t.Fatalf("plan = %+v, want delete of old.txt", plan)
	}
	if label != "old.txt (validated delete)" {
		t.Fatalf("label = %q", label)
	}
}

// TestPlanPatchDevNullTimestampCreate pins the same fix for the create side:
// "--- /dev/null\t2024-…" must classify the section as a create.
func TestPlanPatchDevNullTimestampCreate(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	plan, label, err := exec.planPatch(patchFile{
		oldName: "/dev/null\t2024-01-01 12:00:00.000000000 +0000",
		newName: "b/new.txt",
	}, false)
	if err != nil {
		t.Fatalf("planPatch: %v", err)
	}
	if !plan.create || plan.target != "new.txt" {
		t.Fatalf("plan = %+v, want create of new.txt", plan)
	}
	if label != "new.txt (validated create)" {
		t.Fatalf("label = %q", label)
	}
}

// TestParseHunkHeaderRejectsDegenerate pins the hardening: a header whose
// sides carry no line numbers ("@@ - + @@") must fail loudly instead of
// silently defaulting to line 1/count 1 and mis-positioning the hunk.
func TestParseHunkHeaderRejectsDegenerate(t *testing.T) {
	for _, h := range []string{"@@ - + @@", "@@- +@@", "@@ -1,2 + @@"} {
		if _, err := parseHunkHeader(h); err == nil {
			t.Fatalf("parseHunkHeader(%q) succeeded, want error", h)
		}
	}
}

func TestApplyPatchHunksFuzzyPastEOF(t *testing.T) {
	original := []string{"package main", "", "func main() {", "}"}
	hunks := []patchHunk{{
		oldStart: 20, // stale line number well past EOF
		oldLines: []string{"func main() {", "}"},
		newLines: []string{"func main() {", "\t// hi", "}"},
	}}
	_, _, _, err := applyPatchHunks(original, hunks, false, true)
	if err == nil {
		t.Fatal("expected strict apply to fail with stale line numbers")
	}
	got, _, shifts, err := applyPatchHunks(original, hunks, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[3] != "\t// hi" {
		t.Fatalf("got %#v", got)
	}
	if len(shifts) != 1 {
		t.Fatalf("expected 1 hunk shift, got %d: %v", len(shifts), shifts)
	}
}

func TestSchemaPatchExampleParsesCleanly(t *testing.T) {
	// Keep in sync with the single-file example in tools.go.
	diff := "--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n \n+// new comment\n func main() {\n }\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("parse failed: %+v", files)
	}
	h := files[0].hunks[0]
	if len(h.oldLines) != 4 || len(h.newLines) != 5 {
		t.Fatalf("old=%d new=%d: %#v %#v", len(h.oldLines), len(h.newLines), h.oldLines, h.newLines)
	}
}

func TestParseUnifiedDiffIgnoresTrailingEndPatchMarker(t *testing.T) {
	// Regression test: some models append a "*** End Patch" delimiter after
	// the final hunk of the diff argument, which used to fail the whole patch
	// with "malformed hunk line" (see session 99973f2fb…).
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package main\n" +
		"+// comment\n" +
		" func main() {\n" +
		" }\n" +
		"*** End Patch\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parse with trailing marker: %v", err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d, want 1 and 1", len(files), len(files[0].hunks))
	}
	h := files[0].hunks[0]
	if len(h.oldLines) != 3 || len(h.newLines) != 4 {
		t.Fatalf("old=%d new=%d, want 3 and 4: %#v %#v", len(h.oldLines), len(h.newLines), h.oldLines, h.newLines)
	}
	if h.newLines[1] != "// comment" {
		t.Fatalf("added line = %q, want %q", h.newLines[1], "// comment")
	}
}

func TestParseUnifiedDiffIgnoresMarkerVariants(t *testing.T) {
	markers := []string{
		"*** End Patch",
		"*** End Diff",
		"***endpatch",
		"***EndPatch",
		"*** END OF PATCH",
		"*** End of Patch ***",
		"*** Start Patch",
		"*** End of hunk ***",
		"***",
	}
	for _, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			diff := "--- a/main.go\n+++ b/main.go\n@@ -1,2 +1,2 @@\n package main\n+// x\n" + marker + "\n"
			files, err := parseUnifiedDiff(diff)
			if err != nil {
				t.Fatalf("parse with marker %q: %v", marker, err)
			}
			if len(files) != 1 || len(files[0].hunks) != 1 {
				t.Fatalf("files=%d hunks=%d, want 1 and 1", len(files), len(files[0].hunks))
			}
			if len(files[0].hunks[0].newLines) != 2 {
				t.Fatalf("newLines=%d want 2: %#v", len(files[0].hunks[0].newLines), files[0].hunks[0].newLines)
			}
		})
	}
}

func TestParseUnifiedDiffMarkerEndsPatch(t *testing.T) {
	// A "*** End Patch" marker between file sections terminates the patch:
	// only the section before the marker is applied. Sections after the
	// marker are dropped.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"*** End Patch\n" +
		"--- a/b.txt\n" +
		"+++ b/b.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" alpha\n" +
		"+beta\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1 (marker ends the patch)", len(files))
	}
	if normalizePatchPath(files[0].newName) != "a.txt" {
		t.Fatalf("path=%q want a.txt", files[0].newName)
	}
	if len(files[0].hunks) != 1 || len(files[0].hunks[0].oldLines) != 1 {
		t.Fatalf("hunk corrupted: %#v", files[0].hunks)
	}
}

func TestParseUnifiedDiffEndMarkersTerminatePatch(t *testing.T) {
	// Models sometimes close a diff with repeated "*** End of file" trailers
	// and stray text after them. The first marker must terminate the patch;
	// everything after it (repeated markers, prose) is ignored rather than
	// absorbed as hunk content or failing with "malformed hunk line".
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"*** End of file\n" +
		"*** End of file\n" +
		"*** End of file\n" +
		"stray text after the marker would otherwise be a malformed hunk line\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d want 1 and 1", len(files), len(files[0].hunks))
	}
	if len(files[0].hunks[0].newLines) != 2 {
		t.Fatalf("newLines=%d want 2: %#v", len(files[0].hunks[0].newLines), files[0].hunks[0].newLines)
	}
}

func TestParseUnifiedDiffRepeatedEndPatchTrailer(t *testing.T) {
	// Exact regression for a reported model output: a two-hunk patch against
	// internal/server/workingdir_test.go followed by ~100 repeated
	// "*** End Patch" lines. The FIRST marker must terminate the patch; the
	// repeated trailers must not fail the parse, be absorbed as hunk content,
	// or produce extra file sections.
	diff := "" +
		"--- a/internal/server/workingdir_test.go\n" +
		"+++ b/internal/server/workingdir_test.go\n" +
		"@@ -1,6 +1,7 @@\n" +
		" package server\n" +
		"\n" +
		" import (\n" +
		"+\t\"fmt\"\n" +
		" \t\"strings\"\n" +
		" \t\"testing\"\n" +
		" \t\"time\"\n" +
		"@@ -87,3 +88,56 @@ func TestWorkingDirChangeRequiresGlobalMode(t *testing.T) {\n" +
		" \t})\n" +
		" }\n" +
		"+\n" +
		"+// TestApplyWorkingDirToAllSkipsBusySession verifies the working-dir sweep\n" +
		"+// uses a BOUNDED wait per session: a session whose turn is running (turnMu\n" +
		"+// held, e.g. stuck in a tool) must be skipped and reported instead of\n" +
		"+// blocking the sweep forever on that session's lock. The idle sessions still\n" +
		"+// move to the new directory.\n" +
		"+func TestApplyWorkingDirToAllSkipsBusySession(t *testing.T) {\n" +
		"+\toldDir := t.TempDir()\n" +
		"+\tnewDir := t.TempDir()\n" +
		"+\tprov := llm.NewMockProvider()\n" +
		"+\texec := agent.NewExecutor(oldDir)\n" +
		"+\tctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})\n" +
		"+\ta := agent.NewAgent(prov, exec, ctxMgr)\n" +
		"+\ta.GlobalMode = true\n" +
		"+\tstore := session.NewStore(true)\n" +
		"+\ta.SessionStore = store\n" +
		"+\ts := NewServer(a, &config.Config{})\n" +
		"+\n" +
		"+\t// Register a second session whose \"turn\" is running (turnMu held).\n" +
		"+\ta2 := s.ws.NewSessionAgent(nil, \"busy-sess\")\n" +
		"+\trt2 := newSessionRuntime(a2)\n" +
		"+\ts.registry.register(\"busy-sess\", rt2)\n" +
		"+\trt2.turnMu.Lock() // simulate a running/stuck turn\n" +
		"+\tdefer rt2.turnMu.Unlock()\n" +
		"+\n" +
		"+\tdone := make(chan []string, 1)\n" +
		"+\tgo func() {\n" +
		"+\t\tdone <- s.applyWorkingDirToAll(newDir)\n" +
		"+\t}()\n" +
		"+\n" +
		"+\tvar skipped []string\n" +
		"+\tselect {\n" +
		"+\tcase skipped = <-done:\n" +
		"+\tcase <-time.After(5 * time.Second):\n" +
		"+\t\tt.Fatal(\"applyWorkingDirToAll did not return within 5s (blocking lock on a busy session)\")\n" +
		"+\t}\n" +
		"+\tif len(skipped) != 1 || skipped[0] != \"busy-sess\" {\n" +
		"+\t\tt.Fatalf(\"skipped=%v want [busy-sess]\", skipped)\n" +
		"+\t}\n" +
		"+\n" +
		"+\t// The busy session's agent must NOT be mutated without its lock...\n" +
		"+\tif a2.WorkingDir != oldDir {\n" +
		"+\t\tt.Fatalf(\"busy agent WorkingDir=%q want %q (mutated without its turn lock)\", a2.WorkingDir, oldDir)\n" +
		"+\t}\n" +
		"+\t// ...while the idle (default) session moved to the new directory.\n" +
		"+\tdef := s.registry.first()\n" +
		"+\tif def == nil || def.agent.WorkingDir != newDir {\n" +
		"+\t\tt.Fatalf(\"default agent WorkingDir=%q want %q\", def.agent.WorkingDir, newDir)\n" +
		"+\t}\n" +
		"+\n" +
		"+\t// The skip report message names the busy session.\n" +
		"+\tmsg := workingDirSkipMessage(newDir, skipped)\n" +
		"+\tif !strings.Contains(msg, \"busy-sess\") || !strings.Contains(msg, newDir) {\n" +
		"+\t\tt.Fatalf(\"skip message = %q, want busy-sess and %s\", msg, newDir)\n" +
		"+\t}\n" +
		"+\t_ = fmt.Sprintf // keep fmt imported for future assertions\n" +
		" }\n" +
		"*** End Patch\n" +
		strings.Repeat("*** End Patch\n", 100)

	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parse with repeated end markers: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1", len(files))
	}
	if filepath.ToSlash(normalizePatchPath(files[0].newName)) != "internal/server/workingdir_test.go" {
		t.Fatalf("path=%q", files[0].newName)
	}
	hunks := files[0].hunks
	if len(hunks) != 2 {
		t.Fatalf("hunks=%d want 2", len(hunks))
	}
	// First hunk: the fmt import addition.
	h0 := hunks[0]
	if h0.oldStart != 1 || len(h0.oldLines) != 6 || len(h0.newLines) != 7 {
		t.Fatalf("hunk 1 = oldStart %d, old %d, new %d; want 1/6/7",
			h0.oldStart, len(h0.oldLines), len(h0.newLines))
	}
	if h0.newLines[3] != "\t\"fmt\"" {
		t.Fatalf("hunk 1 added line = %q", h0.newLines[3])
	}
	// Second hunk: the busy-session test; the final context line must not be
	// lost to the marker.
	h1 := hunks[1]
	if h1.oldStart != 87 || len(h1.oldLines) != 3 || len(h1.newLines) != 59 {
		t.Fatalf("hunk 2 = oldStart %d, old %d, new %d; want 87/3/59",
			h1.oldStart, len(h1.oldLines), len(h1.newLines))
	}
	if h1.oldLines[0] != "\t})" || h1.oldLines[1] != "}" || h1.oldLines[2] != "}" {
		t.Fatalf("hunk 2 oldLines=%#v", h1.oldLines)
	}
	if h1.newLines[len(h1.newLines)-1] != "}" {
		t.Fatalf("hunk 2 final line = %q, want context %q", h1.newLines[len(h1.newLines)-1], "}")
	}
}

func TestParseUnifiedDiffMarkerWithNoOpenHunkEndsPatch(t *testing.T) {
	// A "*** End Patch" marker seen while NO hunk is open (here: wedged
	// between a file's +++ header and its hunk) must still terminate the
	// patch. Before the fix the marker was only honored inside a hunk body,
	// so the b.txt section after the marker was parsed and applied.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"\n" +
		"--- a/b.txt\n" +
		"+++ b/b.txt\n" +
		"*** End Patch\n" +
		"@@ -1,1 +1,2 @@\n" +
		" alpha\n" +
		"+beta\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1 (marker with no open hunk must end the patch)", len(files))
	}
	if normalizePatchPath(files[0].newName) != "a.txt" {
		t.Fatalf("path=%q want a.txt (b.txt after the marker must be dropped)", files[0].newName)
	}
	if len(files[0].hunks) != 1 || len(files[0].hunks[0].newLines) != 2 {
		t.Fatalf("a.txt hunk corrupted: %#v", files[0].hunks)
	}
}

func TestParseUnifiedDiffStartMarkerPreambleSkipped(t *testing.T) {
	// A "*** Start Patch" line before the first file header is a preamble and
	// must not terminate an otherwise valid patch.
	diff := "" +
		"*** Start Patch\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d want 1 and 1", len(files), len(files[0].hunks))
	}
	if len(files[0].hunks[0].newLines) != 2 {
		t.Fatalf("newLines=%d want 2: %#v", len(files[0].hunks[0].newLines), files[0].hunks[0].newLines)
	}
}

func TestParseUnifiedDiffContextFormatRangeHeadersRejected(t *testing.T) {
	// diff -c range headers ("*** 16,20 ***", "*** 104,110 ****") are NOT
	// delimiter markers: they mean the model switched to context format, and
	// the patch must keep failing loudly — with a format-specific message —
	// instead of silently dropping diff structure (regression guard for the
	// marker-tolerance fix).
	for _, header := range []string{"*** 16,20 ***", "*** 104,110 ****", "--- 30,34 ----"} {
		diff := "--- a/main.go\n+++ b/main.go\n@@ -1,2 +1,2 @@\n package main\n" + header + "\n+// x\n"
		_, err := parseUnifiedDiff(diff)
		if err == nil {
			t.Fatalf("expected context-format header %q to be rejected, got nil", header)
		}
		if !strings.Contains(err.Error(), "context format") || !strings.Contains(err.Error(), "unified format") {
			t.Fatalf("expected context-format error for %q, got: %v", header, err)
		}
	}
}

func TestParseUnifiedDiffContextFormatSectionRejected(t *testing.T) {
	// A full context-format diff (diff -c) must fail with the format error,
	// not the confusing "no patches found in diff": the "*** path" file
	// header is skipped as preamble, "--- path" parses as a file header,
	// and the "***************" separator signals the format switch.
	diff := "" +
		"*** main.go\t2024-01-01 12:00:00\n" +
		"--- main.go\t2024-01-01 12:00:00\n" +
		"***************\n" +
		"*** 30,34 ****\n" +
		"  line1\n" +
		"- removed\n" +
		"--- 30,34 ----\n" +
		"  line1\n" +
		"+ added\n"
	_, err := parseUnifiedDiff(diff)
	if err == nil {
		t.Fatal("expected context-format diff to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "context format") {
		t.Fatalf("expected context-format error, got: %v", err)
	}

	// The bare separator alone must also fail loudly (mixed formats).
	mixed := "--- a/main.go\n+++ b/main.go\n@@ -1,2 +1,2 @@\n package main\n***************\n"
	_, err = parseUnifiedDiff(mixed)
	if err == nil {
		t.Fatal("expected context-format separator to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "context format") {
		t.Fatalf("expected context-format error, got: %v", err)
	}
}

func TestParseUnifiedDiffContextFormatHeaderInHeaderStateRejected(t *testing.T) {
	// A context-format range header between file sections used to be
	// skipped as stray text (then "no patches found in diff"); it must now
	// fail with the format-specific message.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"*** 30,34 ****\n" +
		"--- a/b.txt\n" +
		"+++ b/b.txt\n"
	_, err := parseUnifiedDiff(diff)
	if err == nil {
		t.Fatal("expected context-format range header to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "context format") {
		t.Fatalf("expected context-format error, got: %v", err)
	}
}

func TestParseUnifiedDiffContextHeaderLookalikesStayUnified(t *testing.T) {
	// Lines that merely look like context-format constructs must keep
	// parsing as unified content: delimiter markers with letters, added
	// "***" lines, a file literally named "30,34" (no trailing "----"),
	// and star runs under the 15-star separator length.
	diff := "" +
		"--- a/notes.md\n" +
		"+++ b/notes.md\n" +
		"@@ -1,2 +1,3 @@\n" +
		" title\n" +
		"+*** horizontal rule\n" +
		" 30,34\n" +
		"*** End Patch\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("lookalikes must not be rejected: %v", err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d want 1 and 1", len(files), len(files[0].hunks))
	}
	h := files[0].hunks[0]
	if len(h.newLines) != 3 || h.newLines[1] != "*** horizontal rule" {
		t.Fatalf("newLines=%#v", h.newLines)
	}

	// "--- 30,34" (no trailing "----") is a file header for a file named
	// "30,34", not a context-format new-side header.
	lookalike := "--- 30,34\n+++ b/30,34\n@@ -1,1 +1,1 @@\n-x\n"
	files, err = parseUnifiedDiff(lookalike)
	if err != nil {
		t.Fatalf("--- header without ---- must stay a file header: %v", err)
	}
	if len(files) != 1 || files[0].oldName != "30,34" {
		t.Fatalf("oldName=%q want 30,34", files[0].oldName)
	}
}

func TestParseUnifiedDiffIgnoresTrailingCodeFence(t *testing.T) {
	// Some models close the diff with a bare markdown fence ("```"); the
	// parser must ignore it like other delimiter markers.
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package main\n" +
		"+// comment\n" +
		" func main() {\n" +
		" }\n" +
		"```\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parse with trailing fence: %v", err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d, want 1 and 1", len(files), len(files[0].hunks))
	}
	h := files[0].hunks[0]
	if len(h.newLines) != 4 || h.newLines[1] != "// comment" {
		t.Fatalf("newLines=%#v", h.newLines)
	}
}

func TestParseUnifiedDiffPrefixedFenceStaysContent(t *testing.T) {
	// A space-prefixed " ```" is a real hunk context line and must NOT be
	// treated as a closing fence.
	diff := "--- a/notes.md\n+++ b/notes.md\n@@ -1,2 +1,2 @@\n title\n ```\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	h := files[0].hunks[0]
	if len(h.oldLines) != 2 || h.oldLines[1] != "```" {
		t.Fatalf("oldLines=%#v", h.oldLines)
	}
	if len(h.newLines) != 2 || h.newLines[1] != "```" {
		t.Fatalf("newLines=%#v", h.newLines)
	}
}

func TestParseUnifiedDiffPrefixedStarLinesStayContent(t *testing.T) {
	// A "***" line that carries a hunk prefix must NOT be treated as a
	// delimiter marker — it is real added/context content.
	diff := "" +
		"--- a/notes.md\n" +
		"+++ b/notes.md\n" +
		"@@ -1,2 +1,3 @@\n" +
		" title\n" +
		"+*** horizontal rule\n" +
		" body\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	h := files[0].hunks[0]
	if len(h.newLines) != 3 || h.newLines[1] != "*** horizontal rule" {
		t.Fatalf("newLines=%#v", h.newLines)
	}
	if len(h.oldLines) != 2 || h.oldLines[0] != "title" {
		t.Fatalf("oldLines=%#v", h.oldLines)
	}
}

func TestPatchFileIgnoresTrailingEndPatchMarker(t *testing.T) {
	// End-to-end: a patch whose diff argument ends with "*** End Patch"
	// must apply cleanly instead of failing with "malformed hunk line".
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n" +
		" package main\n" +
		"\n" +
		"+// comment\n" +
		" func main() {\n" +
		" }\n" +
		"*** End Patch\n"
	exec := NewExecutor(dir)
	if _, err := exec.PatchFile(context.Background(), diff, false, true); err != nil {
		t.Fatalf("PatchFile with trailing marker: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "package main\n\n// comment\nfunc main() {\n}\n"
	if string(data) != want {
		t.Fatalf("content = %q, want %q", string(data), want)
	}
}

func TestPatchFileIgnoresTrailingCodeFence(t *testing.T) {
	// End-to-end: a diff argument closed with a bare "```" fence must apply
	// cleanly instead of failing with "malformed hunk line".
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,4 +1,5 @@\n" +
		" package main\n" +
		"\n" +
		"+// comment\n" +
		" func main() {\n" +
		" }\n" +
		"```\n"
	exec := NewExecutor(dir)
	if _, err := exec.PatchFile(context.Background(), diff, false, true); err != nil {
		t.Fatalf("PatchFile with trailing fence: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "package main\n\n// comment\nfunc main() {\n}\n"
	if string(data) != want {
		t.Fatalf("content = %q, want %q", string(data), want)
	}
}

func TestParseUnifiedDiffBlankSeparatedFiles(t *testing.T) {
	// Blank lines between file sections must act as separators, not empty
	// hunk context. Regression guard for the precomputed boundary lookup
	// (computeBoundaryAhead) that replaced the per-blank-line forward scan.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"\n" + // blank separator between file sections
		"--- a/b.txt\n" +
		"+++ b/b.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" alpha\n" +
		"+beta\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d want 2", len(files))
	}
	for i, f := range files {
		if len(f.hunks) != 1 || len(f.hunks[0].oldLines) != 1 {
			t.Fatalf("file %d hunk corrupted: %#v", i, f.hunks)
		}
	}
}

func TestParseUnifiedDiffTrailingBlanksAreBoundary(t *testing.T) {
	// A run of trailing blank lines after the last hunk must not be absorbed
	// as empty context lines (they terminate the hunk).
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"\n\n\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d want 1 and 1", len(files), len(files[0].hunks))
	}
	h := files[0].hunks[0]
	if len(h.oldLines) != 1 || len(h.newLines) != 2 {
		t.Fatalf("oldLines=%d newLines=%d, want 1 and 2: %#v", len(h.oldLines), len(h.newLines), h)
	}
}

func TestApplyPatchHunksFuzzyAmbiguousRelocationRefused(t *testing.T) {
	// Two blocks that only match under whitespace tolerance, with stale line
	// numbers pointing past EOF: fuzzy relocation must refuse to guess and
	// fail loudly instead of silently editing the wrong block.
	original := []string{"func one() {   ", "}", "func one() {   ", "}"}
	hunks := []patchHunk{{
		oldStart: 20, // stale line number well past EOF
		oldLines: []string{"func one() {", "}"},
		newLines: []string{"func one() {", "\t// hi", "}"},
	}}
	_, _, _, err := applyPatchHunks(original, hunks, true, true)
	if err == nil {
		t.Fatal("expected ambiguous fuzzy relocation to fail")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got: %v", err)
	}

	// The same hunk against a single matching block still relocates cleanly.
	single := []string{"func one() {   ", "}"}
	got, _, shifts, err := applyPatchHunks(single, hunks, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1] != "\t// hi" {
		t.Fatalf("got %#v", got)
	}
	if len(shifts) != 1 {
		t.Fatalf("expected 1 hunk shift, got %d: %v", len(shifts), shifts)
	}
}

func TestAppendShiftsWarnsBeforeSuccess(t *testing.T) {
	plans := []patchPlan{{
		target: "main.go",
		hunkShifts: []string{
			"hunk 1 shifted by +4 lines (expected around line 3, found at line 7)",
		},
	}}
	got := appendShifts("Applied patch to 1 file(s): main.go", plans)
	if !strings.HasPrefix(got, "Warning: 1 hunk(s) were relocated by fuzzy matching") {
		t.Fatalf("expected warning prefix, got: %q", got)
	}
	if !strings.Contains(got, "Applied patch to 1 file(s): main.go") {
		t.Fatalf("expected success message retained, got: %q", got)
	}
	if !strings.Contains(got, "hunk 1 shifted by +4 lines") {
		t.Fatalf("expected shift detail, got: %q", got)
	}
}

func TestPatchFileReportsRelocationWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	diff := "--- a/main.go\n+++ b/main.go\n@@ -20,2 +20,3 @@\n func main() {\n+// hi\n }\n"
	msg, err := exec.PatchFile(context.Background(), diff, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "Warning: 1 hunk(s) were relocated by fuzzy matching") {
		t.Fatalf("expected relocation warning in message, got: %q", msg)
	}
}

func TestPatchFailStreakSteersRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	a := NewAgent(nil, exec, nil)

	badDiff := "--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package wrong\n\n+// x\n func main() {\n }\n"
	goodDiff := "--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n\n+// x\n func main() {\n }\n"
	patch := func(diff string) error {
		_, err := a.executeTool(context.Background(), llm.ToolCall{
			Name: "patch_file",
			Args: map[string]any{"diff": diff},
		})
		return err
	}

	if err := patch(badDiff); err == nil {
		t.Fatal("expected first patch to fail")
	}
	if err := patch(badDiff); err == nil {
		t.Fatal("expected second patch to fail")
	} else if !strings.Contains(err.Error(), "failed 2 times in a row") {
		t.Fatalf("expected streak hint on 2nd consecutive failure, got: %v", err)
	}
	if err := patch(goodDiff); err != nil {
		t.Fatalf("expected success patch to apply: %v", err)
	}
	if err := patch(badDiff); err == nil {
		t.Fatal("expected post-success patch to fail")
	} else if strings.Contains(err.Error(), "times in a row") {
		t.Fatalf("streak should reset after a successful patch, got: %v", err)
	}
}

func TestParseUnifiedDiffSingleTrailingBlankIsBoundary(t *testing.T) {
	// Regression: a diff ending with exactly ONE blank line after the last
	// hunk line must terminate the hunk, exactly like a run of two or more
	// trailing blanks (TestParseUnifiedDiffTrailingBlanksAreBoundary). The
	// boundary precompute used to leave the past-the-end slot false, so the
	// single trailing blank was absorbed as an empty context line and the
	// patch failed to apply against a file without a trailing blank line.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d want 1 and 1", len(files), len(files[0].hunks))
	}
	h := files[0].hunks[0]
	if len(h.oldLines) != 1 || len(h.newLines) != 2 {
		t.Fatalf("trailing blank absorbed as context: old=%d new=%d: %#v", len(h.oldLines), len(h.newLines), h)
	}
}

func TestPatchFileSingleTrailingBlankApplies(t *testing.T) {
	// End-to-end: a diff argument closed with a single extra blank line must
	// apply cleanly (previously it failed with a phantom empty context line).
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"\n"
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	if _, err := exec.PatchFile(context.Background(), diff, false, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\ntwo\n" {
		t.Fatalf("content=%q want %q", string(got), "one\ntwo\n")
	}
}

func TestPatchFileSingleTrailingBlankDoesNotDeleteBlankAtEOF(t *testing.T) {
	// Regression: the phantom trailing blank used to be absorbed into the
	// hunk's OLD lines only when the hunk ended with a deletion, so applying
	// the patch also deleted the file's real trailing blank line at EOF. With
	// the boundary fix the blank terminates the hunk and the file's own
	// trailing blank is preserved.
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,2 +1,1 @@\n" +
		" one\n" +
		"-two\n" +
		"\n"
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	if _, err := exec.PatchFile(context.Background(), diff, false, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\n\n" {
		t.Fatalf("content=%q want %q (trailing blank must be preserved)", string(got), "one\n\n")
	}
}

func TestApplyPatchHunksPrefersFuzzyAtHintOverDistantExact(t *testing.T) {
	// Regression: the hunk header anchors at index 0 where the text differs
	// from the diff context only by trailing whitespace; the same text also
	// exists exactly at index 3. Fuzzy matching must prefer the whitespace-
	// tolerant match at the anchored position over relocating to the distant
	// exact copy — relocating silently edits the wrong block.
	original := []string{
		"func b() {   ", // anchored position: matches context only under trailing-trim
		"}",
		"",
		"func b() {", // exact match of the (clean) context, far from the anchor
		"}",
	}
	hunks := []patchHunk{{
		oldStart: 1,
		oldLines: []string{"func b() {", "}"},
		newLines: []string{"func b() {", "\t// changed", "}"},
	}}
	got, _, shifts, err := applyPatchHunks(original, hunks, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(shifts) != 0 {
		t.Fatalf("expected no relocation (fuzzy match at anchor), got shifts: %v", shifts)
	}
	// The hunk's context line replaces the matched text, normalizing the
	// trailing-space artifact on line 0; the distant exact copy is untouched.
	want := []string{"func b() {", "\t// changed", "}", "", "func b() {", "}"}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q want %q; full: %#v", i, got[i], want[i], got)
		}
	}
}

func TestApplyPatchHunksShiftMessageReportsActualLocation(t *testing.T) {
	// Regression: with a stale header line number (past EOF), the relocation
	// warning used to report "found at line" derived from the clamped hint
	// (e.g. expected 20, found 18) instead of the actual match position.
	original := []string{"package main", "", "func main() {", "}"}
	hunks := []patchHunk{{
		oldStart: 20,
		oldLines: []string{"func main() {", "}"},
		newLines: []string{"func main() {", "\t// hi", "}"},
	}}
	_, _, shifts, err := applyPatchHunks(original, hunks, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(shifts) != 1 {
		t.Fatalf("shifts=%v want 1", shifts)
	}
	if !strings.Contains(shifts[0], "expected around line 20") || !strings.Contains(shifts[0], "found at line 3") {
		t.Fatalf("misleading shift message: %q", shifts[0])
	}
}

func TestParseUnifiedDiffDuplicateFileHeaderSkipped(t *testing.T) {
	// Regression: a repeated ---/+++ header before the real hunk section is a
	// malformed zero-hunk section and must not fail the whole patch with
	// "no hunks found". The second section's hunks still parse and apply.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1 (duplicate header dropped)", len(files))
	}
	if len(files[0].hunks) != 1 || len(files[0].hunks[0].oldLines) != 1 {
		t.Fatalf("hunk corrupted: %#v", files[0].hunks)
	}
}

func TestPatchFileMissingPlusPlusHeaderFallsBackToOldName(t *testing.T) {
	// Regression: a patch that drops the +++ header (models sometimes do)
	// should target the file named in the --- header instead of failing with
	// "could not determine target file from diff headers".
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n"
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	if _, err := exec.PatchFile(context.Background(), diff, false, true); err != nil {
		t.Fatalf("patch without +++ header: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\ntwo\n" {
		t.Fatalf("content=%q want %q", string(got), "one\ntwo\n")
	}
}

func TestParseUnifiedDiffHunkLinesStartingWithDashesNotHeaders(t *testing.T) {
	// Regression: a hunk's REMOVED line whose content starts with "-- "
	// appears on the wire as "--- ..." and was misparsed as a new file
	// header, flushing (and silently dropping) the open hunk. Classic case:
	// deleting a SQL comment "-- foo".
	diff := "" +
		"--- a/schema.sql\n" +
		"+++ b/schema.sql\n" +
		"@@ -1,4 +1,3 @@\n" +
		" SELECT *\n" +
		"--- comment\n" +
		" FROM t\n" +
		" WHERE id = 1\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1 (the --- comment line must stay in the hunk): %#v", len(files), files)
	}
	h := files[0].hunks[0]
	want := []string{"SELECT *", "-- comment", "FROM t", "WHERE id = 1"}
	if len(h.oldLines) != len(want) {
		t.Fatalf("oldLines=%#v want %#v", h.oldLines, want)
	}
	for i, w := range want {
		if h.oldLines[i] != w {
			t.Fatalf("oldLines[%d]=%q want %q", i, h.oldLines[i], w)
		}
	}
	if len(h.newLines) != 3 {
		t.Fatalf("newLines=%#v want 3", h.newLines)
	}
}

func TestParseUnifiedDiffHunkLinesStartingWithPlusesNotHeaders(t *testing.T) {
	// Symmetric to the dashes case: an ADDED line whose content starts with
	// "++ " (wire "+++ ...") must not be swallowed as a +++ file header.
	diff := "" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,2 +1,3 @@\n" +
		" package main\n" +
		"+++ increment\n" +
		" func main() {\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].hunks) != 1 {
		t.Fatalf("files=%d hunks=%d want 1 file / 1 hunk", len(files), len(files[0].hunks))
	}
	h := files[0].hunks[0]
	if len(h.newLines) != 3 || h.newLines[1] != "++ increment" {
		t.Fatalf("newLines=%#v want added line %q", h.newLines, "++ increment")
	}
}

func TestParseUnifiedDiffHeaderAfterCompleteHunkWithoutBlank(t *testing.T) {
	// A second file's ---/+++ headers can follow the previous hunk with NO
	// blank separator (models often omit it). Once the first hunk has
	// consumed its declared counts, the next "--- " line is a file header,
	// not a removed line.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"--- a/b.txt\n" +
		"+++ b/b.txt\n" +
		"@@ -1,1 +1,1 @@\n" +
		" alpha\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d want 2: %#v", len(files), files)
	}
	if files[0].oldName != "a/a.txt" || files[1].oldName != "a/b.txt" {
		t.Fatalf("oldNames=%q %q", files[0].oldName, files[1].oldName)
	}
	if len(files[0].hunks) != 1 || len(files[1].hunks) != 1 {
		t.Fatalf("hunk counts %d %d want 1 1", len(files[0].hunks), len(files[1].hunks))
	}
}

func TestPatchFileRemovesLineStartingWithDoubleDash(t *testing.T) {
	// End-to-end: apply a patch that deletes a "-- comment" line (wire
	// "--- comment") and confirm the file is edited correctly.
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.sql")
	content := "SELECT *\n-- comment\nFROM t\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/schema.sql\n" +
		"+++ b/schema.sql\n" +
		"@@ -1,3 +1,2 @@\n" +
		" SELECT *\n" +
		"--- comment\n" +
		" FROM t\n"
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	if _, err := exec.PatchFile(context.Background(), diff, false, true); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "SELECT *\nFROM t\n" {
		t.Fatalf("content=%q want %q", string(got), "SELECT *\nFROM t\n")
	}
}

func TestParseUnifiedDiffUnderDeclaredHunkThenNewFile(t *testing.T) {
	// Regression: a hunk whose declared counts exceed its emitted lines (LLM
	// under-counting) never reaches hunkComplete, so a following file's
	// "--- " header with NO blank separator used to be absorbed as a removed
	// hunk line — the two files' hunks merged into one and the second file's
	// section was lost.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,5 +1,5 @@\n" +
		" one\n" +
		"-two\n" +
		"+two!\n" +
		" three\n" +
		"--- a/b.txt\n" +
		"+++ b/b.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" alpha\n" +
		"+beta\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d want 2: %#v", len(files), files)
	}
	if normalizePatchPath(files[0].newName) != "a.txt" || normalizePatchPath(files[1].newName) != "b.txt" {
		t.Fatalf("newNames=%q %q, want a.txt and b.txt", files[0].newName, files[1].newName)
	}
	if len(files[0].hunks) != 1 || len(files[1].hunks) != 1 {
		t.Fatalf("hunks=%d %d want 1 and 1", len(files[0].hunks), len(files[1].hunks))
	}
	h0 := files[0].hunks[0]
	if len(h0.oldLines) != 3 || h0.oldLines[1] != "two" {
		t.Fatalf("a.txt hunk corrupted: %#v", h0.oldLines)
	}
	h1 := files[1].hunks[0]
	if len(h1.oldLines) != 1 || h1.oldLines[0] != "alpha" {
		t.Fatalf("b.txt hunk corrupted: %#v", h1.oldLines)
	}
}

func TestPatchFileUnderDeclaredHunkThenNewFile(t *testing.T) {
	// End-to-end: the same under-declared + next-file layout applies cleanly
	// to BOTH files (previously the second file's section was merged into
	// the first and the patch failed or corrupted a.txt).
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(pathA, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,5 +1,5 @@\n" +
		" one\n" +
		"-two\n" +
		"+two!\n" +
		" three\n" +
		"--- a/b.txt\n" +
		"+++ b/b.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" alpha\n" +
		"+beta\n"
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	if _, err := exec.PatchFile(context.Background(), diff, false, true); err != nil {
		t.Fatalf("PatchFile: %v", err)
	}
	gotA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotA) != "one\ntwo!\nthree\n" {
		t.Fatalf("a.txt=%q want %q", string(gotA), "one\ntwo!\nthree\n")
	}
	gotB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotB) != "alpha\nbeta\n" {
		t.Fatalf("b.txt=%q want %q", string(gotB), "alpha\nbeta\n")
	}
}

func TestParseUnifiedDiffDashPlusContentPairStaysInHunk(t *testing.T) {
	// The header-pair lookahead must NOT fire for a deleted "-- X" line
	// immediately followed by an added "++ Y" line (wire "--- X" / "+++ Y")
	// when a hunk content line follows: both stay hunk lines, so the SQL
	// comment case and its "+" symmetric keep working even when adjacent.
	diff := "" +
		"--- a/schema.sql\n" +
		"+++ b/schema.sql\n" +
		"@@ -1,3 +1,3 @@\n" +
		" SELECT *\n" +
		"--- comment\n" +
		"+++ other\n" +
		" FROM t\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1: %#v", len(files), files)
	}
	h := files[0].hunks[0]
	want := []string{"SELECT *", "-- comment", "FROM t"}
	if len(h.oldLines) != len(want) {
		t.Fatalf("oldLines=%#v want %#v", h.oldLines, want)
	}
	for i, w := range want {
		if h.oldLines[i] != w {
			t.Fatalf("oldLines[%d]=%q want %q", i, h.oldLines[i], w)
		}
	}
	wantNew := []string{"SELECT *", "++ other", "FROM t"}
	if len(h.newLines) != len(wantNew) {
		t.Fatalf("newLines=%#v want %#v", h.newLines, wantNew)
	}
	for i, w := range wantNew {
		if h.newLines[i] != w {
			t.Fatalf("newLines[%d]=%q want %q", i, h.newLines[i], w)
		}
	}
}

func TestPatchFailStreakIgnoresNonMismatchErrors(t *testing.T) {
	// The retry-streak hint must only fire for diff-context failures: two
	// consecutive failures caused by a missing target file (an I/O error,
	// not a stale diff) must NOT produce "failed N times in a row".
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	a := NewAgent(nil, exec, nil)

	// The target file does not exist — planPatch fails with a read error.
	missingDiff := "--- a/missing.txt\n+++ b/missing.txt\n@@ -1,1 +1,2 @@\n one\n+two\n"
	patch := func(diff string) error {
		_, err := a.executeTool(context.Background(), llm.ToolCall{
			Name: "patch_file",
			Args: map[string]any{"diff": diff},
		})
		return err
	}

	if err := patch(missingDiff); err == nil {
		t.Fatal("expected first patch to fail")
	}
	if err := patch(missingDiff); err == nil {
		t.Fatal("expected second patch to fail")
	} else if strings.Contains(err.Error(), "times in a row") {
		t.Fatalf("I/O failures must not count toward the mismatch streak, got: %v", err)
	}

	// A real context mismatch still triggers the hint after the reset.
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	badDiff := "--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package wrong\n\n+// x\n func main() {\n }\n"
	if err := patch(badDiff); err == nil {
		t.Fatal("expected mismatch patch to fail")
	}
	if err := patch(badDiff); err == nil {
		t.Fatal("expected mismatch patch to fail again")
	} else if !strings.Contains(err.Error(), "failed 2 times in a row") {
		t.Fatalf("expected streak hint after two mismatches, got: %v", err)
	}
}

// TestParseUnifiedDiffDroppedOldHeader: a "+++ " header inside a hunk body
// (the next file's header with its "--- " dropped) must start a new file
// section instead of overwriting the current file's newName and merging the
// two files' hunks into one patchFile.
func TestParseUnifiedDiffDroppedOldHeader(t *testing.T) {
	diff := "" +
		"--- a/foo.txt\n" +
		"+++ b/foo.txt\n" +
		"@@ -1 +1 @@\n" +
		"-x\n" +
		"+y\n" +
		"+++ b/bar.txt\n" + // --- header for bar.txt was dropped
		"@@ -1 +1 @@\n" +
		"-a\n" +
		"+b\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 patchFiles, got %d: %+v", len(files), files)
	}
	if files[0].oldName != "a/foo.txt" || files[0].newName != "b/foo.txt" {
		t.Fatalf("file 0 headers wrong: %+v", files[0])
	}
	if files[1].oldName != "" || files[1].newName != "b/bar.txt" {
		t.Fatalf("file 1 headers wrong: %+v", files[1])
	}
	if len(files[0].hunks) != 1 || len(files[1].hunks) != 1 {
		t.Fatalf("expected one hunk per file, got %d and %d", len(files[0].hunks), len(files[1].hunks))
	}
}

func TestDetectMarkerOnlyDiff(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want bool
	}{
		{
			name: "repeated end markers only",
			diff: "*** End of diff\n*** End of diff\n*** End of diff\n",
			want: true,
		},
		{
			name: "end markers plus failure narration",
			diff: "*** End of diff\n" +
				"*** Wait — the patch did not fully apply cleanly:\n" +
				"*** The old hunk did not reach the end of the file\n" +
				"*** Removed one hunk and applied the remainder.\n" +
				"*** End of diff\n",
			want: true,
		},
		{
			name: "bare stars plus markers",
			diff: "***\n*** End of Patch\n***endpatch\n",
			want: true,
		},
		{
			name: "real diff with single trailing marker",
			diff: "--- a/foo.txt\n+++ b/foo.txt\n@@ -1 +1 @@\n-x\n+y\n*** End of Patch\n",
			want: false,
		},
		{
			name: "real diff with embedded loop transcript",
			diff: "--- a/foo.txt\n+++ b/foo.txt\n@@ -1 +1 @@\n-x\n+y\n*** End of diff\n" +
				"*** Wait — the patch did not fully apply cleanly:\n" +
				"*** Removed one hunk and applied the remainder.\n" +
				"*** End of diff\n",
			want: false, // has real content: polluted-but-valid diffs still apply
		},
		{
			name: "two markers only",
			diff: "*** End of diff\n*** End of diff\n",
			want: false,
		},
		{
			name: "start and end marker only",
			diff: "*** Start Patch\n*** End Patch\n",
			want: false, // start markers do not count, and <3 frames anyway
		},
		{
			name: "context format range headers are not markers",
			diff: "*** 30,34 ****\n*** 40,44 ***\n*** 50,54 ****\n",
			want: false,
		},
		{
			name: "crlf normalization",
			diff: "*** End of diff\r\n*** End of diff\r\n*** End of diff\r\n",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectMarkerOnlyDiff(tt.diff); got != tt.want {
				t.Fatalf("detectMarkerOnlyDiff() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPatchTurnStopMarkerOnlyDiff(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	a := NewAgent(nil, exec, nil)

	_, err := a.executeTool(context.Background(), llm.ToolCall{
		Name: "patch_file",
		Args: map[string]any{"diff": "*** End of diff\n*** End of diff\n*** End of diff\n"},
	})
	if !errors.Is(err, errPatchTurnStop) {
		t.Fatalf("marker-only diff must stop the turn with errPatchTurnStop, got: %v", err)
	}
	if !strings.Contains(err.Error(), "only patch markers") {
		t.Fatalf("expected marker-only explanation in error, got: %v", err)
	}

	// A polluted diff that still contains real content must NOT be stopped:
	// it applies (or fails) normally.
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = a.executeTool(context.Background(), llm.ToolCall{
		Name: "patch_file",
		Args: map[string]any{"diff": "--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n\n+// x\n func main() {\n }\n*** End of diff\n*** Wait — the patch did not fully apply cleanly:\n*** Removed one hunk and applied the remainder.\n"},
	})
	if errors.Is(err, errPatchTurnStop) {
		t.Fatalf("polluted-but-valid diff must not stop the turn, got: %v", err)
	}
	if err != nil {
		t.Fatalf("expected the polluted diff to apply, got: %v", err)
	}
}

func TestPatchTurnStopAfterThreeMismatches(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	a := NewAgent(nil, exec, nil)

	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Context mismatch: the hunk expects "package wrong" but the file has
	// "package main".
	badDiff := "--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package wrong\n\n+// x\n func main() {\n }\n"
	patch := func() error {
		_, err := a.executeTool(context.Background(), llm.ToolCall{
			Name: "patch_file",
			Args: map[string]any{"diff": badDiff},
		})
		return err
	}

	if err := patch(); err == nil || errors.Is(err, errPatchTurnStop) {
		t.Fatalf("strike 1 should be a normal mismatch error, got: %v", err)
	}
	if err := patch(); err == nil || errors.Is(err, errPatchTurnStop) {
		t.Fatalf("strike 2 should be a normal mismatch error, got: %v", err)
	}
	if err := patch(); !errors.Is(err, errPatchTurnStop) {
		t.Fatalf("strike 3 must stop the turn with errPatchTurnStop, got: %v", err)
	}

	// A success between failures resets the per-turn budget.
	if err := patch(); !errors.Is(err, errPatchTurnStop) {
		t.Fatalf("strike 3 still fires after budget exhausted, got: %v", err)
	}
	goodDiff := "--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n\n+// x\n func main() {\n }\n"
	if _, err := a.executeTool(context.Background(), llm.ToolCall{
		Name: "patch_file",
		Args: map[string]any{"diff": goodDiff},
	}); err != nil {
		t.Fatalf("successful patch should reset the budget, got: %v", err)
	}
	if err := patch(); err == nil || errors.Is(err, errPatchTurnStop) {
		t.Fatalf("after a success, strike 1 should be a normal mismatch error again, got: %v", err)
	}
}

func TestPatchTurnStopDifferentDiffsDoNotStop(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	a := NewAgent(nil, exec, nil)

	// Two files, two different failing diffs: the model is iterating, not
	// looping — failures on different targets must never accumulate toward
	// the turn stop.
	pathA := filepath.Join(dir, "a.go")
	if err := os.WriteFile(pathA, []byte("package a\n\nfunc A() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pathB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(pathB, []byte("package b\n\nfunc B() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	badDiffA := "--- a/a.go\n+++ b/a.go\n@@ -1,4 +1,5 @@\n package wrong\n\n+// x\n func A() {\n }\n"
	badDiffB := "--- a/b.go\n+++ b/b.go\n@@ -1,4 +1,5 @@\n package wrong\n\n+// x\n func B() {\n }\n"
	patch := func(diff string) error {
		_, err := a.executeTool(context.Background(), llm.ToolCall{
			Name: "patch_file",
			Args: map[string]any{"diff": diff},
		})
		return err
	}

	// Six alternating failures on two different diffs: no stop. The loop
	// ends on badDiffB so the subsequent badDiffA streak starts with a
	// fresh key.
	for i := 0; i < 6; i++ {
		diff := badDiffA
		if i%2 == 1 {
			diff = badDiffB
		}
		if err := patch(diff); errors.Is(err, errPatchTurnStop) {
			t.Fatalf("different diffs must not accumulate strikes (attempt %d), got: %v", i, err)
		}
	}

	// The same diff repeated three times still stops.
	for i := 0; i < 3; i++ {
		err := patch(badDiffA)
		if i < 2 && errors.Is(err, errPatchTurnStop) {
			t.Fatalf("strike %d must not stop yet, got: %v", i+1, err)
		}
		if i == 2 && !errors.Is(err, errPatchTurnStop) {
			t.Fatalf("third identical failure must stop the turn, got: %v", err)
		}
	}
}

func TestRunToolRoundStoppedEndsTurn(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	a := NewAgent(nil, exec, nil)

	// First call: marker-only diff (stops the turn). Second call: a valid
	// diff that must never execute — it gets a cancelled placeholder.
	outcome, stopMsg := a.runToolRound(context.Background(), &llm.StreamHandlers{}, []llm.ToolCall{
		{ID: "c1", Name: "patch_file", Args: map[string]any{"diff": "*** End of diff\n*** End of diff\n*** End of diff\n"}},
		{ID: "c2", Name: "patch_file", Args: map[string]any{"diff": "--- a/x.txt\n+++ b/x.txt\n@@ -1 +1 @@\n-a\n+b\n"}},
	})
	if outcome != toolRoundStopped {
		t.Fatalf("expected toolRoundStopped, got %v", outcome)
	}
	if !strings.Contains(stopMsg, "only patch markers") {
		t.Fatalf("expected marker-only explanation in stopMsg, got: %q", stopMsg)
	}

	// The tool-call/result protocol must stay valid: c1 has a real result,
	// c2 has the cancelled placeholder.
	var c1Result, c2Result string
	for _, m := range a.Messages {
		if m.Role != "tool" {
			continue
		}
		switch m.ToolCallID {
		case "c1":
			c1Result = m.Content
		case "c2":
			c2Result = m.Content
		}
	}
	if !strings.Contains(c1Result, "only patch markers") {
		t.Fatalf("expected c1 result with stop explanation, got: %q", c1Result)
	}
	if c2Result != "Tool execution was skipped because the user cancelled the operation." {
		t.Fatalf("expected c2 cancel placeholder, got: %q", c2Result)
	}
}

// TestParseUnifiedDiffRepeatedPlusPlusSameFile: a repeated "+++ " for the
// current file between hunks keeps collecting hunks into the same section
// (the model re-emitted the header rather than starting a new file).
func TestParseUnifiedDiffRepeatedPlusPlusSameFile(t *testing.T) {
	diff := "" +
		"--- a/foo.txt\n" +
		"+++ b/foo.txt\n" +
		"@@ -1,1 +1,2 @@\n" +
		" one\n" +
		"+two\n" +
		"+++ b/foo.txt\n" + // duplicate header for the same file
		"@@ -2,1 +2,2 @@\n" +
		" two\n" +
		"+three\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 patchFile, got %d: %+v", len(files), files)
	}
	if len(files[0].hunks) != 2 {
		t.Fatalf("expected 2 hunks merged into one file, got %d", len(files[0].hunks))
	}
}

func TestParseUnifiedDiffUnderDeclaredHunkThenDroppedPlusPlus(t *testing.T) {
	// Regression: a hunk whose declared counts exceed its emitted lines (LLM
	// under-counting) never reaches hunkComplete, so the next file's "+++ "
	// header with its "--- " dropped used to be absorbed as an ADDED hunk
	// line ("++ b/b.txt") — silently injecting a bogus line into a.txt on
	// apply. The isSectionMarkerAhead lookahead must recognize the header
	// (it is always followed by the section's "@@" marker) and start a new
	// section for b/b.txt instead.
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,5 +1,5 @@\n" +
		" one\n" +
		" two\n" +
		" three\n" +
		"+++ b/b.txt\n" + // --- header for b.txt was dropped
		"@@ -1,1 +1,1 @@\n" +
		" x\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d want 2: %#v", len(files), files)
	}
	if normalizePatchPath(files[0].newName) != "a.txt" || normalizePatchPath(files[1].newName) != "b.txt" {
		t.Fatalf("newNames=%q %q, want a.txt and b.txt", files[0].newName, files[1].newName)
	}
	if len(files[0].hunks) != 1 || len(files[1].hunks) != 1 {
		t.Fatalf("hunks=%d %d want 1 and 1", len(files[0].hunks), len(files[1].hunks))
	}
	h0 := files[0].hunks[0]
	if !reflect.DeepEqual(h0.oldLines, []string{"one", "two", "three"}) {
		t.Fatalf("a.txt hunk oldLines corrupted: %#v", h0.oldLines)
	}
	if !reflect.DeepEqual(h0.newLines, []string{"one", "two", "three"}) {
		t.Fatalf("a.txt hunk newLines corrupted (must not absorb the +++ header): %#v", h0.newLines)
	}
}

func TestPatchFileUnderDeclaredHunkThenDroppedPlusPlus(t *testing.T) {
	// End-to-end: the same layout must apply cleanly to BOTH files with no
	// bogus "++ b/b.txt" line injected into a.txt. This is the silent-
	// corruption regression: before the fix the parse succeeded and the
	// patched a.txt gained a foreign line with no error.
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(pathA, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,5 +1,5 @@\n" +
		" one\n" +
		" two\n" +
		" three\n" +
		"+++ b/b.txt\n" + // --- header for b.txt was dropped
		"@@ -1,1 +1,1 @@\n" +
		" x\n"
	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	if _, err := exec.PatchFile(context.Background(), diff, false, true); err != nil {
		t.Fatalf("PatchFile: %v", err)
	}
	gotA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotA) != "one\ntwo\nthree\n" {
		t.Fatalf("a.txt=%q want unchanged %q (bogus line injected?)", string(gotA), "one\ntwo\nthree\n")
	}
	gotB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotB) != "x\n" {
		t.Fatalf("b.txt=%q want %q", string(gotB), "x\n")
	}
}

func TestParseUnifiedDiffUnderDeclaredHunkThenRepeatedPlusPlusSameFile(t *testing.T) {
	// A repeated "+++ " header for the SAME file between hunks, with the
	// first hunk under-declared, must keep collecting hunks into one
	// section — not absorb the header as an added line.
	diff := "" +
		"--- a/foo.txt\n" +
		"+++ b/foo.txt\n" +
		"@@ -1,5 +1,5 @@\n" +
		" one\n" +
		" two\n" +
		" three\n" +
		"+++ b/foo.txt\n" + // repeated header, first hunk under-declared
		"@@ -2,1 +2,2 @@\n" +
		" two\n" +
		"+three\n"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1: %#v", len(files), files)
	}
	if len(files[0].hunks) != 2 {
		t.Fatalf("hunks=%d want 2", len(files[0].hunks))
	}
	if !reflect.DeepEqual(files[0].hunks[0].newLines, []string{"one", "two", "three"}) {
		t.Fatalf("hunk 1 newLines corrupted (must not absorb the +++ header): %#v", files[0].hunks[0].newLines)
	}
}

func TestParseUnifiedDiffPlusPlusContentLineStaysInHunk(t *testing.T) {
	// A genuine added line whose content starts with "++" (wire "+++ X",
	// e.g. C "++i;" at line start) must stay hunk content when it is NOT
	// followed by a section marker — the isSectionMarkerAhead lookahead must
	// not over-fire on content.
	diff := "" +
		"--- a/x.c\n" +
		"+++ b/x.c\n" +
		"@@ -1,1 +1,3 @@\n" +
		" int a = 0;\n" +
		"+int b = 1;\n" +
		"+++i;\n" // added line with content "++i;"
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1: %#v", len(files), files)
	}
	h := files[0].hunks[0]
	want := []string{"int a = 0;", "int b = 1;", "++i;"}
	if !reflect.DeepEqual(h.newLines, want) {
		t.Fatalf("newLines=%#v want %#v", h.newLines, want)
	}
}

// TestApplyPatchHunksTrailingNewline covers the trailing-newline state:
// a file without a final newline keeps that state when the last line is
// untouched or carries the marker, and gains a newline when a normal line
// is appended at EOF.
func TestApplyPatchHunksTrailingNewline(t *testing.T) {
	cases := []struct {
		name             string
		original         []string
		originalTrailing bool
		hunks            []patchHunk
		wantOut          []string
		wantNoNewline    bool
	}{
		{
			name:             "preserve no trailing newline when last line untouched",
			original:         []string{"line1", "line3"},
			originalTrailing: false,
			hunks: []patchHunk{{
				oldStart: 1,
				oldLines: []string{"line1"},
				newLines: []string{"line1", "line2"},
			}},
			wantOut:       []string{"line1", "line2", "line3"},
			wantNoNewline: true,
		},
		{
			name:             "marker on added line at EOF removes trailing newline",
			original:         []string{"line1", "line2"},
			originalTrailing: true,
			hunks: []patchHunk{{
				oldStart:     2,
				oldLines:     []string{"line2"},
				newLines:     []string{"line2b"},
				newNoNewline: true,
			}},
			wantOut:       []string{"line1", "line2b"},
			wantNoNewline: true,
		},
		{
			name:             "appending a normal line at EOF adds trailing newline",
			original:         []string{"line1"},
			originalTrailing: false,
			hunks: []patchHunk{{
				oldStart: 1,
				oldLines: []string{"line1"},
				newLines: []string{"line1", "line2"},
			}},
			wantOut:       []string{"line1", "line2"},
			wantNoNewline: false,
		},
		{
			// "@@ -1,0 +2,1 @@" inserts AFTER line 1, as patch(1) does: a
			// zero-old-count hunk header names the line the insertion
			// follows.
			name:             "insertion before EOF keeps original trailing state",
			original:         []string{"a", "b"},
			originalTrailing: false,
			hunks: []patchHunk{{
				oldStart: 1,
				oldLines: nil,
				newLines: []string{"x"},
			}},
			wantOut:       []string{"a", "x", "b"},
			wantNoNewline: true,
		},
		{
			// "@@ -0,0 +1,1 @@" is the one case that inserts at index 0.
			name:             "insertion at the start of the file",
			original:         []string{"a", "b"},
			originalTrailing: true,
			hunks: []patchHunk{{
				oldStart: 0,
				oldLines: nil,
				newLines: []string{"x"},
			}},
			wantOut:       []string{"x", "a", "b"},
			wantNoNewline: false,
		},
		{
			name:             "deleting the file tail newline-terminates the previous line",
			original:         []string{"a", "b"},
			originalTrailing: false,
			hunks: []patchHunk{{
				oldStart: 2,
				oldLines: []string{"b"},
				newLines: nil,
			}},
			wantOut:       []string{"a"},
			wantNoNewline: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, noNewline, _, err := applyPatchHunks(tc.original, tc.hunks, true, tc.originalTrailing)
			if err != nil {
				t.Fatal(err)
			}
			if noNewline != tc.wantNoNewline {
				t.Fatalf("noNewline = %v, want %v (got %q)", noNewline, tc.wantNoNewline, got)
			}
			if len(got) != len(tc.wantOut) {
				t.Fatalf("got %q, want %q", got, tc.wantOut)
			}
			for i := range tc.wantOut {
				if got[i] != tc.wantOut[i] {
					t.Fatalf("line %d = %q, want %q", i, got[i], tc.wantOut[i])
				}
			}
		})
	}
}

// parseDiffLineCount returns only the start line of a hunk header range.
//
// Lives in this _test.go file: production callers use parseDiffLineRange
// (which also yields the count); only tests need the start-only convenience.
func parseDiffLineCount(part string) (int, error) {
	start, _, err := parseDiffLineRange(part)
	return start, err
}

// TestPatchMismatchDiagnosticFileShorterThanContext pins the mismatch
// diagnostic when the file runs out before any expected line disagrees: the
// file's remaining lines are a strict prefix of the hunk context, so there
// is no differing line to quote — the diagnostic must report the shortfall
// instead of presenting two IDENTICAL lines as "the mismatch". A real
// differing line keeps the classic quote-the-difference shape.
func TestPatchMismatchDiagnosticFileShorterThanContext(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The hunk starts at line 2 and expects two context lines ("b", "c"),
	// but the file ends after "b". The per-file diagnostic rides on the
	// RESULT (formatPatchReport); the error is the aggregate.
	res, err := exec.PatchFile(context.Background(), "--- a/f.txt\n+++ b/f.txt\n@@ -2,2 +2,2 @@\n b\n-c\n+d\n", false, true)
	if err == nil {
		t.Fatal("expected the mismatch to fail")
	}
	if !strings.Contains(res, "file has 1 line(s) here but the hunk expects 2 context lines") {
		t.Fatalf("diagnostic = %q, want the shortfall explanation", res)
	}
	if strings.Contains(res, `file has "b", patch expects "b"`) {
		t.Fatalf("diagnostic quotes identical lines as the mismatch: %q", res)
	}

	if err := os.WriteFile(filepath.Join(dir, "g.txt"), []byte("a\nX\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = exec.PatchFile(context.Background(), "--- a/g.txt\n+++ b/g.txt\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n", false, false)
	if err == nil {
		t.Fatal("expected the mismatch to fail")
	}
	if !strings.Contains(res, `file has "X", patch expects "b"`) {
		t.Fatalf("diagnostic = %q, want the differing-line quote", res)
	}
}
