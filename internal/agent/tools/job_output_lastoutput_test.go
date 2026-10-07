package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// Revert-check map (test -> the production line it catches):
//   TestJobOutputLastOutput_HeaderRunningWithOutput    -> the `", last output %s ago"` header append in job_output.go
//   TestJobOutputLastOutput_HeaderRunningNoOutput      -> the `", no output yet"` header append in job_output.go
//   TestJobOutputLastOutput_HeaderCompletedUnchanged   -> the done branch keeping the header without any last-output clause

func runJobOutputHeader(t *testing.T, ctx context.Context, bgManager *shell.BackgroundShellManager, shellID string) string {
	t.Helper()
	tool := NewJobOutputTool(nil, nil, bgManager)
	input, err := json.Marshal(JobOutputParams{ShellID: shellID})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "test-call", Name: JobOutputToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	return resp.Content
}

// TestJobOutputLastOutput_HeaderRunningWithOutput proves a running shell whose
// buffers have had a Write gets a "last output <dur> ago" header clause.
func TestJobOutputLastOutput_HeaderRunningWithOutput(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "last-output-session")

	bgManager := shell.NewBackgroundShellManager()
	bgShell, err := bgManager.StartOwned(ctx, "last-output-session", workingDir, nil, "echo hi; sleep 2", "")
	require.NoError(t, err)
	t.Cleanup(func() { bgManager.Close(context.Background()) })

	time.Sleep(300 * time.Millisecond) // let the echo reach the buffer

	text := runJobOutputHeader(t, ctx, bgManager, bgShell.ID)
	require.Contains(t, text, "Status: running")
	require.Contains(t, text, "last output ")
	require.Regexp(t, `last output \d+s ago`, text)
}

// TestJobOutputLastOutput_HeaderRunningNoOutput proves a running shell with no
// writes gets a "no output yet" header clause.
func TestJobOutputLastOutput_HeaderRunningNoOutput(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "last-output-session")

	bgManager := shell.NewBackgroundShellManager()
	bgShell, err := bgManager.StartOwned(ctx, "last-output-session", workingDir, nil, "sleep 2", "")
	require.NoError(t, err)
	t.Cleanup(func() { bgManager.Close(context.Background()) })

	text := runJobOutputHeader(t, ctx, bgManager, bgShell.ID)
	require.Contains(t, text, "Status: running")
	require.Contains(t, text, "no output yet")
	require.NotContains(t, text, "last output")
}

// TestJobOutputLastOutput_HeaderCompletedUnchanged proves a completed job's
// header keeps its original shape with no last-output clause.
func TestJobOutputLastOutput_HeaderCompletedUnchanged(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "last-output-session")

	bgManager := shell.NewBackgroundShellManager()
	bgShell, err := bgManager.StartOwned(ctx, "last-output-session", workingDir, nil, "echo done", "")
	require.NoError(t, err)
	t.Cleanup(func() { bgManager.Close(context.Background()) })

	deadline := time.Now().Add(3 * time.Second)
	var text string
	for {
		text = runJobOutputHeader(t, ctx, bgManager, bgShell.ID)
		if strings.Contains(text, "Status: completed") {
			break
		}
		require.Less(t, time.Now(), deadline, "job did not complete in time")
		time.Sleep(50 * time.Millisecond)
	}

	require.Contains(t, text, "Status: completed")
	require.NotContains(t, text, "last output")
	require.NotContains(t, text, "no output yet")
}
