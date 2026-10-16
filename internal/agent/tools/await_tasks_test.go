package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// fakeAwaitControl returns a canned result.
type fakeAwaitControl struct {
	res AwaitTasksResult
}

func (f *fakeAwaitControl) AwaitTasks(context.Context, string, string, int) (AwaitTasksResult, error) {
	return f.res, nil
}

func awaitResp(t *testing.T, res AwaitTasksResult) fantasy.ToolResponse {
	t.Helper()
	tool := NewAwaitTasksTool(&fakeAwaitControl{res: res})
	input, err := json.Marshal(AwaitTasksParams{Until: "any"})
	require.NoError(t, err)
	resp, runErr := tool.Run(
		context.WithValue(context.Background(), SessionIDContextKey, "sess-1"),
		fantasy.ToolCall{ID: "call-1", Name: AwaitTasksToolName, Input: string(input)})
	require.NoError(t, runErr)
	return resp
}

// TestAwaitTasksTool_BlockedDoesNotStopTurn pins the Blocked branch in the
// tool body: a worker wake must NOT end the turn (the delegation stays open).
func TestAwaitTasksTool_BlockedDoesNotStopTurn(t *testing.T) {
	t.Parallel()
	resp := awaitResp(t, AwaitTasksResult{Mode: "any", Blocked: true, FinishedCount: 1})
	require.False(t, resp.StopTurn, "a blocked worker wake must not stop the turn")
	require.Contains(t, resp.Content, "Woke: 1 task(s) finished.")
	require.NotContains(t, resp.Content, "batch_status/batch_output/batch_stop")
}

func TestBatchAwaitTasksTool_BlockedRendersControlsForBatchRoots(t *testing.T) {
	t.Parallel()
	tool := NewBatchAwaitTasksTool(&fakeAwaitControl{res: AwaitTasksResult{
		Mode: "any", Blocked: true,
		WaitingFor: []AwaitTaskRef{{ToolName: "batch_run", ToolCallID: "call", BatchRootID: "root-1", BatchNodeID: "node-2"}},
	}})
	input, err := json.Marshal(AwaitTasksParams{Until: "any"})
	require.NoError(t, err)
	resp, err := tool.Run(context.WithValue(context.Background(), SessionIDContextKey, "sess-1"), fantasy.ToolCall{ID: "call-1", Name: AwaitTasksToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.StopTurn)
	require.Contains(t, resp.Content, "root_id root-1 node_id node-2")
	require.Contains(t, resp.Content, "batch_status/batch_output/batch_stop")
}

func TestBatchAwaitTasksTool_WithoutBatchRootOmitsControlsHint(t *testing.T) {
	t.Parallel()
	tool := NewBatchAwaitTasksTool(&fakeAwaitControl{res: AwaitTasksResult{Mode: "all", WaitingFor: []AwaitTaskRef{{ToolName: "agent", ToolCallID: "call", ChildSessionID: "child"}}}})
	input, err := json.Marshal(AwaitTasksParams{Until: "all"})
	require.NoError(t, err)
	resp, err := tool.Run(context.WithValue(context.Background(), SessionIDContextKey, "sess-1"), fantasy.ToolCall{ID: "call-1", Name: AwaitTasksToolName, Input: string(input)})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "child session child")
	require.NotContains(t, resp.Content, "batch_status/batch_output/batch_stop")
}

// TestAwaitTasksTool_TimedOutMentionsDeadline pins the TimedOut note.
func TestAwaitTasksTool_TimedOutMentionsDeadline(t *testing.T) {
	t.Parallel()
	resp := awaitResp(t, AwaitTasksResult{
		Mode: "any", Blocked: true, TimedOut: true,
		WaitingFor: []AwaitTaskRef{{ToolName: "bash", ToolCallID: "c1"}},
	})
	require.False(t, resp.StopTurn)
	require.Contains(t, resp.Content, "deadline elapsed")
	require.Contains(t, resp.Content, "await_tasks again")
}

// TestAwaitTasksTool_TopLevelStopsTurn pins that the non-blocked (top-level)
// rendering keeps StopTurn=true exactly as before.
func TestAwaitTasksTool_BatchIDsRendered(t *testing.T) {
	t.Parallel()
	tool := NewBatchAwaitTasksTool(&fakeAwaitControl{res: AwaitTasksResult{Mode: "all", WaitingFor: []AwaitTaskRef{{ToolName: "batch_run", ToolCallID: "call", BatchRootID: "root-1", BatchNodeID: "node-2"}}}})
	input, err := json.Marshal(AwaitTasksParams{Until: "all"})
	require.NoError(t, err)
	resp, err := tool.Run(context.WithValue(context.Background(), SessionIDContextKey, "sess-1"), fantasy.ToolCall{ID: "call-1", Name: AwaitTasksToolName, Input: string(input)})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "root_id root-1 node_id node-2", "await output must expose stable batch IDs")
	require.Contains(t, resp.Content, "batch_status/batch_output/batch_stop", "await output must document batch controls")
}

func TestAwaitTasksTool_FlagOffRenderingPreservesChildSession(t *testing.T) {
	t.Parallel()
	resp := awaitResp(t, AwaitTasksResult{Mode: "all", WaitingFor: []AwaitTaskRef{{ToolName: "agent", ToolCallID: "call", ChildSessionID: "child-session"}}})
	require.Contains(t, resp.Content, "child session child-session")
	require.NotContains(t, resp.Content, "batch root_id")
}

func TestAwaitTasksTool_FlagOffBlockedRenderingPreservesChildSession(t *testing.T) {
	t.Parallel()
	resp := awaitResp(t, AwaitTasksResult{Mode: "all", Blocked: true, WaitingFor: []AwaitTaskRef{{ToolName: "agent", ToolCallID: "call", ChildSessionID: "child-session"}}})
	require.Equal(t, "Woke: 0 task(s) finished.\nStill running:\n- agent (tool_call_id call, running for 0s)\nCall await_tasks again to keep waiting; do not end your turn while jobs you still need are running.", resp.Content)
}

func TestAwaitTasksTool_TopLevelStopsTurn(t *testing.T) {
	t.Parallel()
	resp := awaitResp(t, AwaitTasksResult{Mode: "any", WaitingFor: []AwaitTaskRef{{ToolName: "bash", ToolCallID: "c1"}}})
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "Sleeping (any)")
}
