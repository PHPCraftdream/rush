package tools

import (
	"context"
	"io/fs"
)

// anchoredSuggestionMaxEntries caps how many directory entries are inspected
// when offering "did you mean" suggestions for a missing file.
const anchoredSuggestionMaxEntries = 2000

// boundedReadDirNames reads up to maxEntries entries from dir in batches,
// checking ctx between batches. It returns nil if the directory cannot be
// opened or is not readable as a directory. Entries are NOT sorted; callers
// only scan them for fuzzy name matches.
func boundedReadDirNames(ctx context.Context, fsys fs.FS, dir string, maxEntries int) []fs.DirEntry {
	f, err := fsys.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	rdf, ok := f.(fs.ReadDirFile)
	if !ok {
		fallEntries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return nil
		}
		if len(fallEntries) > maxEntries {
			fallEntries = fallEntries[:maxEntries]
		}
		return fallEntries
	}
	var entries []fs.DirEntry
	for len(entries) < maxEntries {
		if err := ctx.Err(); err != nil {
			return entries
		}
		n := 256
		if remaining := maxEntries - len(entries); remaining < n {
			n = remaining
		}
		batch, err := rdf.ReadDir(n)
		entries = append(entries, batch...)
		if err != nil {
			// io.EOF is the normal end of the directory; any other error is
			// still best-effort for suggestions, so return what we have.
			return entries
		}
		if len(batch) == 0 {
			return entries
		}
	}
	return entries
}
