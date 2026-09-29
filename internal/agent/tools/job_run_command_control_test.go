package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// fakeRunCommandJobResolver is a JobShellResolver that always classifies a
// job_id as a run_command job (RunCommandJobError), for testing job_kill/
// job_output's routing to RunCommandController (task #1023 §3) without a
// real work ledger (internal/agent, which this package must not import).
type fakeRunCommandJobResolver struct {
	stoppedSession, stoppedJob string
}

func (f *fakeRunCommandJobResolver) ResolveJobShellID(_, jobID string) (string, error) {
	return "", &RunCommandJobError{JobID: jobID}
}

func (f *fakeRunCommandJobResolver) MarkJobStopped(sessionID, jobID string) (string, JobStopVerdict) {
	f.stoppedSession, f.stoppedJob = sessionID, jobID
	return "", JobStopNotFound
}

// fakeDelegationJobResolver always classifies a job_id as a delegation.
type fakeDelegationJobResolver struct{}

func (f *fakeDelegationJobResolver) ResolveJobShellID(_, jobID string) (string, error) {
	return "", &DelegationJobError{JobID: jobID, ChildSessionID: "child-session-x"}
}

func (f *fakeDelegationJobResolver) MarkJobStopped(string, string) (string, JobStopVerdict) {
	return "", JobStopNotFound
}

// fakeRunCommandController is a minimal RunCommandController for testing.
type fakeRunCommandController struct {
	data       string
	done       bool
	nextCursor int64
	outputErr  error
	stopErr    error

	calledSession, calledJob string
	calledCursor             int64
	stopCalledSession        string
	stopCalledJob            string
	// stopText is StopRunCommandJob's own success return value (task
	// #1063). Empty in every test that does not set it, reproducing
	// job_kill's pre-existing generic-wording fallback.
	stopText string
}

func (f *fakeRunCommandController) RunCommandOutput(sessionID, jobID string, cursor int64) (string, bool, int64, error) {
	f.calledSession, f.calledJob, f.calledCursor = sessionID, jobID, cursor
	if f.outputErr != nil {
		return "", false, 0, f.outputErr
	}
	return f.data, f.done, f.nextCursor, nil
}

func (f *fakeRunCommandController) StopRunCommandJob(sessionID, jobID string) (string, error) {
	f.stopCalledSession, f.stopCalledJob = sessionID, jobID
	if f.stopErr != nil {
		return "", f.stopErr
	}
	return f.stopText, nil
}

// TestJobOutputTool_RunCommandJobRoutesToController proves job_output routes
// a run_command job_id to RunCommandController instead of failing with
// RunCommandJobError's raw text (task #1023 §3).
func TestJobOutputTool_RunCommandJobRoutesToController(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	resolver := &fakeRunCommandJobResolver{}
	runCtl := &fakeRunCommandController{data: "partial output", done: false, nextCursor: 42}
	tool := NewJobOutputTool(resolver, runCtl)

	input, err := json.Marshal(JobOutputParams{JobID: "call-1", Cursor: 7})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "out-call", Name: JobOutputToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "partial output")
	require.Contains(t, resp.Content, "running")
	require.Equal(t, "session-a", runCtl.calledSession)
	require.Equal(t, "call-1", runCtl.calledJob)
	require.EqualValues(t, 7, runCtl.calledCursor)

	var meta JobOutputResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.EqualValues(t, 42, meta.NextCursor)
	require.False(t, meta.Done)
}

// TestJobOutputTool_RunCommandJobWithoutControllerFallsBackToResolverError
// proves that with no RunCommandController wired, job_output still returns
// RunCommandJobError's plain text (pre-#1023 behavior), not a nil-pointer
// panic.
func TestJobOutputTool_RunCommandJobWithoutControllerFallsBackToResolverError(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	resolver := &fakeRunCommandJobResolver{}
	tool := NewJobOutputTool(resolver, nil)

	input, err := json.Marshal(JobOutputParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "out-call", Name: JobOutputToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "run_command job")
}

// TestJobKillTool_RunCommandJobRoutesToController proves job_kill stops a
// run_command job via RunCommandController.StopRunCommandJob instead of
// refusing it (task #1023 §3).
func TestJobKillTool_RunCommandJobRoutesToController(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	resolver := &fakeRunCommandJobResolver{}
	runCtl := &fakeRunCommandController{}
	tool := NewJobKillTool(resolver, runCtl)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "call-1")
	require.Equal(t, "session-a", runCtl.stopCalledSession)
	require.Equal(t, "call-1", runCtl.stopCalledJob)
}

// TestJobKillTool_RunCommandJobUsesControllerTextAsFinalAnswer proves task
// #1063's wiring for run_command: job_kill returns StopRunCommandJob's own
// success text verbatim, not the old "kill requested; result will arrive as
// a message" placeholder -- job_kill produces no second, later notice for a
// run_command job either.
func TestJobKillTool_RunCommandJobUsesControllerTextAsFinalAnswer(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	resolver := &fakeRunCommandJobResolver{}
	runCtl := &fakeRunCommandController{stopText: "Async job call-1 (run_command) was stopped (job_kill). Partial output before the stop:\n\nline one"}
	tool := NewJobKillTool(resolver, runCtl)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, runCtl.stopText, resp.Content)
	require.NotContains(t, resp.Content, "will arrive as a message", "a fresh stop's real output IS the answer, not a promise of a later one")
}

// TestJobKillTool_RunCommandStopErrorSurfacesAsRecoverableResponse proves a
// StopRunCommandJob failure (already stopped, not found) reaches the model
// as an ordinary tool error, not a fatal Go error.
func TestJobKillTool_RunCommandStopErrorSurfacesAsRecoverableResponse(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	resolver := &fakeRunCommandJobResolver{}
	runCtl := &fakeRunCommandController{stopErr: errNotFoundForTest("call-1")}
	tool := NewJobKillTool(resolver, runCtl)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "not found")
}

// TestJobKillTool_DelegationJobIsRefusedNotRoutedToRunCommand proves a
// delegation job_id is refused with DelegationJobError's own text, not
// mistaken for a run_command job.
func TestJobKillTool_DelegationJobIsRefusedNotRoutedToRunCommand(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	resolver := &fakeDelegationJobResolver{}
	runCtl := &fakeRunCommandController{}
	tool := NewJobKillTool(resolver, runCtl)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "sub-agent delegation")
	require.Contains(t, resp.Content, "child-session-x")
	require.NotContains(t, resp.Content, "stop_agent", "stage 3's stop_agent does not exist yet")
	require.Empty(t, runCtl.stopCalledJob, "must not route a delegation job_id to RunCommandController")
}

// TestJobOutputTool_DelegationJobIsRefusedNotRoutedToRunCommand mirrors the
// job_kill case for job_output.
func TestJobOutputTool_DelegationJobIsRefusedNotRoutedToRunCommand(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	resolver := &fakeDelegationJobResolver{}
	runCtl := &fakeRunCommandController{}
	tool := NewJobOutputTool(resolver, runCtl)

	input, err := json.Marshal(JobOutputParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "out-call", Name: JobOutputToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "sub-agent delegation")
	require.Contains(t, resp.Content, "its result will arrive as a session message")
	require.Empty(t, runCtl.calledJob, "must not route a delegation job_id to RunCommandController")
}

// TestJobKillTool_MarksJobStoppedBeforeKilling proves job_kill records the
// ledger's stop-on-request marker (§2.2) for a resolved shell_id job BEFORE
// the actual kill, using the resolver -- not just the run_command path.
func TestJobKillTool_MarksJobStoppedBeforeKilling(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")

	bgManager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { bgManager.Close(context.Background()) })
	bgShell, err := bgManager.StartOwned(ctx, "session-a", workingDir, nil, "sleep 30", "")
	require.NoError(t, err)

	// markVerdict: JobStopStopped -- see job_shell_resolver_test.go's
	// TestJobKillTool_ResolvesJobIDToShellID for why (B11: job_kill now
	// refuses outright on ok=false instead of falling through to bgManager).
	resolver := &fakeJobShellResolver{shellID: bgShell.ID, markVerdict: JobStopStopped}
	tool := NewJobKillTool(resolver, nil, bgManager)

	input, err := json.Marshal(JobKillParams{JobID: "call-1"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "session-a", resolver.stoppedSession)
	require.Equal(t, "call-1", resolver.stoppedJob)
}

type errNotFoundForTest string

func (e errNotFoundForTest) Error() string { return "job " + string(e) + " not found" }
