package session

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestR9RetentionWholeRootsDebtLiveDelegation(t *testing.T) {
	for _, scenario := range []string{"idle", "pending", "starting", "running", "cancelling", "delivery", "debt", "child-live", "child-debt", "child-notice", "recent"} {
		t.Run(scenario, func(t *testing.T) {
			a, _, conn, _, ctx := r9Stores(t)
			snap := batchStoreTestReserve(t, a, ctx)
			control, err := a.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
			require.NoError(t, err)
			require.NoError(t, a.AnswerControl(ctx, db.AnswerBatchControlRequestParams{RequestID: control.RequestID, OwnerSessionID: "owner", HostID: "host", RootID: control.RootID, NodeID: control.NodeID, AnswerCode: "accepted", AnswerSequence: sql.NullInt64{Int64: 1, Valid: true}, AnsweredAt: sql.NullInt64{Int64: 1, Valid: true}}))
			messages := message.NewService(db.New(conn))
			history, err := messages.Create(ctx, "owner", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "unrelated history"}}})
			require.NoError(t, err)
			_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state='completed',updated_at=1")
			require.NoError(t, err)
			switch scenario {
			case "pending", "starting", "running", "cancelling":
				_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state=? WHERE node_id=?", scenario, snap.Nodes[2].NodeID)
			case "recent":
				_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET updated_at=1000 WHERE node_id=?", snap.Nodes[2].NodeID)
			case "delivery", "debt":
				delivery, wake := "pending", 0
				if scenario == "debt" {
					delivery, wake = "done", 1
				}
				_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET notice_selected=1,notice_kind='task',notice_state='completed',notice_cause='',notice_sequence=1,notice_ordinal=0,delivery=?,wake=?,notice_message_id=? WHERE node_id=?`, delivery, wake, history.ID, snap.Nodes[2].NodeID)
			case "child-live", "child-debt", "child-notice":
				require.NoError(t, seedSession(ctx, db.New(conn), "child"))
				_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET child_session_id='child' WHERE node_id=?", snap.Nodes[1].NodeID)
				require.NoError(t, err)
				if scenario == "child-notice" {
					_, err = db.New(conn).InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: "child", Kind: "question", Text: "help", Wake: 1, CreatedAt: 1, UpdatedAt: 1})
				} else {
					p := batchStoreTestReservation()
					p.Owner = "child"
					res, e := a.Reserve(ctx, p)
					require.NoError(t, e)
					if scenario == "child-debt" {
						_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET state='completed',notice_selected=1,notice_kind='root',notice_state='completed',notice_cause='',notice_sequence=1,notice_ordinal=0,delivery='done',wake=1 WHERE node_id=?`, res.Snapshot.Root.RootID)
					}
				}
			}
			require.NoError(t, err)
			count, err := a.BatchCountExpired(ctx, 100)
			require.NoError(t, err)
			want := int64(0)
			if scenario == "idle" {
				want = 1
			}
			require.Equal(t, want, count)
			purged, err := a.BatchPurgeExpired(ctx, 100)
			require.NoError(t, err)
			require.EqualValues(t, 1, purged)
			_, err = a.Read(ctx, "owner", snap.Root.RootID)
			if want == 1 {
				require.ErrorIs(t, err, sql.ErrNoRows)
				_, err = a.BatchReadControl(ctx, BatchControlTarget{"owner", "host", control.RootID, control.NodeID}, control.RequestID)
				require.ErrorIs(t, err, ErrBatchControlStale)
			} else {
				require.NoError(t, err)
			}
			got, err := messages.Get(ctx, history.ID)
			require.NoError(t, err)
			require.Equal(t, history.ID, got.ID)
		})
	}
}

func TestR9RetentionSettledCLIWakeDebtExpires(t *testing.T) {
	a, _, conn, _, ctx := r9Stores(t)
	runner, executor, runCtx := commitHookRunner(t, NewBatchCommitHook(a, BatchCommitIdentity{Host: "host", Runner: "runner", Owner: "owner"}))
	batch := commitHookSubmit(t, runCtx, runner, "cli-retention", jobbatch.OneItem(jobbatch.TaskSpec{Kind: "retention"}), "batch-cli:retention")
	attempt := commitHookAccept(t, runCtx, runner, executor, nil)
	commitHookFinish(t, runCtx, runner, attempt, jobbatch.Result{State: jobbatch.StateCompleted})
	commitHookWait(t, runCtx, batch)
	root := "cli-retention"
	var children int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id=?`, root).Scan(&children))
	require.Equal(t, 2, children)
	var pending int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id=? AND notice_selected=1 AND wake=1 AND delivery='pending'`, root).Scan(&pending))
	require.Equal(t, 1, pending)
	count, err := a.BatchCountExpired(ctx, 1<<62)
	require.NoError(t, err)
	require.Zero(t, count)
	purged, err := a.BatchPurgeExpired(ctx, 1<<62)
	require.NoError(t, err)
	require.Zero(t, purged)
	require.NoError(t, a.BatchSettleCLIWakeDebt(ctx, "owner", root))
	count, err = a.BatchCountExpired(ctx, 1<<62)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	purged, err = a.BatchPurgeExpired(ctx, 1<<62)
	require.NoError(t, err)
	require.EqualValues(t, 2, purged)
}

func TestR9OwnerCascade(t *testing.T) {
	a, _, conn, _, ctx := r9Stores(t)
	snap := batchStoreTestReserve(t, a, ctx)
	_, err := a.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "DELETE FROM sessions WHERE id='owner'")
	require.NoError(t, err)
	for _, table := range []string{"batch_nodes", "batch_control_requests"} {
		var count int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}
