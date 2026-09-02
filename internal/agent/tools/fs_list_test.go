package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// fsListTestCarveScope builds a scope granting list over the resolved
// temp dir, with a deny carve-out over the secret subdirectory.
func fsListTestCarveScope(t *testing.T, dir string) permission.FolderScope {
	t.Helper()
	resolved, err := resolveScopedPath(context.Background(), OSDisk(), dir, ".")
	require.NoError(t, err)
	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: dir,
		Entries: []permission.FolderScopeEntry{
			{Dir: resolved, Ops: []permission.FileOp{permission.FileOpList}},
			{Dir: filepath.Join(resolved, "secret"), Ops: nil},
		},
	})
	require.NoError(t, err)
	return scope
}

func TestFSListDropsDenyCarvedEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "kept", "sub"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "secret"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kept", "a.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kept", "sub", "b.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret", "hidden.txt"), []byte("x"), 0o644))

	tool := NewFSListTool(fsListTestCarveScope(t, dir), dir, config.ToolLs{}, nil)
	raw, err := json.Marshal(FSListParams{Items: []FSListItem{{Path: "."}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	meta := fsBatchTestMetadata(t, resp)
	require.Equal(t, FSListToolName, meta.Tool)
	require.Equal(t, 1, meta.Succeeded)
	require.Equal(t, 0, meta.Failed)
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, "list", meta.Items[0].Op)

	require.Contains(t, resp.Content, "kept/")
	require.Contains(t, resp.Content, "a.txt")
	require.Contains(t, resp.Content, "sub/")
	require.Contains(t, resp.Content, "b.txt")
	require.NotContains(t, resp.Content, "secret")
	require.NotContains(t, resp.Content, "hidden.txt")
	require.Contains(t, resp.Content, "(2 entries hidden by the folder scope)")
}

func TestFSListDeniesRootOutsideScopePerItem(t *testing.T) {
	t.Parallel()

	dirA := t.TempDir()
	dirB := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dirB, "b.txt"), []byte("x"), 0o644))

	tool := NewFSListTool(fsBatchTestScope(t, dirA, permission.FileOpList), dirA, config.ToolLs{}, nil)
	raw, err := json.Marshal(FSListParams{Items: []FSListItem{
		{Path: "."},
		{Path: dirB},
	}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-2", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError, "one ok item keeps the call successful")

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 2)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusDenied, meta.Items[1].Status)
	require.Contains(t, meta.Items[1].Error, "outside every folder scope")
	require.Contains(t, resp.Content, "a.txt")
	require.NotContains(t, resp.Content, "b.txt")
}

func TestFSListDefaultsToScopeRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644))

	tool := NewFSListTool(fsBatchTestScope(t, dir, permission.FileOpList), dir, config.ToolLs{}, nil)
	raw, err := json.Marshal(FSListParams{Items: []FSListItem{{}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-3", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	meta := fsBatchTestMetadata(t, resp)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Contains(t, resp.Content, "a.txt")
}

func TestFSListItemPathRequiredWhenNoRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := NewFSListTool(permission.FolderScope{}, dir, config.ToolLs{}, nil)
	raw, err := json.Marshal(FSListParams{Items: []FSListItem{{}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-4", Input: string(raw)})
	require.NoError(t, err)
	require.True(t, resp.IsError)

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "path is required")
}

func TestFSListNotADirectoryFailsPerItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644))

	tool := NewFSListTool(fsBatchTestScope(t, dir, permission.FileOpList), dir, config.ToolLs{}, nil)
	raw, err := json.Marshal(FSListParams{Items: []FSListItem{{Path: "a.txt"}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-5", Input: string(raw)})
	require.NoError(t, err)

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "not a directory")
}
