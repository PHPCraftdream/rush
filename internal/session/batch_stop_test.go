package session

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestBatchStopRootFenceSurvivesAnsweredRequest(t *testing.T) {
	for _, answered := range []bool{false, true} {
		name := "pending"
		if answered {
			name = "answered"
		}
		t.Run(name, func(t *testing.T) {
			s, conn, ctx := batchStoreTestDB(t)
			require.NoError(t, seedSession(ctx, db.New(conn), "other-owner"))
			snap := batchStoreTestReserve(t, s, ctx)
			otherSpec := batchStoreTestReservation()
			otherSpec.Owner = "other-owner"
			other, err := s.Reserve(ctx, otherSpec)
			require.NoError(t, err)

			// A real, selected wake notice makes the root-fence suppression observable.
			leaf := snap.Nodes[1].NodeID
			p := BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{
				Owner: "owner", Root: snap.Root.RootID,
				Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(leaf, "completed")},
				Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(leaf)},
			}}}
			require.NoError(t, s.Project(ctx, p))
			var wake int64
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT wake FROM batch_nodes WHERE node_id=? AND notice_selected=1", leaf).Scan(&wake))
			require.EqualValues(t, 1, wake)

			targets, err := s.BatchStopOwners(ctx, []string{"owner"})
			require.NoError(t, err)
			require.Equal(t, []BatchControlTarget{{Owner: "owner", Host: "host", Root: snap.Root.RootID, Node: snap.Root.RootID}}, targets)
			targets, err = s.BatchStopOwners(ctx, []string{"owner"})
			require.NoError(t, err)
			require.Len(t, targets, 1)
			var requests int
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_control_requests WHERE owner_session_id='owner' AND host_id='host' AND root_id=? AND node_id=?", snap.Root.RootID, snap.Root.RootID).Scan(&requests))
			require.Equal(t, 1, requests)
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT wake FROM batch_nodes WHERE node_id=? AND notice_selected=1", leaf).Scan(&wake))
			require.EqualValues(t, 0, wake)
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_control_requests WHERE owner_session_id='other-owner'").Scan(&requests))
			require.Zero(t, requests)

			var requestID string
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT request_id FROM batch_control_requests WHERE owner_session_id='owner' AND root_id=? AND node_id=?", snap.Root.RootID, snap.Root.RootID).Scan(&requestID))
			if answered {
				require.NoError(t, s.AnswerControl(ctx, db.AnswerBatchControlRequestParams{
					RequestID: requestID, RootID: snap.Root.RootID, NodeID: snap.Root.RootID,
					OwnerSessionID: "owner", HostID: "host", AnswerCode: "accepted",
					AnswerSequence: sql.NullInt64{Int64: 3, Valid: true}, AnsweredAt: sql.NullInt64{Int64: 1, Valid: true},
				}))
			}
			status := "pending"
			if answered {
				status = "answered"
			}
			var actualStatus string
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT status FROM batch_control_requests WHERE request_id=?", requestID).Scan(&actualStatus))
			require.Equal(t, status, actualStatus)

			// A fresh store models restart; projection of a terminal root still sees the durable fence.
			s = NewBatchStore(conn)
			root := snap.Root.NodeID
			require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 3, Roots: []BatchRootProjection{{
				Owner: "owner", Root: snap.Root.RootID,
				Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(root, "completed"), batchStoreTestUpdate(snap.Nodes[2].NodeID, "completed")},
				Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(root)},
			}}}))
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT notice_selected FROM batch_nodes WHERE node_id=?", root).Scan(&requests))
			require.Equal(t, 1, requests)
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT wake FROM batch_nodes WHERE node_id=?", root).Scan(&wake))
			require.EqualValues(t, 0, wake)

			// Same projection for an unaffected sibling has no matching root fence.
			sibling := other.Snapshot.Root.NodeID
			require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 3, Roots: []BatchRootProjection{{
				Owner: "other-owner", Root: other.Snapshot.Root.RootID,
				Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(sibling, "completed")},
				Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(sibling)},
			}}}))
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT notice_selected FROM batch_nodes WHERE node_id=?", sibling).Scan(&requests))
			require.Equal(t, 1, requests)
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT wake FROM batch_nodes WHERE node_id=?", sibling).Scan(&wake))
			require.EqualValues(t, 1, wake)
		})
	}
}
