package fsext

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWalkDirBoundedSmallSorted catches: the sort of each ReadDir batch in
// boundedWalker.walkDir (walk_bounded.go).
// TestWalkDirBoundedEntryBudget catches: the entry-budget check in
// boundedWalker.check (walk_bounded.go).
// TestWalkDirBoundedTimeBudget catches: the deadline check in
// boundedWalker.check (walk_bounded.go).
// TestWalkDirBoundedContextCancelled catches: the ctx.Err check in
// boundedWalker.check (walk_bounded.go).
// TestWalkDirBoundedSkipAll catches: the `return nil` for fs.SkipAll sites in
// boundedWalker.walk/walkDir/readDirFallback (walk_bounded.go).
// TestWalkDirBoundedSkipDir catches: the fs.SkipDir handling in
// boundedWalker.walkDir (walk_bounded.go).
// TestWalkDirBoundedMaxDepth catches: the MaxDepth descend guard in
// boundedWalker.walkDir (walk_bounded.go).
// TestWalkDirBoundedMissingStart catches: the fs.Stat error early return in
// boundedWalker.walk (walk_bounded.go).
// TestGlobPatternBase catches: globPatternBase/globMaxDepth/hasDoubleStar
// (walk_bounded.go).
// TestWalkDirBoundedReadsInBatches catches: dirs.ReadDir(boundedWalkBatchSize)
// in boundedWalker.walkDir replaced by a read-everything ReadDir(-1).
// TestGlobGitignoreAwareFSBoundedLiteralBase catches: the base-relative match
// pattern in the fn callback of GlobGitignoreAwareFSBounded (glob_bounded.go).

// TestWalkDirBoundedSmallSorted verifies that small directories are sorted
// like fs.WalkDir would sort them.
func TestWalkDirBoundedSmallSorted(t *testing.T) {
	t.Parallel()
	testFS := fstest.MapFS{
		"a.txt":   &fstest.MapFile{Data: []byte("a")},
		"b.txt":   &fstest.MapFile{Data: []byte("b")},
		"c/d.txt": &fstest.MapFile{Data: []byte("d")},
	}
	var visited []string
	reason, err := WalkDirBounded(context.Background(), testFS, ".", BoundedWalkOptions{}, func(path string, d fs.DirEntry, err error) error {
		visited = append(visited, path)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, WalkCompleted, reason)
	require.Equal(t, []string{".", "a.txt", "b.txt", "c", "c/d.txt"}, visited)
}

// TestWalkDirBoundedEntryBudget verifies that the entry budget stops the walk
// with WalkStoppedEntries. It mutates a package-level budget var, so it must
// not run in parallel with the tests that rely on the defaults.
func TestWalkDirBoundedEntryBudget(t *testing.T) {
	orig := BoundedWalkMaxEntries
	BoundedWalkMaxEntries = 5
	t.Cleanup(func() { BoundedWalkMaxEntries = orig })

	testFS := fstest.MapFS{}
	for i := 0; i < 20; i++ {
		testFS[fmt.Sprintf("f%02d.txt", i)] = &fstest.MapFile{Data: []byte("x")}
	}
	var visited []string
	reason, err := WalkDirBounded(context.Background(), testFS, ".", BoundedWalkOptions{}, func(path string, d fs.DirEntry, err error) error {
		visited = append(visited, path)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, WalkStoppedEntries, reason)
	require.LessOrEqual(t, len(visited), 5)
}

// TestWalkDirBoundedTimeBudget verifies that an exhausted time budget stops
// the walk with WalkStoppedTime. It mutates package-level budget vars, so it
// must not run in parallel with the tests that rely on the defaults.
func TestWalkDirBoundedTimeBudget(t *testing.T) {
	origTimeout := BoundedWalkTimeout
	BoundedWalkTimeout = 200 * time.Millisecond
	origEntries := BoundedWalkMaxEntries
	BoundedWalkMaxEntries = 1 << 30
	t.Cleanup(func() {
		BoundedWalkTimeout = origTimeout
		BoundedWalkMaxEntries = origEntries
	})

	var visited int
	reason, err := WalkDirBounded(context.Background(), infiniteFS{}, ".", BoundedWalkOptions{}, func(path string, d fs.DirEntry, err error) error {
		visited++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, WalkStoppedTime, reason)
	require.Greater(t, visited, 0)
}

// TestWalkDirBoundedContextCancelled verifies that cancelling the context
// stops the walk with WalkStoppedContext and the context error.
func TestWalkDirBoundedContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := true
	reason, err := WalkDirBounded(ctx, infiniteFS{}, ".", BoundedWalkOptions{}, func(path string, d fs.DirEntry, err error) error {
		if first {
			first = false
			cancel()
		}
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, WalkStoppedContext, reason)
}

// TestWalkDirBoundedSkipAll verifies that fs.SkipAll ends the walk cleanly
// after exactly one visited entry.
func TestWalkDirBoundedSkipAll(t *testing.T) {
	t.Parallel()
	testFS := fstest.MapFS{
		"a.txt": &fstest.MapFile{Data: []byte("a")},
		"b.txt": &fstest.MapFile{Data: []byte("b")},
	}
	var visited []string
	reason, err := WalkDirBounded(context.Background(), testFS, ".", BoundedWalkOptions{}, func(path string, d fs.DirEntry, err error) error {
		visited = append(visited, path)
		return fs.SkipAll
	})
	require.NoError(t, err)
	require.Equal(t, WalkCompleted, reason)
	require.Len(t, visited, 1)
}

// TestWalkDirBoundedSkipDir verifies that fs.SkipDir on a directory skips its
// contents.
func TestWalkDirBoundedSkipDir(t *testing.T) {
	t.Parallel()
	testFS := fstest.MapFS{
		"a/x.txt":   &fstest.MapFile{Data: []byte("x")},
		"b/c/d.txt": &fstest.MapFile{Data: []byte("d")},
	}
	var visited []string
	reason, err := WalkDirBounded(context.Background(), testFS, ".", BoundedWalkOptions{}, func(path string, d fs.DirEntry, err error) error {
		visited = append(visited, path)
		if path == "b" {
			return fs.SkipDir
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, WalkCompleted, reason)
	require.Contains(t, visited, "a")
	require.Contains(t, visited, "a/x.txt")
	require.Contains(t, visited, "b")
	require.NotContains(t, visited, "b/c")
	require.NotContains(t, visited, "b/c/d.txt")
}

// TestWalkDirBoundedMaxDepth verifies that MaxDepth=1 stops descending below
// the direct children of the start.
func TestWalkDirBoundedMaxDepth(t *testing.T) {
	t.Parallel()
	testFS := fstest.MapFS{
		"a/b/c.txt": &fstest.MapFile{Data: []byte("c")},
	}
	var visited []string
	reason, err := WalkDirBounded(context.Background(), testFS, ".", BoundedWalkOptions{MaxDepth: 1}, func(path string, d fs.DirEntry, err error) error {
		visited = append(visited, path)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, WalkCompleted, reason)
	require.Equal(t, []string{".", "a"}, visited)
}

// TestWalkDirBoundedMissingStart verifies that a missing start path completes
// without invoking fn.
func TestWalkDirBoundedMissingStart(t *testing.T) {
	t.Parallel()
	testFS := fstest.MapFS{
		"a.txt": &fstest.MapFile{Data: []byte("a")},
	}
	called := false
	reason, err := WalkDirBounded(context.Background(), testFS, "nope/missing", BoundedWalkOptions{}, func(path string, d fs.DirEntry, err error) error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, WalkCompleted, reason)
	require.False(t, called)
}

// TestGlobPatternBase is a table test for globPatternBase, globMaxDepth and
// hasDoubleStar.
func TestGlobPatternBase(t *testing.T) {
	t.Parallel()

	baseCases := []struct {
		pattern string
		base    string
		rest    string
	}{
		{"name-*", "", "name-*"},
		{"a/b/*.txt", "a/b", "*.txt"},
		{"a/b", "a/b", ""},
		{"**/x", "", "**/x"},
		{"a/**/b", "a", "**/b"},
	}
	for _, tc := range baseCases {
		base, rest := globPatternBase(tc.pattern)
		require.Equal(t, tc.base, base, "globPatternBase(%q) base", tc.pattern)
		require.Equal(t, tc.rest, rest, "globPatternBase(%q) rest", tc.pattern)
	}

	depthCases := []struct {
		base  string
		rest  string
		depth int
	}{
		{"", "name-*", 1},
		{"a/b", "*.txt", 1},
		{"", "**/x", 0},
		{"a", "**/b", 0},
		{"a/b", "", 0},
		{"", "*.txt", 1},
	}
	for _, tc := range depthCases {
		require.Equal(t, tc.depth, globMaxDepth(tc.base, tc.rest), "globMaxDepth(%q, %q)", tc.base, tc.rest)
	}

	require.True(t, hasDoubleStar("a/**"))
	require.False(t, hasDoubleStar("a/*"))
}

// TestGlobGitignoreAwareFSBoundedLiteralBase verifies that a pattern with a
// literal directory prefix matches files under that prefix.
func TestGlobGitignoreAwareFSBoundedLiteralBase(t *testing.T) {
	t.Parallel()
	testFS := fstest.MapFS{
		"sub/a.txt": &fstest.MapFile{Data: []byte("a")},
		"sub/b.txt": &fstest.MapFile{Data: []byte("b")},
		"top.txt":   &fstest.MapFile{Data: []byte("t")},
	}
	results, truncated, note, err := GlobGitignoreAwareFSBounded(context.Background(), testFS, ".", ".", "sub/*.txt", 100)
	require.NoError(t, err)
	require.Empty(t, note)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{
		filepath.Join(".", "sub/a.txt"),
		filepath.Join(".", "sub/b.txt"),
	}, results)
}

// infiniteDir is an fs.ReadDirFile that yields fresh synthetic entries on
// every ReadDir call and never returns io.EOF.
type infiniteDir struct {
	counter int
}

// infiniteReadAllCalls counts ReadDir(n<=0) calls: on a real huge directory
// that reads every entry in one call, which the bounded walker must never do.
var infiniteReadAllCalls atomic.Int64

func (d *infiniteDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		infiniteReadAllCalls.Add(1)
		n = 256
	}
	count := n
	if count > 256 {
		count = 256
	}
	entries := make([]fs.DirEntry, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, syntheticEntry{name: fmt.Sprintf("e%08d", d.counter)})
		d.counter++
	}
	return entries, nil
}

func (d *infiniteDir) Read([]byte) (int, error) { return 0, io.EOF }

func (d *infiniteDir) Close() error { return nil }
func (d *infiniteDir) Stat() (fs.FileInfo, error) {
	return syntheticFileInfo{name: "big", dir: true}, nil
}

// infiniteFS is an fs.FS whose Open returns an infiniteDir for any path.
type infiniteFS struct{}

func (f infiniteFS) Open(name string) (fs.File, error) { return &infiniteDir{}, nil }

// syntheticEntry is a minimal non-directory fs.DirEntry.
type syntheticEntry struct {
	name string
}

func (e syntheticEntry) Name() string               { return e.name }
func (e syntheticEntry) IsDir() bool                { return false }
func (e syntheticEntry) Type() fs.FileMode          { return 0 }
func (e syntheticEntry) Info() (fs.FileInfo, error) { return syntheticFileInfo{name: e.name}, nil }

// syntheticFileInfo is a minimal fs.FileInfo.
type syntheticFileInfo struct {
	name string
	dir  bool
}

func (i syntheticFileInfo) Name() string { return i.name }
func (i syntheticFileInfo) Size() int64  { return 0 }
func (i syntheticFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir
	}
	return 0
}
func (i syntheticFileInfo) ModTime() time.Time { return time.Time{} }
func (i syntheticFileInfo) IsDir() bool        { return i.dir }
func (i syntheticFileInfo) Sys() any           { return nil }

func TestWalkDirBoundedReadsInBatches(t *testing.T) {
	before := infiniteReadAllCalls.Load()
	reason, err := WalkDirBounded(t.Context(), infiniteFS{}, ".", BoundedWalkOptions{
		Limits: BoundedWalkLimits{MaxEntries: 600},
	}, func(string, fs.DirEntry, error) error { return nil })
	require.NoError(t, err)
	require.Equal(t, WalkStoppedEntries, reason)
	require.Equal(t, before, infiniteReadAllCalls.Load(), "a directory must be read in bounded batches, never all at once")
}
