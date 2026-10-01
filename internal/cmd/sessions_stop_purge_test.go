package cmd

// Task #1153: `sessions inject <id> --interrupt` followed by `sessions kill
// <id>` used to leave the durable queued-work rows (session_run_queue,
// pending_injects, orphan_call_outbox) behind. The RunQueuePump of the NEXT
// rush process on the same data directory picked them up and kept driving the
// session the operator had just stopped. These tests pin the purge wiring in
// the three stop commands (kill, reset --force, cancel).
//
// Revert-check: dropping the purgeQueuedWorkAfterStop call from
// sessionsKillCmdRun reddens nothing here (kill needs a real live holder to
// reach it), but dropping it from sessionsResetCmd's RunE reddens
// TestSessionsReset_ForcePurgesQueuedWork, and from sessionsCancelCmd reddens
// TestSessionsCancel_PurgesQueuedWork. The helper itself is covered directly
// by TestPurgeQueuedWorkAfterStop_RemovesQueuedWork.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// stopPurgeCmdContext gives cmd an uncancelled context for the duration of
// the test. The stop commands read cmd.Context() — setupApp passes it to
// db.Connect (which panics on a nil context) and purgeQueuedWorkAfterStop
// passes it to the purge transaction — and calling RunE directly leaves
// cobra's internal ctx nil. Each command therefore gets its own fresh
// context; a cancelled one would be as fatal as a nil one.
func stopPurgeCmdContext(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd.SetContext(ctx)
}

// stopPurgeEnv stands up an isolated app (same isolation
// isolateConfigEnvForTests mandates) with a configured --data-dir, so the
// command under test resolves the very directory the seed app wrote to.
func stopPurgeEnv(t *testing.T) (a *app.App, dataDir string) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	dataDir = filepath.Join(tmp, "stop-purge-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	// The debug/data-dir stand-ins and the context must live on
	// sessionsCancelCmd itself, not a carrier command: both setupApp (here
	// and inside assertNoQueuedWork) and purgeQueuedWorkAfterStop read them
	// off the very cobra.Command they are handed.
	ensureRootFlagStandIns(sessionsCancelCmd, dataDir)
	stopPurgeCmdContext(t, sessionsCancelCmd)

	built, err := setupApp(sessionsCancelCmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory,
		"test setup assumption: the command must resolve the configured data dir")

	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = os.Chdir(orig)
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
		waitForSQLiteHandleRelease(t, workDir)
	})

	return built, dataDir
}

// seedQueuedWorkForStop leaves behind exactly what a stopped `sessions inject
// --interrupt` holder leaves: a durable run-queue row, one merge and one
// interrupt pending inject, and an orphan-call outbox row. The inject rows
// reference real message rows (pending_injects.message_id is a foreign key to
// messages), which also lets the caller assert that a purge is not a
// delete-history. Returns the two message ids.
func seedQueuedWorkForStop(t *testing.T, a *app.App, sessionID string) (mergeMsgID, interruptMsgID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, a.Sessions.EnqueueRunQueueEntry(ctx, "rq-x", sessionID, []byte(`{}`)))
	mergeMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "merge me"}},
	})
	require.NoError(t, err)
	interruptMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt me"}},
	})
	require.NoError(t, err)
	require.NoError(t, a.Sessions.CreatePendingInject(ctx, session.PendingInject{SessionID: sessionID, MessageID: mergeMsg.ID}))
	require.NoError(t, a.Sessions.CreatePendingInject(ctx, session.PendingInject{SessionID: sessionID, MessageID: interruptMsg.ID, Interrupt: true}))
	require.NoError(t, a.Sessions.WriteToOrphanOutbox(ctx, "ob-x", sessionID, []byte(`{}`)))
	return mergeMsg.ID, interruptMsg.ID
}

// assertNoQueuedWork verifies through a SECOND app on the same data dir that
// nothing is left for the next process's RunQueuePump to pick up, and shuts
// that app down again before returning: the MCP owner is process-wide, so a
// caller opening yet another app (to read the cancel flag, say) would
// otherwise fail with "application owner is already active".
//
// wantMsgIDs are message ids the purge must have left alone. `sessions reset
// --force` wipes history on purpose, so its test passes none.
func assertNoQueuedWork(t *testing.T, dataDir, sessionID string, wantMsgIDs ...string) {
	t.Helper()
	ctx := context.Background()
	stopPurgeCmdContext(t, sessionsCancelCmd)
	verify, err := setupApp(sessionsCancelCmd)
	require.NoError(t, err)
	defer verify.Shutdown()

	has, err := verify.Sessions.HasOutstandingRunQueueEntriesForSession(ctx, sessionID)
	require.NoError(t, err)
	require.False(t, has, "no run-queue row of the stopped session may survive")

	merge, hasInterrupt, err := verify.Sessions.DrainPendingInjects(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, merge, "no pending merge inject may survive")
	require.False(t, hasInterrupt, "no pending interrupt inject may survive")

	orphanRows, err := verify.Sessions.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	for _, e := range orphanRows {
		require.NotEqual(t, sessionID, e.SessionID, "no orphan-outbox row may survive")
	}

	if len(wantMsgIDs) > 0 {
		remaining, err := verify.Messages.List(ctx, sessionID)
		require.NoError(t, err)
		survived := make(map[string]bool, len(remaining))
		for _, m := range remaining {
			survived[m.ID] = true
		}
		for _, id := range wantMsgIDs {
			require.True(t, survived[id], "message %s must survive the purge: kill is not delete-history", id)
		}
	}
}

// The helper itself: opening the DB with setupAppLite must purge the session's
// durable queued work and must NOT start a RunQueuePump that would drain it.
func TestPurgeQueuedWorkAfterStop_RemovesQueuedWork(t *testing.T) {
	a, dataDir := stopPurgeEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "purge-stop-helper", "stop purge")
	require.NoError(t, err)
	mergeMsgID, interruptMsgID := seedQueuedWorkForStop(t, a, sess.ID)
	a.Shutdown()

	captureStderr(t, func() {
		purgeQueuedWorkAfterStop(sessionsCancelCmd, sess.ID)
	})

	assertNoQueuedWork(t, dataDir, sess.ID, mergeMsgID, interruptMsgID)
}

// `sessions reset --force` proved the previous holder dead; its queued work
// must not survive the reset either.
func TestSessionsReset_ForcePurgesQueuedWork(t *testing.T) {
	a, dataDir := stopPurgeEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "purge-stop-reset", "reset purge")
	require.NoError(t, err)
	seedQueuedWorkForStop(t, a, sess.ID)
	a.Shutdown()

	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "true"))
	ensureRootFlagStandIns(sessionsResetCmd, dataDir)
	stopPurgeCmdContext(t, sessionsResetCmd)
	captureStderr(t, func() {
		require.NoError(t, sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID}))
	})

	assertNoQueuedWork(t, dataDir, sess.ID)
}

// `sessions cancel <id>` is an explicit stop request by name: the queued work
// goes away, but the cancel flag stays set (cancel ≠ purge-only).
func TestSessionsCancel_PurgesQueuedWork(t *testing.T) {
	a, dataDir := stopPurgeEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "purge-stop-cancel", "cancel purge")
	require.NoError(t, err)
	mergeMsgID, interruptMsgID := seedQueuedWorkForStop(t, a, sess.ID)
	a.Shutdown()

	captureStderr(t, func() {
		require.NoError(t, sessionsCancelCmd.RunE(sessionsCancelCmd, []string{sess.ID}))
	})

	assertNoQueuedWork(t, dataDir, sess.ID, mergeMsgID, interruptMsgID)

	stopPurgeCmdContext(t, sessionsCancelCmd)
	verify, err := setupApp(sessionsCancelCmd)
	require.NoError(t, err)
	t.Cleanup(verify.Shutdown)
	cancelled, err := verify.Sessions.IsCancelRequested(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, cancelled, "cancel must still set the cancel flag it is documented to set")
}
