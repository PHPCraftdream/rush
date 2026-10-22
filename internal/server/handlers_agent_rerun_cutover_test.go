// Final-world rerun cutover acceptance (chunk 2c): the committed truncation
// is the batch node-row reconciliation (session.BatchStore.BatchRerunReconcile
// via the batchRerunReconcile seam) and the post-commit stop goes to the
// exact-incarnation agent side (agent.BatchRerunStopper) on a detached
// goroutine. The legacy flag-off VoidedAsyncJob/StopRerunJobs contract is an
// obsolete contract: neither the legacy path nor its types remain in the
// rerun handler, so the flag gate and its test are deleted. Real SQLite
// throughout (the rerun handler fixture is the p630 real-App harness; batch
// rows are real batch_nodes rows reserved through session.BatchStore).
package server

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestRerun_StopsExactBatchIncarnations: the committed reconciliation's Voided
// set routes to the exact-incarnation agent side.
//
// Revert-check: wire the stop back to any legacy surface (or drop the
// BatchRerunReconcile truncation) and this fails — the coordinator receives
// no voided set, or one that is not the exact committed root incarnation.
func TestRerun_StopsExactBatchIncarnations(t *testing.T) {
	f := newRerunHandlerFx(t, "rerun-batch-cutover")
	snap := f.batchSnap

	// SCHEMA TRACE (first real assertion): the incarnation is a REAL
	// batch_nodes root row — kind 'batch', node_id = root_id, tool_call_id
	// NOT NULL — read back through session.BatchStore.Read over the real
	// migrations behind the App's DB.
	root, err := session.NewBatchStore(f.a.DB()).Read(context.Background(), f.sessionID, snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, "batch", root.Root.Kind)
	require.Equal(t, snap.Root.RootID, root.Root.NodeID)
	require.Equal(t, rerunCallID, root.Root.ToolCallID.String)

	coord := &batchStopCoordinator{joined: make(chan struct{})}
	f.a.AgentCoordinator = coord
	f.run(t)

	select {
	case <-coord.joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached BatchRerunStopVoided never ran")
	}
	calls, voided := coord.stopVoided()
	require.Equal(t, 1, calls, "the batch stop must run exactly once")
	require.Len(t, voided, 1)
	require.Equal(t, snap.Root.RootID, voided[0].RootID, "the stop set carries the EXACT committed incarnation")
	require.Equal(t, snap.Root.RootID, voided[0].NodeID)
	require.Equal(t, rerunCallID, voided[0].ToolCallID)
	require.Equal(t, "running", voided[0].State)
}
