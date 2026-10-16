// Rerun flag gating (R12-F cutover acceptance 3): with RUSH_BATCH_RUNTIME on,
// the committed truncation is the R10c node-row reconciliation and the
// post-commit stop goes to the exact-incarnation agent side
// (agent.BatchRerunStopper); with the flag OFF the handler keeps its legacy
// VoidedAsyncJob contract byte-for-byte. Real SQLite throughout (the rerun
// handler fixture is the p630 real-App harness; batch rows are real
// batch_nodes rows reserved through session.BatchStore).
package server

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// dualRerunCoordinator records BOTH stop contracts so a test can assert the
// flag picked exactly one: the flag-off StopRerunJobs (VoidedAsyncJob) and
// the flag-on BatchRerunStopVoided (exact incarnations). Embedding the full
// mailboxLikeCoordinator fake keeps the agent.Coordinator surface intact;
// BatchRerunStopper is additive, so no other fake changes.
type dualRerunCoordinator struct {
	mailboxLikeCoordinator

	mu              sync.Mutex
	batchStopCalls  int
	batchStopVoided []session.BatchRerunVoidedNode
}

func (m *dualRerunCoordinator) BatchRerunStopVoided(_ context.Context, _ string, voided []session.BatchRerunVoidedNode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchStopCalls++
	m.batchStopVoided = append(m.batchStopVoided, voided...)
}

func (m *dualRerunCoordinator) batchSnapshot() (int, []session.BatchRerunVoidedNode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.batchStopCalls, m.batchStopVoided
}

// seedBatchIncarnation reserves a REAL live batch_nodes root under the
// session for the tail's tool call id (never acked: the reconcile's legacy
// live arm matches it by tool_call_id when the tail is deleted).
func seedBatchIncarnation(t *testing.T, f *rerunHandlerFx) session.BatchSnapshot {
	t.Helper()
	store := session.NewBatchStore(f.a.DB())
	r, err := store.Reserve(context.Background(), session.BatchReservation{
		Owner: f.sessionID, Host: "batch-host", RunnerID: "runner-1",
		ToolCallID: rerunCallID, InputHash: "hash-" + rerunCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}},
	})
	require.NoError(t, err)
	return r.Snapshot
}

// TestRerun_FlagOn_StopsExactBatchIncarnations: flag ON routes the committed
// reconciliation's Voided set to the exact-incarnation agent side.
//
// Revert-check: wire the flag-on branch back to StopRerunJobs (or drop the
// BatchRerunReconcile truncation) and this fails — the coordinator receives
// legacy VoidedAsyncJob entries, or none, instead of the exact root_id set.
func TestRerun_FlagOn_StopsExactBatchIncarnations(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	f := newRerunHandlerFx(t, "rerun flag on")
	snap := seedBatchIncarnation(t, f)

	// SCHEMA TRACE (first real assertion): the incarnation is a REAL
	// batch_nodes root row — kind 'batch', node_id = root_id, tool_call_id
	// NOT NULL — read back through session.BatchStore.Read over the real
	// migrations behind the App's DB.
	root, err := session.NewBatchStore(f.a.DB()).Read(context.Background(), f.sessionID, snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, "batch", root.Root.Kind)
	require.Equal(t, snap.Root.RootID, root.Root.NodeID)
	require.Equal(t, rerunCallID, root.Root.ToolCallID.String)

	coord := &dualRerunCoordinator{}
	f.a.AgentCoordinator = coord
	f.run(t)

	require.Eventually(t, func() bool {
		coord.mu.Lock()
		defer coord.mu.Unlock()
		return coord.batchStopCalls == 1 && coord.stopRerunCalls == 0
	}, 5*time.Second, 10*time.Millisecond, "the flag-on stop must run exactly once")
	coord.mu.Lock()
	voided := coord.batchStopVoided
	stopCalls := coord.stopRerunCalls
	coord.mu.Unlock()
	require.Len(t, voided, 1)
	require.Equal(t, snap.Root.RootID, voided[0].RootID, "the stop set carries the EXACT committed incarnation")
	require.Equal(t, snap.Root.RootID, voided[0].NodeID)
	require.Equal(t, rerunCallID, voided[0].ToolCallID)
	require.Equal(t, "pending", voided[0].State)
	require.Zero(t, stopCalls, "the flag-off VoidedAsyncJob path must not run")
}

// TestRerun_FlagOff_KeepsVoidedAsyncJobPath: flag OFF keeps the legacy
// contract — StopRerunJobs with the committed VoidedAsyncJob set; the batch
// stopper is never consulted.
//
// Revert-check: let the flag-on branch leak into the flag-off one (or drop
// the flag check) and the legacy stop disappears / the batch stopper runs on
// a flag-off coordinator.
func TestRerun_FlagOff_KeepsVoidedAsyncJobPath(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	f := newRerunHandlerFx(t, "rerun flag off")
	seedBatchIncarnation(t, f)

	coord := &dualRerunCoordinator{}
	f.a.AgentCoordinator = coord
	f.run(t)

	require.Eventually(t, func() bool {
		coord.mu.Lock()
		defer coord.mu.Unlock()
		return coord.stopRerunCalls == 1
	}, 5*time.Second, 10*time.Millisecond, "the legacy StopRerunJobs must run exactly once")
	coord.mu.Lock()
	stopVoided := coord.stopRerunVoided
	batchCalls, batchVoided := coord.batchStopCalls, coord.batchStopVoided
	coord.mu.Unlock()
	require.Len(t, stopVoided, 1)
	require.Equal(t, rerunCallID, stopVoided[0].ToolCallID, "the legacy VoidedAsyncJob set names the tail's call id")
	require.Zero(t, batchCalls, "the flag-on batch stop must not run")
	require.Empty(t, batchVoided)
}
