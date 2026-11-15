package fsext

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

type scriptedBatchFS struct {
	batches [][]fs.DirEntry
	errs    []error
	calls   int
}

func (f *scriptedBatchFS) Open(name string) (fs.File, error) {
	if name != "." {
		return syntheticFile{name: name}, nil
	}
	return &scriptedBatchDir{owner: f}, nil
}

func (f *scriptedBatchFS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return syntheticFileInfo{name: ".", dir: true}, nil
	}
	return syntheticFileInfo{name: name}, nil
}

type scriptedBatchDir struct{ owner *scriptedBatchFS }

func (d *scriptedBatchDir) Stat() (fs.FileInfo, error) {
	return syntheticFileInfo{name: ".", dir: true}, nil
}
func (d *scriptedBatchDir) Read([]byte) (int, error) { return 0, io.EOF }
func (d *scriptedBatchDir) Close() error             { return nil }
func (d *scriptedBatchDir) ReadDir(int) ([]fs.DirEntry, error) {
	i := d.owner.calls
	d.owner.calls++
	if i >= len(d.owner.batches) {
		return nil, io.EOF
	}
	return d.owner.batches[i], d.owner.errs[i]
}

type syntheticFile struct{ name string }

func (f syntheticFile) Stat() (fs.FileInfo, error) { return syntheticFileInfo{name: f.name}, nil }
func (syntheticFile) Read([]byte) (int, error)     { return 0, io.EOF }
func (syntheticFile) Close() error                 { return nil }

func TestWalkDirBoundedReadDirShortBatchAndTerminalEntries(t *testing.T) {
	t.Parallel()
	fail := errors.New("read failure")
	for _, terminal := range []error{io.EOF, fail} {
		fsys := &scriptedBatchFS{
			batches: [][]fs.DirEntry{{syntheticEntry{name: "first"}}, {syntheticEntry{name: "last"}}},
			errs:    []error{nil, terminal},
		}
		var got []string
		_, err := WalkDirBounded(context.Background(), fsys, ".", BoundedWalkOptions{}, func(path string, _ fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			got = append(got, path)
			return nil
		})
		if terminal == fail {
			require.ErrorIs(t, err, fail)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, []string{".", "first", "last"}, got)
	}
}

func TestWalkDirBoundedNestedSkipAllStopsWholeWalk(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"a/child.txt": &fstest.MapFile{Data: []byte("a")},
		"z.txt":       &fstest.MapFile{Data: []byte("z")},
	}
	var got []string
	reason, err := WalkDirBounded(t.Context(), files, ".", BoundedWalkOptions{}, func(path string, _ fs.DirEntry, _ error) error {
		got = append(got, path)
		if path == "a/child.txt" {
			return fs.SkipAll
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, WalkCompleted, reason)
	require.NotContains(t, got, "z.txt")
}

func TestWalkDirBoundedNestedBudgetStopsWholeWalk(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"a/child.txt": &fstest.MapFile{Data: []byte("a")},
		"z.txt":       &fstest.MapFile{Data: []byte("z")},
	}
	var got []string
	var visited int
	reason, err := WalkDirBounded(t.Context(), files, ".", BoundedWalkOptions{Limits: BoundedWalkLimits{MaxEntries: 2}, Visited: &visited}, func(path string, _ fs.DirEntry, _ error) error {
		got = append(got, path)
		return nil
	})
	require.Equal(t, 2, visited)
	require.NoError(t, err)
	require.Equal(t, WalkStoppedEntries, reason)
	require.NotContains(t, got, "z.txt")
}

func TestWalkDirBoundedReturnsEntriesAccompanyingEOF(t *testing.T) {
	t.Parallel()
	fsys := &scriptedBatchFS{batches: [][]fs.DirEntry{{syntheticEntry{name: "present"}}}, errs: []error{io.EOF}}
	var got []string
	_, err := WalkDirBounded(context.Background(), fsys, ".", BoundedWalkOptions{}, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		got = append(got, path)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{".", "present"}, got)
}
