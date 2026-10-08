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
func TestAwaitTasksTool_TopLevelStopsTurn(t *testing.T) {
	t.Parallel()
	resp := awaitResp(t, AwaitTasksResult{Mode: "any", WaitingFor: []AwaitTaskRef{{ToolName: "bash", ToolCallID: "c1"}}})
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "Sleeping (any)")
}
