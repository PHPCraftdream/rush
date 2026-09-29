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
	// markText/markVerdict are MarkJobStopped's own return values (task
	// #1063, B11). The zero verdict (JobStopNotFound) makes job_kill refuse
	// without touching the shell manager.
	markText    string
	markVerdict JobStopVerdict
}

func (f *fakeJobShellResolver) ResolveJobShellID(sessionID, jobID string) (string, error) {
	f.calledSession, f.calledJob = sessionID, jobID
	if f.err != nil {
		return "", f.err
	}
	return f.shellID, nil
}

func (f *fakeJobShellResolver) MarkJobStopped(sessionID, jobID string) (string, JobStopVerdict) {
	f.stoppedSession, f.stoppedJob = sessionID, jobID
	return f.markText, f.markVerdict
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

	// markVerdict: JobStopStopped -- the shell is genuinely still running (StartOwned
	// above), so a real ledger would report a fresh stop here; B11 made
	// job_kill refuse outright (never touching bgManager) when the
	// resolver reports ok=false, so this fixture must reflect a real
	// fresh stop to keep exercising the kill path this test is about.
	resolver := &fakeJobShellResolver{shellID: bgShell.ID, markVerdict: JobStopStopped}
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

// TestJobKillTool_UsesResolverTextAsFinalAnswer proves task #1063's wiring
// in job_kill.go itself: when MarkJobStopped reports a fresh stop (JobStopStopped)
// with its own text, job_kill returns that text VERBATIM as the tool's
// final answer instead of its generic "terminated successfully" wording --
// the real output snapshot the resolver captured is what the model sees.
func TestJobKillTool_UsesResolverTextAsFinalAnswer(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")

	bgManager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { bgManager.Close(context.Background()) })
	bgShell, err := bgManager.StartOwned(ctx, "session-a", workingDir, nil, "sleep 30", "")
	require.NoError(t, err)

	resolver := &fakeJobShellResolver{
		shellID:     bgShell.ID,
		markText:    "Async job call-1 (bash) was stopped (job_kill). Partial output before the stop:\n\nline one\nline two",
		markVerdict: JobStopStopped,
	}
	tool := NewJobKillTool(resolver, nil, bgManager)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, resolver.markText, resp.Content, "job_kill's own final answer must be the resolver's real-output text, not the generic wording")
	require.True(t, bgShell.IsDone(), "the shell must still actually be killed on a fresh stop")
}

// TestJobKillTool_NotFoundVerdictRefusesWithoutTouchingShellManager pins B11:
// when MarkJobStopped reports JobStopNotFound (a concurrent job_kill already
// claimed the job, or it is gone/delivered), job_kill must refuse outright --
// never fall through to bgManager and fabricate a "terminated successfully"
// answer for a stop it did not actually cause. The shell here is genuinely
// still running (StartOwned), so a pre-fix job_kill would happily kill it and
// claim credit; the fix must leave it untouched.
//
// Revert-check performed: made job_kill.go fall through to bgManager for any
// verdict other than JobStopStopped -- this test FAILED (resp.IsError was
// false and bgShell.IsDone() was true). Restored; re-ran, passed.
func TestJobKillTool_NotFoundVerdictRefusesWithoutTouchingShellManager(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")

	bgManager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { bgManager.Close(context.Background()) })
	bgShell, err := bgManager.StartOwned(ctx, "session-a", workingDir, nil, "sleep 30", "")
	require.NoError(t, err)

	// markVerdict left at its zero value (JobStopNotFound).
	resolver := &fakeJobShellResolver{shellID: bgShell.ID}
	tool := NewJobKillTool(resolver, nil, bgManager)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "not found")
	require.Contains(t, resp.Content, "already stopped")
	require.False(t, bgShell.IsDone(), "must not kill a shell it did not actually stop")
}

// TestJobKillTool_AlreadyTerminalAnswersFromCommittedRowWithoutKilling pins
// B11's lost-race half: when MarkJobStopped reports JobStopAlreadyTerminal
// with text worded from the COMMITTED row, job_kill returns that text
// verbatim (not an error, no fabricated "stopped"/"terminated successfully"),
// carries no JobKillResponseMetadata (so A3's result fusion has nothing to
// record), and never calls bgManager.KillOwned -- proven with a genuinely
// running shell that must survive, and a second scenario where the shell
// manager no longer knows the shell at all, which must NOT surface the
// manager's generic "background shell not found" text.
//
// Revert-check performed: reverted to the previous two-state contract
// (lost-race answered with the text but still fell through to KillOwned) --
// the first scenario FAILED (bgShell.IsDone() was true); moving the shell
// lookup back before the verdict made the second scenario FAILED (content
// was "background shell not found: ..."). Restored; re-ran, passed.
func TestJobKillTool_AlreadyTerminalAnswersFromCommittedRowWithoutKilling(t *testing.T) {
	const committed = "Async job call-1 (bash) finished.\n\nreal committed output"
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")

	t.Run("live shell survives", func(t *testing.T) {
		bgManager := shell.NewBackgroundShellManager()
		t.Cleanup(func() { bgManager.Close(context.Background()) })
		bgShell, err := bgManager.StartOwned(ctx, "session-a", t.TempDir(), nil, "sleep 30", "")
		require.NoError(t, err)

		resolver := &fakeJobShellResolver{shellID: bgShell.ID, markText: committed, markVerdict: JobStopAlreadyTerminal}
		tool := NewJobKillTool(resolver, nil, bgManager)
		input, err := json.Marshal(JobKillParams{JobID: "call-1"})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Equal(t, committed, resp.Content)
		require.Empty(t, resp.Metadata, "no JobKillResponseMetadata: nothing for A3's fusion to record")
		require.False(t, bgShell.IsDone(), "job_kill must not kill a shell for a job that already reached a terminal state")
	})

	t.Run("shell manager no longer knows the shell", func(t *testing.T) {
		bgManager := shell.NewBackgroundShellManager()
		t.Cleanup(func() { bgManager.Close(context.Background()) })

		resolver := &fakeJobShellResolver{shellID: "gone-shell", markText: committed, markVerdict: JobStopAlreadyTerminal}
		tool := NewJobKillTool(resolver, nil, bgManager)
		input, err := json.Marshal(JobKillParams{JobID: "call-1"})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Equal(t, committed, resp.Content, "the committed row's answer, never the shell manager's generic not-found text")
		require.NotContains(t, resp.Content, "background shell not found")
	})
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
