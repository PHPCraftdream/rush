// Batch history consumer coverage (R10c chunk 3): delivery reconciliation
// against deleted history (D-R10c-3, all four delivery states), the owner
// deletion cascade (D-R10c-1) and legacy bound-notice disposal exactly once
// (D-R10c-5). Disposable real SQLite through the guarded Connect/Release
// pair.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// batchConsumersNotice creates a real message row to stand for a delivered
// notice message the caller is about to delete.
func batchConsumersNotice(t *testing.T, m message.Service, ctx context.Context, text string) string {
	t.Helper()
	msg, err := m.Create(ctx, "owner", message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}, BackgroundJobNotice: true})
	require.NoError(t, err)
	return msg.ID
}

func batchConsumersSetNode(t *testing.T, conn *sql.DB, ctx context.Context, id, delivery string, wake, reacted, wakeAttempts, reactedFailed int64, msgID string) {
	t.Helper()
	var nID sql.NullString
	if msgID != "" {
		nID = sql.NullString{String: msgID, Valid: true}
	}
	_, err := conn.ExecContext(ctx, `UPDATE batch_nodes SET delivery=?, wake=?, reacted=?, wake_attempts=?, reacted_failed=?, notice_message_id=? WHERE node_id=?`,
		delivery, wake, reacted, wakeAttempts, reactedFailed, nID, id)
	require.NoError(t, err)
}

func batchConsumersNode(t *testing.T, conn *sql.DB, ctx context.Context, owner, root, id string) db.BatchNode {
	t.Helper()
	n, err := db.New(conn).GetBatchNode(ctx, db.GetBatchNodeParams{RootID: root, NodeID: id, OwnerSessionID: owner})
	require.NoError(t, err)
	return n
}

// Revert-check (mutation "reconciliation drops a delivery state"): removing
// any of the four states' expectations below dies here -- the done/pending
// repend, the none/void untouched guarantees and the counter reset are all
// asserted per state.
func TestBatchHistoryReconcileDeletedHistoryPerDeliveryState(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"none", "pending", "done", "void"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s, conn, ctx := batchStoreTestDB(t)
			m := message.NewService(db.New(conn))
			snap := batchStoreTestReserve(t, s, ctx)
			_, err := s.BatchAnnounceStarted(ctx, m, "owner", snap.Root.RootID, batchOutboxText("started"))
			require.NoError(t, err)
			batchOutboxSettle(t, s, ctx, snap, 2, snap.Nodes[1].NodeID)
			dead := batchConsumersNotice(t, m, ctx, "notice message")
			leaf := snap.Nodes[1].NodeID
			switch state {
			case "none":
				// Unselected node: delivery 'none', stale id is never touched.
				leaf = snap.Nodes[2].NodeID
				batchConsumersSetNode(t, conn, ctx, leaf, "none", 0, 0, 0, 0, dead)
			case "pending":
				batchConsumersSetNode(t, conn, ctx, leaf, "pending", 1, 0, 2, 1, dead)
			case "done":
				batchConsumersSetNode(t, conn, ctx, leaf, "done", 1, 0, 3, 1, dead)
			case "void":
				batchConsumersSetNode(t, conn, ctx, leaf, "void", 0, 0, 0, 0, dead)
			}
			// The reconciliation target history is really gone.
			_, err = conn.ExecContext(ctx, `DELETE FROM messages WHERE id=?`, dead)
			require.NoError(t, err)

			var repended int64
			repended, err = s.BatchHistoryReconcileDelivery(ctx, "owner", []string{dead})
			require.NoError(t, err)
			got := batchConsumersNode(t, conn, ctx, "owner", snap.Root.RootID, leaf)
			switch state {
			case "none", "void":
				require.Zero(t, repended, state+" rows are untouched by design")
				require.Equal(t, state, got.Delivery)
				require.Equal(t, dead, got.NoticeMessageID.String)
			case "pending", "done":
				require.EqualValues(t, 1, repended)
				require.Equal(t, "pending", got.Delivery, state+" with deleted history repends")
				require.False(t, got.NoticeMessageID.Valid, "the dead message reference is cleared")
				require.Zero(t, got.WakeAttempts)
				require.Zero(t, got.ReactedFailed)
				// Debt consistency: the repended row is outstanding exactly once.
				debt, e := db.New(conn).BatchOutboxDebtNodes(ctx, "owner")
				require.NoError(t, e)
				require.Len(t, debt, 1)
				require.Equal(t, leaf, debt[0].NodeID)
			}
			// No selected row anywhere stays 'done' for the deleted message.
			var stale int
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE delivery='done' AND notice_message_id=?`, dead).Scan(&stale))
			require.Zero(t, stale, "done is never left for a nonexistent message")
		})
	}
}

// Revert-check (mutation "owner cascade leaves control requests" and
// "owner delete refuses live roots like reset"): dropping the control FK
// cascade assertion, the no-refusal behavior or any deletion leg fails here.
func TestBatchOwnerDeleteCascade(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(conn))
	snap := batchStoreTestReserve(t, s, ctx)
	// A LIVE root: reset would refuse; deletion must not (D-R10c-1).
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Root.NodeID, "running")}}}}))
	_, err := s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.NoError(t, err)
	require.NotNil(t, batchConsumersNotice(t, m, ctx, "history"))
	_, err = conn.ExecContext(ctx, `INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at) VALUES ('owner','supervision','bound','1','pending',?,1,1)`, snap.Root.ToolCallID.String)
	require.NoError(t, err)
	// CancelAllWakeSchedulesForOwner covers the owner's legacy schedules.
	_, err = conn.ExecContext(ctx, `INSERT INTO wake_schedules (id, owner_session_id, kind, message, next_run_at, state, created_at, updated_at) VALUES ('sched','owner','once','wake',9999999999,'active',1,1)`)
	require.NoError(t, err)

	out, err := s.BatchOwnerDeleteHistory(ctx, m, "owner")
	require.NoError(t, err, "deletion refuses nothing, live root included")
	require.Equal(t, 1, out.MessagesDeleted)
	// 1: RowsAffected counts only the directly-matched root row; the task
	// children go via FK cascade and are not counted (same semantics as the
	// reset test).
	require.EqualValues(t, 1, out.NodesDeleted)
	require.EqualValues(t, 1, out.NoticesVoided)
	require.EqualValues(t, 1, out.SchedulesCancelled)
	// batch_nodes, batch_control_requests and messages are DELETED; legacy
	// session_notices are voided and wake schedules cancelled BY DESIGN (rows
	// remain, mirroring VoidUndeliveredSessionNoticesForOwner /
	// CancelAllWakeSchedulesForOwner; the session row itself goes later via
	// session.Service.Delete's FKs).
	for _, table := range []string{"batch_nodes", "batch_control_requests", "messages"} {
		var count int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count), table)
		require.Zero(t, count, table+" is gone after the owner delete")
	}
	var notices int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM session_notices WHERE delivery='void'`).Scan(&notices))
	require.Equal(t, 1, notices, "session_notices are voided in place, not deleted")
	var schedules int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM wake_schedules WHERE state='cancelled'`).Scan(&schedules))
	require.Equal(t, 1, schedules, "wake_schedules are cancelled in place, not deleted")
	// A later projection of the dead incarnation is contained by conflict.
	require.ErrorIs(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 3, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Nodes[1].NodeID, "completed")}}}}), ErrBatchProjectionConflict)
}

// Revert-check (mutation "disposal guard missing", m12): dropping the
// delivery IN ('pending','done') AND wake = 1 guards (reacted = 0 kept) lets
// a second dispose re-void an already-void row; the exactly-once assertions
// below fail.
func TestBatchDisposeLegacyBoundNoticesExactlyOnce(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	rows := []struct {
		delivery      string
		wake, reacted int64
	}{
		// pending woken unreacted, done woken unreacted, done woken reacted,
		// done unwoken unreacted, void woken unreacted.
		{"pending", 1, 0}, {"done", 1, 0}, {"done", 1, 1}, {"done", 0, 0}, {"void", 1, 0},
	}
	for i, r := range rows {
		_, err := conn.ExecContext(ctx, `INSERT INTO session_notices (owner, kind, text, wake, delivery, reacted, job_tool_call_id, created_at, updated_at) VALUES ('owner','supervision',?,?,?,?,?,?,?)`,
			fmt.Sprintf("n%d", i), r.wake, r.delivery, r.reacted, "call", time.Now().Unix(), time.Now().Unix())
		require.NoError(t, err)
	}
	n, err := s.BatchDisposeLegacyBoundNotices(ctx, "owner", "call")
	require.NoError(t, err)
	require.EqualValues(t, 2, n, "only pending/done unreacted woken rows dispose")
	notices, err := db.New(conn).BatchListBoundNoticesByToolCallID(ctx, db.BatchListBoundNoticesByToolCallIDParams{Owner: "owner", ToolCallID: sql.NullString{String: "call", Valid: true}})
	require.NoError(t, err)
	require.Len(t, notices, 5)
	byText := map[string]db.SessionNotice{}
	for _, notice := range notices {
		byText[notice.Text] = notice
	}
	for _, text := range []string{"n0", "n1"} {
		require.Equal(t, "void", byText[text].Delivery, text+" is disposed")
		require.Zero(t, byText[text].Wake, "wake zeroed so debt readers cannot double-count")
	}
	require.Equal(t, "done", byText["n2"].Delivery, "a reacted notice is left as history")
	require.Equal(t, "done", byText["n3"].Delivery, "an unwoken notice is untouched")
	require.Equal(t, "void", byText["n4"].Delivery, "an already-void notice is untouched")
	require.EqualValues(t, 1, byText["n4"].Wake, "the pre-void notice keeps its wake")
	// Debt reader consistency: BatchOutboxDebtNotices requires reacted=0, so
	// after disposal NO row is outstanding (the reacted row is excluded by
	// design, the disposed rows by their zeroed wake).
	debt, err := db.New(conn).BatchOutboxDebtNotices(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, debt, "no disposed or reacted row is outstanding debt")
	// Double-dispose is a no-op.
	again, err := s.BatchDisposeLegacyBoundNotices(ctx, "owner", "call")
	require.NoError(t, err)
	require.Zero(t, again, "disposal happens exactly once")
}
