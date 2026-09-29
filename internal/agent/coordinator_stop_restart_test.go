// DUR-9's restart half with a REAL Stop (docs/async-invariants.md; doc sec.6:
// "Stop of a session with N sub-agents ... after a restart -- not one extra
// fact"). The session-level twin (session.TestSweepDeadHosts_Unstopped...)
// proves what a restart sweep does for rows nobody stopped; this proves that
// after coordinator.Cancel actually stopped the whole delegation tree, the
// restart sweep of the dead host adds nothing. Real SQLite throughout.
package agent

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts: a root delegates to N
// sub-agents, each of which owns a running bash job. The operator Stops the
// root (coordinator.Cancel walks the tree and stops every job), the process
// then dies, and a restart's dead-host sweep finds no running row: it
// recovers ZERO facts, and every row keeps the state Stop gave it (never
// re-labelled 'interrupted'), with no reaction debt anywhere in the tree.
//
// Revert-check: make Cancel a no-op (skip stopTree) -- the rows are still
// running when the host dies and the sweep interrupts all of them.
func TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 3
	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry(), currentAgent: &mockSessionAgent{}}
	ledger := newWorkLedger(coord.notifyAsyncCompletion)
	ledger.store = store
	ledger.coord = coord
	coord.asyncJobs = ledger

	const root = "stop-restart-root"
	children := make([]string, n)
	for i := range n {
		child := fmt.Sprintf("stop-restart-child-%d", i)
		children[i] = child
		_, _, err := ledger.Start(root, "delegate-"+child, "do work", AgentToolName, child, false, false, nil, func() {})
		require.NoError(t, err)
		ledger.acknowledged(root, "delegate-"+child)
		_, _, err = ledger.Start(child, "bash-call", "sleep", "bash", "", false, false, nil, func() {})
		require.NoError(t, err)
		ledger.acknowledged(child, "bash-call")
	}

	coord.Cancel(root) // the real Stop

	for _, child := range children {
		row, err := store.Get(ctx, child, "bash-call")
		require.NoError(t, err)
		require.NotEqual(t, "running", row.State, "Stop must have stopped %s's job", child)
	}

	// The process dies after the Stop; another process restarts and sweeps.
	require.NoError(t, store.SimulateCrashForTest())
	restart := session.NewAsyncJobStore(conn, dataDir, os.Getpid()+1, "restart")
	t.Cleanup(func() { _ = restart.Close(context.Background()) })

	outcomes, err := restart.SweepDeadHosts(ctx, nil)
	require.NoError(t, err)
	total := 0
	for _, o := range outcomes {
		total += o.Interrupted
	}
	require.Zero(t, total, "a restart after Stop must add no extra facts")

	for _, child := range children {
		row, err := restart.Get(ctx, child, "bash-call")
		require.NoError(t, err)
		require.NotEqual(t, "interrupted", row.State, "the sweep must not re-label a job Stop already ended")
		require.EqualValues(t, 0, row.Wake)
		debt, err := restart.ReactionDebtExists(ctx, child)
		require.NoError(t, err)
		require.False(t, debt, "a stopped tree owes no reaction")
	}
	rootDebt, err := restart.ReactionDebtExists(ctx, root)
	require.NoError(t, err)
	require.False(t, rootDebt)
}
