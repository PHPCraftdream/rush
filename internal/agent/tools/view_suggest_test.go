package tools

import (
	"context"
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type suggestionReadDirFS struct {
	reads   int
	entries []fs.DirEntry
}

func (f *suggestionReadDirFS) Open(string) (fs.File, error) {
	return &suggestionReadDirFile{owner: f}, nil
}
func (f *suggestionReadDirFS) ReadDir(string) ([]fs.DirEntry, error) {
	f.reads++
	return nil, fs.ErrNotExist
}

type suggestionReadDirFile struct {
	owner *suggestionReadDirFS
	index int
}

func (f *suggestionReadDirFile) Stat() (fs.FileInfo, error) {
	return bwalkFileInfo{name: ".", dir: true}, nil
}
func (f *suggestionReadDirFile) Read([]byte) (int, error) { return 0, io.EOF }
func (f *suggestionReadDirFile) Close() error             { return nil }
func (f *suggestionReadDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	f.owner.reads++
	end := f.index + n
	if end > len(f.owner.entries) {
		end = len(f.owner.entries)
	}
	entries := f.owner.entries[f.index:end]
	f.index = end
	if len(entries) < n {
		return entries, io.EOF
	}
	return entries, nil
}

func TestBoundedReadDirNamesHonorsEntryLimit(t *testing.T) {
	t.Parallel()
	fsys := &suggestionReadDirFS{entries: make([]fs.DirEntry, 513)}
	for i := range fsys.entries {
		fsys.entries[i] = bwalkDirEntry{name: "entry"}
	}
	entries := boundedReadDirNamesWithClock(t.Context(), fsys, ".", 513, func() time.Time { return time.Unix(1, 0) })
	require.Len(t, entries, 513)
	require.Equal(t, 3, fsys.reads)
}

func TestBoundedReadDirNamesHonorsContext(t *testing.T) {
	t.Parallel()
	fsys := &suggestionReadDirFS{entries: make([]fs.DirEntry, 10000)}
	for i := range fsys.entries {
		fsys.entries[i] = bwalkDirEntry{name: "entry"}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &cancelAfterReadFS{suggestionReadDirFS: fsys, cancel: cancel}
	entries := boundedReadDirNamesWithClock(ctx, f, ".", 10000, func() time.Time { return time.Unix(1, 0) })
	require.Len(t, entries, 256)
	require.Equal(t, 1, fsys.reads)
}

type cancelAfterReadFS struct {
	*suggestionReadDirFS
	cancel context.CancelFunc
}

func (f *cancelAfterReadFS) Open(string) (fs.File, error) {
	return &cancelAfterReadFile{suggestionReadDirFile: &suggestionReadDirFile{owner: f.suggestionReadDirFS}, cancel: f.cancel}, nil
}

type cancelAfterReadFile struct {
	*suggestionReadDirFile
	cancel context.CancelFunc
}

func (f *cancelAfterReadFile) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := f.suggestionReadDirFile.ReadDir(n)
	f.cancel()
	return entries, err
}

type unsupportedSuggestionFS struct{ reads int }

func (f *unsupportedSuggestionFS) Open(string) (fs.File, error) {
	return unsupportedSuggestionFile{}, nil
}
func (f *unsupportedSuggestionFS) ReadDir(string) ([]fs.DirEntry, error) {
	f.reads++
	return []fs.DirEntry{bwalkDirEntry{name: "needle"}}, nil
}

type unsupportedSuggestionFile struct{}

func (unsupportedSuggestionFile) Stat() (fs.FileInfo, error) {
	return bwalkFileInfo{name: ".", dir: true}, nil
}
func (unsupportedSuggestionFile) Read([]byte) (int, error) { return 0, io.EOF }
func (unsupportedSuggestionFile) Close() error             { return nil }

func TestBoundedReadDirNamesSkipsUnsupportedUnboundedEnumeration(t *testing.T) {
	t.Parallel()
	fsys := &unsupportedSuggestionFS{}
	require.Nil(t, boundedReadDirNames(t.Context(), fsys, ".", 100))
	require.Zero(t, fsys.reads)
}

type suggestionClock struct {
	now  time.Time
	fsys *suggestionReadDirFS
}

func (c *suggestionClock) Now() time.Time { return c.now }
func (c *suggestionClock) Open(string) (fs.File, error) {
	return &advancingSuggestionFile{suggestionReadDirFile: &suggestionReadDirFile{owner: c.fsys}, clock: c}, nil
}

type advancingSuggestionFile struct {
	*suggestionReadDirFile
	clock *suggestionClock
}

func (f *advancingSuggestionFile) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := f.suggestionReadDirFile.ReadDir(n)
	f.clock.now = f.clock.now.Add(101 * time.Millisecond)
	return entries, err
}

func TestBoundedReadDirNamesHonorsDeadline(t *testing.T) {
	t.Parallel()
	fsys := &suggestionReadDirFS{entries: make([]fs.DirEntry, 1024)}
	for i := range fsys.entries {
		fsys.entries[i] = bwalkDirEntry{name: "entry"}
	}
	clock := &suggestionClock{now: time.Unix(1, 0), fsys: fsys}
	entries := boundedReadDirNamesWithClock(t.Context(), clock, ".", 1024, clock.Now)
	require.Len(t, entries, 256)
	require.Equal(t, 1, fsys.reads)
}

func TestAnchoredSuggestionsParentReadCountBounded(t *testing.T) {
	t.Parallel()
	fsys := &suggestionReadDirFS{entries: make([]fs.DirEntry, anchoredSuggestionMaxEntries+256)}
	for i := range fsys.entries {
		fsys.entries[i] = bwalkDirEntry{name: "nonmatch"}
	}
	suggestions := anchoredSuggestions(t.Context(), fsys, ".", "nope/missing.txt")
	require.Empty(t, suggestions)
	require.Equal(t, 8, fsys.reads)
}
