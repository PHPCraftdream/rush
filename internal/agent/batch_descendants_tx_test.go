// The descendant closure (batchCoordinatorDescendants -> BatchLiveForOwner)
// must cross the same transactional dispatch as every other live read:
// with TxStarter set, DescendantsLive opens exactly one read transaction,
// and a revert to a non-transactional helper leaves begins at zero.
package agent

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
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

// TestBatchDescendantsLiveInterleavesPurge: a purge of the only live root
// landing between the descendant closure's root list and its node reads must
// not turn the closure's answer into a complete-empty scope — the read tx
// preserves the purged root's rows. The purge runs through a SECOND pool
// with production pragmas (foreign_keys ON cascades the subtree); the
// single-connection env pool is held by the open read tx and must not be
// touched inside the seam. MUTATION: dispatching the closure through the
// nontransactional two-step helper lets the purge land between the
// statements -> complete-empty -> gotLive=false -> the gotLive assertion
// fails.
func TestBatchDescendantsLiveInterleavesPurge(t *testing.T) {
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, &countingTool{name: "view"})
	counting := &batchPullCountingTxStarter{DB: env.conn}
	env.live.TxStarter = counting
	root := batchPullReserveLiveRoot(t, env)
	raw, err := sql.Open("sqlite", filepath.Join(env.dataDir, "rush.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	restore := session.SetBatchLiveServedSeamForTest(func() {
		rawStore := session.NewBatchStore(raw)
		require.NoError(t, rawStore.DeleteRoot(env.ctx, env.sessionID, "host", root))
	})
	defer restore()
	deps := CoordinatorBatchAgentDepsWithLive(env.live)
	gotLive, complete := deps.Descendants.DescendantsLive(env.ctx, batchAgentRunRequest{ChildSessionID: env.sessionID})
	require.True(t, gotLive, "the snapshot must still show the purged root")
	require.True(t, complete, "the transactional read must not report the purge as a closed scope")
}
