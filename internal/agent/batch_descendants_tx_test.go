// The descendant closure (batchCoordinatorDescendants -> BatchLiveForOwner)
// must cross the same transactional dispatch as every other live read:
// with TxStarter set, DescendantsLive opens exactly one read transaction,
// and a revert to a non-transactional helper leaves begins at zero.
package agent

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchDescendantsLiveUsesTxDispatch(t *testing.T) {
	env := newBatchInventoryEnv(t, "view")
	counting := &batchPullCountingTxStarter{DB: env.conn}
	live := session.NewBatchLiveReader(env.q, t.TempDir())
	live.IsOwnHost = func(string) bool { return false }
	live.ProbeHost = func(string, string) (session.HostLockStatus, error) { return session.HostStatusAlive, nil }
	live.TxStarter = counting
	deps := CoordinatorBatchAgentDepsWithLive(live)
	// No live root: closed scope, still exactly one dispatch.
	gotLive, complete := deps.Descendants.DescendantsLive(env.ctx, batchAgentRunRequest{ChildSessionID: env.sessionID})
	require.False(t, gotLive)
	require.True(t, complete)
	require.Equal(t, 1, counting.begins)
	// Reserve a live root for the child: open scope, one more dispatch.
	_, err := env.store.Reserve(env.ctx, session.BatchReservation{
		Owner: env.sessionID, Host: "host", RunnerID: "runner", ToolCallID: "call-live",
		InputHash: "hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true},
			State: "pending", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	gotLive, complete = deps.Descendants.DescendantsLive(env.ctx, batchAgentRunRequest{ChildSessionID: env.sessionID})
	require.True(t, gotLive)
	require.True(t, complete)
	require.Equal(t, 2, counting.begins)
}
