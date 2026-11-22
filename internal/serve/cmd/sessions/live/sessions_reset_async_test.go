package sesslive

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func seedResetDebris(t *testing.T, a *app.App, msgs message.Service, owner string) {
	t.Helper()
	ctx := context.Background()
	_, err := msgs.Create(ctx, owner, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "start the soak"}}})
	require.NoError(t, err)
	store := sessionhosts.NewHostStore(a.DB(), a.DataDirectory(), os.Getpid(), "reset-debris")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	batchStore := session.NewBatchStore(a.DB())
	reserved, err := batchStore.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "reset-debris-runner", ToolCallID: "reset-debris-call", InputHash: "reset-debris-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "pending", ControlState: "none"}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "pending", ControlState: "none"}},
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "pending", ControlState: "none"}},
		}},
	})
	require.NoError(t, err)
	rootID := reserved.Snapshot.Root.RootID
	// Snapshot.Nodes includes the root and is ordered by node_id: derive the
	// task children by parent_id, never by index.
	var kids []string
	for _, node := range reserved.Snapshot.Nodes {
		if node.ParentID.Valid && node.ParentID.String == rootID {
			kids = append(kids, node.NodeID)
		}
	}
	require.Len(t, kids, 2, "the fixture reserves exactly two task children")
	nodeA := kids[0]
	_, err = a.DB().ExecContext(ctx, `UPDATE batch_nodes SET announced = 1, announce_message_id = 'debt-msg' WHERE node_id = ? AND root_id = ?`, rootID, rootID)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx, `UPDATE batch_nodes SET notice_selected = 1, notice_kind = 'task', notice_state = 'completed', notice_cause = '', notice_sequence = 1, notice_ordinal = 0, delivery = 'done', wake = 1 WHERE node_id = ? AND parent_id = ?`, nodeA, rootID)
	require.NoError(t, err)
	settleResetBatchRoot(t, a, owner, "reset-debris-call")
	require.NoError(t, session.NewSessionNoticeStore(a.DB()).InsertSessionNotice(ctx, owner, session.NoticeKindSupervision, "supervision: check the soak", true, ""))
	debt, err := session.NewBatchStore(a.DB()).BatchReactionPendingInclusiveDebtRows(ctx, owner)
	require.NoError(t, err)
	require.NotEmpty(t, debt.Nodes, "precondition: the completed root's completion debt exists")
	require.NotEmpty(t, debt.Notices, "precondition: the completion notice is debt")
}

func settleResetBatchRoot(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	_, err := a.DB().ExecContext(context.Background(), `UPDATE batch_nodes SET state = 'completed', control_state = 'none', updated_at = ? WHERE owner_session_id = ? AND (tool_call_id = ? OR root_id IN (SELECT root_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?))`, time.Now().Unix(), owner, toolCallID, owner, toolCallID)
	require.NoError(t, err)
}

func assertResetClean(t *testing.T, a *app.App, msgs message.Service, owner string) {
	t.Helper()
	ctx := context.Background()
	debt, err := session.NewBatchStore(a.DB()).BatchReactionPendingInclusiveDebtRows(ctx, owner)
	require.NoError(t, err)
	require.Empty(t, debt.Nodes, "old batch debt must not survive the wipe")
	require.Empty(t, debt.Notices, "old batch debt must not survive the wipe")
	notices, err := session.NewSessionNoticeStore(a.DB()).PullSessionNotices(ctx, msgs, owner, func(row sessionnotice.SessionNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: row.Text}}}
	})
	require.NoError(t, err)
	require.Empty(t, notices, "no session notice may reach the fresh history")
}

func TestSessionsReset_VoidsBatchDebtAndNotices(t *testing.T) {
	a, cmd, _, shutdown := resetTestEnv(t, "", "")
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-async-debris", "debris")
	require.NoError(t, err)
	seedResetDebris(t, a, a.Messages, sess.ID)
	shutdown()
	stderr := cmdtest.CaptureStderr(t, func() {
		setResetForce(t, cmd, false)
		require.NoError(t, cmd.RunE(cmd, []string{sess.ID}))
	})
	t.Logf("reset stderr:\n%s", stderr)
	verify := resetVerifyApp(t, cmd)
	left, err := verify.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, left)
	assertResetClean(t, verify, verify.Messages, sess.ID)
}

func TestSessionsReset_LiveLoopRefusesEvenWithForce(t *testing.T) {
	for _, force := range []string{"false", "true"} {
		t.Run("force="+force, func(t *testing.T) {
			tmp := cmdtest.IsolateConfigEnvForTests(t)
			dataDir := filepath.Join(tmp, "configured-elsewhere-data")
			a, cmd, resolvedDir, shutdown := resetTestEnv(t, dataDir, "")
			require.Equal(t, dataDir, resolvedDir)
			ctx := context.Background()
			sess, err := a.Sessions.CreateWithID(ctx, "reset-live-loop-"+force, "driven")
			require.NoError(t, err)
			seedResetDebris(t, a, a.Messages, sess.ID)
			driverHost := sessionhosts.NewHostStore(a.DB(), dataDir, 4321, "reset-live-loop")
			driverHostID, err := driverHost.EnsureHost(ctx)
			require.NoError(t, err)
			require.NoError(t, driverHost.ClaimSessionDriver(ctx, sess.ID))
			driverHost.CloseKeepLock()
			t.Cleanup(func() { sessionhosts.ReleaseRetainedHostLocksForTest(driverHostID) })
			shutdown()
			setResetForce(t, cmd, force == "true")
			var runErr error
			cmdtest.CaptureStderr(t, func() { runErr = cmd.RunE(cmd, []string{sess.ID}) })
			require.ErrorContains(t, runErr, "still being worked on")
			require.ErrorContains(t, runErr, "--force only kills the process holding the session lock")
			verify := resetVerifyApp(t, cmd)
			left, err := verify.Messages.List(ctx, sess.ID)
			require.NoError(t, err)
			require.NotEmpty(t, left, "the live loop's history is intact")
		})
	}
}
