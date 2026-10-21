package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func psfSettleTerminalRoot(t *testing.T, s *BatchStore, ctx context.Context, snap BatchSnapshot, sequence int64) {
	t.Helper()
	updates := make([]db.ProjectBatchNodeParams, 0, len(snap.Nodes))
	for _, node := range snap.Nodes {
		updates = append(updates, db.ProjectBatchNodeParams{NodeID: node.NodeID, State: "cancelled", Cause: "stop", ControlState: "none", OutcomeState: "cancelled", OutcomeCode: "stop_accepted"})
	}
	notice := batchStoreTestNotice(snap.Root.RootID)
	notice.NoticeKind = batchStoreTestString("root")
	notice.NoticeState = batchStoreTestString("cancelled")
	notice.NoticeCause = batchStoreTestString("stop")
	notice.NoticeCompleted = 0
	notice.NoticeCancelled = int64(len(snap.Nodes))
	require.NoError(t, s.Project(ctx, BatchProjection{Host: snap.Root.HostID, RunnerID: snap.Root.RunnerID, Sequence: sequence, Roots: []BatchRootProjection{{Owner: snap.Root.OwnerSessionID, Root: snap.Root.RootID, Updates: updates, Notices: []db.SelectBatchNodeNoticeParams{notice}}}}))
}

func psfAssertTerminalNotice(t *testing.T, s *BatchStore, conn *sql.DB, ctx context.Context, owner, root string, wake int64) {
	t.Helper()
	var selected, actualWake int64
	var state, cause string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT notice_selected, wake, notice_state, notice_cause FROM batch_nodes WHERE root_id=? AND node_id=root_id`, root).Scan(&selected, &actualWake, &state, &cause))
	require.EqualValues(t, 1, selected)
	require.EqualValues(t, wake, actualWake)
	require.Equal(t, "cancelled", state)
	require.Equal(t, "stop", cause)
	if wake == 0 {
		return
	}
	m := message.NewService(db.New(conn))
	batchOutboxAck(t, s, ctx, m, root)
	has, err := s.BatchReactionDebtExists(ctx, owner)
	require.NoError(t, err)
	require.True(t, has)
	debt, err := s.BatchReactionPendingInclusiveDebtRows(ctx, owner)
	require.NoError(t, err)
	require.NotEmpty(t, debt.Nodes)
	pulled, err := s.BatchPullNotices(ctx, m, owner, batchOutboxParams)
	require.NoError(t, err)
	require.NotEmpty(t, pulled)
	for _, item := range pulled {
		if item.RootID == root {
			require.Equal(t, root, item.NodeID)
			require.Equal(t, wake != 0, item.Wake)
			var pulledState, pulledCause string
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT notice_state, notice_cause FROM batch_nodes WHERE node_id=? AND root_id=?`, item.NodeID, root).Scan(&pulledState, &pulledCause))
			require.Equal(t, "cancelled", pulledState)
			require.Equal(t, "stop", pulledCause)
			return
		}
	}
	require.FailNow(t, "terminal root notice was not pulled")
}

func psfTestReservation(owner, host, runner, call string) BatchReservation {
	p := batchStoreTestReservation()
	p.Owner, p.Host, p.RunnerID, p.ToolCallID = owner, host, runner, call
	return p
}

func psfInsertOwnerFence(t *testing.T, conn *sql.DB, ctx context.Context, snap BatchSnapshot, runner string) {
	t.Helper()
	_, err := conn.ExecContext(ctx, `INSERT INTO batch_control_requests (request_id, owner_session_id, root_id, node_id, host_id, operation, status, answer_code, created_at, runner_id) VALUES (?, ?, ?, ?, ?, 'stop', 'pending', '', 1, ?)`, psfOwnerStopRequestID(), snap.Root.OwnerSessionID, snap.Root.RootID, snap.Root.RootID, snap.Root.HostID, runner)
	require.NoError(t, err)
}

func TestBatchStopFence_RequestIDNamespace(t *testing.T) {
	id := psfOwnerStopRequestID()
	parsed, err := uuid.Parse(id[len(psfOwnerStopPrefix):])
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, parsed)
	require.True(t, psfIsOwnerStopRequestID(id))
	s, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx)
	ordinary, err := s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Root.RootID)
	require.NoError(t, err)
	_, err = uuid.Parse(ordinary.RequestID)
	require.NoError(t, err)
	require.False(t, psfIsOwnerStopRequestID(ordinary.RequestID))
}

func TestBatchStopFence_OrdinaryRequestsDoNotSuppressDelayedNotice(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "answered"}[answered], func(t *testing.T) {
			s, conn, ctx := batchStoreTestDB(t)
			snap := batchStoreTestReserve(t, s, ctx)
			request, err := s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Root.RootID)
			require.NoError(t, err)
			_, parseErr := uuid.Parse(request.RequestID)
			require.NoError(t, parseErr)
			require.False(t, psfIsOwnerStopRequestID(request.RequestID))
			if answered {
				require.NoError(t, s.AnswerControl(ctx, db.AnswerBatchControlRequestParams{RequestID: request.RequestID, OwnerSessionID: "owner", HostID: "host", RootID: snap.Root.RootID, NodeID: snap.Root.RootID, RunnerID: request.RunnerID, AnswerCode: "accepted", AnswerSequence: sql.NullInt64{Int64: 2, Valid: true}, AnsweredAt: sql.NullInt64{Int64: 1, Valid: true}}))
			}
			psfSettleTerminalRoot(t, s, ctx, snap, 2)
			psfAssertTerminalNotice(t, s, conn, ctx, "owner", snap.Root.RootID, 1)
		})
	}
}

func TestBatchStopFence_OwnerRequestSuppressesLateProjection(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "answered"}[answer], func(t *testing.T) {
			s, conn, ctx := batchStoreTestDB(t)
			snap := batchStoreTestReserve(t, s, ctx)
			targets, err := s.BatchStopOwners(ctx, []string{"owner"})
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.True(t, psfIsOwnerStopRequestID(targets[0].RequestID))
			if answer {
				require.NoError(t, s.BatchAnswerOwnerStop(ctx, targets[0]))
			}
			s = NewBatchStore(conn)
			psfSettleTerminalRoot(t, s, ctx, snap, 2)
			psfAssertTerminalNotice(t, s, conn, ctx, "owner", snap.Root.RootID, 0)
		})
	}
}

func TestBatchStopFence_ModelAndOwnerRequestsRemainDistinct(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending-model", true: "answered-model"}[answered], func(t *testing.T) {
			s, conn, ctx := batchStoreTestDB(t)
			snap := batchStoreTestReserve(t, s, ctx)
			model, err := s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Root.RootID)
			require.NoError(t, err)
			targets, err := s.BatchStopOwners(ctx, []string{"owner"})
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.NotEqual(t, model.RequestID, targets[0].RequestID)
			again, err := s.BatchStopOwners(ctx, []string{"owner"})
			require.NoError(t, err)
			require.Len(t, again, 1)
			require.Equal(t, targets[0].RequestID, again[0].RequestID)
			require.Equal(t, "pending", again[0].Status)
			if answered {
				require.NoError(t, s.BatchAnswerOwnerStop(ctx, targets[0]))
				again, err = s.BatchStopOwners(ctx, []string{"owner"})
				require.NoError(t, err)
				require.Len(t, again, 1)
				require.Equal(t, targets[0].RequestID, again[0].RequestID)
				require.Equal(t, "answered", again[0].Status)
			}
			if answered {
				require.NoError(t, s.AnswerControl(ctx, db.AnswerBatchControlRequestParams{RequestID: model.RequestID, OwnerSessionID: "owner", HostID: "host", RootID: snap.Root.RootID, NodeID: snap.Root.RootID, RunnerID: model.RunnerID, AnswerCode: "accepted", AnswerSequence: sql.NullInt64{Int64: 2, Valid: true}, AnsweredAt: sql.NullInt64{Int64: 1, Valid: true}}))
				require.ErrorIs(t, s.BatchAnswerOwnerStop(ctx, BatchStopOwnerTarget{BatchControlTarget: BatchControlTarget{Owner: "owner", Host: "host", Root: snap.Root.RootID, Node: snap.Root.RootID}, RequestID: model.RequestID, RunnerID: model.RunnerID}), ErrBatchControlStale)
			} else {
				require.ErrorIs(t, s.BatchAnswerOwnerStop(ctx, BatchStopOwnerTarget{BatchControlTarget: BatchControlTarget{Owner: "owner", Host: "host", Root: snap.Root.RootID, Node: snap.Root.RootID}, RequestID: model.RequestID, RunnerID: model.RunnerID}), ErrBatchControlStale)
			}
			var beforeStatus, beforeAnswer string
			var beforeSeq, beforeAnsweredAt sql.NullInt64
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT status, answer_code, answer_sequence, answered_at FROM batch_control_requests WHERE request_id=?`, model.RequestID).Scan(&beforeStatus, &beforeAnswer, &beforeSeq, &beforeAnsweredAt))
			psfSettleTerminalRoot(t, s, ctx, snap, 2)
			again, err = s.BatchStopOwners(ctx, []string{"owner"})
			require.NoError(t, err)
			require.Empty(t, again)
			var status, answer string
			var seq, answeredAt sql.NullInt64
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT status, answer_code, answer_sequence, answered_at FROM batch_control_requests WHERE request_id=?`, model.RequestID).Scan(&status, &answer, &seq, &answeredAt))
			require.Equal(t, beforeStatus, status)
			require.Equal(t, beforeAnswer, answer)
			require.Equal(t, beforeSeq, seq)
			require.Equal(t, beforeAnsweredAt, answeredAt)
			var ownerStatus string
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT status FROM batch_control_requests WHERE request_id=?`, targets[0].RequestID).Scan(&ownerStatus))
			require.Equal(t, "answered", ownerStatus)
			psfAssertTerminalNotice(t, s, conn, ctx, "owner", snap.Root.RootID, 0)
		})
	}
}

func TestBatchStopFence_OwnerHostRootAndRunnerMismatches(t *testing.T) {
	for _, tc := range []string{"owner", "host", "root", "runner"} {
		t.Run(tc, func(t *testing.T) {
			s, conn, ctx := batchStoreTestDB(t)
			require.NoError(t, seedSession(ctx, db.New(conn), "other-owner"))
			primary := batchStoreTestReserve(t, s, ctx)
			otherOwner, err := s.Reserve(ctx, psfTestReservation("other-owner", "host", primary.Root.RunnerID, "other-call"))
			require.NoError(t, err)
			otherHost, err := s.Reserve(ctx, psfTestReservation("owner", "other-host", "other-runner", "host-call"))
			require.NoError(t, err)
			otherRoot, err := s.Reserve(ctx, psfTestReservation("owner", "host", primary.Root.RunnerID, "root-call"))
			require.NoError(t, err)
			switch tc {
			case "owner":
				psfInsertOwnerFence(t, conn, ctx, otherOwner.Snapshot, otherOwner.Snapshot.Root.RunnerID)
				psfSettleTerminalRoot(t, s, ctx, primary, 2)
				psfAssertTerminalNotice(t, s, conn, ctx, "owner", primary.Root.RootID, 1)
			case "host":
				psfInsertOwnerFence(t, conn, ctx, otherHost.Snapshot, otherHost.Snapshot.Root.RunnerID)
				psfSettleTerminalRoot(t, s, ctx, primary, 2)
				psfAssertTerminalNotice(t, s, conn, ctx, "owner", primary.Root.RootID, 1)
			case "root":
				psfInsertOwnerFence(t, conn, ctx, otherRoot.Snapshot, otherRoot.Snapshot.Root.RunnerID)
				psfSettleTerminalRoot(t, s, ctx, primary, 2)
				psfAssertTerminalNotice(t, s, conn, ctx, "owner", primary.Root.RootID, 1)
			case "runner":
				psfInsertOwnerFence(t, conn, ctx, primary, "stale-runner")
				psfSettleTerminalRoot(t, s, ctx, primary, 2)
				psfAssertTerminalNotice(t, s, conn, ctx, "owner", primary.Root.RootID, 1)
			}
		})
	}
}

func TestBatchStopFence_OwnerStopLeavesSameOwnerSiblingActionable(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	primary := batchStoreTestReserve(t, s, ctx)
	targets, err := s.BatchStopOwners(ctx, []string{"owner"})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, primary.Root.RootID, targets[0].Root)
	other := batchStoreTestReservation()
	other.ToolCallID = "sibling-call"
	result, err := s.Reserve(ctx, other)
	require.NoError(t, err)
	psfSettleTerminalRoot(t, s, ctx, primary, 2)
	psfAssertTerminalNotice(t, s, conn, ctx, "owner", primary.Root.RootID, 0)
	psfSettleTerminalRoot(t, s, ctx, result.Snapshot, 2)
	psfAssertTerminalNotice(t, s, conn, ctx, "owner", result.Snapshot.Root.RootID, 1)
}

func TestBatchStopFence_ModelCreatedAfterOwnerDoesNotClearFence(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx)
	targets, err := s.BatchStopOwners(ctx, []string{"owner"})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	model, err := s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Root.RootID)
	require.NoError(t, err)
	require.False(t, psfIsOwnerStopRequestID(model.RequestID))
	s = NewBatchStore(conn)
	psfSettleTerminalRoot(t, s, ctx, snap, 2)
	psfAssertTerminalNotice(t, s, conn, ctx, "owner", snap.Root.RootID, 0)
}

func TestBatchStopFence_CancellationFailureLeavesPendingFence(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx)
	targets, err := s.BatchStopOwners(ctx, []string{"owner"})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	var status string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT status FROM batch_control_requests WHERE request_id=?`, targets[0].RequestID).Scan(&status))
	require.Equal(t, "pending", status)
	s = NewBatchStore(conn)
	psfSettleTerminalRoot(t, s, ctx, snap, 2)
	psfAssertTerminalNotice(t, s, conn, ctx, "owner", snap.Root.RootID, 0)
}
