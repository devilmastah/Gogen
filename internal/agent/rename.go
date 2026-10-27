package agent

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gogen/internal/ioutil"
	"gogen/internal/treesitter"
)

type RenameResult struct {
	FilesModified int
	Changes       []FileChange
}

type FileChange struct {
	Path         string
	LinesChanged []int
	Count        int
	OldCount     int
	NewCount     int
}

// RenameSymbol renames a symbol across all files.
// Uses tree-sitter for supported languages, falls back to word-boundary text search.
func (e *Executor) RenameSymbol(ctx context.Context, oldName, newName, subpath, glob string, dryRun bool) (string, error) {
	if oldName == "" || newName == "" {
		return "", fmt.Errorf("old_name and new_name are required")
	}
	if oldName == newName {
		return "", fmt.Errorf("old_name and new_name are the same")
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	var changes []FileChange

	// skips collects unreadable-subtree errors from the walk so a rename that
	// missed files under an unreadable directory is not reported as complete.
	skips := &walkSkips{}

	// Try tree-sitter first for supported languages
	if treesitter.Enabled() {
		changes, err = e.renameWithAST(ctx, searchRoot, relPrefix, glob, oldName, newName, dryRun, skips)
		if err == nil && len(changes) > 0 {
			return formatRenameResult(oldName, newName, changes, dryRun) + skips.footer(), nil
		}
	}

	// Fallback: word-boundary text search (works for all languages)
	changes, err = e.renameWithText(ctx, searchRoot, relPrefix, glob, oldName, newName, dryRun, skips)
	if err != nil {
		return "", err
	}

	return formatRenameResult(oldName, newName, changes, dryRun) + skips.footer(), nil
}

func (e *Executor) renameWithAST(ctx context.Context, searchRoot, relPrefix, glob, oldName, newName string, dryRun bool, skips *walkSkips) ([]FileChange, error) {
	var changes []FileChange

	err := e.walkReadableFiles(ctx, searchRoot, relPrefix, glob, skips, true, func(path, rel string, content []byte) error {
		// Uncapped on purpose: this list drives an in-place rewrite, so a
		// truncated search would rename only the first 200 occurrences in a
		// file and still report success.
		refs, err := treesitter.FindSymbolReferencesAll(path, content, oldName)
		if err != nil || len(refs) == 0 {
			return nil
		}

		// Refs are non-overlapping identifier spans sorted by Start; splice
		// exactly those spans into the file. Replacing whole lines with a
		// word-boundary regex would also rename unrelated occurrences of the
		// identifier (string literals, comments, doc text) on the same line,
		// making the AST path no more precise than the text fallback.
		var b strings.Builder
		b.Grow(len(content))
		last := 0
		replaced := 0
		linesChangedSet := make(map[int]struct{})
		for _, ref := range refs {
			if ref.Start < last || ref.End > len(content) || ref.Start > ref.End {
				// Defensive: malformed spans must never corrupt output.
				continue
			}
			b.Write(content[last:ref.Start])
			b.WriteString(newName)
			last = ref.End
			replaced++
			linesChangedSet[ref.Line] = struct{}{}
		}
		b.Write(content[last:])
		newContent := b.String()
		if !dryRun {
			if err := ioutil.WriteFileAtomic(path, []byte(newContent), defaultFilePerm); err != nil {
				return err
			}
		}

		// Sorted, not map order: the tool result is part of the prompt tail,
		// so an unstable order breaks prompt caching across identical runs.
		linesChanged := make([]int, 0, len(linesChangedSet))
		for line := range linesChangedSet {
			linesChanged = append(linesChanged, line)
		}
		slices.Sort(linesChanged)
		changes = append(changes, FileChange{
			Path:         rel,
			LinesChanged: linesChanged,
			Count:        replaced,
		})

		return nil
	})

	return changes, err
}

func (e *Executor) renameWithText(ctx context.Context, searchRoot, relPrefix, glob, oldName, newName string, dryRun bool, skips *walkSkips) ([]FileChange, error) {
	// Word-boundary pattern for text fallback
	pattern := `\b` + regexp.QuoteMeta(oldName) + `\b`
	re := regexp.MustCompile(pattern)

	var changes []FileChange

	err := e.walkReadableFiles(ctx, searchRoot, relPrefix, glob, skips, false, func(path, rel string, content []byte) error {
		// Check for matches
		matches := re.FindAllIndex(content, -1)
		if len(matches) == 0 {
			return nil
		}

		// Apply replacement
		newContent := re.ReplaceAll(content, []byte(newName))

		if !dryRun {
			if err := ioutil.WriteFileAtomic(path, newContent, defaultFilePerm); err != nil {
				return err
			}
		}

		// Track which lines changed
		var linesChanged []int
		oldLines := strings.Split(string(content), "\n")
		newLines := strings.Split(string(newContent), "\n")
		for i, line := range oldLines {
			if i < len(newLines) && line != newLines[i] {
				linesChanged = append(linesChanged, i+1)
			}
		}

		changes = append(changes, FileChange{
			Path:         rel,
			LinesChanged: linesChanged,
			Count:        len(matches),
		})

		return nil
	})

	return changes, err
}

func formatRenameResult(oldName, newName string, changes []FileChange, dryRun bool) string {
	var b strings.Builder
	action := "Renamed"
	if dryRun {
		action = "Would rename"
	}

	total := 0
	for _, c := range changes {
		total += c.Count
	}

	fmt.Fprintf(&b, "%s %d occurrence(s) of %q -> %q in %d file(s):\n\n",
		action, total, oldName, newName, len(changes))

	for _, c := range changes {
		fmt.Fprintf(&b, "  %s (lines: %v)\n", c.Path, c.LinesChanged)
	}

	return b.String()
}
