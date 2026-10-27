package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"

	"gogen/internal/ioutil"
)

type patchFile struct {
	oldName string
	newName string
	hunks   []patchHunk
}

type patchHunk struct {
	oldStart int
	oldLines []string
	newLines []string
	oldCount int // declared line counts from the hunk header
	newCount int
	// newNoNewline records that the last added line of this hunk carries a
	// "\ No newline at end of file" marker: the patched file's final line
	// has no trailing newline when this hunk reaches the end of the file.
	newNoNewline bool
}

type patchPlan struct {
	target     string
	secure     string
	updated    string
	delete     bool
	create     bool
	hunkShifts []string // non-nil when fuzzy mode relocated hunks
}

// ErrPatchMismatch marks patch failures caused by stale or malformed diff
// context: hunk context mismatches, ambiguous fuzzy relocations, malformed
// diff syntax (bad headers/hunk lines), an empty patch, or a diff whose
// headers cannot be resolved. The agent's patch_file retry-streak hint
// (executeTool) counts only these toward "failed N times in a row", so
// permission, I/O, and path-safety failures never trigger stale-diff advice.
var ErrPatchMismatch = errors.New("patch context mismatch")

// patchMismatchError wraps an error so errors.Is(err, ErrPatchMismatch)
// succeeds while Error() preserves the original message verbatim — no
// "patch context mismatch:" prefix — keeping model- and test-visible text
// unchanged.
type patchMismatchError struct{ err error }

func (e *patchMismatchError) Error() string { return e.err.Error() }
func (e *patchMismatchError) Unwrap() error { return e.err }

// Is reports the wrapper as ErrPatchMismatch so errors.Is(err,
// ErrPatchMismatch) matches any error wrapped by wrapPatchMismatch (the
// sentinel itself is never in the Unwrap chain — Unwrap yields the original
// error so its message text is preserved verbatim).
func (e *patchMismatchError) Is(target error) bool { return target == ErrPatchMismatch }

// wrapPatchMismatch marks err as a patch-context failure. Nil and already-
// marked errors pass through unchanged.
func wrapPatchMismatch(err error) error {
	if err == nil || errors.Is(err, ErrPatchMismatch) {
		return err
	}
	return &patchMismatchError{err: err}
}

// errPatchTurnStop marks a patch_file failure after which the agent loop
// must end the turn instead of letting the model retry. It fires when the
// diff contains only patch framing with no content (a degenerate retry-loop
// reply) or when the per-turn mismatch budget is exhausted. The wrapped
// error message carries the user-visible explanation; the sentinel itself
// is never shown verbatim.
var errPatchTurnStop = errors.New("patch turn stopped")

// PatchFile applies a unified diff to files under the working directory.
// When dryRun is true, patches are validated but not written.
// When fuzzy is true, hunks may be relocated when exact context no longer matches.
func (e *Executor) PatchFile(ctx context.Context, diff string, dryRun, fuzzy bool) (string, error) {
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		return "", wrapPatchMismatch(err)
	}
	if len(files) == 0 {
		return "", wrapPatchMismatch(fmt.Errorf("no patches found in diff"))
	}

	var plans []patchPlan
	var okFiles []string
	var failFiles []string
	mismatchSeen := false
	seenTargets := make(map[string]struct{})

	for _, pf := range files {
		plan, label, err := e.planPatch(pf, fuzzy)
		if err != nil {
			failFiles = append(failFiles, fmt.Sprintf("%s: %v", label, err))
			if errors.Is(err, ErrPatchMismatch) {
				mismatchSeen = true
			}
			continue
		}
		if _, dup := seenTargets[plan.secure]; dup {
			failFiles = append(failFiles, fmt.Sprintf("%s: duplicate path in patch; combine into one file section", plan.target))
			continue
		}
		seenTargets[plan.secure] = struct{}{}
		plans = append(plans, plan)
		okFiles = append(okFiles, label)
	}

	if len(failFiles) > 0 {
		msg := formatPatchReport(okFiles, failFiles, dryRun)
		err := fmt.Errorf("patch failed for %d file(s)", len(failFiles))
		if mismatchSeen {
			// At least one failing file was a context/format failure: mark
			// the aggregate so the retry-streak hint treats it as stale-diff
			// advice rather than an I/O or permission problem.
			err = wrapPatchMismatch(err)
		}
		return msg, err
	}

	if dryRun {
		return fmt.Sprintf("Dry run OK — would change %d file(s): %s\n\nNo files were modified.", len(plans), strings.Join(okFiles, ", ")), nil
	}

	var applied []string
	snapshots := make(map[string][]byte, len(plans))
	created := make([]string, 0)
	for _, plan := range plans {
		if plan.delete {
			if err := e.requireDeleteApproval(ctx, []string{plan.target}, "patch_file"); err != nil {
				rollbackPatches(snapshots, created)
				return "", err
			}
			// Snapshot the target's CONTENT (rollback restores it), but remove
			// the ENTRY: plan.secure is fully resolved, so removing it would
			// delete what a symlink points at and leave the link dangling.
			if data, err := os.ReadFile(plan.secure); err == nil {
				snapshots[plan.secure] = data
			} else if !os.IsNotExist(err) {
				rollbackPatches(snapshots, created)
				return "", err
			}
			removePath := plan.secure
			if noFollow, nfErr := e.SecurePathNoFollow(plan.target); nfErr == nil {
				removePath = noFollow
			}
			if err := os.Remove(removePath); err != nil && !os.IsNotExist(err) {
				rollbackPatches(snapshots, created)
				return "", err
			}
			applied = append(applied, plan.target+" (deleted)")
			continue
		}

		if !plan.create {
			data, err := os.ReadFile(plan.secure)
			if err != nil {
				rollbackPatches(snapshots, created)
				return "", err
			}
			snapshots[plan.secure] = data
		}

		if err := ioutil.WriteFileAtomic(plan.secure, []byte(plan.updated), defaultFilePerm); err != nil {
			rollbackPatches(snapshots, created)
			return "", err
		}
		if plan.create {
			created = append(created, plan.secure)
			applied = append(applied, plan.target+" (created)")
		} else {
			applied = append(applied, plan.target)
		}
	}

	msg := fmt.Sprintf("Applied patch to %d file(s): %s", len(applied), strings.Join(applied, ", "))
	msg = appendShifts(msg, plans)
	return e.AppendSyntaxCheck(msg, appliedPaths(applied)...), nil
}

func appliedPaths(applied []string) []string {
	out := make([]string, 0, len(applied))
	for _, a := range applied {
		a = strings.TrimSuffix(a, " (deleted)")
		a = strings.TrimSuffix(a, " (created)")
		out = append(out, a)
	}
	return out
}

func appendShifts(msg string, plans []patchPlan) string {
	var shifts []string
	for _, p := range plans {
		for _, s := range p.hunkShifts {
			shifts = append(shifts, p.target+": "+s)
		}
	}
	if len(shifts) == 0 {
		return msg
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Warning: %d hunk(s) were relocated by fuzzy matching — verify the changes before continuing.\n\n", len(shifts))
	b.WriteString(msg)
	b.WriteString("\n\nFuzzy-matching shifts (hunks relocated from original positions):\n")
	for _, s := range shifts {
		b.WriteString("  " + s + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func rollbackPatches(snapshots map[string][]byte, created []string) {
	for _, path := range created {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("patch rollback: remove %s: %v", path, err)
		}
	}
	for path, data := range snapshots {
		if err := ioutil.WriteFileAtomic(path, data, defaultFilePerm); err != nil {
			log.Printf("patch rollback: restore %s: %v", path, err)
		}
	}
}

func formatPatchReport(ok, fail []string, dryRun bool) string {
	var b strings.Builder
	if dryRun {
		b.WriteString("Dry run failed.\n")
	} else {
		b.WriteString("Patch failed.\n")
	}
	if len(ok) > 0 {
		b.WriteString("OK: " + strings.Join(ok, ", ") + "\n")
	}
	if len(fail) > 0 {
		b.WriteString("FAILED:\n")
		for _, f := range fail {
			b.WriteString("  " + f + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *Executor) planPatch(pf patchFile, fuzzy bool) (patchPlan, string, error) {
	// Normalize the header names BEFORE any /dev/null comparison: git
	// appends tab-timestamps ("+++ /dev/null\t2024-…"), so the raw text
	// never matches. The empty case is guarded because normalizePatchPath
	// of "" would Clean to "." and swallow the "could not determine
	// target" error below.
	newName := pf.newName
	if newName != "" {
		newName = normalizePatchPath(newName)
	}
	oldName := pf.oldName
	if oldName != "" {
		oldName = normalizePatchPath(oldName)
	}

	// The +++ header names the target. When it is missing (models sometimes
	// drop it) or it is /dev/null (delete), fall back to the --- header.
	target := newName
	if target == "" || target == "/dev/null" {
		target = oldName
	}
	if target == "" {
		return patchPlan{}, "", wrapPatchMismatch(fmt.Errorf("could not determine target file from diff headers"))
	}

	secure, err := e.SecurePath(target)
	if err != nil {
		return patchPlan{}, target, err
	}

	if newName == "/dev/null" {
		return patchPlan{target: target, secure: secure, delete: true}, target + " (validated delete)", nil
	}

	isCreate := oldName == "/dev/null"
	if !isCreate && len(pf.hunks) == 0 {
		return patchPlan{}, target, wrapPatchMismatch(fmt.Errorf("no hunks found (check @@ headers use the form '@@ -start,count +start,count @@' with a space after @@)"))
	}

	if isCreate {
		if _, err := os.Stat(secure); err == nil {
			return patchPlan{}, target, wrapPatchMismatch(fmt.Errorf("file already exists; use a modify patch (--- a/%s) instead of creating from /dev/null", target))
		} else if !os.IsNotExist(err) {
			return patchPlan{}, target, err
		}
	}

	var original []string
	originalTrailing := true
	// eol is the file's dominant line ending, restored on write (see
	// joinLinesPreserveTrailing): a patch to a CRLF checkout must not rewrite
	// every line ending in it.
	eol := "\n"
	if !isCreate {
		data, err := os.ReadFile(secure)
		if err != nil {
			return patchPlan{}, target, fmt.Errorf("read: %w", err)
		}
		originalTrailing = strings.HasSuffix(string(data), "\n")
		original, eol = splitLinesPreserveTrailing(string(data))
	}

	updated, outNoNewline, shifts, err := applyPatchHunks(original, pf.hunks, fuzzy, originalTrailing)
	if err != nil {
		// applyPatchHunks failures are context mismatches, ambiguous fuzzy
		// relocations, or stale line numbers — all stale-diff failures.
		return patchPlan{}, target, wrapPatchMismatch(err)
	}

	label := target + " (validated)"
	if isCreate {
		label = target + " (validated create)"
	}

	return patchPlan{
		target:     target,
		secure:     secure,
		updated:    joinLinesPreserveTrailing(updated, !outNoNewline, eol),
		create:     isCreate,
		hunkShifts: shifts,
	}, label, nil
}

func normalizePatchPath(name string) string {
	name = strings.TrimSpace(name)
	// Git diffs often append a tab + timestamp: "path\t2024-01-01 12:00:00.000000000 +0000"
	if i := strings.IndexByte(name, '\t'); i >= 0 {
		name = name[:i]
	}
	// Some tools use a space before an ISO-ish timestamp instead of a tab.
	if i := strings.IndexByte(name, ' '); i >= 0 {
		rest := strings.TrimSpace(name[i+1:])
		if looksLikePatchTimestamp(rest) {
			name = name[:i]
		}
	}
	name = strings.TrimSpace(name)
	if len(name) >= 2 {
		if (name[0] == '"' && name[len(name)-1] == '"') || (name[0] == '\'' && name[len(name)-1] == '\'') {
			name = name[1 : len(name)-1]
		}
	}
	// Strip the conventional unified-diff a/ and b/ prefixes — exactly one,
	// never both in sequence: a file legitimately named "b/foo.txt" appears
	// as "--- a/b/foo.txt", and chaining both trims would resolve it to
	// "foo.txt". This matches git/patch tooling and cannot distinguish a
	// literal path that starts with "a/" or "b/" (e.g. a repo file named
	// a/foo.go) — a known limitation of the unified diff format.
	switch {
	case strings.HasPrefix(name, "a/"):
		name = name[len("a/"):]
	case strings.HasPrefix(name, "b/"):
		name = name[len("b/"):]
	}
	// path.Clean (not filepath.Clean): diff headers are slash-separated by
	// convention on every platform, and filepath.Clean would rewrite the
	// separators to backslashes on Windows — turning "/dev/null" into
	// "dev\null" (breaking delete/create detection) and "b/foo.txt" into
	// "b\foo.txt".
	return path.Clean(name)
}

func looksLikePatchTimestamp(s string) bool {
	if s == "" {
		return false
	}
	// ISO date / epoch-style: "2024-01-01 ..."
	if s[0] >= '0' && s[0] <= '9' {
		return true
	}
	lower := strings.ToLower(s)
	for _, day := range []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"} {
		if strings.HasPrefix(lower, day) {
			return true
		}
	}
	return false
}

// patchParseState tracks the diff section the parser is in. The plan's
// three-state sketch (fileHeader/hunkHeader/hunkBody) collapses to two: a
// freshly parsed @@ line and a hunk with body lines behave identically
// (zero-count hunks complete immediately via hunkComplete), so hunkHeader
// carries no distinct behavior.
type patchParseState int

const (
	// stateFileHeader expects a file header (---/+++), a hunk header (@@),
	// git metadata, or a patch delimiter; any other line is skipped (stray
	// text between sections is tolerated).
	stateFileHeader patchParseState = iota
	// stateHunkBody consumes hunk body lines until the hunk's declared
	// counts are consumed, the next section begins, or the patch ends.
	stateHunkBody
	// stateDone ends parsing: a model-added delimiter was seen and the
	// pending file was flushed. Lines after the delimiter are ignored.
	stateDone
)

// diffParser holds the state of a parseUnifiedDiff run. The per-state line
// handlers are methods so each stays small and testable.
type diffParser struct {
	lines         []string
	boundaryAhead []bool
	files         []patchFile
	current       *patchFile
	hunk          *patchHunk
	state         patchParseState
}

func (p *diffParser) flushHunk() {
	if p.current != nil && p.hunk != nil && (len(p.hunk.oldLines) > 0 || len(p.hunk.newLines) > 0) {
		p.current.hunks = append(p.current.hunks, *p.hunk)
	}
	p.hunk = nil
}

func (p *diffParser) flushFile() {
	p.flushHunk()
	if p.current != nil {
		// A file section with no hunks and no /dev/null target is
		// malformed. Models sometimes repeat the ---/+++ header before
		// the real hunk section (a duplicate header); failing the whole
		// patch on such a section is spurious. Drop it and keep parsing —
		// a lone header-only section yields "no patches found" below
		// (still loud), and real hunks in a later section still apply.
		// The /dev/null check tolerates git's tab-timestamp suffix, which
		// the parser stores verbatim.
		isDevNull := p.current.newName == "/dev/null" ||
			strings.HasPrefix(p.current.newName, "/dev/null\t")
		if !isDevNull && len(p.current.hunks) == 0 {
			p.current = nil
			return
		}
		p.files = append(p.files, *p.current)
		p.current = nil
	}
}

// handleFileHeaderLine processes one line between file sections: git
// metadata, ---/+++ headers, @@ hunk headers, delimiters, and stray text
// (skipped). Returns the next state.
func (p *diffParser) handleFileHeaderLine(i int) (patchParseState, error) {
	switch {
	case strings.HasPrefix(p.lines[i], "diff --git "):
		p.flushFile()
	case isContextRangeHeader(p.lines[i]) || isContextSeparator(p.lines[i]):
		// Context-format (diff -c) constructs between sections mean the
		// model switched diff formats. Fail loudly with a format-specific
		// message; the generic header-state fallthrough would otherwise
		// skip the range header and yield a confusing "no patches found".
		return stateFileHeader, contextFormatError(p.lines[i])
	case isGitPreamble(p.lines[i]):
		// git metadata between file sections — skip.
	case strings.HasPrefix(p.lines[i], "--- "):
		p.flushFile()
		p.current = &patchFile{oldName: strings.TrimSpace(strings.TrimPrefix(p.lines[i], "--- "))}
	case strings.HasPrefix(p.lines[i], "+++ "):
		if p.current == nil {
			return stateFileHeader, fmt.Errorf("malformed diff: +++ before ---")
		}
		p.current.newName = strings.TrimSpace(strings.TrimPrefix(p.lines[i], "+++ "))
	case strings.HasPrefix(p.lines[i], "@@"):
		p.flushHunk()
		if p.current == nil {
			return stateFileHeader, fmt.Errorf("malformed diff: hunk before file header")
		}
		parsed, err := parseHunkHeader(p.lines[i])
		if err != nil {
			return stateFileHeader, err
		}
		p.hunk = &parsed
		return stateHunkBody, nil
	case isPatchDelimiterMarker(p.lines[i]) || isBareCodeFence(p.lines[i]):
		// A model-added delimiter (e.g. "*** End Patch", "***endpatch",
		// "*** End of file", a bare "***", or a closing code fence) marks
		// the end of the patch. Flush the pending file and stop parsing,
		// so repeated markers or stray text after the marker are ignored
		// instead of being parsed as new files. Prefixed lines such as
		// "+*** note" still parse as added lines because they start with
		// a hunk prefix, not "***". Context-format range headers
		// ("*** 16,20 ***", diff -c) are NOT delimiters: they signal the
		// model switched diff formats and must keep failing loudly. A
		// delimiter before the first file header (e.g. "*** Start Patch")
		// is a preamble and is skipped.
		if p.current != nil {
			p.flushFile()
			return stateDone, nil
		}
	}
	return stateFileHeader, nil
}

// handleHunkBodyLine processes one hunk body line. Returns the next state;
// stateDone ends the patch (delimiter seen).
func (p *diffParser) handleHunkBodyLine(i int) (patchParseState, error) {
	switch {
	case strings.HasPrefix(p.lines[i], "diff --git "):
		p.flushFile()
		return stateFileHeader, nil
	case isContextRangeHeader(p.lines[i]) || isContextSeparator(p.lines[i]):
		// A context-format range header or section separator inside a hunk
		// body means the model switched to diff -c mid-patch. Fail loudly:
		// the old-side header would otherwise be a "malformed hunk line"
		// and the new-side header a spurious "--- " file header or removed
		// line, both silently corrupting or confusingly failing the patch.
		return stateHunkBody, contextFormatError(p.lines[i])
	// "--- "/"+++ " are file headers ONLY outside a hunk (or after a
	// hunk that has consumed its declared line counts). A hunk's own
	// removed/added lines can themselves start with "-- "/"++ " — e.g.
	// deleting a SQL comment line "-- foo" yields the wire line
	// "--- foo" — and an unconditional header check would flush the
	// current hunk and silently corrupt the patch. git resolves the
	// same ambiguity with section state; we use the hunk's declared
	// counts as the tie-breaker (>= because LLM counts are frequently
	// imprecise). A hunk whose declared counts EXCEED its emitted lines
	// never completes; when the next file's header pair follows with no
	// blank separator, isHeaderPairAhead hands control back to header
	// mode instead of absorbing "--- a/b.txt" as a removed line. The
	// same lookahead (isSectionMarkerAhead) rescues the "+++ " header in
	// an incomplete hunk: the next file's "+++ " with its "--- " dropped,
	// or a repeated "+++ " between hunks, is always followed by the
	// section marker its hunks start with — while a genuine added line
	// ("++ X" content, wire "+++ X") is not. Absorbing a "+++ " header as
	// an added line would inject a bogus "++ X" line into the patched
	// file with no error.
	case strings.HasPrefix(p.lines[i], "--- ") && (hunkComplete(p.hunk) || isHeaderPairAhead(p.lines, i)):
		p.flushFile()
		p.current = &patchFile{oldName: strings.TrimSpace(strings.TrimPrefix(p.lines[i], "--- "))}
		return stateFileHeader, nil
	case strings.HasPrefix(p.lines[i], "+++ ") && (hunkComplete(p.hunk) || isSectionMarkerAhead(p.lines, i)):
		if p.current == nil {
			return stateHunkBody, fmt.Errorf("malformed diff: +++ before ---")
		}
		name := strings.TrimSpace(strings.TrimPrefix(p.lines[i], "+++ "))
		if name == p.current.newName {
			// A repeated +++ for the same file (the model re-emitted the
			// header between hunks): keep collecting hunks into the
			// current section.
			return stateFileHeader, nil
		}
		// The current file's own +++ was consumed in header state before
		// any hunk, so a +++ with a different name in hunk-body state can
		// only be the next file's header with its --- dropped. Flush the
		// current file (its completed hunk stays with it) and start a new
		// section carrying only the newName; overwriting newName in place
		// would merge the next file's hunks into this file and patch the
		// wrong target.
		p.flushFile()
		p.current = &patchFile{newName: name}
		return stateFileHeader, nil
	case strings.HasPrefix(p.lines[i], "@@"):
		p.flushHunk()
		if p.current == nil {
			return stateHunkBody, fmt.Errorf("malformed diff: hunk before file header")
		}
		parsed, err := parseHunkHeader(p.lines[i])
		if err != nil {
			return stateHunkBody, err
		}
		p.hunk = &parsed
		return stateHunkBody, nil
	case isPatchDelimiterMarker(p.lines[i]) || isBareCodeFence(p.lines[i]):
		if p.current != nil {
			p.flushFile()
			return stateDone, nil
		}
		return stateHunkBody, nil
	case p.lines[i] == `\ No newline at end of file`:
		// The marker applies to the hunk body line above it: after an added
		// line it says the new file's final line has no trailing newline;
		// after a context line it applies to both sides. A marker after a
		// removed line only describes the old file and does not affect the
		// patched output.
		if p.hunk != nil && i > 0 {
			switch prev := p.lines[i-1]; {
			case strings.HasPrefix(prev, "+"), strings.HasPrefix(prev, " "):
				p.hunk.newNoNewline = true
			}
		}
		return stateHunkBody, nil
	case len(p.lines[i]) == 0:
		// Empty lines in a hunk body are treated as empty context lines.
		// Unified diffs normally encode them as a single space (" "), but
		// LLMs often emit a bare blank line instead — dropping those
		// corrupts patches. Exception: blanks that only separate file
		// sections must not become context.
		if p.boundaryAhead[i+1] {
			p.flushHunk()
			return stateFileHeader, nil
		}
		p.hunk.oldLines = append(p.hunk.oldLines, "")
		p.hunk.newLines = append(p.hunk.newLines, "")
		return stateHunkBody, nil
	}
	if err := appendHunkLine(p.hunk, p.lines[i]); err != nil {
		return stateHunkBody, err
	}
	return stateHunkBody, nil
}

func parseUnifiedDiff(diff string) ([]patchFile, error) {
	// Normalize line endings: handle CRLF (Windows) and bare CR (legacy Mac).
	diff = strings.ReplaceAll(diff, "\r\n", "\n")
	diff = strings.ReplaceAll(diff, "\r", "\n")
	lines := strings.Split(diff, "\n")
	// strings.Split leaves a trailing empty element for newline-terminated input;
	// that is not a hunk body line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	p := &diffParser{
		lines:         lines,
		boundaryAhead: computeBoundaryAhead(lines),
		state:         stateFileHeader,
	}
	for i := range p.lines {
		var err error
		switch p.state {
		case stateFileHeader:
			p.state, err = p.handleFileHeaderLine(i)
		case stateHunkBody:
			p.state, err = p.handleHunkBodyLine(i)
		default: // stateDone: ignore everything after the delimiter.
		}
		if err != nil {
			return nil, err
		}
		if p.state == stateDone {
			return p.files, nil
		}
	}
	p.flushFile()
	return p.files, nil
}

// appendHunkLine appends one unified-diff body line to the hunk: a leading
// space marks context, "-" a removed line, "+" an added line. Anything else
// is a malformed hunk line (context lines need a leading space).
func appendHunkLine(hunk *patchHunk, line string) error {
	switch line[0] {
	case ' ':
		text := line[1:]
		hunk.oldLines = append(hunk.oldLines, text)
		hunk.newLines = append(hunk.newLines, text)
	case '-':
		hunk.oldLines = append(hunk.oldLines, line[1:])
	case '+':
		hunk.newLines = append(hunk.newLines, line[1:])
	default:
		return fmt.Errorf("malformed hunk line: %q (context lines need a leading space)", line)
	}
	return nil
}

// isGitPreamble reports whether line is git metadata emitted between file
// sections (index/mode/rename/similarity headers).
func isGitPreamble(line string) bool {
	return strings.HasPrefix(line, "index ") ||
		strings.HasPrefix(line, "old mode ") ||
		strings.HasPrefix(line, "new mode ") ||
		strings.HasPrefix(line, "new file mode ") ||
		strings.HasPrefix(line, "deleted file mode ") ||
		strings.HasPrefix(line, "similarity index ") ||
		strings.HasPrefix(line, "rename from ") ||
		strings.HasPrefix(line, "rename to ")
}

// hunkComplete reports whether the hunk has already consumed its declared
// line counts (>=, not ==: LLM-generated counts are frequently off by one
// or more, and treating a nearly-full hunk as done beats swallowing the next
// file's header as a hunk body line).
func hunkComplete(h *patchHunk) bool {
	return h != nil &&
		len(h.oldLines) >= h.oldCount &&
		len(h.newLines) >= h.newCount
}

// isHeaderPairAhead reports whether the next non-empty line after lines[i]
// is a "+++ " header whose own next non-empty line starts a new hunk or
// file section (see isSectionMarkerAhead). It lets an incomplete hunk
// (declared counts exceeding its emitted lines — LLM under-counting) hand
// the parser back to file-header mode when a real next-file section follows
// WITHOUT a blank separator, instead of absorbing "--- a/b.txt" as a removed
// hunk line (which merged the two files' hunks into one).
//
// The lookahead deliberately requires the third line: a deleted "-- X" line
// immediately followed by an added "++ Y" line (wire "--- X" / "+++ Y") must
// stay hunk content unless a section marker follows. EOF is NOT accepted as
// that marker: a "--- X\n+++ Y" pair at the end of the diff is more likely a
// content pair in an incomplete hunk, and keeping it as content makes apply
// fail loudly instead of applying a partial hunk.
func isHeaderPairAhead(lines []string, i int) bool {
	j := i + 1
	for j < len(lines) && lines[j] == "" {
		j++
	}
	if j >= len(lines) || !strings.HasPrefix(lines[j], "+++ ") {
		return false
	}
	return isSectionMarkerAhead(lines, j)
}

// isSectionMarkerAhead reports whether the next non-empty line after
// lines[i] starts a hunk or file section: "@@", "--- ", or "diff --git ".
// It is the lookahead that lets an incomplete hunk (declared counts
// exceeding its emitted lines — LLM under-counting) hand the parser back
// to file-header mode when a real section boundary follows without a blank
// separator, instead of absorbing the header as hunk content. EOF and
// content lines are deliberately NOT accepted as markers: a header at the
// end of the diff is more likely a content pair in an incomplete hunk, and
// keeping it as content makes apply fail loudly instead of applying a
// partial hunk.
func isSectionMarkerAhead(lines []string, i int) bool {
	j := i + 1
	for j < len(lines) && lines[j] == "" {
		j++
	}
	if j >= len(lines) {
		return false
	}
	return strings.HasPrefix(lines[j], "@@") ||
		strings.HasPrefix(lines[j], "--- ") ||
		strings.HasPrefix(lines[j], "diff --git ")
}

// isPatchDelimiterMarker reports whether line is a model-added patch
// delimiter such as "*** End Patch", "***endpatch", "*** Start Patch" or a
// bare "***". Only letter/space text after the stars counts as a delimiter:
// context-format range headers (e.g. "*** 16,20 ***" / "*** 104,110 ****")
// contain digits and are NOT delimiters — swallowing them would silently
// drop real diff structure and replace a clear error with a confusing one.
func isPatchDelimiterMarker(line string) bool {
	if !strings.HasPrefix(line, "***") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimRight(line[3:], "*"))
	if rest == "" {
		return true
	}
	for _, r := range rest {
		if !unicode.IsLetter(r) && r != ' ' {
			return false
		}
	}
	return true
}

// isPatchEndMarker reports whether line is a model-added patch END
// delimiter: "*** End of diff", "***endpatch", "*** End of Patch",
// "***_END_OF_PATCH", or a bare "***". Start markers and other "***" text
// are deliberately excluded: only end markers (and narration lines, see
// isPatchNarrationLine) signal a degenerate marker-only reply, while a
// "*** Start Patch" preamble is harmless and common.
func isPatchEndMarker(line string) bool {
	if !isPatchDelimiterMarker(line) {
		return false
	}
	if strings.TrimSpace(line) == "***" {
		return true
	}
	return strings.Contains(strings.ToLower(line), "end")
}

// patchNarrationPhrases are status phrases patch tools print between failed
// apply attempts. They never appear in a genuine unified diff, so their
// presence marks a diff argument polluted with a previous patch session's
// transcript.
var patchNarrationPhrases = []string{
	"did not fully apply cleanly",
	"did not reach the end of the file",
	"rest of the patch not applied",
	"removed one hunk",
	"hunk still not applied",
	"unapplied hunks",
	"assigned the panic",
}

// isPatchNarrationLine reports whether line is patch-session narration
// (failure status lines quoted from a previous apply attempt) rather than
// diff content.
func isPatchNarrationLine(line string) bool {
	l := strings.ToLower(line)
	for _, p := range patchNarrationPhrases {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// detectMarkerOnlyDiff reports whether diff consists of patch framing —
// end delimiters ("*** End of diff" …) and failure narration — with no
// actual patch content: no "--- " or "+++ " file headers and no "@@" hunk
// headers. A model stuck in a patch retry loop eventually emits such diffs
// (every hunk dropped, only the framing remains); executeTool treats the
// reply as degenerate and stops the turn.
func detectMarkerOnlyDiff(diff string) bool {
	diff = strings.ReplaceAll(diff, "\r\n", "\n")
	diff = strings.ReplaceAll(diff, "\r", "\n")
	frames := 0
	hasContent := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "@@"):
			hasContent = true
		default:
			t := strings.TrimSpace(line)
			if isPatchEndMarker(t) || isPatchNarrationLine(t) {
				frames++
			}
		}
	}
	return frames >= 3 && !hasContent
}

// isContextRangeHeader reports whether line is a context-format (diff -c)
// hunk range header: "*** 30,34 ****" / "*** 16,20 ***" for the old side and
// "--- 30,34 ----" for the new side. Models sometimes switch to context
// format (or emit a context diff outright); the parser only accepts unified
// format, so these must fail loudly with a format-specific message instead
// of being absorbed as a file header ("--- 30,34 ----"), a removed line, or
// a patch delimiter. The trailing star/dash runs are required so a file
// literally named "30,34" (a "--- 30,34" header) still parses.
func isContextRangeHeader(line string) bool {
	var rest string
	switch {
	case strings.HasPrefix(line, "***") && strings.Contains(line[3:], "***"):
		// Old side: "*** 30,34 ****" or "*** 16,20 ***".
		rest = strings.TrimSpace(strings.TrimPrefix(line, "***"))
		rest = strings.TrimSuffix(rest, "****")
		rest = strings.TrimSuffix(rest, "***")
		rest = strings.TrimSpace(rest)
	case strings.HasPrefix(line, "--- ") && strings.HasSuffix(line, "----"):
		// New side: "--- 30,34 ----".
		rest = strings.TrimSpace(strings.TrimPrefix(line, "--- "))
		rest = strings.TrimSuffix(rest, "----")
		rest = strings.TrimSpace(rest)
	default:
		return false
	}
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && r != ',' {
			return false
		}
	}
	return true
}

// isContextSeparator reports whether line is the context-format (diff -c)
// section separator: a run of 15 asterisks ("***************"). Unified
// format never produces an unprefixed all-star line (a star-only context
// line would carry a leading space, a removed/added line a -/+ prefix), so
// an all-star run is an unambiguous switch-to-context-format signal that
// must fail loudly instead of silently ending the patch like a "***"
// delimiter marker.
func isContextSeparator(line string) bool {
	return len(line) >= 15 && strings.Trim(line, "*") == ""
}

// contextFormatError explains a context-format (diff -c) construct in an
// actionable way: the parser only accepts unified format, so the model must
// regenerate the diff instead of retrying the same format.
func contextFormatError(line string) error {
	return fmt.Errorf("diff uses context format (diff -c) at %q: patch_file only accepts unified format ('--- a/x'/'+++ b/x' headers with '@@ -start,count +start,count @@' hunks). Regenerate the diff in unified format", line)
}

// isBareCodeFence reports whether line is a bare markdown code fence
// ("```"), which models sometimes append to close a diff. The line must
// START with the backticks — a space-prefixed " ```" is a real hunk context
// line and must be kept. Fences with a language tag ("```go") are not
// treated as closers either.
func isBareCodeFence(line string) bool {
	if !strings.HasPrefix(line, "```") {
		return false
	}
	return strings.TrimSpace(line) == "```"
}

// computeBoundaryAhead precomputes, for every index i, whether the next
// non-empty line at or after i starts a new file section (or only trailing
// blanks remain). It is built in one backward pass so the parser's blank-line
// handling is O(1) per blank line instead of re-scanning forward for each one
// (a quadratic loop on blank-heavy generated diffs).
func computeBoundaryAhead(lines []string) []bool {
	boundary := make([]bool, len(lines)+1)
	// Past the end of the diff only trailing blanks remain, so a blank line
	// at the very end (i == len(lines)-1, checked via boundary[i+1]) must
	// terminate the hunk. Without this the LAST blank would be absorbed as an
	// empty context line while a run of two or more trailing blanks flushes
	// the hunk — an off-by-one that made patches ending in a single extra
	// blank line fail spuriously (or inject a stray blank at EOF when the
	// file itself ended with a blank line).
	boundary[len(lines)] = true
	nextBoundary := true // past the end: only trailing blanks remain
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] == "" {
			boundary[i] = nextBoundary
			continue
		}
		nextBoundary = strings.HasPrefix(lines[i], "--- ") || strings.HasPrefix(lines[i], "diff --git ")
		boundary[i] = nextBoundary
	}
	return boundary
}

func parseHunkHeader(line string) (patchHunk, error) {
	// Accept both "@@ -1,4 +1,5 @@" and compacted "@@-1,4 +1,5@@".
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "@@"))
	rest = strings.TrimSuffix(rest, "@@")
	rest = strings.TrimSpace(rest)
	parts := strings.Fields(rest)
	if len(parts) < 2 {
		return patchHunk{}, fmt.Errorf("invalid hunk header: %q", line)
	}
	oldPart := strings.TrimPrefix(parts[0], "-")
	newPart := strings.TrimPrefix(parts[1], "+")
	if oldPart == parts[0] || newPart == parts[1] || oldPart == "" || newPart == "" {
		return patchHunk{}, fmt.Errorf("invalid hunk header: %q", line)
	}
	oldStart, oldCount, err := parseDiffLineRange(oldPart)
	if err != nil {
		return patchHunk{}, err
	}
	newStart, newCount, err := parseDiffLineRange(newPart)
	if err != nil {
		return patchHunk{}, err
	}
	_ = newStart // only oldStart is used for positioning; counts drive hunkComplete
	return patchHunk{oldStart: oldStart, oldCount: oldCount, newCount: newCount}, nil
}

// parseDiffLineRange parses a hunk header range ("1" or "1,4") into
// (start, count). A missing count means 1; a missing start also means 1
// (both per unified-diff semantics).
func parseDiffLineRange(part string) (start, count int, err error) {
	if part == "" {
		return 1, 1, nil
	}
	num := part
	count = 1
	if idx := strings.IndexByte(part, ','); idx >= 0 {
		num = part[:idx]
		c, cErr := strconv.Atoi(part[idx+1:])
		if cErr != nil || c < 0 {
			return 0, 0, fmt.Errorf("invalid hunk line count %q", part)
		}
		count = c
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid hunk line number %q", part)
	}
	if n < 0 {
		return 0, 0, fmt.Errorf("invalid hunk line number %q: must be non-negative", part)
	}
	return n, count, nil
}

// applyPatchHunks applies hunks to original. originalTrailing reports
// whether the original file's final line ends with a newline; the returned
// outNoNewline reports the same property for the patched output, which
// joinLinesPreserveTrailing uses to reproduce git's trailing-newline
// semantics (a file that lacked a final newline must not gain one, and a
// "\ No newline at end of file" marker on the last added line must be
// honored). Only the final output line can lack a newline, so each step
// re-derives the flag from whichever chunk contributes the final line of
// the rebuilt output.
func applyPatchHunks(original []string, hunks []patchHunk, fuzzy bool, originalTrailing bool) (outLines []string, outNoNewline bool, hunkShifts []string, err error) {
	out := append([]string(nil), original...)
	outNoNewline = !originalTrailing
	lineDelta := 0
	for hi, h := range hunks {
		start := h.oldStart - 1 + lineDelta
		if start < 0 {
			start = 0
		}
		n := len(h.oldLines)
		if n == 0 {
			if len(h.newLines) > 0 {
				// A hunk with zero old lines is a pure INSERTION. In a
				// unified diff the header's old line number names the line
				// the insertion FOLLOWS ("@@ -5,0 +6,2 @@" inserts after
				// line 5), so the 0-based index to insert at is oldStart
				// itself — not oldStart-1, which landed every pure insertion
				// one line early. patch(1) positions these hunks from the old
				// side too, so a sloppily generated new-side number cannot
				// move the insert.
				start = h.oldStart + lineDelta
				if start < 0 {
					start = 0
				}
				if start > len(out) {
					start = len(out)
				}
				// Pre-allocate to avoid repeated slice growth.
				newCap := len(out) + len(h.newLines)
				newOut := make([]string, 0, newCap)
				newOut = append(newOut, out[:start]...)
				newOut = append(newOut, h.newLines...)
				newOut = append(newOut, out[start:]...)
				if start == len(out) {
					// Insertion at EOF: the last added line becomes the
					// file's final line.
					outNoNewline = h.newNoNewline
				}
				out = newOut
				lineDelta += len(h.newLines)
			}
			continue
		}
		inBounds := start <= len(out) && start+n <= len(out)
		if !inBounds && !fuzzy {
			if start > len(out) {
				return nil, false, nil, fmt.Errorf("hunk %d/%d starts at line %d but file has %d lines", hi+1, len(hunks), h.oldStart, len(out)-lineDelta)
			}
			return nil, false, nil, fmt.Errorf("hunk %d/%d extends past end of file (line %d)", hi+1, len(hunks), h.oldStart+n-1)
		}
		hint := start
		if hint > len(out) {
			hint = len(out)
		}
		var actual []string
		if inBounds {
			actual = out[start : start+n]
		} else if hint < len(out) {
			actual = out[hint:]
		}
		matched, ambiguous := findHunkMatch(out, h.oldLines, hint, fuzzy)
		if matched < 0 {
			if ambiguous {
				return nil, false, nil, fmt.Errorf("hunk %d/%d context is ambiguous: fuzzy matching found multiple candidate locations; re-read the file and regenerate the diff with more surrounding context",
					hi+1, len(hunks))
			}
			return nil, false, nil, formatHunkMismatch(hi+1, len(hunks), hint+1, actual, h.oldLines, fuzzy)
		}
		// Track hunk relocation when fuzzy matching found the hunk at a
		// different position than the diff headers indicated. The shift is
		// measured against the un-clamped header position (oldStart), not the
		// clamped hint: when the header line number is stale and points past
		// EOF, hint is clamped to len(out) and reporting the clamp would show
		// a bogus "found at line" that matches neither the header nor the
		// actual match location.
		if shift := matched - (h.oldStart - 1 + lineDelta); shift != 0 {
			hunkShifts = append(hunkShifts, fmt.Sprintf("hunk %d shifted by %+d lines (expected around line %d, found at line %d)",
				hi+1, shift, h.oldStart+lineDelta, matched+1))
		}
		start = matched
		end := start + n
		// Pre-allocate output for this hunk: old[:start] + newLines + old[end:].
		replacement := h.newLines
		newCap := len(out) + len(replacement) - n
		newOut := make([]string, 0, newCap)
		newOut = append(newOut, out[:start]...)
		newOut = append(newOut, replacement...)
		newOut = append(newOut, out[end:]...)
		if end < len(out) {
			// The tail survives: the file's final line is unchanged.
		} else if len(replacement) > 0 {
			// The hunk reaches EOF and supplies the new final line.
			outNoNewline = h.newNoNewline
		} else {
			// Pure deletion of the file tail: the new final line is
			// out[start-1], a mid-file line, hence newline-terminated.
			outNoNewline = false
		}
		out = newOut
		lineDelta += len(replacement) - n
	}
	return out, outNoNewline, hunkShifts, nil
}

// findHunkMatch locates oldLines within lines. Returns the start index, or -1
// if no match is found. ambiguous is true when fuzzy relocation was refused
// because the hunk matched multiple positions. When fuzzy is true, relocation
// and whitespace-tolerant matching are attempted before giving up.
func findHunkMatch(lines, oldLines []string, hint int, fuzzy bool) (matched int, ambiguous bool) {
	n := len(oldLines)
	end := hint + n
	if end <= len(lines) && linesEqual(lines[hint:end], oldLines) {
		return hint, false
	}
	if !fuzzy {
		return -1, false
	}
	// Try a whitespace-tolerant match at the hinted position before
	// relocating. The hunk header line number anchors where the hunk belongs;
	// when the anchored text only differs by trailing whitespace (a common
	// LLM artifact), it is a stronger signal than an exact match somewhere
	// else in the file — relocating to the distant copy would silently edit
	// the wrong block.
	if end <= len(lines) && linesEqualFuzzy(lines[hint:end], oldLines) {
		return hint, false
	}
	// Try exact relocation (nearest candidate; the hunk header line number is
	// authoritative and the text matches exactly).
	if alt, _, ok := findHunkLocationWith(lines, oldLines, hint, linesEqual); ok {
		return alt, false
	}
	// Try relocation with whitespace-tolerant matching. Only apply it when the
	// hunk matches a single position — ambiguous matches would guess at the
	// target block and can silently corrupt the file, so fail loudly instead
	// and let the agent re-read and regenerate with more context.
	best, count, ok := findHunkLocationWith(lines, oldLines, hint, linesEqualFuzzy)
	if ok {
		if count == 1 {
			return best, false
		}
		return -1, true
	}
	return -1, false
}

func formatHunkMismatch(hunkNum, hunkTotal, line int, actual, expected []string, fuzzy bool) error {
	// firstDiff is -1 when the compared prefix contains no difference. That
	// happens when actual (the file's remaining lines) is a STRICT PREFIX of
	// the expected context — the file ran out before any line disagreed.
	// Quoting actual[0]/expected[0] there would print two IDENTICAL lines as
	// "the mismatch", so the diagnostic reports the shortfall instead.
	firstDiff := -1
	for i := 0; i < len(actual) && i < len(expected); i++ {
		if actual[i] != expected[i] {
			firstDiff = i
			break
		}
	}
	atLine := line
	if firstDiff >= 0 {
		atLine = line + firstDiff
	}
	msg := fmt.Sprintf("hunk %d/%d context mismatch at line %d", hunkNum, hunkTotal, atLine)
	switch {
	case firstDiff >= 0:
		msg += fmt.Sprintf(": file has %q, patch expects %q", actual[firstDiff], expected[firstDiff])
	case len(actual) == 0:
		msg += ": hunk context not found in file"
	case len(actual) < len(expected):
		msg += fmt.Sprintf(": file has %d line(s) here but the hunk expects %d context lines", len(actual), len(expected))
	default:
		msg += ": hunk context not found in file"
	}
	if !fuzzy {
		msg += " (fuzzy matching is disabled; re-read the file and regenerate the diff, or omit fuzzy=false)"
	} else {
		msg += " (re-read the file and regenerate the diff with exact current context)"
	}
	return fmt.Errorf("%s", msg)
}

// findHunkLocationWith locates oldLines within lines using the given comparison
// function. Returns the best match index (closest to hint), the number of
// matching positions, and whether any match was found. The best match is
// tracked in O(1) memory while scanning once.
func findHunkLocationWith(lines, oldLines []string, hint int, cmp func([]string, []string) bool) (int, int, bool) {
	n := len(oldLines)
	if n == 0 {
		return hint, 1, true
	}
	best, bestDist, count := -1, 0, 0
	first := oldLines[:1]
	for i := 0; i <= len(lines)-n; i++ {
		// Lossless candidate filter: a window can match only if its first
		// line matches oldLines[0] under the same comparator (both
		// comparators are element-wise), so windows whose first line cannot
		// match are skipped without paying the full window comparison. This
		// keeps the exact-match and fuzzy-relocation scans near-linear for
		// typical patches instead of O(file lines x hunk lines) per hunk.
		if !cmp(lines[i:i+1], first) {
			continue
		}
		if !cmp(lines[i:i+n], oldLines) {
			continue
		}
		count++
		if d := absInt(i - hint); best == -1 || d < bestDist {
			best, bestDist = i, d
		}
	}
	if count == 0 {
		return 0, 0, false
	}
	return best, count, true
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// linesEqualFuzzy is like linesEqual but normalises trailing whitespace
// on each line. This tolerates LLM-generated diffs that add spurious
// trailing spaces or tabs to context lines.
func linesEqualFuzzy(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimRight(a[i], " \t\r") != strings.TrimRight(b[i], " \t\r") {
			return false
		}
	}
	return true
}

func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitLinesPreserveTrailing splits file content into lines for patch
// matching and reports the line ending to restore on write.
//
// Only "\n" separates lines. A trailing "\r" is stripped from each line so a
// CRLF checkout still matches the LF-normalized diff, but the ending itself is
// returned as eol and written back by joinLinesPreserveTrailing — otherwise a
// one-line patch to a CRLF file rewrote every line ending in it. A BARE "\r"
// is not a separator: content that uses it (legacy Mac, and embedded \r in
// strings) stays on one line instead of being silently split into several.
func splitLinesPreserveTrailing(s string) (lines []string, eol string) {
	if s == "" {
		return nil, "\n"
	}
	eol = "\n"
	if strings.Contains(s, "\r\n") {
		eol = "\r\n"
	}
	lines = strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines, eol
}

// joinLinesPreserveTrailing joins lines, appending a trailing newline only
// when trailing is true, so files that end without a newline keep that state
// after patching (matching git apply rather than unconditionally adding one).
// eol is the file's dominant line ending (splitLinesPreserveTrailing's second
// result): added lines inherit it, so patching a CRLF file keeps CRLF.
func joinLinesPreserveTrailing(lines []string, trailing bool, eol string) string {
	if len(lines) == 0 {
		return ""
	}
	s := strings.Join(lines, eol)
	if trailing {
		s += eol
	}
	return s
}
