package cmd

// R8A-3 (batch part): `sessions reset` must wipe the session's batch rows
// and notices together with the messages, so the next turn's start pull
// injects no old notices into the "clean slate" and no done-but-unreacted
// debt survives; `reset --force` must not wipe a session a live `rush run`
// loop still drives between turns.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seedResetDebris gives owner what a timed-out `rush run` leaves behind on
// the batch substrate: a completed batch root with delivered-but-unreacted
// completion debt, plus a pending supervision notice, with a history
// message behind them.
func seedResetDebris(t *testing.T, a *app.App, msgs message.Service, owner string) {
	t.Helper()
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

	require.NoError(t, session.NewSessionNoticeStore(a.DB()).InsertSessionNotice(ctx, owner, session.NoticeKindSupervision, "supervision: check the soak", true, ""))

	// Precondition: the root's pending-inclusive debt rows exist BEFORE the
	// reset (the batch equivalent of "the completed job's notice is in
	// history" plus visible debt).
	debt, err := session.NewBatchStore(a.DB()).BatchReactionPendingInclusiveDebtRows(ctx, owner)
	require.NoError(t, err)
	require.NotEmpty(t, debt.Nodes, "precondition: the completed root's completion debt exists")
	require.NotEmpty(t, debt.Notices, "precondition: the completion notice is debt")
}

// assertResetClean: no batch debt of either kind and no notice left to pull.
func assertResetClean(t *testing.T, a *app.App, msgs message.Service, owner string) {
	t.Helper()
	ctx := context.Background()
	debt, err := session.NewBatchStore(a.DB()).BatchReactionPendingInclusiveDebtRows(ctx, owner)
	require.NoError(t, err)
	require.Empty(t, debt.Nodes, "old batch debt must not survive the wipe")
	require.Empty(t, debt.Notices, "old batch debt must not survive the wipe")
	notices, err := session.NewSessionNoticeStore(a.DB()).PullSessionNotices(ctx, msgs, owner, func(row session.SessionNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: row.Text}}}
	})
	require.NoError(t, err)
	require.Empty(t, notices, "no session notice may reach the fresh history")
}

// The command's own documented flow: reset, then run again from a clean slate.
//
// Revert-check: dropping the batch reset from the command (a plain message
// wipe again) makes the debt checks non-empty and the pulls return the old
// notices.
func TestSessionsReset_VoidsBatchDebtAndNotices(t *testing.T) {
	a, _ := isolatedResetEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-async-debris", "debris")
	require.NoError(t, err)
	seedResetDebris(t, a, a.Messages, sess.ID)
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
	assertResetClean(t, verify, verify.Messages, sess.ID)
}

// `reset --force` takes over a stale lock but must not wipe a session a live
// loop still drives between turns (it holds no lock, so the lock takeover
// cannot see it); without force the same refusal applies.
//
// Revert-check: dropping resetLiveLoopRefusal from the reset path wipes the
// history under a live loop for both force values.
func TestSessionsReset_LiveLoopRefusesEvenWithForce(t *testing.T) {
	for _, force := range []string{"false", "true"} {
		t.Run("force="+force, func(t *testing.T) {
			a, _, dataDir := isolatedResetEnvWithConfiguredDataDir(t)
			ctx := context.Background()
			sess, err := a.Sessions.CreateWithID(ctx, "reset-live-loop-"+force, "driven")
			require.NoError(t, err)
			seedResetDebris(t, a, a.Messages, sess.ID)
			driverHost := session.NewHostStore(a.DB(), dataDir, 4321, "reset-live-loop")
			driverHostID, err := driverHost.EnsureHost(ctx)
			require.NoError(t, err)
			require.NoError(t, driverHost.ClaimSessionDriver(ctx, sess.ID))
			// A12 pattern: pin the host lock and keep the driver marker.
			driverHost.CloseKeepLock()
			t.Cleanup(func() { session.ReleaseRetainedHostLocksForTest(driverHostID) })
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
