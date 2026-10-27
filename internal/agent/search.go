package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gogen/internal/ioutil"
)

// SearchMatch is one structured hit from SearchCodeMatches (no context lines).
type SearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

const (
	searchMaxMatches     = 200
	searchMaxOutputBytes = 512 * 1024
	searchMaxFileBytes   = 1_000_000
	searchBinaryProbe    = 8192
)

// searchTimeout bounds a single SearchCode call.
const searchTimeout = 30 * time.Second

// replaceTreeTimeout bounds a ReplaceInTree call, which may scan many files
// and is given a longer budget than a plain search.
const replaceTreeTimeout = 60 * time.Second

// gitLsFilesTimeout bounds the git ls-files call inside search scoping.
const gitLsFilesTimeout = 15 * time.Second

var searchSkipDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"vendor":       {},
	"__pycache__":  {},
	".cursor":      {},
}

// shouldSkipSearchEntry mirrors ripgrep's default filtering: respect hidden
// dotfiles/dotdirs and skip common vendor trees. To search inside a hidden
// directory (e.g. .github), pass it as search_code's path argument.
func shouldSkipSearchEntry(name string, isDir bool) bool {
	if isDir {
		if _, skip := searchSkipDirs[name]; skip {
			return true
		}
	}
	return strings.HasPrefix(name, ".") && name != "."
}

// SearchCodeMatches returns structured matches (context_lines=0) for UI
// find-in-files. When ignoreCase is true, matching ignores letter case (rg -i
// / a (?i)-prefixed regex), the same semantics SearchCode uses.
// truncated is true when the result set hit search caps.
func (e *Executor) SearchCodeMatches(ctx context.Context, pattern, subpath, glob string, ignoreCase bool) (matches []SearchMatch, truncated bool, err error) {
	if err := validateSearchArgs(pattern, glob); err != nil {
		return nil, false, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	searchRoot, relPrefix, glob, err := e.resolveSearchScope(subpath, glob)
	if err != nil {
		return nil, false, err
	}
	// Structured results for the UI: skipped subtrees are not surfaced here
	// (nil skips); SearchCode carries the footer for the model.
	matches, truncated, _, err = e.searchStructured(ctx, searchRoot, relPrefix, pattern, glob, ignoreCase, nil)
	return matches, truncated, err
}

// validateSearchArgs checks pattern and glob for the common search entry
// points (SearchCode, SearchCodeMatches).
func validateSearchArgs(pattern, glob string) error {
	if pattern == "" {
		return fmt.Errorf("pattern is required")
	}
	if err := rejectLeadingDashArg("pattern", pattern); err != nil {
		return err
	}
	if glob != "" {
		if err := rejectLeadingDashArg("glob", glob); err != nil {
			return err
		}
	}
	return nil
}

// resolveSearchScope applies the single-file subpath shortcut (searching a
// file scopes the root to its directory and the glob to its basename) and
// returns the search root plus the workspace-relative prefix.
func (e *Executor) resolveSearchScope(subpath, glob string) (searchRoot, relPrefix, effectiveGlob string, err error) {
	effectiveGlob = glob
	if strings.TrimSpace(subpath) != "" {
		secure, err := e.SecurePath(subpath)
		if err == nil {
			info, err := os.Stat(secure)
			if err == nil && !info.IsDir() {
				// Use the file's directory as the search root and the
				// filename as an implicit glob (unless one is already set).
				if effectiveGlob == "" {
					effectiveGlob = filepath.Base(secure)
				}
				subpath = filepath.Dir(secure)
			}
		}
	}
	searchRoot, relPrefix, err = e.searchRoot(subpath)
	return searchRoot, relPrefix, effectiveGlob, err
}

// SearchCode finds pattern matches using system rg when available, otherwise
// a Go fallback. When ignoreCase is true, matching ignores letter case (rg -i,
// or a (?i)-prefixed regex in the Go fallback); literal patterns keep their
// literal meaning — they simply match any casing.
func (e *Executor) SearchCode(ctx context.Context, pattern, subpath, glob string, contextLines int, ignoreCase bool) (string, error) {
	if err := validateSearchArgs(pattern, glob); err != nil {
		return "", err
	}
	if contextLines < 0 {
		return "", fmt.Errorf("context_lines must be non-negative")
	}
	// contextLines is not pre-clamped: the searchMaxOutputBytes cap (with its
	// truncation notice) is the output backstop. A silent per-argument clamp
	// returned byte-identical output for every larger request, which sent
	// models into retry loops incrementing context_lines forever.
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	searchRoot, relPrefix, glob, err := e.resolveSearchScope(subpath, glob)
	if err != nil {
		return "", err
	}

	// skips accumulates unreadable-subtree errors from the Go-fallback walk
	// and is appended as a footer so a partial result is never presented as
	// complete. The rg path never walks, so it leaves skips empty.
	skips := &walkSkips{}

	// Context-free searches go through the structured path shared with
	// SearchCodeMatches; context-lines mode keeps the raw-output formatter.
	if contextLines == 0 {
		matches, truncated, engine, sErr := e.searchStructured(ctx, searchRoot, relPrefix, pattern, glob, ignoreCase, skips)
		if sErr != nil {
			return "", sErr
		}
		if len(matches) == 0 {
			if engine == "go" {
				return "No matches found (go fallback; install ripgrep for faster search)" + skips.footer(), nil
			}
			return "No matches found" + skips.footer(), nil
		}
		out := formatSearchMatches(matches)
		if engine == "go" {
			out += "\n(search: go fallback; install ripgrep for faster search)"
		}
		if truncated {
			out += fmt.Sprintf("\n… truncated (showing first %d matches)", len(matches))
		}
		return out + skips.footer(), nil
	}

	if rgAvailable() {
		out, rgErr := e.searchWithRipgrep(ctx, searchRoot, relPrefix, pattern, glob, contextLines, ignoreCase)
		if rgErr == nil {
			return out + skips.footer(), nil
		}
	}

	out, goErr := e.searchWithGo(ctx, searchRoot, relPrefix, pattern, glob, contextLines, ignoreCase, skips)
	if goErr != nil {
		return "", goErr
	}
	return out + skips.footer(), nil
}

// ReplaceInTree replaces every regex match of pattern with replacement under
// subpath (same pattern semantics as SearchCode). Unlike search, it is not
// capped by searchMaxMatches — every matching text file is updated.
// It also returns the workspace-relative slash paths of the files that were
// modified, so callers (e.g. the editor's open-buffer sync) can tell exactly
// which files changed on disk.
func (e *Executor) ReplaceInTree(ctx context.Context, pattern, replacement, subpath, glob string) (replaced, fileCount int, files []string, err error) {
	if pattern == "" {
		return 0, 0, nil, fmt.Errorf("search pattern is required")
	}
	if err := rejectLeadingDashArg("pattern", pattern); err != nil {
		return 0, 0, nil, err
	}
	if glob != "" {
		if err := rejectLeadingDashArg("glob", glob); err != nil {
			return 0, 0, nil, err
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, replaceTreeTimeout)
	defer cancel()

	re, err := compileSearchPattern(pattern, false)
	if err != nil {
		return 0, 0, nil, err
	}

	// Single-file scope: replace only in that file.
	if strings.TrimSpace(subpath) != "" {
		secure, secErr := e.SecurePath(subpath)
		if secErr == nil {
			info, statErr := os.Stat(secure)
			if statErr == nil && !info.IsDir() {
				// Resolve the working dir the same way SecurePath does
				// (evalPath follows symlinks — e.g. macOS /var →
				// /private/var, Windows 8.3 short names). Comparing a
				// resolved path against an unresolved base makes
				// filepath.Rel emit a "../../…" climb instead of the
				// clean relative name.
				absWD, absErr := evalPath(e.GetWorkingDir())
				if absErr != nil {
					return 0, 0, nil, absErr
				}
				rel, relErr := filepath.Rel(absWD, secure)
				if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					rel = subpath
				}
				rel = filepath.ToSlash(rel)
				n, writeErr := e.replaceInFilePath(secure, rel, re, replacement)
				if writeErr != nil {
					return 0, 0, nil, writeErr
				}
				if n > 0 {
					return n, 1, []string{rel}, nil
				}
				return 0, 0, nil, nil
			}
		}
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return 0, 0, nil, err
	}
	glob = strings.TrimSpace(glob)

	err = walkTree(ctx, searchRoot, relPrefix, walkOpts{glob: glob, checkReadable: true}, func(path, rel string, d os.DirEntry) error {
		n, writeErr := e.replaceInFilePath(path, rel, re, replacement)
		if writeErr != nil {
			return writeErr
		}
		if n > 0 {
			replaced += n
			fileCount++
			files = append(files, rel)
		}
		return nil
	})
	return replaced, fileCount, files, err
}

func (e *Executor) replaceInFilePath(absPath, relPath string, re *regexp.Regexp, replacement string) (int, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return 0, fmt.Errorf("failed to read %s: %w", relPath, err)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return 0, nil
	}
	content := string(data)
	locs := re.FindAllStringIndex(content, -1)
	if len(locs) == 0 {
		return 0, nil
	}
	newContent := re.ReplaceAllString(content, replacement)
	if newContent == content {
		return 0, nil
	}
	if err := ioutil.WriteFileAtomic(absPath, []byte(newContent), defaultFilePerm); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", relPath, err)
	}
	return len(locs), nil
}

func (e *Executor) searchRoot(subpath string) (absRoot, relPrefix string, err error) {
	if strings.TrimSpace(subpath) == "" {
		abs, err := filepath.Abs(e.GetWorkingDir())
		return abs, "", err
	}
	secure, err := e.SecurePath(subpath)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(secure)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("search path must be a directory: %s", subpath)
	}
	// evalPath (not filepath.Abs): secure is symlink-resolved, so the base
	// must be resolved the same way or filepath.Rel emits a "../../…"
	// climb (macOS /var → /private/var, Windows 8.3 short names).
	absWD, err := evalPath(e.GetWorkingDir())
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(absWD, secure)
	if err != nil {
		return "", "", err
	}
	return secure, rel, nil
}

// runRipgrep runs rg with the standard search arguments and returns the
// trimmed combined output, capped at searchMaxOutputBytes. rg's --max-count
// is PER FILE, so a repo with many matching files could otherwise produce
// unbounded output; the writer keeps the first searchMaxOutputBytes and the
// overflowed flag reports whether anything was dropped. A non-zero exit
// (including rg's exit code 1 for "no matches") is returned as an
// *exec.ExitError; callers distinguish the no-match case via errors.As.
func runRipgrep(ctx context.Context, searchRoot, pattern, glob string, contextLines int, ignoreCase bool) (text string, overflowed bool, err error) {
	args := []string{
		"-n",
		"--no-heading",
		"--color=never",
		"--max-count", fmt.Sprintf("%d", searchMaxMatches),
		"--max-columns", "500",
	}
	if ignoreCase {
		args = append(args, "-i")
	}
	if contextLines > 0 {
		args = append(args, "-C", fmt.Sprintf("%d", contextLines))
	}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	// "--" prevents patterns like --pre=… from being treated as rg flags.
	args = append(args, "--", pattern, ".")

	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = searchRoot
	out := newCommandOutputWriter("rg", nil, searchMaxOutputBytes, nil)
	cmd.Stdout = out
	cmd.Stderr = out
	runErr := cmd.Run()
	return strings.TrimSpace(out.String()), out.overflowed, runErr
}

// appendRgTruncationNotice appends the standard truncation footer when rg
// output was capped at searchMaxOutputBytes (see runRipgrep).
func appendRgTruncationNotice(out string, overflowed bool) string {
	if !overflowed {
		return out
	}
	return out + fmt.Sprintf("\n… truncated (output exceeds %d bytes)", searchMaxOutputBytes)
}

// runRipgrepChecked runs rg and interprets its exit status in one place:
// no matches (exit code 1 with empty output) is reported via noMatches,
// a context cancellation is returned as ctx.Err() (with any partial output
// preserved in text), and a failed run that still produced output is
// treated as success with what was captured. Any other error is wrapped
// as "rg failed".
func runRipgrepChecked(ctx context.Context, searchRoot, pattern, glob string, contextLines int, ignoreCase bool) (text string, overflowed, noMatches bool, err error) {
	text, overflowed, runErr := runRipgrep(ctx, searchRoot, pattern, glob, contextLines, ignoreCase)
	if runErr != nil {
		if ctx.Err() != nil {
			return text, overflowed, false, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 && text == "" {
			return "", false, true, nil
		}
		if text == "" {
			return "", false, false, fmt.Errorf("rg failed: %w", runErr)
		}
	}
	if text == "" {
		return "", false, true, nil
	}
	return text, overflowed, false, nil
}

func (e *Executor) searchWithRipgrep(ctx context.Context, searchRoot, relPrefix, pattern, glob string, contextLines int, ignoreCase bool) (string, error) {
	text, overflowed, noMatches, err := runRipgrepChecked(ctx, searchRoot, pattern, glob, contextLines, ignoreCase)
	if err != nil {
		if ctx.Err() != nil && text != "" {
			return formatSearchOutput("rg", relPrefix, text), ctx.Err()
		}
		return "", err
	}
	if noMatches {
		return "No matches found", nil
	}
	return appendRgTruncationNotice(formatSearchOutput("rg", relPrefix, text), overflowed), nil
}

// searchWithRipgrepMatches runs ripgrep and returns structured matches
// (context_lines=0 semantics), capped at the same total-match and byte
// budgets as the Go fallback so both engines report identical limits.
// Nil matches mean "no matches found"; truncated reports that the result
// set hit a cap; an error means the caller should fall back to the Go
// walker.
func (e *Executor) searchWithRipgrepMatches(ctx context.Context, searchRoot, relPrefix, pattern, glob string, ignoreCase bool) ([]SearchMatch, bool, error) {
	text, overflowed, noMatches, err := runRipgrepChecked(ctx, searchRoot, pattern, glob, 0, ignoreCase)
	if err != nil {
		return nil, false, err
	}
	if noMatches {
		return nil, false, nil
	}
	ms, truncated := capRgMatches(parseRgMatches(text, relPrefix))
	return ms, truncated || overflowed, nil
}

// searchMatchLineLen reports the rendered byte length of a structured
// match's "path:line:content" line, excluding the trailing newline. Both
// search engines (rg and Go fallback) account match sizes against
// searchMaxOutputBytes with this single expression.
func searchMatchLineLen(m SearchMatch) int {
	return len(m.Path) + 1 + len(strconv.Itoa(m.Line)) + 1 + len(m.Text)
}

// capRgMatches applies the same total-match and byte budgets the Go
// fallback uses (searchMaxMatches matches, searchMaxOutputBytes of rendered
// "path:line:content" lines), so the rg engine reports identical limits and
// the truncation flag becomes truthful.
func capRgMatches(matches []SearchMatch) ([]SearchMatch, bool) {
	truncated := false
	var size int
	out := make([]SearchMatch, 0, len(matches))
	for _, m := range matches {
		if len(out) >= searchMaxMatches {
			truncated = true
			break
		}
		lineLen := searchMatchLineLen(m)
		if size+lineLen+1 > searchMaxOutputBytes {
			truncated = true
			break
		}
		out = append(out, m)
		size += lineLen + 1
	}
	return out, truncated
}

// parseRgMatches converts rg's "path:line:content" output into structured
// matches, applying relPrefix to each path.
func parseRgMatches(text, relPrefix string) []SearchMatch {
	var matches []SearchMatch
	for _, line := range strings.Split(text, "\n") {
		path, lineNum, content, ok := splitSearchMatchLine(line)
		if !ok {
			continue
		}
		// Normalize to forward slashes on every platform: rg prints
		// backslash paths on Windows for a root search (e.g.
		// ".\internal\target.go"), while the Go fallback always emits
		// workspace-relative slash paths. The leading "./" goes too: a root
		// search prints every path with it, which made rg paths differ from
		// the walker's ("app/main.go") for the same file.
		path = strings.TrimPrefix(filepath.ToSlash(path), "./")
		if relPrefix != "" {
			path = filepath.ToSlash(filepath.Join(relPrefix, path))
		}
		matches = append(matches, SearchMatch{Path: path, Line: lineNum, Text: content})
	}
	return matches
}

// splitSearchLineParts splits ripgrep's "filepathSepNumSepRest" output into
// its parts. sep1 and sep2 are the separator characters: ':' for matched
// lines and '-' for context lines. ok is false when the line does not match
// the "path + separator + digits + separator" shape. Both splitSearchLine
// and splitSearchMatchLine are derived from this single parser so the line
// format is only parsed in one place.
func splitSearchLineParts(line string) (file string, sep1 byte, num string, sep2 byte, rest string, ok bool) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c != ':' && c != '-' {
			continue
		}
		j := i + 1
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j > i+1 && j < len(line) && (line[j] == ':' || line[j] == '-') {
			return line[:i], c, line[i+1 : j], line[j], line[j+1:], true
		}
	}
	return "", 0, "", 0, "", false
}

// splitSearchMatchLine splits rg output "path:line:content" into its parts.
// The separator is ':' — context-free output has no '-' separators.
func splitSearchMatchLine(line string) (path string, lineNum int, content string, ok bool) {
	file, sep1, num, sep2, rest, ok := splitSearchLineParts(line)
	if !ok || sep1 != ':' || sep2 != ':' {
		return "", 0, "", false
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return "", 0, "", false
	}
	return file, n, rest, true
}

// rgAvailable reports whether ripgrep is on PATH, memoized per PATH value: it
// is consulted once per search (twice in the structured path) and a PATH
// lookup each time is a needless pair of syscalls. Keying on the PATH string
// keeps a test that swaps PATH — or a user installing rg mid-session — exact.
var rgAvail struct {
	mu    sync.Mutex
	path  string
	found bool
	valid bool
}

func rgAvailable() bool {
	path := os.Getenv("PATH")
	rgAvail.mu.Lock()
	defer rgAvail.mu.Unlock()
	if rgAvail.valid && rgAvail.path == path {
		return rgAvail.found
	}
	_, err := exec.LookPath("rg")
	rgAvail.path, rgAvail.found, rgAvail.valid = path, err == nil, true
	return rgAvail.found
}

// searchStructured runs the search through ripgrep when available, else the
// Go fallback, and returns structured matches (context_lines=0 semantics).
// engine is "rg" or "go" so callers can reproduce the per-engine output
// footer SearchCode shows.
func (e *Executor) searchStructured(ctx context.Context, searchRoot, relPrefix, pattern, glob string, ignoreCase bool, skips *walkSkips) (matches []SearchMatch, truncated bool, engine string, err error) {
	if rgAvailable() {
		ms, trunc, rgErr := e.searchWithRipgrepMatches(ctx, searchRoot, relPrefix, pattern, glob, ignoreCase)
		if rgErr == nil {
			return ms, trunc, "rg", nil
		}
		// Real rg failure — fall back to the Go walker (matches SearchCode).
	}
	ms, truncated, err := e.searchWithGoMatches(ctx, searchRoot, relPrefix, pattern, glob, ignoreCase, skips)
	return ms, truncated, "go", err
}

func prefixRelPaths(body, relPrefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			b.WriteByte('\n')
			continue
		}
		// ripgrep's context separator ("--") is not a path; pass it through
		// unchanged so compactSearchOutput still recognizes it.
		if line == "--" {
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		// Join ONLY the path portion with relPrefix. The old code split on
		// the first ':' alone, so context lines ("path-NUM-content", no ':'
		// before the content) fed the WHOLE line into filepath.Join — and
		// filepath.Clean collapsed "//" in the content to "/" (Go comments
		// arrived at the model as "/ comment"). splitSearchLineParts handles both
		// separators and is the same parse compactSearchOutput applies.
		file, sep1, num, sep2, tail, ok := splitSearchLineParts(line)
		if !ok {
			// Not a structured line; pass through unchanged.
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		b.WriteString(filepath.ToSlash(filepath.Join(relPrefix, file)))
		b.WriteByte(sep1)
		b.WriteString(num + string(sep2) + tail)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// errSearchWalkStop stops walkSearchFiles early once the match/output
// caps are reached. It is swallowed by the walker, so callers only see it as
// a nil error.
var errSearchWalkStop = errors.New("search walk stopped")

// walkSearchFiles walks searchRoot with the Go fallback semantics (glob
// filter, readable-file probe) and scans each searchable file with scan,
// collecting up to searchMaxMatches items and searchMaxOutputBytes of
// rendered output (size reports each item's byte cost including its
// trailing newline). truncated is true when either cap was hit. The pattern
// is compiled once and the glob trimmed once by the walker, so the formatted
// and structured search paths cannot drift on setup. scan errors are
// swallowed (the file is skipped), matching the historical behavior.
func walkSearchFiles[T any](ctx context.Context, searchRoot, relPrefix, pattern, glob string, ignoreCase bool, skips *walkSkips,
	scan func(reader io.Reader, rel string, re *regexp.Regexp, matchLimit int) ([]T, error),
	size func(item T) int) (items []T, truncated bool, err error) {
	re, err := compileSearchPattern(pattern, ignoreCase)
	if err != nil {
		return nil, false, err
	}
	glob = strings.TrimSpace(glob)

	var bytesUsed int
	err = walkTree(ctx, searchRoot, relPrefix, walkOpts{glob: glob, checkReadable: true, onSkip: skips.observe}, func(path, rel string, d os.DirEntry) error {
		limit := searchMaxMatches - len(items)
		if limit <= 0 {
			// Match cap reached: stop the walk and report truncation.
			truncated = true
			return errSearchWalkStop
		}
		reader, closer, binary, openErr := openSearchableFile(path)
		if openErr != nil {
			return nil
		}
		defer closer.Close()
		if binary {
			return nil
		}
		fileItems, scanErr := scan(reader, rel, re, limit)
		if scanErr != nil {
			return nil
		}
		for _, item := range fileItems {
			if len(items) >= searchMaxMatches {
				truncated = true
				return errSearchWalkStop
			}
			if bytesUsed+size(item) > searchMaxOutputBytes {
				truncated = true
				return errSearchWalkStop
			}
			items = append(items, item)
			bytesUsed += size(item)
		}
		return nil
	})
	if errors.Is(err, errSearchWalkStop) {
		return items, truncated, nil
	}
	return items, truncated, err
}

// searchWithGo runs the Go fallback walker with context lines and returns
// the rendered output. The match cap, byte budget, and file probing are
// shared with searchWithGoMatches via walkSearchFiles; only the per-file
// scanner (scanFileSinglePass) and the rendering differ.
func (e *Executor) searchWithGo(ctx context.Context, searchRoot, relPrefix, pattern, glob string, contextLines int, ignoreCase bool, skips *walkSkips) (string, error) {
	lines, truncated, err := walkSearchFiles(ctx, searchRoot, relPrefix, pattern, glob, ignoreCase, skips,
		func(reader io.Reader, rel string, re *regexp.Regexp, matchLimit int) ([]string, error) {
			return scanFileSinglePass(reader, rel, re, contextLines, matchLimit)
		},
		func(line string) int { return len(line) + 1 })
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "No matches found (go fallback; install ripgrep for faster search)", nil
	}
	out := formatSearchOutput("go", relPrefix, strings.Join(lines, "\n"))
	if truncated {
		out += fmt.Sprintf("\n… truncated (showing first %d matches)", len(lines))
	}
	return out, nil
}

// searchWithGoMatches runs the Go fallback walker and returns structured
// matches (context_lines=0 semantics) plus the truncation flag. Paths are
// already workspace-relative with relPrefix applied by walkTree.
func (e *Executor) searchWithGoMatches(ctx context.Context, searchRoot, relPrefix, pattern, glob string, ignoreCase bool, skips *walkSkips) ([]SearchMatch, bool, error) {
	matches, truncated, err := walkSearchFiles(ctx, searchRoot, relPrefix, pattern, glob, ignoreCase, skips,
		scanFileMatches,
		func(m SearchMatch) int { return searchMatchLineLen(m) + 1 })
	if err != nil {
		return nil, false, err
	}
	return matches, truncated, nil
}

// scanFileMatches reads the file once (via the caller-provided reader, which
// already includes the binary probe) in a single streaming pass and returns
// structured matches (no context lines).
func scanFileMatches(r io.Reader, relPath string, re *regexp.Regexp, matchLimit int) ([]SearchMatch, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), searchMaxFileBytes)

	var out []SearchMatch
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if re.MatchString(scanner.Text()) {
			out = append(out, SearchMatch{Path: relPath, Line: lineNum, Text: scanner.Text()})
			if len(out) >= matchLimit {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// formatSearchMatches renders structured matches in the same compact layout
// SearchCode shows: a file header line followed by "line:content" lines,
// grouped by file in match order. Paths are already workspace-relative.
func formatSearchMatches(matches []SearchMatch) string {
	var b strings.Builder
	prevFile := ""
	for _, m := range matches {
		if m.Path != prevFile {
			b.WriteString(m.Path)
			b.WriteByte('\n')
			prevFile = m.Path
		}
		fmt.Fprintf(&b, "%d:%s\n", m.Line, m.Text)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// compileSearchPattern compiles pattern as a regex, falling back to a quoted
// literal when it is not valid regex syntax. With ignoreCase, the (?i) inline
// flag is prepended to the validated source so it applies to regex and
// literal patterns alike; the flag is part of the regex memo key, so
// case-sensitive and case-insensitive compiles of one pattern stay separate.
func compileSearchPattern(pattern string, ignoreCase bool) (*regexp.Regexp, error) {
	re, err := compiledRegex(pattern)
	if err != nil {
		re, err = compiledRegex(regexp.QuoteMeta(pattern))
		if err != nil {
			return nil, fmt.Errorf("invalid search pattern: %w", err)
		}
	}
	if ignoreCase {
		re, err = compiledRegex("(?i)" + re.String())
		if err != nil {
			return nil, fmt.Errorf("invalid search pattern: %w", err)
		}
	}
	return re, nil
}

// scanFileSinglePass reads the file once (via the caller-provided reader,
// which already includes the binary probe), finds matches, and emits results
// with context lines (contextLines > 0). The context-free case is handled by
// scanFileMatches.
//
// The scan is streaming: only a ring of the last contextLines lines plus the
// match list are held in memory, instead of buffering the whole file. A line
// j is emitted once no future match can mark it: a match at m marks lines
// m-contextLines..m+contextLines, so once the reader is past j+contextLines
// (j has left the ring), its status is final and is computed from the matches
// seen so far. Output is byte-identical to the two-pass buffered version,
// and the scan stops right after the matchLimit-th window is emitted.
func scanFileSinglePass(r io.Reader, relPath string, re *regexp.Regexp, contextLines, matchLimit int) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), searchMaxFileBytes)

	c := contextLines
	if c < 0 {
		c = 0
	}
	ring := make([]string, c)
	ringPos := 0
	ringFull := false
	var matchNums []int
	var out []string
	lineNum := 0
	// lo is a sliding pointer over the sorted matchNums; emitted line
	// numbers are strictly increasing, so it only moves forward.
	lo := 0

	// emit reports line j if it lies inside a match window; by the time it
	// is called, every match that can affect j has already been recorded.
	// The ':' separator is used for every emitted line, matching the original
	// two-pass formatter (whose '-' branch was unreachable: any unmarked
	// line in a window was set to ':').
	emit := func(j int, line string) {
		for lo < len(matchNums) && matchNums[lo] < j-c {
			lo++
		}
		if lo >= len(matchNums) || matchNums[lo] > j+c {
			return
		}
		out = append(out, fmt.Sprintf("%s:%d:%s", relPath, j, line))
	}

	if c == 0 {
		// No context requested: emit match lines as they are found.
		for scanner.Scan() {
			lineNum++
			if re.MatchString(scanner.Text()) {
				matchNums = append(matchNums, lineNum)
				emit(lineNum, scanner.Text())
				if len(matchNums) >= matchLimit {
					break
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		if len(matchNums) == 0 {
			return nil, nil
		}
		return out, nil
	}

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if re.MatchString(line) && len(matchNums) < matchLimit {
			// Stop collecting at matchLimit, like the buffered reference:
			// later matches must neither extend the emitted windows nor
			// delay the stop past the last needed window.
			matchNums = append(matchNums, lineNum)
		}
		if ringFull {
			// Line lineNum-c just left the ring; the next match is at the
			// earliest line lineNum+1, whose window starts at
			// lineNum+1-c > lineNum-c, so its status is final.
			emit(lineNum-c, ring[ringPos])
		}
		ring[ringPos] = line
		ringPos++
		if ringPos == c {
			ringPos = 0
			ringFull = true
		}
		if len(matchNums) >= matchLimit && ringFull && lineNum-c >= matchNums[len(matchNums)-1]+c {
			// Every line through the last needed window has been emitted;
			// the rest of the file cannot add matches or context.
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	// EOF: flush the remaining ring lines (oldest first); with no future
	// matches, their status is final.
	if ringFull {
		for i := 0; i < c; i++ {
			emit(lineNum-c+1+i, ring[(ringPos+i)%c])
		}
	} else {
		for i := 0; i < ringPos; i++ {
			emit(i+1, ring[i])
		}
	}
	if len(matchNums) == 0 {
		return nil, nil
	}
	return out, nil
}

// binaryProbePool reuses 8KB buffers for binary-file detection to avoid
// allocating a new buffer on every file walked during search.
var binaryProbePool = sync.Pool{
	New: func() any {
		b := make([]byte, searchBinaryProbe)
		return &b
	},
}

// openSearchableFile opens path and probes the first searchBinaryProbe bytes
// for NUL (binary detection) in the SAME open the scan will use. A binary
// file returns binary=true with a nil reader (the probe bytes are discarded,
// so binaries still cost only one 8KB read). A text file returns a reader
// over the whole file — the probe bytes followed by the rest — so the caller
// reads the file exactly once. The previous design probed with a separate
// open (isBinaryFile) and then opened the file again for the scan, doubling
// file opens on slow filesystems whenever ripgrep was unavailable.
//
// The caller always receives a valid closer (the open file) and owns the
// close, binary or not, so callers can defer Close unconditionally.
//
// The probe bytes are copied out of the pooled buffer before it is returned
// to the pool: the returned reader outlives the pool's ownership of the
// buffer, and another search goroutine may reuse the buffer as soon as it is
// Put. The Put is deferred so it runs only after this function's last read
// of the buffer (the NUL scan and the copy-out), on every return path.
func openSearchableFile(path string) (reader io.Reader, closer io.Closer, binary bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false, err
	}
	bp := binaryProbePool.Get().(*[]byte)
	buf := *bp
	defer binaryProbePool.Put(bp)
	n, _ := f.Read(buf)
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return nil, f, true, nil
		}
	}
	probe := make([]byte, n)
	copy(probe, buf[:n])
	return io.MultiReader(bytes.NewReader(probe), f), f, false, nil
}

// searchableWalkFile reports whether a walked file is worth reading: the
// stat succeeded, the entry is a REGULAR file, and it is within the search
// size cap. The binary probe is deferred to the read (openSearchableFile) so
// each file is opened exactly once; walkers silently skip files that fail this
// check.
//
// The regular-file requirement is the symlink containment: filepath.WalkDir
// lstats, so a symlink entry carries its own (tiny) size and the link target
// is never inspected. Without this check every reader followed it, so a link
// inside the workspace made the agent read outside the boundary (os.ReadFile
// on "x.go -> ~/.ssh/config"), block forever on a device ("x.go -> /dev/zero"
// grows until the process dies), and replace/rename turned the link into a
// copy of its target. Devices, FIFOs and sockets fail the same test.
func searchableWalkFile(path string, info os.FileInfo, err error) bool {
	if err != nil || info == nil || info.Size() > searchMaxFileBytes {
		return false
	}
	return info.Mode().IsRegular()
}

// compactSearchOutput rewrites lines of the form "filepath:line:content" or
// "filepath-line-content" so that the filepath appears on its own line only
// when it changes, and subsequent lines for the same file show just
// "line:content" (or "line-content" for context lines).  Separator lines
// like "--" are passed through unchanged.
func compactSearchOutput(body string) string {
	var b strings.Builder
	prevFile := ""
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			b.WriteByte('\n')
			continue
		}

		// Separator lines from ripgrep context output (e.g. "--").
		if line == "--" {
			b.WriteString("--\n")
			prevFile = ""
			continue
		}

		// Try to split off the leading filepath.
		// Matched lines use ':', context lines use '-'.
		// We look for "filepathSepNumSepRest" where sep is ':' or '-'.
		file, rest, ok := splitSearchLine(line)
		if !ok {
			// Not a structured line (e.g. truncation notice); pass through.
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}

		if file != prevFile {
			b.WriteString(file)
			b.WriteByte('\n')
			prevFile = file
		}
		b.WriteString(rest)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// splitSearchLine splits "filepathSepNumSepRest" into (filepath, "numSepRest", true).
// The separator sep is either ':' (matched line) or '-' (context line).
func splitSearchLine(line string) (file, rest string, ok bool) {
	file, _, num, sep, tail, ok := splitSearchLineParts(line)
	if !ok {
		return "", "", false
	}
	return file, num + string(sep) + tail, true
}

func formatSearchOutput(engine, relPrefix, body string) string {
	if engine == "rg" {
		body = prefixRelPaths(body, relPrefix)
	}
	body = compactSearchOutput(body)
	if engine == "go" {
		return body + "\n(search: go fallback; install ripgrep for faster search)"
	}
	return body
}
