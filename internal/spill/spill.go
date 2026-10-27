// Package spill persists oversized tool output to disk so a capped tool
// result never loses data: the full text is saved to a private,
// session-scoped directory under the working dir's .gogen state dir, and
// the inline result becomes a head/tail preview plus a model-facing
// locator ("full output saved to <path>") with a retrieval hint. The model
// retrieves specific parts on demand with read_file (offset/limit) or
// search_code.
//
// Everything here is best-effort: callers fall back to the plain
// truncation path whenever a spill is unavailable (no session id, no
// working dir) or a save fails. A spill must never turn a lost-tail
// truncation into a tool error.
//
// Safety contract: the session directory is created 0700 and every spill
// file is opened O_WRONLY|O_CREATE|O_EXCL with 0600, so a planted symlink
// (or any pre-existing file) at the target name makes the open fail
// instead of redirecting the write; partial files are removed on error.
package spill

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"gogen/internal/contextmgr"
	"gogen/internal/randhex"
)

// Store spills tool output under the spill root, one session-<id>
// directory per session (see Dir). A Store is stateless (only the root
// path), so one can be constructed per save; the root is resolved at
// construction from the working dir (or the process-wide global root).
type Store struct {
	root string
}

// globalRoot is the process-wide spill root used in global mode ("Use
// ~/.local/share/gogen/ instead of project .gogen/"): set once at startup,
// before any spill is written, and read on every path computation so the
// session store's cleanup and the fork repoint resolve the SAME directory.
// Empty (the default) keeps the project-local root.
var globalRoot atomic.Pointer[string]

// SetGlobalRoot points the spill package at a process-wide root (global
// mode): "" restores the default <workingDir>/.gogen/spill. Called once at
// startup — the mode is fixed for the process lifetime — so it is not a live
// setting; tests may set and clear it.
func SetGlobalRoot(dir string) {
	if dir == "" {
		globalRoot.Store(nil)
		return
	}
	d := dir
	globalRoot.Store(&d)
}

// rootFor returns the effective spill root for a working dir.
func rootFor(workingDir string) string {
	if p := globalRoot.Load(); p != nil && *p != "" {
		return *p
	}
	return filepath.Join(workingDir, ".gogen", "spill")
}

// NewStore returns a spill store rooted at the effective spill root for
// workingDir (global mode → the configured global root), or nil when
// workingDir is empty (spilling unavailable).
func NewStore(workingDir string) *Store {
	if workingDir == "" {
		return nil
	}
	return &Store{root: rootFor(workingDir)}
}

// Dir returns the session-scoped spill directory for id:
// <root>/session-<id>. The raw id stays in the name on purpose — it mirrors
// the session store's .gogen/sessions/<id>.json, so a user can map spilled
// output back to its session by eye. Generated ids (session.NewID()) are
// 32 lowercase hex chars and pass through untouched; sessionDirName
// sanitizes anything else into a single safe path element. No I/O.
func (s *Store) Dir(sessionID string) string {
	return filepath.Join(s.root, "session-"+sessionDirName(sessionID))
}

// sessionDirPath returns the spill directory for one session under the
// effective root for workingDir (project or global mode — see rootFor):
// <root>/session-<sanitized id>. Package-level so non-Store callers (the
// session store's lifecycle cleanup) share the exact layout Store writes.
func sessionDirPath(workingDir, sessionID string) string {
	return filepath.Join(rootFor(workingDir), "session-"+sessionDirName(sessionID))
}

// RemoveSessionDir removes the spill directory of one session under
// <workingDir>/.gogen/spill — the session's whole spill tree. It is the
// lifecycle hook for session destruction: the session store calls it
// wherever it destroys a session's persisted state (user delete, count/age
// prune, nested cascade, sibling cap), matching the archive-sidecar
// contract. It is deliberately NOT called on fork or /new: a forked
// transcript copies the parent's locator lines verbatim, and those
// absolute paths must stay valid until the parent itself is deleted. A
// missing directory is not an error. An empty session id is a no-op.
func RemoveSessionDir(workingDir, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	err := os.RemoveAll(sessionDirPath(workingDir, sessionID))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// sessionDirName sanitizes a session id into one safe path element under
// the spill root. It applies the same rules the session store's
// validateSessionID enforces before writing <id>.json (no separators, no
// path traversal, single element) — locally, so an unsafe id reaching the
// agent through SetSessionID (a hand-edited snapshot, a test, a host) can
// still never escape the spill root. Real generated ids never hit the
// sanitizer, so findability is not affected. Two distinct unsafe ids could
// in principle sanitize to the same name ("a/b" vs "a_b"); the worst case
// is two sessions sharing a spill dir, whose files are randomly named —
// harmless.
func sessionDirName(sessionID string) string {
	const maxNameLen = 120
	var b strings.Builder
	b.Grow(len(sessionID))
	for i := 0; i < len(sessionID); i++ {
		c := sessionID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	if name == "" || name == "." || name == ".." {
		// Cosmetic only (the "session-" prefix already makes these safe
		// elements) — kept so no dir ever reads as a bare dot-name.
		name = "unnamed"
	}
	return name
}

// Save writes text to a new exclusive file in the session's spill dir and
// returns its absolute path. The session dir is created 0700 and the file
// 0600 (see the package safety contract). label is sanitized into the
// file name (e.g. the tool name). A partial file is removed on error.
func (s *Store) Save(sessionID, label string, text []byte) (string, error) {
	t := s.NewTarget(sessionID, label)
	if _, err := t.Write(text); err != nil {
		t.Finish() // removes any partial file (see Finish)
		return "", err
	}
	path, _, ok := t.Finish()
	if !ok {
		return "", fmt.Errorf("spill: file was never created")
	}
	return path, nil
}

// writeBufferSize is the bufio buffer coalescing a streaming producer's
// chunks before they reach the spill file: pipe reads deliver small
// chunks, and writing each straight through would cost one write syscall
// (and one SSD program/erase cycle's worth of metadata) per chunk.
const writeBufferSize = 64 << 10

// Target is a lazily-opened spill file for one streaming producer (a
// command's combined output). The file is opened on the first Write, so a
// producer that never overflows its in-memory cap touches no disk. A
// sticky error (open or write failure) disables the target: subsequent
// Writes return the same error and the caller falls back to plain
// truncation. Target is safe for concurrent use.
// MaxTargetBytes caps one spill file. A command that streams forever (`yes`)
// would otherwise fill the disk: the output is spilled precisely because it is
// too big for memory, so nothing else bounds it. Hitting the cap is reported to
// the writer as an error, which surfaces as the tool result.
const MaxTargetBytes int64 = 64 << 20

type Target struct {
	store     *Store
	sessionID string
	name      string

	mu      sync.Mutex
	f       *os.File
	bw      *bufio.Writer
	path    string
	written int64
	err     error
	done    bool
}

// NewTarget returns a streaming spill target that will persist to
// <sessionDir>/<label>-<random>.log. The file is NOT created here — it is
// opened by the first Write.
func (s *Store) NewTarget(sessionID, label string) *Target {
	return &Target{store: s, sessionID: sessionID, name: fileName(label)}
}

// Write appends p to the spill file, opening it on the first call.
// Satisfies io.Writer.
func (t *Target) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return 0, t.err
	}
	if t.done {
		return 0, fmt.Errorf("spill: target already finished")
	}
	if t.f == nil {
		dir := t.store.Dir(t.sessionID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.err = err
			return 0, err
		}
		path := filepath.Join(dir, t.name)
		// O_EXCL is the symlink defense: a planted symlink (or any
		// pre-existing file) at this exact name fails the open instead of
		// receiving the write.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.err = err
			return 0, err
		}
		t.f = f
		t.path = path
		t.bw = bufio.NewWriterSize(f, writeBufferSize)
	}
	if t.written >= MaxTargetBytes {
		t.err = fmt.Errorf("spill: output exceeded %d bytes; the rest is not being captured", MaxTargetBytes)
		return 0, t.err
	}
	// Writes land in the buffer, not the file: the file is only read back
	// after Finish (which flushes), never concurrently with an open
	// target, so buffering cannot expose stale content.
	n, err := t.bw.Write(p)
	t.written += int64(n)
	if err != nil {
		t.err = err
	}
	return n, err
}

// Path returns the spill file's path, empty until the first successful
// Write opened it.
func (t *Target) Path() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.path
}

// Finish flushes any buffered output, closes the spill file (if one was
// opened) and reports its path and total bytes written; ok is false when
// nothing was ever written OR when the target hit an error — a partial
// file is never advertised as "full output": it is removed so callers
// fall back to the plain truncation path. A flush failure counts as a
// target error for this purpose. Finish is idempotent by value; after it
// returns, the target is closed for good.
func (t *Target) Finish() (path string, total int64, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
	if t.bw != nil {
		// Flush before the error check so a flush failure (ENOSPC at
		// drain time) marks the target failed: the partial file is
		// removed below and ok=false reported, never a locator pointing
		// at a truncated file.
		if err := t.bw.Flush(); err != nil && t.err == nil {
			t.err = err
		}
		t.bw = nil
	}
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
	if t.path == "" {
		return "", 0, false
	}
	if t.err != nil {
		_ = os.Remove(t.path)
		t.path = ""
		return "", 0, false
	}
	return t.path, t.written, true
}

// Discard closes the spill file (if one was opened) and removes it, without
// flushing: the caller decided the persisted copy is not wanted — the write
// failed, or the result turned out not to need a locator (the cap changed
// mid-command). Best-effort by contract, like the rest of the package: a
// failed removal still leaves the descriptor closed. Unlike Finish, a
// successful discard never advertises a path. Idempotent; after it returns
// the target is closed for good.
func (t *Target) Discard() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
	if t.bw != nil {
		// Buffered-but-unwritten bytes are dropped with the file: nothing
		// reads it again.
		t.bw = nil
	}
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
	if t.path != "" {
		_ = os.Remove(t.path)
		t.path = ""
	}
}

// fileName sanitizes label into a spill file name:
// <label>-<random>.log. An empty (or fully sanitized-away) label becomes
// "output".
func fileName(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" || name == "." || name == ".." {
		name = "output"
	}
	return name + "-" + randhex.ID(4, "") + ".log"
}

// context plumbing: the executor reads the per-command spill target from
// the command's context (mirrors ToolOutputSink), so the shared executor
// stays session-agnostic and concurrent sessions spill into their own
// directories.

type targetKey struct{}

// ContextWithTarget returns a copy of ctx carrying t as the spill target
// for oversized command output.
func ContextWithTarget(ctx context.Context, t *Target) context.Context {
	return context.WithValue(ctx, targetKey{}, t)
}

// TargetFromContext returns the spill target attached to ctx, or nil when
// none was set.
func TargetFromContext(ctx context.Context) *Target {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(targetKey{}).(*Target)
	return t
}

// Preview builds the model-facing replacement for an oversized tool
// result: a rune-safe head, the locator line, and a rune-safe tail of
// content, all within max bytes. The locator carries the standard
// "\n… truncated (" prefix, so the context manager's idempotency pass
// leaves the preview untouched and the UI's truncated-results indicator
// still fires.
func Preview(content string, max int, path string, total int64) string {
	loc := locatorLine(path, total)
	// The locator is pre-rendered (locatorLine already Sprintf-formatted
	// the total size), so it passes through TruncateHeadTail verbatim:
	// that helper formats a marker only on explicit opt-in
	// (FormatDropped), and a literal '%' in the PATH (a working dir like
	// /home/u/proj%s) is never verb-parsed. budgets reserves the locator
	// inside max, so the result stays within the cap.
	headMax, tailMax, ok := budgets(max, len(loc))
	if !ok {
		// Degenerate cap (smaller than the locator): just the locator.
		// TruncateRuneSafe runs no format pass, so the raw locator goes
		// out as-is.
		return contextmgr.TruncateRuneSafe(loc, max)
	}
	// TruncateHeadTail keeps the rune-safe head/tail and inserts the
	// locator between them (the trailing newline puts the tail on its
	// own line). ForceMarker: the locator must appear even if the
	// content happens to fit both budgets — the full output is saved
	// either way.
	return contextmgr.TruncateHeadTail(content, contextmgr.TruncateHeadTailOptions{
		HeadBytes:   headMax,
		TailBytes:   tailMax,
		Marker:      loc + "\n",
		ForceMarker: true,
	})
}

// PreviewStreamed is Preview for a streamed spill (a command's output)
// where only the in-memory head prefix is available: the tail is read
// back from the persisted file with a bounded read, so the preview never
// pulls the full output into memory. total is the file's byte count. If
// the tail cannot be read, the head expands over the tail budget instead
// (the locator is still there; only the tail view is lost).
func PreviewStreamed(head string, max int, path string, total int64) string {
	loc := locatorLine(path, total)
	headMax, tailMax, ok := budgets(max, len(loc))
	if !ok {
		return contextmgr.TruncateRuneSafe(loc, max)
	}
	tail := readFileTail(path, tailMax)
	if tail == "" {
		// No tail view available: spend its budget on the head.
		headMax += tailMax
	}
	return compose(contextmgr.TruncateRuneSafe(head, headMax), tail, loc)
}

const (
	// LocatorMarker opens the saved-file path inside a locator line.
	// Exported so fork-time repointing (RepointFile / the agent's
	// RepointSpillLocators) can cheaply skip messages with no locator.
	LocatorMarker = "full output saved to "
	// locatorTail closes the path; it must never appear inside a real path.
	locatorTail = " — use read_file"
	// markerPrefix opens every locator line. It is contextmgr's standard
	// tool-result truncation marker (toolResultTruncationMarker); the
	// literal is repeated here like locatorLine's own format string, so the
	// parse below stays self-contained.
	markerPrefix = "\n… truncated ("
	// totalSuffix sits between the total size and the saved path in a
	// locator line (see locatorLine).
	totalSuffix = " bytes total; "
)

// locatorLine renders the model-facing locator: the standard truncation
// marker prefix, the total size, the spill path, and the retrieval hint.
func locatorLine(path string, total int64) string {
	return fmt.Sprintf(markerPrefix+"%d"+totalSuffix+"%s%s%s (offset/limit) or search_code on that path to retrieve specific parts)",
		total, LocatorMarker, path, locatorTail)
}

// locatorSpans returns the [start,end) byte spans of the spill paths of
// every locator-like line in s, in order. strict additionally requires the
// exact prologue locatorLine writes (the standard marker and "<N> bytes
// total; " immediately before the phrase), which is what distinguishes a
// real preview from text that merely quotes the marker — see LocatorPaths
// (strict) and RewriteLocators (permissive). Both modes require the
// retrieval hint after the path.
//
// The spans are what make the path list and the fork-time rewrite exact: the
// rewrite replaces the locator's own bytes, never a copy of the same path
// text elsewhere in the result.
func locatorSpans(s string, strict bool) [][2]int {
	var out [][2]int
	for from := 0; ; {
		i := strings.Index(s[from:], LocatorMarker)
		if i < 0 {
			return out
		}
		i += from
		pathStart := i + len(LocatorMarker)
		end := strings.Index(s[pathStart:], locatorTail)
		if end > 0 && (!strict || hasLocatorPrologue(s[:i])) {
			out = append(out, [2]int{pathStart, pathStart + end})
			from = pathStart + end
			continue
		}
		// Not a locator here (or an unterminated one): keep scanning after
		// this phrase — the tail may contain a complete locator.
		from = pathStart
	}
}

// hasLocatorPrologue reports whether prefix ends with the text a locator
// line carries between the standard truncation marker and the saved path:
// the marker, the decimal total, and "<N> bytes total; ".
func hasLocatorPrologue(prefix string) bool {
	if !strings.HasSuffix(prefix, totalSuffix) {
		return false
	}
	num := prefix[:len(prefix)-len(totalSuffix)]
	k := len(num)
	for k > 0 && num[k-1] >= '0' && num[k-1] <= '9' {
		k--
	}
	if k == len(num) {
		return false // no digits at all
	}
	return strings.HasSuffix(num[:k], markerPrefix)
}

// LocatorPaths returns the spill file paths embedded in s by locatorLine —
// one per intact locator, in order. The parse is STRICT (see locatorSpans):
// the surrounding structure must be exactly what locatorLine writes, so a
// body that merely quotes the marker or the path phrase (source code, docs,
// a log line) yields no paths. That is the property callers rely on to
// recognise an already-spilled result — an intact locator means the body is
// a preview, not a full stream.
func LocatorPaths(s string) []string {
	spans := locatorSpans(s, true)
	if len(spans) == 0 {
		return nil
	}
	out := make([]string, 0, len(spans))
	for _, sp := range spans {
		out = append(out, s[sp[0]:sp[1]])
	}
	return out
}

// RewriteLocators returns s with the path of every locator-like line
// replaced by resolve(path)'s result (which reports whether it produced a
// usable replacement; false keeps the original path). The rewrite runs on the
// parsed spans, so a copy of a path elsewhere in the content is never the one
// rewritten. Used by fork-time repointing (see RepointFile).
//
// Unlike LocatorPaths it accepts any marker-phrase + path + retrieval-hint
// shape, not just the exact prologue locatorLine writes: for repointing, a
// false positive is safe (RepointFile rejects any path outside the spill
// root) while a false negative would leave the child's locator dangling.
func RewriteLocators(s string, resolve func(path string) (string, bool)) string {
	spans := locatorSpans(s, false)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		next, ok := resolve(s[sp[0]:sp[1]])
		if !ok {
			continue
		}
		b.WriteString(s[prev:sp[0]])
		b.WriteString(next)
		prev = sp[1]
	}
	if prev == 0 {
		return s // every resolve declined
	}
	b.WriteString(s[prev:])
	return b.String()
}

// RepointFile links a spilled file into toSessionID's spill dir (same base
// name) and returns the new path. Used when a session is forked: the
// forked transcript references the ORIGINAL session's spill files, which
// the session store removes when that session is deleted
// (RemoveSessionDir) — repointing gives the child its own directory entry
// so its locators keep working afterwards. The hardlink shares the file's
// data (spill files are never rewritten after creation, so the two
// sessions can never diverge) and costs no space; a full copy is the
// fallback when links are unavailable. oldPath must be under the working
// dir's spill root — locator text lives in message content and must never
// trick a fork into linking arbitrary files. An already-existing target is
// returned as-is (idempotent, e.g. a re-fork). On failure the caller keeps
// the original locator (best-effort; the head/tail preview survives in the
// transcript regardless).
func RepointFile(workingDir, oldPath, toSessionID string) (string, error) {
	if workingDir == "" || toSessionID == "" || oldPath == "" {
		return "", fmt.Errorf("spill: repoint needs a working dir, target session, and path")
	}
	root, err := filepath.Abs(rootFor(workingDir))
	if err != nil {
		return "", err
	}
	absOld, err := filepath.Abs(oldPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, absOld)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("spill: %q is not under the spill root", oldPath)
	}
	newDir := sessionDirPath(workingDir, toSessionID)
	newPath := filepath.Join(newDir, filepath.Base(absOld))
	if _, err := os.Lstat(newPath); err == nil {
		return newPath, nil
	}
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Link(absOld, newPath); err == nil {
		return newPath, nil
	}
	// Hardlinks unavailable (cross-device mount, exotic filesystem): copy.
	data, err := os.ReadFile(absOld)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(newPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		// Remove the partial copy: a truncated file at newPath would satisfy
		// the Lstat check above on the NEXT repoint and be handed to the model
		// as if it were complete.
		_ = os.Remove(newPath)
		if werr != nil {
			return "", werr
		}
		return "", cerr
	}
	return newPath, nil
}

// budgets splits max into head and tail byte budgets after reserving the
// locator and the joining newline: two thirds head, one third tail (the
// opening context matters, but errors cluster at the end). ok is false
// when the cap cannot even hold the locator.
func budgets(max, locatorLen int) (headMax, tailMax int, ok bool) {
	budget := max - locatorLen - 1
	if budget <= 0 {
		return 0, 0, false
	}
	headMax = budget * 2 / 3
	return headMax, budget - headMax, true
}

// compose assembles head + locator (+ newline + tail). head and tail must
// already be within their budgets. (PreviewStreamed composes its parts
// itself because they come from different sources — in-memory head,
// on-disk tail — rather than one TruncateHeadTail cut.)
func compose(head, tail, loc string) string {
	out := head + loc
	if tail != "" {
		out += "\n" + tail
	}
	return out
}

// readFileTail returns the last max bytes of path as a string, advancing
// over a split UTF-8 rune at the read's start. Best-effort: any error
// returns "". The read is bounded (never the whole file).
func readFileTail(path string, max int) string {
	if max <= 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return ""
	}
	start := st.Size() - int64(max)
	if start < 0 {
		start = 0
	}
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return ""
	}
	// A tail cut can land mid-rune: skip the continuation bytes (at most
	// utf8.UTFMax-1 of them) so the preview never starts with invalid
	// UTF-8.
	cut := 0
	for cut < len(buf) && cut < utf8.UTFMax-1 && !utf8.RuneStart(buf[cut]) {
		cut++
	}
	return string(buf[cut:])
}
