package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// fsFindTestCarveScope builds a scope granting find over the resolved
// temp dir, with a deny carve-out over the secret subdirectory.
func fsFindTestCarveScope(t *testing.T, dir string) permission.FolderScope {
	t.Helper()
	resolved, err := resolveScopedPath(dir, ".")
	require.NoError(t, err)
	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: dir,
		Entries: []permission.FolderScopeEntry{
			{Dir: resolved, Ops: []permission.FileOp{permission.FileOpFind}},
			{Dir: filepath.Join(resolved, "secret"), Ops: nil},
		},
	})
	require.NoError(t, err)
	return scope
}

func TestFSFindDropsDenyCarvedResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "secret"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret", "hidden.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nested", "deep.txt"), []byte("x"), 0o644))

	tool := NewFSFindTool(fsFindTestCarveScope(t, dir), dir)
	raw, err := json.Marshal(FSFindParams{Items: []FSFindItem{{Pattern: "**/*.txt"}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	meta := fsBatchTestMetadata(t, resp)
	require.Equal(t, FSFindToolName, meta.Tool)
	require.Equal(t, 1, meta.Succeeded)
	require.Equal(t, 0, meta.Failed)
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, "find", meta.Items[0].Op)

	require.Contains(t, resp.Content, "keep.txt")
	require.Contains(t, resp.Content, "deep.txt")
	require.NotContains(t, resp.Content, "hidden.txt")
	require.NotContains(t, resp.Content, "secret")
	require.Contains(t, resp.Content, "(1 results hidden by the folder scope)")
}

func TestFSFindDeniesRootOutsideScopePerItem(t *testing.T) {
	t.Parallel()

	dirA := t.TempDir()
	dirB := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dirB, "b.txt"), []byte("x"), 0o644))

	tool := NewFSFindTool(fsBatchTestScope(t, dirA, permission.FileOpFind), dirA)
	raw, err := json.Marshal(FSFindParams{Items: []FSFindItem{
		{Pattern: "**/*.txt", Path: "."},
		{Pattern: "**/*.txt", Path: dirB},
	}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-2", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError, "a per-item denial is not a whole-batch error")

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 2)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusDenied, meta.Items[1].Status)
	require.Contains(t, meta.Items[1].Error, "outside every folder scope")
	require.Contains(t, resp.Content, "a.txt")
	require.NotContains(t, resp.Content, "b.txt")
}

func TestFSFindDefaultsToScopeRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "root.txt"), []byte("x"), 0o644))

	tool := NewFSFindTool(fsBatchTestScope(t, dir, permission.FileOpFind), dir)
	raw, err := json.Marshal(FSFindParams{Items: []FSFindItem{{Pattern: "**/*.txt"}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-3", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Contains(t, resp.Content, "root.txt")
}

func TestFSFindRequiresPattern(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tool := NewFSFindTool(fsBatchTestScope(t, dir, permission.FileOpFind), dir)
	raw, err := json.Marshal(FSFindParams{Items: []FSFindItem{{Path: "."}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-4", Input: string(raw)})
	require.NoError(t, err)

	meta := fsBatchTestMetadata(t, resp)
	require.Len(t, meta.Items, 1)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "pattern is required")
}
