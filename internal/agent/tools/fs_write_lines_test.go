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

func fsWriteLinesRun(t *testing.T, ctx context.Context, tool fantasy.AgentTool, params FSWriteLinesParams) (fantasy.ToolResponse, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: FSWriteLinesToolName, Input: string(raw)})
}

// fsWriteLinesCountingHistory counts CreateVersion calls. commitFileChange
// emits the old-content snapshot version plus the new version exactly once
// per real disk write (a second write would produce 4).
type fsWriteLinesCountingHistory struct {
	*mockHistoryService
	versions int
}

func (m *fsWriteLinesCountingHistory) CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error) {
	m.versions++
	return history.File{}, nil
}

// fsWriteLinesDenyService refuses every permission request so the
// whole-call gate in NewFSWriteLinesTool takes its denial branch.
type fsWriteLinesDenyService struct {
	*mockPermissionService
}

func (m *fsWriteLinesDenyService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return false, nil
}

func TestFSWriteLinesInsertDeleteReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-lines")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ins.txt"), []byte("a\nb\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "del.txt"), []byte("a\nb\nc\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rep.txt"), []byte("a\nb\nc\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tool := NewFSWriteLinesTool(scope, &mockPermissionService{}, &mockHistoryService{}, tracker, dir)
	resp, err := fsWriteLinesRun(t, ctx, tool, FSWriteLinesParams{Items: []FSWriteLinesItem{
		{Path: "ins.txt", StartLine: 2, EndLine: 1, Content: "X"},
		{Path: "del.txt", StartLine: 2, EndLine: 2, Content: ""},
		{Path: "rep.txt", StartLine: 2, EndLine: 2, Content: "B2\nB3"},
	}})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 3, meta.Succeeded)
	for _, item := range meta.Items {
		require.Equal(t, FSStatusOK, item.Status)
		require.Equal(t, "write_lines", item.Op)
	}

	content, err := os.ReadFile(filepath.Join(dir, "ins.txt"))
	require.NoError(t, err)
	require.Equal(t, "a\nX\nb\n", string(content))
	content, err = os.ReadFile(filepath.Join(dir, "del.txt"))
	require.NoError(t, err)
	require.Equal(t, "a\nc\n", string(content))
	content, err = os.ReadFile(filepath.Join(dir, "rep.txt"))
	require.NoError(t, err)
	require.Equal(t, "a\nB2\nB3\nc\n", string(content))
}

func TestFSWriteLinesOverlapFailsBoth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-lines")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("1\n2\n3\n4\n5\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tool := NewFSWriteLinesTool(scope, &mockPermissionService{}, &mockHistoryService{}, tracker, dir)
	resp, err := fsWriteLinesRun(t, ctx, tool, FSWriteLinesParams{Items: []FSWriteLinesItem{
		{Path: "f.txt", StartLine: 2, EndLine: 4, Content: "A"},
		{Path: "f.txt", StartLine: 3, EndLine: 5, Content: "B"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 2, meta.Failed)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "overlapping range")
	require.Equal(t, FSStatusFailed, meta.Items[1].Status)
	require.Contains(t, meta.Items[1].Error, "overlapping range")

	content, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	require.NoError(t, err)
	require.Equal(t, "1\n2\n3\n4\n5\n", string(content))
}

func TestFSWriteLinesInsertionVsRangeOverlapRule(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-lines")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "g.txt"), []byte("1\n2\n3\n4\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "h.txt"), []byte("1\n2\n3\n4\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tool := NewFSWriteLinesTool(scope, &mockPermissionService{}, &mockHistoryService{}, tracker, dir)
	resp, err := fsWriteLinesRun(t, ctx, tool, FSWriteLinesParams{Items: []FSWriteLinesItem{
		// Insert before 3, anchored strictly inside 2..4: collides.
		{Path: "g.txt", StartLine: 3, EndLine: 2, Content: "X"},
		{Path: "g.txt", StartLine: 2, EndLine: 4, Content: "R"},
		// Insert before 2 lands above the replaced block 2..3: fine.
		{Path: "h.txt", StartLine: 2, EndLine: 1, Content: "X"},
		{Path: "h.txt", StartLine: 2, EndLine: 3, Content: "R"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "overlapping range")
	require.Equal(t, FSStatusFailed, meta.Items[1].Status)
	require.Contains(t, meta.Items[1].Error, "overlapping range")
	require.Equal(t, FSStatusOK, meta.Items[2].Status)
	require.Equal(t, FSStatusOK, meta.Items[3].Status)

	content, err := os.ReadFile(filepath.Join(dir, "g.txt"))
	require.NoError(t, err)
	require.Equal(t, "1\n2\n3\n4\n", string(content))
	content, err = os.ReadFile(filepath.Join(dir, "h.txt"))
	require.NoError(t, err)
	require.Equal(t, "1\nX\nR\n4\n", string(content))
}

func TestFSWriteLinesNonOverlappingApplyBottomUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-lines")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "n.txt"), []byte("1\n2\n3\n4\n5\n"), 0o644))

	// One commitFileChange per real disk write emits the old snapshot
	// plus the new version — two would mean two writes.
	files := &fsWriteLinesCountingHistory{mockHistoryService: &mockHistoryService{}}
	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tool := NewFSWriteLinesTool(scope, &mockPermissionService{}, files, tracker, dir)
	resp, err := fsWriteLinesRun(t, ctx, tool, FSWriteLinesParams{Items: []FSWriteLinesItem{
		{Path: "n.txt", StartLine: 4, EndLine: 4, Content: "FOUR"},
		{Path: "n.txt", StartLine: 2, EndLine: 2, Content: "TWO"},
	}})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusOK, meta.Items[1].Status)

	content, err := os.ReadFile(filepath.Join(dir, "n.txt"))
	require.NoError(t, err)
	require.Equal(t, "1\nTWO\n3\nFOUR\n5\n", string(content))
	require.Equal(t, 2, files.versions)
}

func TestFSWriteLinesBoundsFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-lines")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("1\n2\n3\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tool := NewFSWriteLinesTool(scope, &mockPermissionService{}, &mockHistoryService{}, tracker, dir)
	resp, err := fsWriteLinesRun(t, ctx, tool, FSWriteLinesParams{Items: []FSWriteLinesItem{
		{Path: "b.txt", StartLine: 5, EndLine: 5, Content: "X"},
		{Path: "b.txt", StartLine: 2, EndLine: 9, Content: "X"},
		{Path: "b.txt", StartLine: 0, EndLine: 1, Content: "X"},
		{Path: "b.txt", StartLine: 3, EndLine: 1, Content: "X"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "beyond the end")
	require.Equal(t, FSStatusFailed, meta.Items[1].Status)
	require.Contains(t, meta.Items[1].Error, "beyond the end")
	// Preflight failures come back as FSStatusFailed with the preflight
	// error text.
	require.Equal(t, FSStatusFailed, meta.Items[2].Status)
	require.Contains(t, meta.Items[2].Error, "start_line must be at least 1")
	require.Equal(t, FSStatusFailed, meta.Items[3].Status)
	require.Contains(t, meta.Items[3].Error, "end_line must be at least start_line - 1")

	content, err := os.ReadFile(filepath.Join(dir, "b.txt"))
	require.NoError(t, err)
	require.Equal(t, "1\n2\n3\n", string(content))
}

func TestFSWriteLinesPermissionDeniedStopsTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-lines")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead}},
	)

	tool := NewFSWriteLinesTool(scope, &fsWriteLinesDenyService{}, &mockHistoryService{}, &mockEditFileTracker{}, dir)
	resp, err := fsWriteLinesRun(t, ctx, tool, FSWriteLinesParams{Items: []FSWriteLinesItem{
		{Path: "denied.txt", StartLine: 1, EndLine: 1, Content: "never"},
	}})
	require.NoError(t, err)
	require.True(t, resp.StopTurn)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "User denied permission")

	_, statErr := os.Stat(filepath.Join(dir, "denied.txt"))
	require.True(t, os.IsNotExist(statErr))
	require.NotContains(t, resp.Content, "items ok")
}
