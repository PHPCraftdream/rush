// T2 flag-ON live-wait/timeout batch fixtures (package T2, RUSH_BATCH_RUNTIME
// shadow): the live-work seeds the ON wait, timeout and heartbeat paths read
// (batch_nodes through the production BatchStore). Dual-mode with the legacy
// AsyncJobStore seeds is kept in the tests themselves byte-for-byte; these
// helpers are ON-side only and never touch the flag.
package app

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// pt2ReserveLiveRoot reserves a live batch root with one task child per
// given task kind, under an EXPLICIT host id (the f6ReserveDelegation shape,
// which always stamps the app's own host, cannot seed foreign-host work).
// State is running for root and tasks, so BatchLiveForOwner reports the
// root live. Returns the root id and the task child node ids.
func pt2ReserveLiveRoot(t *testing.T, application *App, owner, host, toolCallID string, taskKinds ...string) (root string, nodes []string) {
	t.Helper()
	ctx := context.Background()
	children := make([]session.BatchReservationNode, 0, len(taskKinds))
	for _, kind := range taskKinds {
		children = append(children, session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: kind, Valid: true}, State: "running", ControlState: "none",
		}})
	}
	res, err := session.NewBatchStore(application.DB()).Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: f6BatchRunner, ToolCallID: toolCallID,
		InputHash: "pt2-hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: children},
	})
	require.NoError(t, err)
	root = res.Snapshot.Root.RootID
	for _, n := range res.Snapshot.Nodes {
		if n.NodeID != root {
			nodes = append(nodes, n.NodeID)
		}
	}
	require.Len(t, nodes, len(taskKinds), "every task child must be reserved")
	return root, nodes
}
