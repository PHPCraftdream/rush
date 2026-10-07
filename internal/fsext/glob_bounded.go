package fsext

import (
	"context"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/bmatcuk/doublestar/v4"
)

// GlobGitignoreAwareFSBounded is the ctx- and budget-bounded counterpart of
// GlobGitignoreAwareFS. The returned note is "" unless the walk hit a budget.
func GlobGitignoreAwareFSBounded(ctx context.Context, fsys fs.FS, start, displayRoot, pattern string, limit int) ([]string, bool, string, error) {
	pattern = filepath.ToSlash(pattern)
	base, rest := globPatternBase(pattern)
	// Display paths stay relative to the original start, matching
	// GlobGitignoreAwareFS.
	startSlash := filepath.ToSlash(start)
	walkStart := filepath.ToSlash(start)
	if base != "" && base != "." {
		if walkStart == "." {
			walkStart = base
		} else {
			walkStart = walkStart + "/" + base
		}
	}
	// Existence pre-check: a literal base that does not exist must short-
	// circuit before any walk, so the caller renders "No files found".
	if _, err := fs.Stat(fsys, walkStart); err != nil {
		return nil, false, "", nil
	}
	var maxDepth int
	if rest == "" {
		// The walk starts at the base itself, so depth 1 suffices.
		maxDepth = 1
	} else {
		maxDepth = globMaxDepth(base, rest)
	}
	var visited int
	opts := BoundedWalkOptions{MaxDepth: maxDepth, Visited: &visited}
	walker := newFastGlobWalkerFSAt(fsys, walkStart)
	found := csync.NewSlice[FileInfo]()
	skipAll := false
	reason, err := WalkDirBounded(ctx, fsys, walkStart, opts, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		isDir := d.IsDir()
		if isDir {
			if walker.ShouldSkipDir(p) {
				return fs.SkipDir
			}
		} else if walker.ShouldSkip(p) {
			return nil
		}
		relPath := p
		if walkStart != "." {
			relPath, _ = filepath.Rel(walkStart, p)
		}
		relPath = filepath.ToSlash(relPath)
		var matched bool
		var matchErr error
		if base != "" && base != "." && rest == "" {
			// A fully literal pattern can only match the base path itself.
			matched = p == walkStart
		} else {
			// With a literal base the walk is rooted inside it, so the
			// matching pattern must be the base-relative remainder.
			matchAgainst := pattern
			if base != "" && base != "." {
				matchAgainst = rest
			}
			matched, matchErr = doublestar.Match(matchAgainst, relPath)
		}
		if matchErr != nil || !matched {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		displayPath := p
		if startSlash != "." {
			displayPath, _ = filepath.Rel(startSlash, p)
		}
		found.Append(FileInfo{Path: filepath.Join(displayRoot, filepath.FromSlash(displayPath)), ModTime: info.ModTime()})
		if limit > 0 && found.Len() >= limit*2 {
			skipAll = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, false, "", err
	}
	note := ""
	if reason == WalkStoppedEntries || reason == WalkStoppedTime {
		note = PartialNote(visited, BoundedWalkTimeout, walkStart)
	}
	matches := slices.SortedFunc(found.Seq(), func(a, b FileInfo) int { return b.ModTime.Compare(a.ModTime) })
	matches, truncated := truncate(matches, limit)
	results := make([]string, len(matches))
	for i, match := range matches {
		results[i] = match.Path
	}
	return results, truncated || skipAll, note, nil
}
