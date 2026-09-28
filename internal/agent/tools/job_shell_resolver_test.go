package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// fakeJobShellResolver is a minimal JobShellResolver for testing job_kill/
// job_output's job_id wiring without a real work ledger (internal/agent,
// which this package must not import).
type fakeJobShellResolver struct {
	shellID                    string
	err                        error
	calledSession, calledJob   string
	stoppedSession, stoppedJob string
}

func (f *fakeJobShellResolver) ResolveJobShellID(sessionID, jobID string) (string, error) {
	f.calledSession, f.calledJob = sessionID, jobID
	if f.err != nil {
		return "", f.err
	}
	return f.shellID, nil
}

func (f *fakeJobShellResolver) MarkJobStopped(sessionID, jobID string) {
	f.stoppedSession, f.stoppedJob = sessionID, jobID
}

func TestResolveShellID_NeitherGivenIsRejected(t *testing.T) {
	_, err := resolveShellID(nil, "session-a", "", "")
	require.ErrorIs(t, err, errJobShellIDRequired)
}

func TestResolveShellID_BothGivenIsRejected(t *testing.T) {
	_, err := resolveShellID(&fakeJobShellResolver{}, "session-a", "job-1", "shell-1")
	require.ErrorIs(t, err, errJobShellIDRequired)
}

func TestResolveShellID_ShellIDBypassesResolver(t *testing.T) {
	resolver := &fakeJobShellResolver{}
	got, err := resolveShellID(resolver, "session-a", "", "  shell-1  ")
	require.NoError(t, err)
	require.Equal(t, "shell-1", got)
	require.Empty(t, resolver.calledJob, "resolver must not be consulted when shell_id is given directly")
}

func TestResolveShellID_JobIDDelegatesToResolverScopedToSession(t *testing.T) {
	resolver := &fakeJobShellResolver{shellID: "003"}
	got, err := resolveShellID(resolver, "session-a", "  job-1  ", "")
	require.NoError(t, err)
	require.Equal(t, "003", got)
	require.Equal(t, "session-a", resolver.calledSession)
	require.Equal(t, "job-1", resolver.calledJob)
}

func TestResolveShellID_JobIDResolverErrorIsSurfaced(t *testing.T) {
	resolver := &fakeJobShellResolver{err: errors.New("job job-1 not found (not owned by this session)")}
	_, err := resolveShellID(resolver, "session-a", "job-1", "")
	require.EqualError(t, err, "job job-1 not found (not owned by this session)")
}

func TestResolveShellID_NilResolverRefusesJobID(t *testing.T) {
	_, err := resolveShellID(nil, "session-a", "job-1", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "job-1")
}

// TestJobKillTool_ResolvesJobIDToShellID proves job_kill's job_id path end
// to end: given a resolver, job_id="call-1" is resolved to the shell id and
// that shell is the one actually killed.
func TestJobKillTool_ResolvesJobIDToShellID(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")

	bgManager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { bgManager.Close(context.Background()) })
	bgShell, err := bgManager.StartOwned(ctx, "session-a", workingDir, nil, "sleep 30", "")
	require.NoError(t, err)

	resolver := &fakeJobShellResolver{shellID: bgShell.ID}
	tool := NewJobKillTool(resolver, nil, bgManager)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "call-1")
	require.Contains(t, resp.Content, bgShell.ID)
	require.True(t, bgShell.IsDone())
	require.Equal(t, "session-a", resolver.calledSession)
	require.Equal(t, "call-1", resolver.calledJob)
}

// TestJobKillTool_JobIDResolverErrorSurfacesAsRecoverableResponse proves a
// resolver failure (foreign session, still starting, not a command, ...)
// reaches the model as an ordinary tool error, not a fatal Go error.
func TestJobKillTool_JobIDResolverErrorSurfacesAsRecoverableResponse(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	bgManager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { bgManager.Close(context.Background()) })

	resolver := &fakeJobShellResolver{err: errors.New("job call-1 not found (not owned by this session, or already delivered)")}
	tool := NewJobKillTool(resolver, nil, bgManager)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "not owned by this session")
}

func TestJobKillTool_BothJobIDAndShellIDRejected(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	tool := NewJobKillTool(nil, nil, shell.NewBackgroundShellManager())
	input, err := json.Marshal(JobKillParams{JobID: "call-1", ShellID: "003"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "exactly one of job_id or shell_id")
}

func TestJobKillTool_NeitherJobIDNorShellIDRejected(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	tool := NewJobKillTool(nil, nil, shell.NewBackgroundShellManager())
	input, err := json.Marshal(JobKillParams{})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "exactly one of job_id or shell_id")
}

// TestJobOutputTool_ResolvesJobIDToShellID mirrors the job_kill case for
// job_output: job_id resolves to the shell whose output is read.
func TestJobOutputTool_ResolvesJobIDToShellID(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	bgManager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { bgManager.Close(context.Background()) })
	bgShell, err := bgManager.StartOwned(ctx, "session-a", workingDir, nil, "echo hi-from-job-id", "")
	require.NoError(t, err)
	require.Eventually(t, bgShell.IsDone, 5*time.Second, 25*time.Millisecond)

	resolver := &fakeJobShellResolver{shellID: bgShell.ID}
	tool := NewJobOutputTool(resolver, nil, bgManager)

	input, err := json.Marshal(JobOutputParams{JobID: "call-2"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "out-call", Name: JobOutputToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "hi-from-job-id")
	require.Equal(t, "call-2", resolver.calledJob)
}

func TestJobOutputTool_BothJobIDAndShellIDRejected(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	tool := NewJobOutputTool(nil, nil, shell.NewBackgroundShellManager())
	input, err := json.Marshal(JobOutputParams{JobID: "call-1", ShellID: "003"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "out-call", Name: JobOutputToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "exactly one of job_id or shell_id")
}
