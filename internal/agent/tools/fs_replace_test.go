package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func fsReplaceRun(t *testing.T, ctx context.Context, tool fantasy.AgentTool, params FSReplaceParams) (fantasy.ToolResponse, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: FSReplaceToolName, Input: string(raw)})
}

// fsReplaceDenyService refuses every permission request so the whole-call
// gate in NewFSReplaceTool takes its denial branch.
type fsReplaceDenyService struct {
	*mockPermissionService
}

func (m *fsReplaceDenyService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return false, nil
}

func TestFSReplaceReadBeforeWriteEnforced(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-replace")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("alpha\nbeta\n"), 0o644))

	tool := NewFSReplaceTool(scope, &mockPermissionService{}, &mockHistoryService{}, &mockEditFileTracker{}, dir)
	resp, err := fsReplaceRun(t, ctx, tool, FSReplaceParams{Items: []FSReplaceItem{
		{Path: "doc.txt", OldString: "beta", NewString: "BETA"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "read the file before")

	content, err := os.ReadFile(filepath.Join(dir, "doc.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha\nbeta\n", string(content))

	tool = NewFSReplaceTool(scope, &mockPermissionService{}, &mockHistoryService{}, &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}, dir)
	resp, err = fsReplaceRun(t, ctx, tool, FSReplaceParams{Items: []FSReplaceItem{
		{Path: "doc.txt", OldString: "beta", NewString: "BETA"},
	}})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	content, err = os.ReadFile(filepath.Join(dir, "doc.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha\nBETA\n", string(content))
}

func TestFSReplaceNoReadGrantGetsExplicitMessage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-replace")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("alpha\nbeta\n"), 0o644))

	tool := NewFSReplaceTool(scope, &mockPermissionService{}, &mockHistoryService{}, &mockEditFileTracker{}, dir)
	resp, err := fsReplaceRun(t, ctx, tool, FSReplaceParams{Items: []FSReplaceItem{
		{Path: "doc.txt", OldString: "beta", NewString: "BETA"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "scope")
	require.Contains(t, meta.Items[0].Error, "never")
	require.NotContains(t, meta.Items[0].Error, "Use the fs_read tool first")
}

// fsReplaceCountingHistory counts CreateVersion calls. commitFileChange
// emits the old-content snapshot version plus the new version exactly
// once per real disk write (a second write would produce 4).
type fsReplaceCountingHistory struct {
	*mockHistoryService
	versions int
}

func (m *fsReplaceCountingHistory) CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error) {
	m.versions++
	return history.File{}, nil
}

func TestFSReplaceSequentialItemsOneWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-replace")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "num.txt"), []byte("one\ntwo\nthree\n"), 0o644))

	files := &fsReplaceCountingHistory{mockHistoryService: &mockHistoryService{}}
	tool := NewFSReplaceTool(scope, &mockPermissionService{}, files, &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}, dir)
	resp, err := fsReplaceRun(t, ctx, tool, FSReplaceParams{Items: []FSReplaceItem{
		{Path: "num.txt", OldString: "one", NewString: "ONE"},
		{Path: "num.txt", OldString: "three", NewString: "THREE"},
	}})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	content, err := os.ReadFile(filepath.Join(dir, "num.txt"))
	require.NoError(t, err)
	require.Equal(t, "ONE\ntwo\nTHREE\n", string(content))
	require.Equal(t, 2, files.versions)
	require.Contains(t, resp.Content, "fs_replace: 2 of 2 items ok")
}

func TestFSReplaceReplaceAllAndFailuresBestEffort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-replace")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x\nx\n"), 0o644))

	tool := NewFSReplaceTool(scope, &mockPermissionService{}, &mockHistoryService{}, &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}, dir)
	resp, err := fsReplaceRun(t, ctx, tool, FSReplaceParams{Items: []FSReplaceItem{
		{Path: "x.txt", OldString: "x", NewString: "y", ReplaceAll: true},
		{Path: "x.txt", OldString: "zzz", NewString: "q"},
		{Path: "x.txt", OldString: "", NewString: "bad"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusFailed, meta.Items[1].Status)
	require.Contains(t, meta.Items[1].Error, "not found")
	require.Equal(t, FSStatusFailed, meta.Items[2].Status)
	require.Contains(t, meta.Items[2].Error, "cannot be empty")

	content, err := os.ReadFile(filepath.Join(dir, "x.txt"))
	require.NoError(t, err)
	require.Equal(t, "y\ny\n", string(content))
}

func TestFSReplacePermissionDeniedStopsTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-replace")
	scope := fsWriteTestScope(t, dir)

	tool := NewFSReplaceTool(scope, &fsReplaceDenyService{}, &mockHistoryService{}, &mockEditFileTracker{}, dir)
	resp, err := fsReplaceRun(t, ctx, tool, FSReplaceParams{Items: []FSReplaceItem{
		{Path: "denied.txt", OldString: "a", NewString: "b"},
	}})
	require.NoError(t, err)
	require.True(t, resp.StopTurn)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "User denied permission")

	_, statErr := os.Stat(filepath.Join(dir, "denied.txt"))
	require.True(t, os.IsNotExist(statErr))
	require.NotContains(t, resp.Content, "items ok")
}

func TestFSReplaceCRLFPreserved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-replace")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crlf.txt"), []byte("alpha\r\nbeta\r\n"), 0o644))

	tool := NewFSReplaceTool(scope, &mockPermissionService{}, &mockHistoryService{}, &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}, dir)
	resp, err := fsReplaceRun(t, ctx, tool, FSReplaceParams{Items: []FSReplaceItem{
		{Path: "crlf.txt", OldString: "beta", NewString: "BETA"},
	}})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	content, err := os.ReadFile(filepath.Join(dir, "crlf.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha\r\nBETA\r\n", string(content))
}
