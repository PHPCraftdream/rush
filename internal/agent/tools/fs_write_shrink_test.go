package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fs_write is write's replacement in folder-scoped runs, so it carries the same
// shrink guard (#1192): an item that would replace a large existing file with a
// fraction of its size fails, per item, unless the item sets allow_shrink.

func fsWriteShrinkRun(t *testing.T, dir string, items ...FSWriteItem) FSBatchResponseMetadata {
	t.Helper()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-shrink")
	tool := NewFSWriteTool(fsWriteTestScope(t, dir), &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: items})
	require.NoError(t, err)
	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	return meta
}

func fsWriteShrinkSeed(t *testing.T, name string, size int) (dir, path, body string) {
	t.Helper()
	dir = t.TempDir()
	body = strings.Repeat("x", size)
	path = filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return dir, path, body
}

// REVERT CHECK: dropping the writeWouldShrinkFile branch from
// fsWriteExecuteGroup lets the one-row item overwrite the file -- the first
// subtest FAILED (item status ok, file overwritten).
func TestFSWriteRefusesToShrinkALargeFile(t *testing.T) {
	t.Parallel()

	t.Run("one row over a large file is refused and nothing is written", func(t *testing.T) {
		t.Parallel()
		dir, path, body := fsWriteShrinkSeed(t, "registry.md", 6000)
		meta := fsWriteShrinkRun(t, dir, FSWriteItem{Path: "registry.md", Content: "| ASYNC-13 | one row |\n"})
		require.Equal(t, 1, meta.Failed)
		require.Equal(t, FSStatusFailed, meta.Items[0].Status)
		require.Contains(t, meta.Items[0].Error, "allow_shrink=true")
		require.Contains(t, meta.Items[0].Error, "fs_replace or fs_write_lines", "the way out must name the scoped toolset's own tools")
		require.NotContains(t, meta.Items[0].Error, "multiedit")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, body, string(got), "the refused item must leave the file untouched")
	})

	t.Run("allow_shrink makes the replacement deliberate", func(t *testing.T) {
		t.Parallel()
		dir, path, _ := fsWriteShrinkSeed(t, "registry.md", 6000)
		meta := fsWriteShrinkRun(t, dir, FSWriteItem{Path: "registry.md", Content: "short\n", AllowShrink: true})
		require.Equal(t, 1, meta.Succeeded)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "short\n", string(got))
	})

	t.Run("a small file, a growing rewrite and a new file are never guarded", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "small.txt"), []byte(strings.Repeat("x", 500)), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "grow.txt"), []byte(strings.Repeat("x", 3000)), 0o644))
		meta := fsWriteShrinkRun(t, dir,
			FSWriteItem{Path: "small.txt", Content: "y"},
			FSWriteItem{Path: "grow.txt", Content: strings.Repeat("z", 6000)},
			FSWriteItem{Path: "fresh.txt", Content: "hi"},
		)
		require.Equal(t, 3, meta.Succeeded, "%+v", meta.Items)
	})

	t.Run("items are judged independently: the refused one does not sink its sibling", func(t *testing.T) {
		t.Parallel()
		dir, bigPath, body := fsWriteShrinkSeed(t, "big.md", 6000)
		meta := fsWriteShrinkRun(t, dir,
			FSWriteItem{Path: "big.md", Content: "tiny"},
			FSWriteItem{Path: "new.md", Content: "fine"},
		)
		require.Equal(t, FSStatusFailed, meta.Items[0].Status)
		require.Equal(t, FSStatusOK, meta.Items[1].Status)
		got, err := os.ReadFile(bigPath)
		require.NoError(t, err)
		require.Equal(t, body, string(got))
	})

	// A file that does not exist yet has nothing to lose: last write wins, as
	// before. REVERT CHECK: dropping the `exists &&` condition refuses the second
	// item -- this subtest FAILED (status failed).
	t.Run("two items for a file that does not exist yet are not guarded", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := fsWriteShrinkRun(t, dir,
			FSWriteItem{Path: "fresh.md", Content: strings.Repeat("g", 6000)},
			FSWriteItem{Path: "fresh.md", Content: "tiny"},
		)
		require.Equal(t, 2, meta.Succeeded, "%+v", meta.Items)
		got, err := os.ReadFile(filepath.Join(dir, "fresh.md"))
		require.NoError(t, err)
		require.Equal(t, "tiny", string(got))
	})

	// The guard measures each item against the file as the previous item left
	// it, not against the disk: here the disk file is small (unguarded) but the
	// first item grew it, so the second item's shrink is a real clobber.
	//
	// REVERT CHECK: measuring against oldContent instead of current lets the
	// second item through -- this subtest FAILED (status ok, disk holds the tiny
	// content).
	t.Run("a shrink of what an earlier item in the same call just wrote is refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "doc.md")
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 300)), 0o644))
		grown := strings.Repeat("g", 6000)
		meta := fsWriteShrinkRun(t, dir,
			FSWriteItem{Path: "doc.md", Content: grown},
			FSWriteItem{Path: "doc.md", Content: "tiny"},
		)
		require.Equal(t, FSStatusOK, meta.Items[0].Status)
		require.Equal(t, FSStatusFailed, meta.Items[1].Status)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, grown, string(got))
	})
}
