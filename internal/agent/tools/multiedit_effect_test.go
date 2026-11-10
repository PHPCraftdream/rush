package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/stretchr/testify/require"
)

func runMultiEditEffect(t *testing.T, tool fantasy.AgentTool, path string, edits []MultiEditOperation) (fantasy.ToolResponse, error) {
	t.Helper()
	input, err := json.Marshal(MultiEditParams{FilePath: path, Edits: edits})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "multiedit-effect")
	return tool.Run(ctx, fantasy.ToolCall{ID: "multiedit", Name: MultiEditToolName, Input: string(input)})
}

func TestMultiEditFinalEqualityProofPrecedesPermissionsAndHistory(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, tc := range []struct {
		name, content string
		edits         []MultiEditOperation
		failed        []int
	}{
		{"missing", "alpha\nbeta\n", []MultiEditOperation{{OldString: "missing", NewString: "new"}}, []int{1}},
		{"equal", "alpha\nbeta\n", []MultiEditOperation{{OldString: "alpha", NewString: "alpha"}}, nil},
		{"CRLF equal", "alpha\r\nbeta\r\n", []MultiEditOperation{{OldString: "alpha", NewString: "alpha"}}, nil},
		{"multiple matches", "alpha\nalpha\n", []MultiEditOperation{{OldString: "alpha", NewString: "new"}}, []int{1}},
		{"multiple equal matches", "alpha\nalpha\n", []MultiEditOperation{{OldString: "alpha", NewString: "alpha"}}, []int{1}},
		{"net cancel", "alpha\nbeta\n", []MultiEditOperation{{OldString: "alpha", NewString: "new"}, {OldString: "new", NewString: "alpha"}}, nil},
		{"equal and failures", "alpha\nbeta\n", []MultiEditOperation{{OldString: "missing", NewString: "new"}, {OldString: "alpha", NewString: "alpha"}, {OldString: "absent", NewString: ""}}, []int{1, 3}},
		{"net cancel and failure", "alpha\nbeta\n", []MultiEditOperation{{OldString: "alpha", NewString: "new"}, {OldString: "absent", NewString: ""}, {OldString: "new", NewString: "alpha"}}, []int{2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.txt")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Hour)}
			// Nil services prove final equality exits before permissions/history.
			response, err := runMultiEditEffect(t, NewMultiEditTool(nil, nil, tracker, dir), path, tc.edits)
			require.NoError(t, err)
			require.True(t, response.IsError)
			require.Equal(t, fmt.Sprintf("no changes made; %d edit operation(s) failed", len(tc.failed)), response.Content)
			var proof EditEffectMetadata
			require.NoError(t, json.Unmarshal([]byte(response.Metadata), &proof))
			require.True(t, proof.EditEffect.NoWrite)
			require.Equal(t, "unchanged_content", proof.EditEffect.Reason)
			var metadata MultiEditResponseMetadata
			require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
			require.Zero(t, metadata.EditsApplied, "preserve existing final-equality accounting")
			require.Zero(t, metadata.Additions)
			require.Zero(t, metadata.Removals)
			require.Len(t, metadata.EditsFailed, len(tc.failed))
			for i, index := range tc.failed {
				failure := metadata.EditsFailed[i]
				require.Equal(t, index, failure.Index)
				require.Equal(t, tc.edits[index-1], failure.Edit)
				_, matchErr := applyEditToContent(tc.content, failure.Edit)
				require.Error(t, matchErr)
				require.Equal(t, matchErr.Error(), failure.Error)
			}
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(content))
			require.Empty(t, tracker.reads)
		})
	}
}

func TestMultiEditChangedPartialWriteAndHistoryErrorRemainEffectful(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, failHistory := range []bool{false, true} {
		t.Run(fmt.Sprint(failHistory), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.txt")
			require.NoError(t, os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600))
			var files history.Service = &mockHistoryService{}
			failure := errors.New("history failed after multiedit write")
			if failHistory {
				files = &editEffectFailingHistory{err: failure}
			}
			tool := NewMultiEditTool(&mockPermissionService{}, files, &mockEditFileTracker{lastRead: time.Now().Add(time.Hour)}, dir)
			response, err := runMultiEditEffect(t, tool, path, []MultiEditOperation{{OldString: "alpha", NewString: "new"}, {OldString: "missing", NewString: "bad"}})
			if failHistory {
				require.ErrorIs(t, err, failure)
			} else {
				require.NoError(t, err)
				require.False(t, response.IsError)
				var metadata MultiEditResponseMetadata
				require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
				require.Equal(t, 1, metadata.EditsApplied)
				require.Len(t, metadata.EditsFailed, 1)
				require.Equal(t, 2, metadata.EditsFailed[0].Index)
			}
			require.NotContains(t, response.Metadata, "edit_effect")
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "new\nbeta\n", string(content))
		})
	}
}

func TestMultiEditEmptyFileCreationRemainsEffectful(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "created.txt")
	tool := NewMultiEditTool(&mockPermissionService{}, &mockHistoryService{}, &mockEditFileTracker{}, dir)
	response, err := runMultiEditEffect(t, tool, path, []MultiEditOperation{{}})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.NotContains(t, response.Metadata, "edit_effect")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Empty(t, content)
}

// RecordRead advances the tracker to the controlled disk timestamp without sleeps.
type effectRefreshTracker struct {
	mockEditFileTracker
	refreshed time.Time
}

func (m *effectRefreshTracker) RecordRead(ctx context.Context, sessionID, path string) {
	m.mockEditFileTracker.RecordRead(ctx, sessionID, path)
	m.lastRead = m.refreshed
}

func TestNativeNoWriteExternalRefreshIsConservativeThenCertifiable(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, name := range []string{EditToolName, MultiEditToolName} {
		for _, old := range []string{"missing", "alpha"} {
			t.Run(name+"/"+old, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "fixture.txt")
				require.NoError(t, os.WriteFile(path, []byte("alpha\nalpha\n"), 0o600))
				mtime := time.Unix(1700000010, 0)
				require.NoError(t, os.Chtimes(path, mtime, mtime))
				tracker := &effectRefreshTracker{mockEditFileTracker: mockEditFileTracker{lastRead: mtime.Add(-time.Hour)}, refreshed: mtime.Add(time.Second)}
				var tool fantasy.AgentTool
				var input []byte
				var err error
				if name == EditToolName {
					tool = NewEditTool(nil, nil, tracker, dir)
					input, err = json.Marshal(EditParams{FilePath: path, OldString: old, NewString: old})
				} else {
					tool = NewMultiEditTool(nil, nil, tracker, dir)
					input, err = json.Marshal(MultiEditParams{FilePath: path, Edits: []MultiEditOperation{{OldString: old, NewString: old}}})
				}
				require.NoError(t, err)
				ctx := context.WithValue(t.Context(), SessionIDContextKey, "refresh")
				for attempt := range 2 {
					response, err := tool.Run(ctx, fantasy.ToolCall{ID: "refresh", Name: name, Input: string(input)})
					require.NoError(t, err)
					require.True(t, response.IsError)
					if attempt == 0 {
						require.NotContains(t, response.Metadata, "edit_effect", "refresh must invalidate progress coverage")
					} else {
						var proof EditEffectMetadata
						require.NoError(t, json.Unmarshal([]byte(response.Metadata), &proof))
						require.True(t, proof.EditEffect.NoWrite, "later stable attempts may qualify")
					}
				}
				require.Equal(t, []string{path}, tracker.reads)
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "alpha\nalpha\n", string(content))
			})
		}
	}
}
