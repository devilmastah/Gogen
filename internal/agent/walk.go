package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// walkOpts configures walkTree.
type walkOpts struct {
	// glob, when non-empty, restricts visits to files whose workspace-relative
	// path matches the glob (matchGlobPattern).
	glob string

	// checkReadable runs the stat/size/binary probe (searchableWalkFile)
	// before visiting files. Name-only walkers (GlobFiles, RepoOverview,
	// ListFiles, FindFile) leave it off.
	checkReadable bool

	// includeDirs also visits directories that pass skip filtering (after
	// their rel path is computed). Only ListFiles needs this, to emit
	// directory entries.
	includeDirs bool

	// includeHidden also visits hidden FILES (dotfiles). Hidden/vendor
	// DIRECTORIES are still pruned either way. Only GlobFiles uses this: as a
	// name-based discovery tool it must keep matching dotfiles (e.g. .env,
	// .gitignore via ".*" or "*.env"); content/listing walkers keep the
	// ripgrep-like default of skipping them.
	includeHidden bool

	// onSkip, when non-nil, is called for every non-fatal walk error: a path
	// that could not be stat'd, or a directory that could not be read
	// (permission denied, vanished mid-walk). The walk continues past the
	// error so one unreadable subtree never aborts a whole search, and the
	// callback lets the caller surface how much of the tree was skipped
	// instead of silently omitting it. rel is the workspace-relative slash
	// path (with relPrefix applied), matching what visit receives.
	onSkip func(rel string, err error)
}

// walkTree is the shared file-walk skeleton used by every Executor tree
// walker (ListFiles, GlobFiles, RepoOverview, SearchCode's Go fallback,
// ReplaceInTree, FindFile, walkSymbolReferences, walkSymbolReferencesText,
// findDefinitionAST, renameWithAST, renameWithText). One place owns the
// policy that would otherwise drift between the copies:
//
//   - hidden files are skipped unless opts.includeHidden is set, and
//     hidden/vendor dirs are always pruned via filepath.SkipDir,
//   - the root itself is never visited,
//   - ctx cancellation aborts the walk (when ctx is non-nil),
//   - rel is the workspace-relative slash path with relPrefix applied, and
//   - the glob filter and (optionally) the size/binary probe run before the
//     visitor is called.
//
// visit receives the absolute path, the workspace-relative path, and the
// DirEntry. Returning a sentinel error from visit (errExploreTruncated,
// errFindFileLimit) stops the walk and is propagated unchanged; any other
// non-nil error also stops the walk.
//
// Errors reported by WalkDir itself (an entry that cannot be stat'd, or a
// directory that cannot be read) are non-fatal: the walk skips past them and
// continues, so a single permission-denied subtree does not abort a search.
// They are reported through opts.onSkip (when set) rather than swallowed
// silently, so callers can tell the model/user that part of the tree was
// omitted. Beware that without an onSkip handler an unreadable directory is
// indistinguishable from an empty one.
func walkTree(ctx context.Context, searchRoot, relPrefix string, opts walkOpts, visit func(path, rel string, d os.DirEntry) error) error {
	// relOf renders a walk path as the workspace-relative slash path visit
	// sees (relPrefix applied). Shared by the visitor path and the onSkip
	// error path so both use one path convention.
	relOf := func(walkPath string) (string, bool) {
		rel, err := filepath.Rel(searchRoot, walkPath)
		if err != nil {
			return "", false
		}
		rel = filepath.ToSlash(rel)
		if relPrefix != "" {
			rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
		}
		return rel, true
	}
	return filepath.WalkDir(searchRoot, func(walkPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if opts.onSkip != nil {
				rel, ok := relOf(walkPath)
				if !ok {
					rel = filepath.ToSlash(walkPath)
				}
				opts.onSkip(rel, walkErr)
			}
			// Returning nil keeps walking the rest of the tree; the error is
			// surfaced through onSkip instead of aborting (see walkTree doc).
			return nil
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if walkPath == searchRoot {
			return nil
		}
		if d.IsDir() {
			if shouldSkipSearchEntry(d.Name(), true) {
				return filepath.SkipDir
			}
			if !opts.includeDirs {
				return nil
			}
		} else if !opts.includeHidden && shouldSkipSearchEntry(d.Name(), false) {
			return nil
		}
		rel, ok := relOf(walkPath)
		if !ok {
			return nil
		}
		if opts.glob != "" && !matchGlobPattern(opts.glob, rel) {
			return nil
		}
		if opts.checkReadable && !d.IsDir() {
			info, err := d.Info()
			if !searchableWalkFile(walkPath, info, err) {
				return nil
			}
		}
		return visit(walkPath, rel, d)
	})
}

// readFileBounded reads a walked file's content, capped at
// searchMaxFileBytes. The stat-based gate in searchableWalkFile can go stale
// (a file can grow between the stat and the read, and a device reports size 0
// forever), so the read itself is bounded: one extra byte is read to detect
// growth and the file is skipped rather than loaded.
func readFileBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, searchMaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	// Binary probe: these walkers feed regex and tree-sitter scans, where a NUL
	// byte means the "file" is an object/binary blob. Skipping it here stops
	// wasted work and keeps NUL-laden noise out of tool output (the search
	// fallback probes for the same thing in openSearchableFile).
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%s looks binary", path)
	}
	if len(data) > searchMaxFileBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte search cap", path, searchMaxFileBytes)
	}
	return data, nil
}

// walkSkips accumulates the non-fatal errors walkTree reports through
// walkOpts.onSkip so a walker can append one compact note to its
// model-facing output. A nil *walkSkips is a valid no-op receiver, so a
// caller that does not surface skips can pass nil. Repeated reports for the
// same path are collapsed (e.g. an AST walk and its text fallback both hit
// the same unreadable directory) so the count stays truthful. It is not safe
// for concurrent use; each walk owns one.
type walkSkips struct {
	seen     map[string]struct{}
	count    int
	firstRel string
	firstErr error
}

// observe records one skipped path; it is the value assigned to
// walkOpts.onSkip.
func (w *walkSkips) observe(rel string, err error) {
	if w == nil {
		return
	}
	if w.seen == nil {
		w.seen = make(map[string]struct{})
	}
	if _, dup := w.seen[rel]; dup {
		return
	}
	w.seen[rel] = struct{}{}
	w.count++
	if w.firstErr == nil {
		w.firstRel = rel
		w.firstErr = err
	}
}

// footer renders the skipped-path note appended to a walker's output, or ""
// when nothing was skipped.
func (w *walkSkips) footer() string {
	if w == nil || w.count == 0 {
		return ""
	}
	if w.count == 1 {
		return fmt.Sprintf("\n… skipped 1 unreadable path (%s: %s)", w.firstRel, walkErrText(w.firstErr))
	}
	return fmt.Sprintf("\n… skipped %d unreadable paths (first: %s: %s)", w.count, w.firstRel, walkErrText(w.firstErr))
}

// walkErrText extracts the OS-level reason from a walk error without the
// absolute path, which is redundant with the reported relative path and would
// leak the workspace location into tool output.
func walkErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
