package cmd

// R8A-3: `sessions reset` wiped the messages but left the session's async_jobs
// / session_notices rows alone, so the next turn's start pull injected the old
// notices into the "clean slate" and old done-but-unreacted debt still counted;
// `reset --force` ignored a live `rush run` loop waiting between turns.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func startedJob(t *testing.T, store *session.AsyncJobStore, msgs message.Service, owner, id string) {
	t.Helper()
	// Legacy-path only (all its callers are pinned flag-OFF): the same claim
	// shape claimOwnJob's OFF branch uses, but on the caller's store -- the
	// dead-host variant claims on a store that is deliberately NOT the app's.
	_, err := store.Claim(context.Background(), session.ClaimParams{
		Owner: owner, ToolCallID: id, Kind: session.JobKindCommand, Input: "sleep 60", ToolName: "bash",
	})
	require.NoError(t, err)
	_, err = store.AnnounceStarted(context.Background(), msgs, owner, id, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: id, Name: "bash", Content: "started"}},
	})
	require.NoError(t, err)
}

// seedResetDebris gives owner what a timed-out `rush run` leaves behind: a
// completed job whose notice was pulled but never reacted to (debt), an
// announced job cancelled by Shutdown (delivery pending), and a pending wake
// notice, with history messages behind them.
func seedResetDebris(t *testing.T, store *session.AsyncJobStore, msgs message.Service, owner string) {
	t.Helper()
	ctx := context.Background()
	_, err := msgs.Create(ctx, owner, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "start the soak"}},
	})
	require.NoError(t, err)

	startedJob(t, store, msgs, owner, "soak-done")
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: owner, ToolCallID: "soak-done", State: "completed", NoticeKind: "completed", Wake: true,
	})
	require.NoError(t, err)
	pulled, err := store.PullJobNotices(ctx, msgs, owner, func(row session.JobNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{
			Role: message.User, BackgroundJobNotice: true, NoticeKind: row.NoticeKind,
			Parts: []message.ContentPart{message.TextContent{Text: "job notice " + row.ToolCallID}},
		}
	})
	require.NoError(t, err)
	require.Len(t, pulled, 1, "precondition: the completed job's notice is in history")
	debt, err := store.VisibleReactionDebtExists(ctx, owner)
	require.NoError(t, err)
	require.True(t, debt, "precondition: done-but-unreacted debt")

	startedJob(t, store, msgs, owner, "soak-cancelled")
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: owner, ToolCallID: "soak-cancelled", State: "cancelled", NoticeKind: "session_cancel", Wake: true,
	})
	require.NoError(t, err)
	require.NoError(t, store.InsertSessionNotice(ctx, owner, session.NoticeKindSupervision, "supervision: check the soak", true, ""))
}

// assertResetClean: nothing left to pull, no debt of either kind.
func assertResetClean(t *testing.T, store *session.AsyncJobStore, msgs message.Service, owner string) {
	t.Helper()
	ctx := context.Background()
	jobs, err := store.PullJobNotices(ctx, msgs, owner, func(row session.JobNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: row.ToolCallID}}}
	})
	require.NoError(t, err)
	require.Empty(t, jobs, "no job notice may reach the fresh history")
	notices, err := store.PullSessionNotices(ctx, msgs, owner, func(row session.SessionNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: row.Text}}}
	})
	require.NoError(t, err)
	require.Empty(t, notices, "no session notice may reach the fresh history")
	debt, err := store.ReactionDebtExists(ctx, owner)
	require.NoError(t, err)
	require.False(t, debt, "old debt must not survive the wipe")
	visible, err := store.VisibleReactionDebtExists(ctx, owner)
	require.NoError(t, err)
	require.False(t, visible, "old visible debt must not survive the wipe")
}

// seedResetDebrisDual is the dual-mode seedResetDebris: flag off it is
// seedResetDebris verbatim; flag on the same debris shape is seeded on the
// batch substrate (one completed batch root with completion debt, plus the
// same session notice), and the legacy precondition ("the completed job's
// notice is in history") becomes the batch debt precondition.
func seedResetDebrisDual(t *testing.T, a *app.App, msgs message.Service, owner string) {
	t.Helper()
	if !config.BatchRuntimeEnabled() {
		seedResetDebris(t, a.AsyncJobStore(), msgs, owner)
		return
	}
	ctx := context.Background()
	_, err := msgs.Create(ctx, owner, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "start the soak"}},
	})
	require.NoError(t, err)

	// One completed batch root with completion debt, seeded exactly like
	// TestSessionsJobsCmdRun_CompletionDebtNotAwaitingAnswer: Reserve via
	// seedBatchReadRoot, then the two raw debt UPDATEs, then settle the root
	// terminal via the finishOwnJob-style raw UPDATE.
	f := seedBatchReadRoot(t, a, owner)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET announced = 1, announce_message_id = 'debt-msg' WHERE node_id = ? AND root_id = ?`,
		f.rootID, f.rootID)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET notice_selected = 1, notice_kind = 'task', notice_state = 'completed',
		 notice_cause = '', notice_sequence = 1, notice_ordinal = 0, delivery = 'done', wake = 1
		 WHERE node_id = ? AND parent_id = ?`, f.nodeA, f.rootID)
	require.NoError(t, err)
	settleBatchRoot(t, a, owner, f.toolCallID)

	require.NoError(t, a.AsyncJobStore().InsertSessionNotice(ctx, owner, session.NoticeKindSupervision, "supervision: check the soak", true, ""))

	// The legacy precondition ("the completed job's notice is in history" +
	// visible debt) becomes, flag ON: the root's pending-inclusive debt rows
	// must exist BEFORE the reset.
	debt, err := session.NewBatchStore(a.DB()).BatchReactionPendingInclusiveDebtRows(ctx, owner)
	require.NoError(t, err)
	require.NotEmpty(t, debt.Nodes, "precondition: the completed root's completion debt exists")
	require.NotEmpty(t, debt.Notices, "precondition: the completion notice is debt")
}

// assertResetCleanDual is the dual-mode assertResetClean: the legacy checks
// run VERBATIM, and flag on the batch debt -- the flag-ON subject of "old
// debt must not survive the wipe" -- must be gone too.
func assertResetCleanDual(t *testing.T, a *app.App, msgs message.Service, owner string) {
	t.Helper()
	assertResetClean(t, a.AsyncJobStore(), msgs, owner)
	if config.BatchRuntimeEnabled() {
		debt, err := session.NewBatchStore(a.DB()).BatchReactionPendingInclusiveDebtRows(context.Background(), owner)
		require.NoError(t, err)
		require.Empty(t, debt.Nodes, "old batch debt must not survive the wipe")
		require.Empty(t, debt.Notices, "old batch debt must not survive the wipe")
	}
}

// The command's own documented flow: reset, then run again from a clean slate.
//
// Revert-check: dropping the store reset from the command (a plain message
// wipe again) makes the pulls return the old notices and the debt checks true.
func TestSessionsReset_VoidsAsyncNoticesAndDebt(t *testing.T) {
	a, _ := isolatedResetEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-async-debris", "debris")
	require.NoError(t, err)
	seedResetDebrisDual(t, a, a.Messages, sess.ID)
	a.Shutdown()

	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "false"))
	captureStderr(t, func() {
		require.NoError(t, sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID}))
	})

	verify, err := setupApp(sessionsResetCmd)
	require.NoError(t, err)
	t.Cleanup(verify.Shutdown)
	left, err := verify.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, left)
	assertResetCleanDual(t, verify, verify.Messages, sess.ID)
}

// resetSeeded seeds a session with debris plus a message, then returns it.
func resetSeeded(t *testing.T, s session.Service, m message.Service, store *session.AsyncJobStore, title string) session.Session {
	t.Helper()
	sess, err := s.Create(context.Background(), title)
	require.NoError(t, err)
	seedResetDebris(t, store, m, sess.ID)
	return sess
}

// The wipe refuses while the session is still worked on and changes nothing
// (messages and notice rows intact).
//
// Revert-check: dropping the classifier gate from
// resetSessionHistory (and, for the running-job row, the store's running-rows
// guard) wipes the history of a live session; every case fails.
func TestResetSessionHistory_RefusesWhileLiveWork(t *testing.T) {
	// F11 env pin: drives the LEGACY resetSessionHistory directly and asserts
	// its legacy debris/refusal state. Under Go 1.26 a test calling t.Setenv
	// must not be parallel, so the whole test -- parent AND subtests -- is
	// sequential and the subtests carry no t.Parallel: the pin, not the port,
	// changed the parallelism.
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	for _, tc := range []struct {
		name string
		hold func(t *testing.T, a *app.App, s session.Service, store *session.AsyncJobStore, id string)
		note string
	}{
		{"loop between turns", func(t *testing.T, _ *app.App, _ session.Service, store *session.AsyncJobStore, id string) {
			require.NoError(t, store.ClaimSessionDriver(context.Background(), id))
		}, "`rush run`"},
		{"running own job", func(t *testing.T, a *app.App, _ session.Service, _ *session.AsyncJobStore, id string) {
			claimOwnJob(t, a, id, "bash-live")
		}, "1 running job"},
		{"running delegation", func(t *testing.T, a *app.App, s session.Service, _ *session.AsyncJobStore, id string) {
			child, err := s.CreateTaskSession(context.Background(), "reset-child-"+id, id, "sub-agent")
			require.NoError(t, err)
			claimDelegation(t, a, id, "delegate-live", child.ID)
		}, "delegat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s, m, store, _ := newRunDriverTestApp(t)
			sess := resetSeeded(t, s, m, store, "live "+tc.name)
			tc.hold(t, a, s, store, sess.ID)

			_, err := resetSessionHistory(context.Background(), a, sess.ID, false)
			require.ErrorContains(t, err, "still being worked on")
			require.ErrorContains(t, err, tc.note)
			require.ErrorContains(t, err, "nothing was reset")

			left, err := m.List(context.Background(), sess.ID)
			require.NoError(t, err)
			require.NotEmpty(t, left, "a refused reset keeps the history")
			visible, err := store.VisibleReactionDebtExists(context.Background(), sess.ID)
			require.NoError(t, err)
			require.True(t, visible, "and the notice rows")
		})
	}
}

// A force-killed run leaves a 'running' row and a marker on a dead host: the
// reset recovers them and goes through, voiding what recovery produced (the
// interrupted notice must not show up in the clean slate).
//
// Revert-check: dropping RecoverOwnerScope from resetSessionHistory makes the
// dead-host row count as running and the reset refuses.
func TestResetSessionHistory_DeadHostRowsAreRecoveredAndVoided(t *testing.T) {
	// F11 env pin: drives the LEGACY resetSessionHistory directly and asserts
	// its legacy dead-host recovery state.
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	sess := resetSeeded(t, s, m, store, "killed run")

	dead := newDeadHostStore(t, dataDir, 4242, "dead-run")
	startedJob(t, dead, m, sess.ID, "soak-killed")
	require.NoError(t, dead.ClaimSessionDriver(ctx, sess.ID))
	require.NoError(t, dead.SimulateCrashForTest())

	out, err := resetSessionHistory(ctx, a, sess.ID, false)
	require.NoError(t, err)
	require.Positive(t, out.JobsVoided)

	row, err := store.Get(ctx, sess.ID, "soak-killed")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row.State, "recovery ran first")
	require.Equal(t, "void", row.Delivery, "and its notice is voided")
	assertResetClean(t, store, m, sess.ID)
}

// `reset --force` takes over a stale lock but must not wipe a session a live
// loop still drives between turns (it holds no lock, so the lock takeover
// cannot see it); without force the same refusal applies.
//
// Revert-check: dropping the live-work gate from resetSessionHistory wipes
// the history under a live loop for both flag values.
func TestSessionsReset_LiveLoopRefusesEvenWithForce(t *testing.T) {
	for _, force := range []string{"false", "true"} {
		t.Run("force="+force, func(t *testing.T) {
			a, _, _ := isolatedResetEnvWithConfiguredDataDir(t)
			ctx := context.Background()
			sess, err := a.Sessions.CreateWithID(ctx, "reset-live-loop-"+force, "driven")
			require.NoError(t, err)
			seedResetDebris(t, a.AsyncJobStore(), a.Messages, sess.ID)
			store := a.AsyncJobStore()
			require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))
			a.SetAsyncJobStoreForTest(nil)
			defer func() { _ = store.Close(context.Background()) }()
			a.Shutdown()

			require.NoError(t, resetSessionCmdFlags().Flags().Set("force", force))
			var runErr error
			captureStderr(t, func() {
				runErr = sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID})
			})
			require.ErrorContains(t, runErr, "still being worked on")
			require.ErrorContains(t, runErr, "--force only kills the process holding the session lock")

			verify, err := setupApp(sessionsResetCmd)
			require.NoError(t, err)
			t.Cleanup(verify.Shutdown)
			left, err := verify.Messages.List(ctx, sess.ID)
			require.NoError(t, err)
			require.NotEmpty(t, left, "the live loop's history is intact")
		})
	}
}
