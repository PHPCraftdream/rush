package server

// Durable delegation child-link bind (task #1490), end-to-end: after
// BatchBindChildSession the live batch walk must reach the child session and
// carry the bound link on the returned rows.

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchBindChildSessionReachesLiveWalk(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)

	parent, err := a.Sessions.Create(ctx, "walk parent")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "walk-child-0", parent.ID, "delegation child")
	require.NoError(t, err)

	// Reserve a live root with the child link still NULL — the bind is what
	// must create it.
	res, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
		Owner: parent.ID, Host: host, RunnerID: "walk-runner", ToolCallID: "call-walk", InputHash: "hash-walk", Sequence: 1,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{
				Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
				State: "running", ControlState: "none",
			},
			Children: []session.BatchReservationNode{{
				Values: db.ReserveBatchNodeParams{
					Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true},
					State: "starting", ControlState: "none",
				},
			}},
		},
	})
	require.NoError(t, err)
	rootID := res.Snapshot.Root.RootID
	taskNodeID := res.Snapshot.Nodes[1].NodeID

	require.NoError(t, session.NewBatchStore(a.DB()).BatchBindChildSession(
		ctx, parent.ID, host, "walk-runner", rootID, taskNodeID, 0, child.ID))

	// MUTATION: reverting the durable bind leaves child_session_id NULL →
	// the walk never visits the child (batchCollectRows follows the link)
	// → the visited and ChildSessionID assertions below fail.
	realSrc := batchSourceFor(a)
	rowsByOwner, visited, complete := batchCollectRows(ctx, realSrc, parent.ID)
	require.True(t, complete, "the walk over one live root must be complete")
	require.Contains(t, visited, child.ID, "the bound child must be visited by the live walk")
	rows, ok := rowsByOwner[parent.ID]
	require.True(t, ok, "the owning session must have live rows")
	found := false
	for _, n := range rows {
		if n.NodeID == taskNodeID {
			found = true
			require.Equal(t, child.ID, n.ChildSessionID.String, "the row must carry the bound child link")
		}
	}
	require.True(t, found, "the delegation task node must be among the live rows")
}
