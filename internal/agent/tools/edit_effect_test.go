package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/stretchr/testify/require"
)

func TestEditNoWriteProofPrecedesPermissionsAndHistory(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, tc := range []struct {
		name, content, old, replacement, reason string
		all                                     bool
	}{
		{"missing replacement", "alpha\nbeta\n", "missing", "new", "missing_old", false},
		{"missing all replacements", "alpha\nbeta\n", "missing", "new", "missing_old", true},
		{"missing deletion", "alpha\nbeta\n", "missing", "", "missing_old", false},
		{"missing all deletions", "alpha\nbeta\n", "missing", "", "missing_old", true},
		{"equal replacement", "alpha\nbeta\n", "beta", "beta", "equal_content", false},
		{"equal all replacements", "alpha\nbeta\n", "beta", "beta", "equal_content", true},
		{"equal CRLF replacement", "alpha\r\nbeta\r\n", "beta", "beta", "equal_content", false},
		{"multiple matches untagged", "alpha\nalpha\n", "alpha", "new", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.txt")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
			ctx := context.WithValue(t.Context(), SessionIDContextKey, "edit-effect")
			// Nil services make any permission/history access fail this test.
			tool := NewEditTool(nil, nil, tracker, dir)
			input, err := json.Marshal(EditParams{FilePath: path, OldString: tc.old, NewString: tc.replacement, ReplaceAll: tc.all})
			require.NoError(t, err)
			response, err := tool.Run(ctx, fantasy.ToolCall{ID: "edit", Name: EditToolName, Input: string(input)})
			require.NoError(t, err)
			require.True(t, response.IsError)
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(content), "native edit changed bytes despite rejection")
			if tc.reason == "" {
				require.Empty(t, response.Metadata, "unproven errors must remain conservative")
				return
			}
			var metadata EditEffectMetadata
			require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
			require.True(t, metadata.EditEffect.NoWrite)
			require.Equal(t, tc.reason, metadata.EditEffect.Reason)
		})
	}
}

func TestEditExternalChangeAllowsFreshReadAfterRejection(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, old := range []string{"missing", "current"} {
		t.Run(old, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.txt")
			require.NoError(t, os.WriteFile(path, []byte("current\n"), 0o600))
			tracker := &mockEditFileTracker{lastRead: time.Now().Add(-time.Hour)}
			ctx := context.WithValue(t.Context(), SessionIDContextKey, "external-change")
			tool := NewEditTool(nil, nil, tracker, dir)
			input, err := json.Marshal(EditParams{FilePath: path, OldString: old, NewString: "current"})
			require.NoError(t, err)
			response, err := tool.Run(ctx, fantasy.ToolCall{ID: "edit", Name: EditToolName, Input: string(input)})
			require.NoError(t, err)
			require.True(t, response.IsError)
			require.Empty(t, response.Metadata, "external modification must permit the existing coverage reset")
			require.Equal(t, []string{path}, tracker.reads)
		})
	}
}

type editEffectFailingHistory struct {
	mockHistoryService
	err error
}

func (h *editEffectFailingHistory) GetByPathAndSession(context.Context, string, string) (history.File, error) {
	return history.File{}, os.ErrNotExist
}

func (h *editEffectFailingHistory) Create(context.Context, string, string, string) (history.File, error) {
	return history.File{}, h.err
}

func TestEditPostWriteHistoryErrorHasNoNoWriteProof(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\n"), 0o600))
	failure := errors.New("history failed after disk write")
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "edit-effect")
	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tool := NewEditTool(&mockPermissionService{}, &editEffectFailingHistory{err: failure}, tracker, dir)
	input, err := json.Marshal(EditParams{FilePath: path, OldString: "alpha", NewString: "beta"})
	require.NoError(t, err)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "edit", Name: EditToolName, Input: string(input)})
	require.ErrorIs(t, err, failure)
	require.Empty(t, response.Metadata)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "beta\n", string(content), "history failure must not be mistaken for a no-write outcome")
}
