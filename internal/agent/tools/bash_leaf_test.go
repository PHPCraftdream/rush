package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

func TestBashLeafForegroundEquality(t *testing.T) {
	tool := newBashToolForTest(t.TempDir())
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "leaf-equality")
	for _, command := range []string{"echo equal", "printf err >&2; exit 7", "true", `echo "unclosed`} {
		p := BashParams{Command: command, Description: "same"}
		normal := runBashTool(t, tool, ctx, p)
		leaf := runBashTool(t, tool, WithShellLeaf(ctx, ShellLeaf{}), p)
		require.Equal(t, normal.Content, leaf.Content)
		require.Equal(t, normal.IsError, leaf.IsError)
		var a, b BashResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(normal.Metadata), &a))
		require.NoError(t, json.Unmarshal([]byte(leaf.Metadata), &b))
		a.StartTime, a.EndTime, b.StartTime, b.EndTime = 0, 0, 0, 0
		require.Equal(t, a, b)
	}
}

func TestBashLeafGuardsAndPermissionOnce(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		linked, allow bool
		calls         int
	}{
		{"blocked", "sudo echo denied", false, true, 0},
		{"agent", "claude", false, true, 0},
		{"git", "git checkout -- file", true, true, 0},
		{"permission", "printf denied", false, false, 1},
		{"allowed", "printf allowed", false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.linked {
				dir = linkedWorktreeFixture(t)
			}
			tool, perms := newBashToolWithRecordingPerms(dir, tc.allow)
			ctx := context.WithValue(t.Context(), SessionIDContextKey, "leaf-policy")
			var handle *shell.BackgroundExecution
			response := runBashTool(t, tool, WithShellLeaf(ctx, ShellLeaf{OnStart: func(e *shell.BackgroundExecution) { handle = e }}), BashParams{Command: tc.command})
			require.Equal(t, tc.calls, perms.requestCount)
			if tc.name == "allowed" {
				require.NotNil(t, handle)
				require.NoError(t, handle.Dispose(t.Context()))
			} else {
				require.Nil(t, handle)
				require.True(t, response.IsError)
			}
		})
	}
}

func TestBashLeafCancellationWaitsDoneNoBackgroundEscape(t *testing.T) {
	dir := t.TempDir()
	manager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { manager.Close(context.Background()) })
	tool := NewBashTool(&mockBashPermissionService{}, dir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, manager)
	base := context.WithValue(t.Context(), SessionIDContextKey, "leaf-cancel")
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	started := make(chan *shell.BackgroundExecution, 1)
	ctx = WithShellLeaf(ctx, ShellLeaf{OnStart: func(e *shell.BackgroundExecution) { started <- e }})
	input, err := json.Marshal(BashParams{Command: "printf ready; sleep 30", RunInBackground: true, AutoBackgroundAfter: -1})
	require.NoError(t, err)
	returned := make(chan error, 1)
	response := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "leaf", Name: BashToolName, Input: string(input)})
		response <- resp
		returned <- err
	}()
	var e *shell.BackgroundExecution
	select {
	case e = <-started:
	case <-returned:
		t.Fatal("LEAF_BACKGROUND_ESCAPE_BEFORE_ONSTART")
	case <-time.After(10 * time.Second):
		t.Fatal("leaf did not publish handle")
	}
	readCtx, readCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer readCancel()
	chunk, err := e.ReadOutput(readCtx, 0)
	require.NoError(t, err)
	require.Equal(t, "ready", string(chunk.Data))
	select {
	case <-returned:
		t.Fatal("leaf escaped as background work")
	default:
	}
	cancel()
	select {
	case err := <-returned:
		require.ErrorIs(t, err, context.Canceled)
	case <-readCtx.Done():
		t.Fatal("cancelled leaf did not return")
	}
	select {
	case <-e.Done():
	default:
		t.Fatal("leaf returned before real done")
	}
	require.Contains(t, (<-response).Content, "ready")
	require.NoError(t, e.Dispose(t.Context()))
}
