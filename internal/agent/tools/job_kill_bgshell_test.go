package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// fakeBGStopResolver adds the optional BGShellStopper to the plain fake.
type fakeBGStopResolver struct {
	fakeJobShellResolver
	stopText, stopClaim string
	stopVerdict         JobStopVerdict
	stopCalls           []string
}

func (f *fakeBGStopResolver) StopBackgroundShellRow(_, shellID string) (string, string, JobStopVerdict) {
	f.stopCalls = append(f.stopCalls, shellID)
	return f.stopText, f.stopClaim, f.stopVerdict
}

func runJobKill(t *testing.T, resolver JobShellResolver, bg *shell.BackgroundShellManager, params JobKillParams) fantasy.ToolResponse {
	t.Helper()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-a")
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := NewJobKillTool(resolver, nil, bg).Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	return resp
}

func startLongShell(t *testing.T) (*shell.BackgroundShellManager, *shell.BackgroundShell) {
	t.Helper()
	bg := shell.NewBackgroundShellManager()
	t.Cleanup(func() { bg.Close(context.Background()) })
	sh, err := bg.StartOwned(context.Background(), "session-a", t.TempDir(), nil, "sleep 30", "")
	require.NoError(t, err)
	return bg, sh
}

// A raw shell_id with a durable bg_shell row is stopped through the row, then
// killed; the answer is the stopped text, fused onto the row by its claim.
//
// REVERT CHECK: dropping the tryStopBGShellRow call from the raw shell_id path
// answers "terminated successfully" and the stopper is never asked -- this
// test FAILED (stopCalls empty).
func TestJobKillTool_RawShellIDStopsTheDurableRowFirst(t *testing.T) {
	bg, sh := startLongShell(t)
	resolver := &fakeBGStopResolver{stopText: "stopped-text", stopClaim: "claim-9", stopVerdict: JobStopStopped}

	resp := runJobKill(t, resolver, bg, JobKillParams{ShellID: sh.ID})

	require.False(t, resp.IsError, resp.Content)
	require.Equal(t, "stopped-text", resp.Content)
	require.Equal(t, []string{sh.ID}, resolver.stopCalls)
	require.True(t, sh.IsDone(), "the shell is killed after the row is stopped")
	var meta JobKillResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, sh.ID, meta.JobID)
	require.Equal(t, "claim-9", meta.KilledClaimID)
	require.Empty(t, resolver.stoppedJob, "MarkJobStopped is for ledger jobs, not consulted for a raw shell id")
}

func TestJobKillTool_RawShellIDAlreadyTerminalAnswersWithoutKilling(t *testing.T) {
	bg, sh := startLongShell(t)
	resolver := &fakeBGStopResolver{stopText: "already finished: exit 0", stopVerdict: JobStopAlreadyTerminal}

	resp := runJobKill(t, resolver, bg, JobKillParams{ShellID: sh.ID})

	require.False(t, resp.IsError)
	require.Equal(t, "already finished: exit 0", resp.Content)
	require.False(t, sh.IsDone(), "a row that is already terminal answers from the row and kills nothing")
}

func TestJobKillTool_RawShellIDWithoutARowKeepsTheOlderPath(t *testing.T) {
	bg, sh := startLongShell(t)
	resolver := &fakeBGStopResolver{stopVerdict: JobStopNotFound}

	resp := runJobKill(t, resolver, bg, JobKillParams{ShellID: sh.ID})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "terminated successfully")
	require.True(t, sh.IsDone())
}

// A model that passes the shell id as job_id (the id the bash tool printed)
// got a false "job not found": the ledger has no such job. The durable row is
// consulted before the refusal.
//
// REVERT CHECK: dropping the job_id fallback leaves the refusal -- this test
// FAILED (resp.IsError true, "job 001 not found").
func TestJobKillTool_ShellIDPassedAsJobIDFallsBackToTheRow(t *testing.T) {
	bg, sh := startLongShell(t)
	resolver := &fakeBGStopResolver{stopText: "stopped-text", stopClaim: "claim-7", stopVerdict: JobStopStopped}
	resolver.err = errors.New("job " + sh.ID + " not found (not owned by this session, already delivered, or not a command)")

	resp := runJobKill(t, resolver, bg, JobKillParams{JobID: sh.ID})

	require.False(t, resp.IsError, resp.Content)
	require.Equal(t, "stopped-text", resp.Content)
	require.True(t, sh.IsDone())
	require.Empty(t, resolver.stoppedJob, "the ledger's own stop marker must not be consulted for a row-only shell")
	var meta JobKillResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, sh.ID, meta.JobID)
	require.Equal(t, "claim-7", meta.KilledClaimID)
}

func TestJobKillTool_ShellIDPassedAsJobIDWithoutARowStaysARefusal(t *testing.T) {
	bg, sh := startLongShell(t)
	resolver := &fakeBGStopResolver{stopVerdict: JobStopNotFound}
	resolver.err = errors.New("job " + sh.ID + " not found")

	resp := runJobKill(t, resolver, bg, JobKillParams{JobID: sh.ID})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "not found")
	require.False(t, sh.IsDone())
}
