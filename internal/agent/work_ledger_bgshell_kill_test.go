package agent

// #1189 (R-BG-1 remainder, docs/plans/2026-10-05-bg-shell-remainder.md step 2):
// job_kill on a shell that has a durable bg_shell row but no ledger job must
// commit the row's "cancelled" transition BEFORE the process is killed.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

func newBGShellKillFixture(t *testing.T) (*coordinator, *session.AsyncJobStore, string) {
	t.Helper()
	coord, store, mgr, dir := newBGShellFixture(t)
	coord.asyncJobs.coord = coord
	sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "sleep 30", "long running")
	require.NoError(t, err)
	bgShellEscape(t, coord, sh.ID)
	return coord, store, sh.ID
}

// The row is cancelled while the process is still alive: the transition does
// not wait for the process, so a process that refuses to die (R8B-2) cannot
// leave the row running.
//
// REVERT CHECK: removing the Transition call from StopBackgroundShellRow
// leaves the row running -- this test FAILED (row.State "running", want
// "cancelled").
func TestStopBackgroundShellRow_CancelsTheRowWhileTheProcessStillRuns(t *testing.T) {
	t.Parallel()
	coord, store, shellID := newBGShellKillFixture(t)
	sh, ok := coord.background.GetOwned("session", shellID)
	require.True(t, ok)

	text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)

	require.Equal(t, tools.JobStopStopped, verdict)
	require.NotEmpty(t, text)
	require.False(t, sh.IsDone(), "the row is cancelled BEFORE the kill, not because of it")
	row, err := store.Get(context.Background(), "session", shellID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", row.State)
	require.Equal(t, "job_kill", row.NoticeKind)
	require.Equal(t, "done", row.Delivery, "job_kill's own tool result is the answer; the row is never pulled")
	require.Equal(t, int64(1), row.Reacted)
	require.Equal(t, int64(0), row.Wake, "a stopped shell is no wake debt")
	require.Equal(t, row.ClaimID, claimID, "the claim id lets the ledger fuse the tool result onto exactly this row")
}

// After job_kill's cancel, the process's own exit must not write the row again
// and must not raise a second wake: its completion callback stands down.
//
// REVERT CHECK: removing the bgShellRowCancelled early return from
// notifyBackgroundJobDone lets the exit write a bg_shell_done notice -- this
// test FAILED (notices not empty).
func TestStopBackgroundShellRow_TheExitAfterTheCancelWritesNothingMore(t *testing.T) {
	t.Parallel()
	coord, store, shellID := newBGShellKillFixture(t)
	sh, ok := coord.background.GetOwned("session", shellID)
	require.True(t, ok)
	sh.OnDone(func() { coord.notifyBackgroundJobDone("session", sh) })
	_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
	require.Equal(t, tools.JobStopStopped, verdict)
	before, err := store.Get(context.Background(), "session", shellID)
	require.NoError(t, err)

	require.NoError(t, coord.background.KillOwned(context.Background(), "session", shellID))
	sh.Wait()

	require.Eventually(t, func() bool { return coord.background.PendingCompletionsOwned("session") == 0 },
		5*time.Second, 10*time.Millisecond, "the completion hold must end")
	after, err := store.Get(context.Background(), "session", shellID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", after.State)
	require.Equal(t, before.ResultSummary, after.ResultSummary, "the exit must not rewrite the cancelled row")
	notices, err := store.ListSessionNotices(context.Background(), "session")
	require.NoError(t, err)
	require.Empty(t, notices, "a job_kill-stopped shell must not wake the session with a completion notice")
}

// The verdicts around the happy path.
func TestStopBackgroundShellRow_Verdicts(t *testing.T) {
	t.Run("a shell without a row is not found", func(t *testing.T) {
		coord, _, mgr, dir := newBGShellFixture(t)
		coord.asyncJobs.coord = coord
		sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "sleep 30", "rowless")
		require.NoError(t, err)
		_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", sh.ID)
		require.Equal(t, tools.JobStopNotFound, verdict)
	})
	t.Run("a row without a shell in this process is not found", func(t *testing.T) {
		coord, store, _, _ := newBGShellFixture(t)
		coord.asyncJobs.coord = coord
		_, err := store.ClaimShell(context.Background(), "session", "sh-elsewhere", false)
		require.NoError(t, err)
		_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", "sh-elsewhere")
		require.Equal(t, tools.JobStopNotFound, verdict)
		row, err := store.Get(context.Background(), "session", "sh-elsewhere")
		require.NoError(t, err)
		require.Equal(t, "running", row.State, "a shell this process cannot kill must not have its row cancelled")
	})
	t.Run("a row of another kind is not found and untouched", func(t *testing.T) {
		coord, store, mgr, dir := newBGShellFixture(t)
		coord.asyncJobs.coord = coord
		sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "sleep 30", "other kind")
		require.NoError(t, err)
		_, err = store.Claim(context.Background(), session.ClaimParams{Owner: "session", ToolCallID: sh.ID, Kind: session.JobKindCommand, Input: "echo"})
		require.NoError(t, err)
		_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", sh.ID)
		require.Equal(t, tools.JobStopNotFound, verdict)
		row, err := store.Get(context.Background(), "session", sh.ID)
		require.NoError(t, err)
		require.Equal(t, "running", row.State)
	})
	t.Run("an already finished row answers from the committed row", func(t *testing.T) {
		coord, store, shellID := newBGShellKillFixture(t)
		(&asyncTool{coordinator: coord}).finishBGShellRow(context.Background(), "session", shellID, "the final output", false)
		text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
		require.Equal(t, tools.JobStopAlreadyTerminal, verdict)
		require.Contains(t, text, "the final output")
		require.Empty(t, claimID)
		row, err := store.Get(context.Background(), "session", shellID)
		require.NoError(t, err)
		require.Equal(t, "completed", row.State, "a finished row must not be rewritten as cancelled")
	})
}

// The real tool over the real ledger, store and shell: a raw shell_id job_kill
// cancels the row, kills the process, answers with the stopped text carrying
// the killed claim, and leaves no wake notice behind.
//
// REVERT CHECK: dropping the tryStopBGShellRow call from job_kill's raw
// shell_id path leaves the row to the exit (failed, "finished: exit 1") --
// this test FAILED (row.State "failed", want "cancelled").
func TestJobKill_RawShellIDOverTheRealLedger(t *testing.T) {
	t.Parallel()
	coord, store, shellID := newBGShellKillFixture(t)
	sh, ok := coord.background.GetOwned("session", shellID)
	require.True(t, ok)
	sh.OnDone(func() { coord.notifyBackgroundJobDone("session", sh) })
	tool := tools.NewJobKillTool(coord.asyncJobs, coord.asyncJobs, coord.background)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "session")
	input, err := json.Marshal(tools.JobKillParams{ShellID: shellID})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: tools.JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	sh.Wait()

	require.True(t, sh.IsDone())
	row, err := store.Get(context.Background(), "session", shellID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", row.State)
	var meta tools.JobKillResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, shellID, meta.JobID, "the result names the stopped row so it can be fused onto it")
	require.Equal(t, row.ClaimID, meta.KilledClaimID)
	require.Eventually(t, func() bool { return coord.background.PendingCompletionsOwned("session") == 0 },
		5*time.Second, 10*time.Millisecond)
	notices, err := store.ListSessionNotices(context.Background(), "session")
	require.NoError(t, err)
	require.Empty(t, notices)
}

// A shell id keys a running row of ANOTHER rush process's host (cross-process
// id collision): job_kill must not cancel that row or kill anything.
//
// REVERT CHECK: temporarily removed the row.HostID != store.HostID() guard
// from StopBackgroundShellRow -- this test FAILED (verdict JobStopStopped,
// want JobStopNotFound: job_kill cancelled another process's running row,
// row.State "cancelled", want "running"). Restored the guard; re-ran, passed.
func TestStopBackgroundShellRow_ForeignHostRowIsNotOurs(t *testing.T) {
	t.Parallel()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	registry := newWorkLedger(nil)
	registry.store = store
	mgr := shell.NewBackgroundShellManager()
	t.Cleanup(func() { mgr.KillAll(context.Background()) })
	coord := &coordinator{asyncJobs: registry, background: mgr}
	coord.asyncJobs.coord = coord
	sh, err := mgr.StartOwned(t.Context(), "session", t.TempDir(), nil, "sleep 30", "foreign row")
	require.NoError(t, err)
	bgShellEscape(t, coord, sh.ID)
	res, err := conn.ExecContext(context.Background(), `UPDATE async_jobs SET host_id = 'other-rush-host' WHERE owner_session_id = 'session' AND tool_call_id = ?`, sh.ID)
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	require.Equal(t, int64(1), n)

	text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow("session", sh.ID)

	require.Equal(t, tools.JobStopNotFound, verdict)
	require.Empty(t, text)
	require.Empty(t, claimID)
	require.False(t, sh.IsDone(), "the foreign shell itself must not be touched")
	row, err := store.Get(context.Background(), "session", sh.ID)
	require.NoError(t, err)
	require.Equal(t, "other-rush-host", row.HostID)
	require.Equal(t, "running", row.State, "another process's running row must not be cancelled")
}
