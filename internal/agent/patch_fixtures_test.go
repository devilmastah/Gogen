package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPatchFixtures runs every fixture under testdata/patches through the
// real parseUnifiedDiff + applyPatchHunks pipeline and asserts the resulting
// file contents. The fixtures capture real model-generated diff shapes (CRLF
// and bare-CR input, duplicate ---/+++ headers, SQL comment lines starting
// "-- ", declared hunk-count drift over/under, /dev/null new and deleted
// files, rename sections, "\ No newline at end of file" markers, multi-file
// patches, and trailing garbage the model added). This corpus is the
// regression net for the parseUnifiedDiff state-machine refactor and any
// future LLM-output parsing change.
//
// normalizeEOL maps CRLF and bare CR to LF, matching the normalization
// splitLinesPreserveTrailing applies to original file contents.
func normalizeEOL(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// Layout: testdata/patches/<name>/patch.diff plus orig/<path> (pre-image
// contents, one per --- header, path without the a/ prefix) and want/<path>
// (post-image contents, path without the b/ prefix). A newName of /dev/null
// asserts the file is reduced to zero lines; an oldName of /dev/null asserts
// the insertion applies onto an empty file.
func TestPatchFixtures(t *testing.T) {
	dirs, err := filepath.Glob("testdata/patches/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no fixture directories under testdata/patches")
	}
	for _, dir := range dirs {
		dir := dir
		t.Run(filepath.Base(dir), func(t *testing.T) {
			diffB, err := os.ReadFile(filepath.Join(dir, "patch.diff"))
			if err != nil {
				t.Fatal(err)
			}
			files, err := parseUnifiedDiff(string(diffB))
			if err != nil {
				t.Fatalf("parseUnifiedDiff: %v", err)
			}
			if len(files) == 0 {
				t.Fatal("parseUnifiedDiff returned no file sections")
			}
			for _, pf := range files {
				// Normalize exactly like planPatch: strip one a//b/ prefix
				// and any tab-timestamp, so fixtures can carry real git
				// header shapes (timestamped /dev/null, b/-prefixed names).
				oldPath := ""
				if pf.oldName != "" {
					oldPath = normalizePatchPath(pf.oldName)
				}
				newPath := ""
				if pf.newName != "" {
					newPath = normalizePatchPath(pf.newName)
				}
				if oldPath == "" {
					// A section with only a +++ header (the --- was dropped)
					// modifies the file named by the new header.
					oldPath = newPath
				}
				var orig []string
				origTrailing := true
				origEOL := "\n"
				if oldPath != "/dev/null" {
					origB, err := os.ReadFile(filepath.Join(dir, "orig", filepath.FromSlash(oldPath)))
					if err != nil {
						t.Fatalf("orig/%s: %v", oldPath, err)
					}
					origTrailing = strings.HasSuffix(string(origB), "\n")
					orig, origEOL = splitLinesPreserveTrailing(string(origB))
				}
				got, outNoNewline, _, err := applyPatchHunks(orig, pf.hunks, true, origTrailing)
				if err != nil {
					t.Fatalf("%s: applyPatchHunks: %v", pf.newName, err)
				}
				if newPath == "/dev/null" {
					if len(got) != 0 {
						t.Fatalf("%s: expected deletion (0 lines), got %d lines", pf.oldName, len(got))
					}
					continue
				}
				wantB, err := os.ReadFile(filepath.Join(dir, "want", filepath.FromSlash(newPath)))
				if err != nil {
					t.Fatalf("want/%s: %v", newPath, err)
				}
				// Compare the joined output byte-for-byte (after EOL
				// normalization) so the trailing-newline state — which a
				// line-slice comparison cannot see — is asserted too.
				gotStr := normalizeEOL(joinLinesPreserveTrailing(got, !outNoNewline, origEOL))
				wantStr := normalizeEOL(string(wantB))
				if gotStr != wantStr {
					t.Fatalf("%s: got %q, want %q", newPath, gotStr, wantStr)
				}
			}
		})
	}
}
