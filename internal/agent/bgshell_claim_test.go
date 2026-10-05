package agent

// R-BG-1 (#1126, docs/plans/2026-10-01-bg-shell-ledger.md): a SYNC bash call's
// background escape (SDK origin / Drain turns) must leave a durable bg_shell
// row behind, and the observer registered at claim time (#1188) must
// transition that row at the shell's completion, with or without a
// notification callback.

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// bgShellInnerTool is a stand-in for the inner bash tool's background escape:
// it answers with the BashResponseMetadata shape the real tool produces.
func bgShellInnerTool(shellID string) fantasy.AgentTool {
	return fantasy.NewAgentTool("bash", "Run a command", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.WithResponseMetadata(
			fantasy.NewTextResponse("Background shell started with ID: "+shellID),
			tools.BashResponseMetadata{Background: true, ShellID: shellID},
		), nil
	})
}

// REVERT CHECK: removing the `if t.name == tools.BashToolName && sync` claim
// block from t.run (async_tool.go) leaves no row -- this test FAILED (Get:
// sql.ErrNoRows). Restored the block; re-ran, passed.
func TestAsyncTool_SyncBashBackgroundEscapeClaimsBGShellRow(t *testing.T) {
	t.Parallel()
	registry := newWorkLedger(nil)
	registry.store = newTestAsyncJobStore(t)
	wrapped := &asyncTool{inner: bgShellInnerTool("sh-1"), coordinator: &coordinator{asyncJobs: registry}, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.BashToolName, Input: `{"command":"sleep 30","run_in_background":true}`})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	row, err := registry.store.Get(context.Background(), "session", "sh-1")
	require.NoError(t, err, "the background escape must have claimed a bg_shell row before the response was delivered")
	require.Equal(t, string(session.JobKindBGShell), row.Kind)
	require.Equal(t, "running", row.State)
	require.Equal(t, int64(1), row.Announced)
}

// A store that cannot commit the claim fails the background start closed: the
// model gets an error result instead of a shell with no row.
//
// REVERT CHECK: removing the kill+error branch from
// claimBackgroundShellRow's error path (leaving the claim failure logged only)
// leaves this test FAILED (resp.IsError false: the escape answered "started"
// over a failed claim). Restored the branch; re-ran, passed.
func TestAsyncTool_ClaimFailureRefusesBackgroundStart(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	// Force the claim to fail deterministically: the shell id's key is already
	// held by a row with a DIFFERENT input (ErrAsyncJobInputMismatch).
	_, err := store.Claim(context.Background(), session.ClaimParams{
		Owner: "session", ToolCallID: "sh-x", Kind: session.JobKindCommand, Input: "different",
	})
	require.NoError(t, err)
	registry := newWorkLedger(nil)
	registry.store = store
	wrapped := &asyncTool{inner: bgShellInnerTool("sh-x"), coordinator: &coordinator{asyncJobs: registry}, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.BashToolName, Input: `{"command":"sleep 30","run_in_background":true}`})
	require.NoError(t, err)
	require.True(t, resp.IsError, "a failed claim must fail the background start (fail-closed)")
	require.Contains(t, resp.Content, "refused")
}

// finishBGShellRow is the ONLY writer of a claimed bg_shell row's terminal
// transition (#1188): state and result_summary in one CAS (DUR-1), with
// delivery='done'/reacted=1 so the row is never pulled and never debt.
//
// REVERT CHECK: making finishBGShellRow return before the Transition leaves
// the row running -- this test FAILED (row.State "running", want "completed").
func TestFinishBGShellRow_TransitionsClaimedRow(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
	ctx := context.Background()

	_, err := store.ClaimShell(ctx, "sess", "sh-9", false)
	require.NoError(t, err)

	tool.finishBGShellRow(ctx, "sess", "sh-9", "Background job sh-9 finished", false)

	row, err := store.Get(ctx, "sess", "sh-9")
	require.NoError(t, err)
	require.Equal(t, "completed", row.State)
	require.Equal(t, "Background job sh-9 finished", row.ResultSummary.String)
	require.Equal(t, int64(1), row.Reacted, "the row itself is never debt")
	require.Equal(t, "done", row.Delivery, "the notice insert remains the wake channel; the row is never pulled")

	// A second completion (duplicate callback) must not move the row again.
	tool.finishBGShellRow(ctx, "sess", "sh-9", "second", false)
	row2, err := store.Get(ctx, "sess", "sh-9")
	require.NoError(t, err)
	require.Equal(t, "Background job sh-9 finished", row2.ResultSummary.String, "the first transition wins")
}

func TestFinishBGShellRow_FailedShellMarksRowFailed(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
	ctx := context.Background()

	_, err := store.ClaimShell(ctx, "sess", "sh-f", false)
	require.NoError(t, err)

	tool.finishBGShellRow(ctx, "sess", "sh-f", "exit 1", true)
	row, err := store.Get(ctx, "sess", "sh-f")
	require.NoError(t, err)
	require.Equal(t, "failed", row.State)
	require.Equal(t, int64(1), row.ResultIsError.Int64)
}

// A row of a DIFFERENT kind (a real async job whose id equals the shell id) is
// never transitioned as a shell.
func TestFinishBGShellRow_NeverTouchesNonShellRow(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}
	ctx := context.Background()

	_, err := store.Claim(ctx, session.ClaimParams{Owner: "sess", ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "echo"})
	require.NoError(t, err)

	tool.finishBGShellRow(ctx, "sess", "call-1", "summary", false)
	row, err := store.Get(ctx, "sess", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State)
}

// persistBGShellCompletion no longer touches the row: it records the wake
// notice and takes the auto-resume slot (the arbiter cap is keyed by the
// notice id). The terminal transition belongs to the observer registered at
// claim time, so it cannot depend on a notification callback existing.
//
// REVERT CHECK: restoring a Transition call inside persistBGShellCompletion
// moves the row to completed -- this test FAILED (row.State "completed", want
// "running").
func TestPersistBGShellCompletion_LeavesTheRowToTheObserver(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	c := &coordinator{asyncJobs: registry}
	ctx := context.Background()

	_, err := store.ClaimShell(ctx, "sess", "sh-9", false)
	require.NoError(t, err)

	_ = c.persistBGShellCompletion("sess", "sh-9", "Background job sh-9 finished")

	row, err := store.Get(ctx, "sess", "sh-9")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "persistBGShellCompletion must not write the row")
	notices, err := store.ListSessionNotices(ctx, "sess")
	require.NoError(t, err)
	require.Len(t, notices, 1, "the bg_shell_done notice remains the wake channel")
	require.Equal(t, session.NoticeKindBGShellDone, notices[0].Kind)
}

// A rowless shell (pre-claim shell or store-less fixture) keeps today's
// notice-only path: no row is invented.
func TestPersistBGShellCompletion_RowlessShellWritesNoticeOnly(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	c := &coordinator{asyncJobs: registry}
	ctx := context.Background()

	c.persistBGShellCompletion("sess", "sh-legacy", "legacy")
	_, err := store.Get(ctx, "sess", "sh-legacy")
	require.Error(t, err, "no row must be invented for a shell that never claimed one")
	notices, err := store.ListSessionNotices(ctx, "sess")
	require.NoError(t, err)
	require.Len(t, notices, 1)
}

// bgShellEscape runs a SYNC-branch bash call whose inner tool answers with the
// background-escape metadata of an already started shell, the way the real
// bash tool does after manager.StartOwned.
func bgShellEscape(t *testing.T, coord *coordinator, shellID string) {
	t.Helper()
	wrapped := &asyncTool{inner: bgShellInnerTool(shellID), coordinator: coord, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)
	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call-" + shellID, Name: tools.BashToolName, Input: `{"command":"x","run_in_background":true}`})
	require.NoError(t, err)
	require.False(t, resp.IsError)
}

func newBGShellFixture(t *testing.T) (*coordinator, *session.AsyncJobStore, *shell.BackgroundShellManager, string) {
	t.Helper()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	mgr := shell.NewBackgroundShellManager()
	t.Cleanup(func() { mgr.KillAll(context.Background()) })
	return &coordinator{asyncJobs: registry, background: mgr}, store, mgr, t.TempDir()
}

// The #1188 bug: a shell started by a SYNC bash call with NO notification
// callback (notify_on_background_job_done=false) left its claimed row running
// forever, which holds the session's scope open. The observer registered at
// claim time closes the row at the process's exit regardless.
//
// REVERT CHECK: removing the sh.OnDone observer registration from
// claimBackgroundShellRow leaves the row running -- this test FAILED
// (Eventually: row.State "running").
func TestAsyncTool_SyncBashBackgroundEscape_ObserverFinishesRowWithoutNotifier(t *testing.T) {
	t.Parallel()
	coord, store, mgr, dir := newBGShellFixture(t)
	sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "echo done", "no notifier")
	require.NoError(t, err)

	bgShellEscape(t, coord, sh.ID)
	sh.Wait()

	require.Eventually(t, func() bool {
		row, err := store.Get(context.Background(), "session", sh.ID)
		return err == nil && row.State == "completed"
	}, 5*time.Second, 10*time.Millisecond, "the observer must close the claimed row")
	row, err := store.Get(context.Background(), "session", sh.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), row.Reacted)
	require.Equal(t, "done", row.Delivery)
	require.Contains(t, row.ResultSummary.String, "done")
	notices, err := store.ListSessionNotices(context.Background(), "session")
	require.NoError(t, err)
	require.Empty(t, notices, "no notifier was registered, so no wake notice exists")
	require.Zero(t, mgr.PendingCompletionsOwned("session"))
}

func TestAsyncTool_SyncBashBackgroundEscape_ObserverMarksFailedExit(t *testing.T) {
	t.Parallel()
	coord, store, mgr, dir := newBGShellFixture(t)
	sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "exit 3", "fails")
	require.NoError(t, err)

	bgShellEscape(t, coord, sh.ID)
	sh.Wait()

	require.Eventually(t, func() bool {
		row, err := store.Get(context.Background(), "session", sh.ID)
		return err == nil && row.State == "failed"
	}, 5*time.Second, 10*time.Millisecond)
	row, err := store.Get(context.Background(), "session", sh.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), row.ResultIsError.Int64)
}

// With a notifier registered too (notify_on_background_job_done=true) both
// callbacks run on their own goroutines in either order: the row is closed by
// the observer exactly once, and the notifier's persistBGShellCompletion writes
// the one wake notice without touching the row.
//
// REVERT CHECK: the same observer removal leaves the row running -- this test
// FAILED (Eventually: row.State "running").
func TestAsyncTool_SyncBashBackgroundEscape_ObserverAndNotifierCoexist(t *testing.T) {
	t.Parallel()
	coord, store, mgr, dir := newBGShellFixture(t)
	sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "echo both", "with notifier")
	require.NoError(t, err)
	sh.OnDone(func() { coord.persistBGShellCompletion("session", sh.ID, "notifier summary") })

	bgShellEscape(t, coord, sh.ID)
	sh.Wait()

	require.Eventually(t, func() bool {
		row, err := store.Get(context.Background(), "session", sh.ID)
		if err != nil || row.State != "completed" {
			return false
		}
		notices, err := store.ListSessionNotices(context.Background(), "session")
		return err == nil && len(notices) == 1
	}, 5*time.Second, 10*time.Millisecond)
	row, err := store.Get(context.Background(), "session", sh.ID)
	require.NoError(t, err)
	require.Contains(t, row.ResultSummary.String, "both", "the observer's summary, not the notifier's")
}

// A shell the manager no longer has (cleaned up between the tool's answer and
// the claim) must not leave the claimed row running: it ends failed.
//
// REVERT CHECK: removing the !ok branch from claimBackgroundShellRow leaves the
// row running -- this test FAILED (row.State "running", want "failed").
func TestAsyncTool_SyncBashBackgroundEscape_MissingShellFinishesRowFailed(t *testing.T) {
	t.Parallel()
	coord, store, _, _ := newBGShellFixture(t)

	bgShellEscape(t, coord, "sh-gone")

	require.Eventually(t, func() bool {
		row, err := store.Get(context.Background(), "session", "sh-gone")
		return err == nil && row.State == "failed"
	}, 5*time.Second, 10*time.Millisecond)
	row, err := store.Get(context.Background(), "session", "sh-gone")
	require.NoError(t, err)
	require.Equal(t, "output unavailable", row.ResultSummary.String)
}

// A transient store error (here: the row is not readable yet) is retried, not
// swallowed -- the old single-shot transition left such a row running forever.
//
// REVERT CHECK: calling the op once instead of through retryAsyncStoreOp leaves
// the row running -- this test FAILED (row.State "running" or never finished).
func TestFinishBGShellRow_RetriesUntilTheRowIsReadable(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	registry := newWorkLedger(nil)
	registry.store = store
	tool := &asyncTool{coordinator: &coordinator{asyncJobs: registry}}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tool.finishBGShellRow(context.Background(), "sess", "sh-late", "late", false)
	}()
	// The first attempts see no row (sql.ErrNoRows) and must keep retrying.
	time.Sleep(50 * time.Millisecond)
	_, err := store.ClaimShell(context.Background(), "sess", "sh-late", false)
	require.NoError(t, err)

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("finishBGShellRow never completed")
	}
	row, err := store.Get(context.Background(), "sess", "sh-late")
	require.NoError(t, err)
	require.Equal(t, "completed", row.State)
}
