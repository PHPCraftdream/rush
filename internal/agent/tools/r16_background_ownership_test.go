package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

func TestBackgroundJobTools_EnforceSessionOwnership(t *testing.T) {
	manager := shell.NewBackgroundShellManager()
	job, err := manager.StartOwned(t.Context(), "session-a", t.TempDir(), nil, "sleep 30", "private")
	require.NoError(t, err)
	t.Cleanup(func() { manager.Close(context.Background()) })

	outputTool := NewJobOutputTool(manager)
	killTool := NewJobKillTool(manager)

	input, err := json.Marshal(JobOutputParams{ShellID: job.ID})
	require.NoError(t, err)
	foreignCtx := context.WithValue(context.Background(), SessionIDContextKey, "session-b")
	response, err := outputTool.Run(foreignCtx, fantasy.ToolCall{
		ID: "foreign-output", Name: JobOutputToolName, Input: string(input),
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "background shell not found")
	require.False(t, job.IsDone())

	killInput, err := json.Marshal(JobKillParams{ShellID: job.ID})
	require.NoError(t, err)
	response, err = killTool.Run(foreignCtx, fantasy.ToolCall{
		ID: "foreign-kill", Name: JobKillToolName, Input: string(killInput),
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "background shell not found")
	require.False(t, job.IsDone())

	noSessionCtx := context.Background()
	response, err = outputTool.Run(noSessionCtx, fantasy.ToolCall{
		ID: "missing-owner", Name: JobOutputToolName, Input: string(input),
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "session ID is required")
	response, err = killTool.Run(noSessionCtx, fantasy.ToolCall{
		ID: "missing-owner-kill", Name: JobKillToolName, Input: string(killInput),
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "session ID is required")
	require.False(t, job.IsDone())

	ownerCtx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	response, err = killTool.Run(ownerCtx, fantasy.ToolCall{
		ID: "owner-kill", Name: JobKillToolName, Input: string(killInput),
	})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.True(t, job.IsDone())
}
