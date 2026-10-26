package fsext

import (
	"context"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/csync"
)

// ListDirectoryFSBounded is the ctx- and budget-bounded counterpart of
// ListDirectoryFS. The returned note is "" unless the walk hit a budget.
func ListDirectoryFSBounded(ctx context.Context, fsys fs.FS, start, displayRoot string, ignorePatterns []string, depth, limit int) ([]string, bool, string, error) {
	start = filepath.ToSlash(start)
	// Existence pre-check: a missing start short-circuits to "No files
	// found" instead of a walk error.
	if _, err := fs.Stat(fsys, start); err != nil {
		return nil, false, "", nil
	}
	// The walker counts the start dir itself as depth 1; MaxDepth matches the
	// legacy listing boundary, which skips descendants at relative depth depth.
	maxDepth := depth
	var visited int
	opts := BoundedWalkOptions{MaxDepth: maxDepth, Visited: &visited}
	found := csync.NewSlice[string]()
	dl := newDirectoryListerFSAt(fsys, start)
	skipAll := false
	reason, err := WalkDirBounded(ctx, fsys, start, opts, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		isDir := d.IsDir()
		if dl.shouldIgnore(path, ignorePatterns, isDir) {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.ToSlash(path) != filepath.ToSlash(start) {
			displayPath := path
			if start != "." {
				displayPath, _ = filepath.Rel(start, path)
			}
			displayPath = filepath.Join(displayRoot, filepath.FromSlash(displayPath))
			if isDir {
				displayPath += string(filepath.Separator)
			}
			found.Append(displayPath)
		}
		if limit > 0 && found.Len() >= limit {
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
		note = PartialNote(visited, BoundedWalkTimeout, start)
	}
	matches, truncated := truncate(slices.Collect(found.Seq()), limit)
	return matches, truncated || skipAll, note, nil
}
