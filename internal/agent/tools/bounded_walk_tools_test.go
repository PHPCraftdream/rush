package tools

// Each test names the single production line it must catch if reverted:
// TestGlobFilesFSLiteralPatternNeverDescends catches: the fs.Stat existence
// pre-check plus the literal-base walkStart join in GlobGitignoreAwareFSBounded
// (glob_bounded.go).
// TestGlobFilesFSInfiniteDirBounded catches: the time-budget stop in
// WalkDirBounded surfacing as the PartialNote in GlobGitignoreAwareFSBounded
// (glob_bounded.go).
// TestGlobFilesFSEntryBudget catches: the entry-budget stop in WalkDirBounded
// surfacing as the PartialNote in GlobGitignoreAwareFSBounded
// (glob_bounded.go).
// TestGlobFilesFSContextCancelled catches: the ctx-error propagation in
// GlobGitignoreAwareFSBounded (glob_bounded.go).
// TestGlobFilesFSMissingBaseNoWalk catches: the fs.Stat pre-check short
// circuit in GlobGitignoreAwareFSBounded (glob_bounded.go).
// TestListDirectoryFSBoundedInfinite catches: the entry-budget stop and
// PartialNote in ListDirectoryFSBounded (list_bounded.go).
// TestSearchFilesFSInfiniteBounded catches: the time-budget PartialNote in
// searchFilesFS (grep.go).
// TestAnchoredSuggestionsInfiniteParentBounded catches: the batched, capped
// boundedReadDirNames in view_suggest.go.
// TestSearchFilesFSRegularTreeStillWorks catches: regressions of the fn body
// passed to fsext.WalkDirBounded in searchFilesFS (grep.go).

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/stretchr/testify/require"
)

// infiniteFS is a directory that never ends: ReadDir always yields fresh
// synthetic file entries and never reports EOF.
type infiniteFS struct{}

func (infiniteFS) Open(name string) (fs.File, error) {
	if name == "." {
		return &infiniteDirFile{}, nil
	}
	return &infiniteSynthFile{}, nil
}

type infiniteDirFile struct {
	seq int
}

func (f *infiniteDirFile) Stat() (fs.FileInfo, error) {
	return bwalkFileInfo{name: ".", dir: true}, nil
}

func (f *infiniteDirFile) Read([]byte) (int, error) { return 0, io.EOF }

func (f *infiniteDirFile) Close() error { return nil }

func (f *infiniteDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		n = 1
	}
	batch := make([]fs.DirEntry, n)
	for i := range batch {
		f.seq++
		batch[i] = bwalkDirEntry{name: fmt.Sprintf("file%06d", f.seq)}
	}
	return batch, nil
}

type infiniteSynthFile struct{}

func (infiniteSynthFile) Stat() (fs.FileInfo, error) {
	return bwalkFileInfo{name: "file", dir: false}, nil
}

func (infiniteSynthFile) Read([]byte) (int, error) { return 0, io.EOF }

func (infiniteSynthFile) Close() error { return nil }

type bwalkFileInfo struct {
	name string
	dir  bool
}

func (i bwalkFileInfo) Name() string { return i.name }
func (i bwalkFileInfo) Size() int64  { return 0 }
func (i bwalkFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i bwalkFileInfo) ModTime() time.Time { return time.Time{} }
func (i bwalkFileInfo) IsDir() bool        { return i.dir }
func (i bwalkFileInfo) Sys() any           { return nil }

type bwalkDirEntry struct {
	name string
}

func (e bwalkDirEntry) Name() string               { return e.name }
func (e bwalkDirEntry) IsDir() bool                { return false }
func (e bwalkDirEntry) Type() fs.FileMode          { return 0 }
func (e bwalkDirEntry) Info() (fs.FileInfo, error) { return bwalkFileInfo{name: e.name}, nil }

// countingFS wraps a fstest.MapFS and records every Open call by path. Stat
// is implemented directly so fs.Stat never routes through Open.
type countingFS struct {
	inner fstest.MapFS
	opens map[string]int
}

func newCountingFS(inner fstest.MapFS) *countingFS {
	return &countingFS{inner: inner, opens: map[string]int{}}
}

func (c *countingFS) Open(name string) (fs.File, error) {
	c.opens[name]++
	return c.inner.Open(name)
}

func (c *countingFS) Stat(name string) (fs.FileInfo, error) {
	return c.inner.Stat(name)
}

func setBudgets(t *testing.T, maxEntries int, timeout time.Duration) {
	t.Helper()
	origEntries := fsext.BoundedWalkMaxEntries
	origTimeout := fsext.BoundedWalkTimeout
	if maxEntries > 0 {
		fsext.BoundedWalkMaxEntries = maxEntries
	}
	if timeout > 0 {
		fsext.BoundedWalkTimeout = timeout
	}
	t.Cleanup(func() {
		fsext.BoundedWalkMaxEntries = origEntries
		fsext.BoundedWalkTimeout = origTimeout
	})
}

func TestGlobFilesFSLiteralPatternNeverDescends(t *testing.T) {
	t.Parallel()
	cfs := newCountingFS(fstest.MapFS{
		"name-a.txt":           &fstest.MapFile{Data: []byte("a")},
		"sub/a/b/c/d/deep.txt": &fstest.MapFile{Data: []byte("deep")},
	})
	files, truncated, note, err := globFilesFS(t.Context(), "name-*", cfs, ".", ".", 100)
	require.NoError(t, err)
	require.Empty(t, note)
	require.False(t, truncated)
	require.Len(t, files, 1)
	for opened := range cfs.opens {
		require.NotContains(t, opened, "sub")
	}
}

func TestGlobFilesFSInfiniteDirBounded(t *testing.T) {
	setBudgets(t, 0, 200*time.Millisecond)
	files, _, note, err := globFilesFS(t.Context(), "zzz-*", infiniteFS{}, ".", ".", 100)
	require.NoError(t, err)
	require.Empty(t, files)
	require.Contains(t, note, "results are incomplete")
}

func TestGlobFilesFSEntryBudget(t *testing.T) {
	setBudgets(t, 5, 0)
	testFS := fstest.MapFS{}
	for i := 0; i < 20; i++ {
		testFS[fmt.Sprintf("f%02d.txt", i)] = &fstest.MapFile{Data: []byte("x")}
	}
	files, _, note, err := globFilesFS(t.Context(), "*.txt", testFS, ".", ".", 100)
	require.NoError(t, err)
	require.NotEmpty(t, note)
	require.LessOrEqual(t, len(files), 10)
	require.NotEmpty(t, files)
}

func TestGlobFilesFSContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, _, note, err := globFilesFS(ctx, "zzz-*", infiniteFS{}, ".", ".", 100)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, note)
}

func TestGlobFilesFSMissingBaseNoWalk(t *testing.T) {
	t.Parallel()
	cfs := newCountingFS(fstest.MapFS{
		"root.txt": &fstest.MapFile{Data: []byte("r")},
	})
	files, _, note, err := globFilesFS(t.Context(), "a/**", cfs, ".", ".", 100)
	require.NoError(t, err)
	require.Empty(t, note)
	require.Empty(t, files)
	require.Empty(t, cfs.opens)
}

func TestListDirectoryFSBoundedInfinite(t *testing.T) {
	setBudgets(t, 50, 0)
	results, truncated, note, err := fsext.ListDirectoryFSBounded(t.Context(), infiniteFS{}, ".", "DISP", nil, -1, 1000)
	require.NoError(t, err)
	require.NotEmpty(t, note)
	require.False(t, truncated)
	require.LessOrEqual(t, len(results), 50)
}

func TestSearchFilesFSInfiniteBounded(t *testing.T) {
	setBudgets(t, 0, 200*time.Millisecond)
	_, truncated, note, err := searchFilesFS(t.Context(), "needle", infiniteFS{}, ".", ".", ".", "", 100)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Contains(t, note, "results are incomplete")
}

func TestAnchoredSuggestionsInfiniteParentBounded(t *testing.T) {
	t.Parallel()
	suggestions := anchoredSuggestions(t.Context(), infiniteFS{}, ".", "nope/missing.txt")
	require.LessOrEqual(t, len(suggestions), 3)
}

func TestSearchFilesFSRegularTreeStillWorks(t *testing.T) {
	t.Parallel()
	testFS := fstest.MapFS{
		"a.txt": &fstest.MapFile{Data: []byte("needle\n")},
	}
	matches, _, note, err := searchFilesFS(t.Context(), "needle", testFS, ".", ".", ".", "", 100)
	require.NoError(t, err)
	require.Empty(t, note)
	require.GreaterOrEqual(t, len(matches), 1)
}
