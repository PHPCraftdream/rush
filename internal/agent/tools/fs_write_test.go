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

// fsWriteTestScope builds a FolderScope over the given entries, resolving
// each entry dir through resolveScopedPath the same way the batch runner
// resolves item paths, so matcher and items compare resolved-to-resolved.
func fsWriteTestScope(t *testing.T, workingDir string, entries ...permission.FolderScopeEntry) permission.FolderScope {
	t.Helper()
	if len(entries) == 0 {
		entries = []permission.FolderScopeEntry{{
			Dir: ".",
			Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite},
		}}
	}
	resolved := make([]permission.FolderScopeEntry, len(entries))
	for i, e := range entries {
		dir, err := resolveScopedPath(workingDir, e.Dir)
		require.NoError(t, err)
		resolved[i] = permission.FolderScopeEntry{Dir: dir, Ops: e.Ops}
	}
	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{WorkingDir: workingDir, Entries: resolved})
	require.NoError(t, err)
	return scope
}

func fsWriteRun(t *testing.T, ctx context.Context, tool fantasy.AgentTool, params FSWriteParams) (fantasy.ToolResponse, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: FSWriteToolName, Input: string(raw)})
}

func TestFSWriteCreateVsOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "exist.txt"), []byte("old"), 0o644))

	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "new.txt", Content: "hello"},
		{Path: "exist.txt", Content: "new"},
	}})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 2, meta.Succeeded)
	require.Equal(t, "create", meta.Items[0].Op)
	require.Equal(t, "overwrite", meta.Items[1].Op)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusOK, meta.Items[1].Status)

	content, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(content))
	content, err = os.ReadFile(filepath.Join(dir, "exist.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(content))

	require.Contains(t, resp.Content, "fs_write: 2 of 2 items ok")
	require.Contains(t, resp.Content, "new.txt (create")
	require.Contains(t, resp.Content, "exist.txt (overwrite")
}

func TestFSWriteCreateOnlyRefusesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644))

	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "keep.txt", Content: "clobber", CreateOnly: true},
		{Path: "fresh.txt", Content: "brand", CreateOnly: true},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "already exists")
	require.Equal(t, FSStatusOK, meta.Items[1].Status)

	content, err := os.ReadFile(filepath.Join(dir, "keep.txt"))
	require.NoError(t, err)
	require.Equal(t, "keep", string(content))
	content, err = os.ReadFile(filepath.Join(dir, "fresh.txt"))
	require.NoError(t, err)
	require.Equal(t, "brand", string(content))
}

func TestFSWriteParentCarveOutBlocksDirCreation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpCreate}},
		permission.FolderScopeEntry{Dir: "locked"},
	)

	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: filepath.Join("locked", "new.txt"), Content: "inside"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 1, meta.Failed)
	require.Equal(t, FSStatusDenied, meta.Items[0].Status)

	_, statErr := os.Stat(filepath.Join(dir, "locked"))
	require.True(t, os.IsNotExist(statErr))
}

func TestFSWriteExecuteRechecksParentBeforeMkdir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-deep")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{
			Dir: filepath.Join("locked", "deep"),
			Ops: []permission.FileOp{permission.FileOpCreate},
		},
	)

	groupPath, err := resolveScopedPath(dir, filepath.Join("locked", "deep", "f.txt"))
	require.NoError(t, err)
	group := FSBatchGroup[FSWriteItem]{
		Path:  groupPath,
		Items: []FSGroupItem[FSWriteItem]{{Index: 0, RawPath: "locked/deep/f.txt", Item: FSWriteItem{Path: "locked/deep/f.txt", Content: "x"}}},
	}

	outcomes, err := fsWriteExecuteGroup(ctx, scope, &mockHistoryService{}, mockFileTrackerService{}, dir, group)
	require.NoError(t, err)
	require.Len(t, outcomes, 1)
	require.Equal(t, FSStatusFailed, outcomes[0].Status)
	require.Contains(t, outcomes[0].Error, "outside every folder scope")

	_, statErr := os.Stat(filepath.Join(dir, "locked"))
	require.True(t, os.IsNotExist(statErr))
}

func TestFSWriteLastWriteWinsWithinGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)

	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "multi.txt", Content: "first"},
		{Path: "multi.txt", Content: "second"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusOK, meta.Items[1].Status)

	content, err := os.ReadFile(filepath.Join(dir, "multi.txt"))
	require.NoError(t, err)
	require.Equal(t, "second", string(content))
	require.GreaterOrEqual(t, meta.Items[1].Additions, 1)
}

// fsWriteDenyService refuses every permission request so the whole-call
// gate in NewFSWriteTool takes its denial branch.
type fsWriteDenyService struct {
	*mockPermissionService
}

func (m *fsWriteDenyService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return false, nil
}

func TestFSWritePermissionDeniedStopsTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)

	tool := NewFSWriteTool(scope, &fsWriteDenyService{}, &mockHistoryService{}, mockFileTrackerService{}, dir)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "denied.txt", Content: "never"},
	}})
	require.NoError(t, err)
	require.True(t, resp.StopTurn)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "User denied permission")

	_, statErr := os.Stat(filepath.Join(dir, "denied.txt"))
	require.True(t, os.IsNotExist(statErr))
	require.NotContains(t, resp.Content, "items ok")
}
