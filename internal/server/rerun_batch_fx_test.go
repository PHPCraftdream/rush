package server

// Batch seeding fake for the rerun cancel-window/hold tests (task #630 /
// R2C-4): the handler's committed truncation is session.BatchRerunReconcile
// over batch_nodes rows, and its post-commit stop goes to the optional
// agent.BatchRerunStopper on a detached goroutine; batchStopCoordinator
// records the stop.

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// rerunBatchFxHost is the host id seedRerunBatchRoot reserves its root
// incarnation under; it comes back verbatim in the reconcile's Voided set.
const rerunBatchFxHost = "rerun-handler-fx-host"

// seedRerunBatchRoot reserves one live batch root incarnation for the
// fixture's owner/tool call (root kind=batch, mode parallel, state running,
// ControlState none, never acked) and records its id on the fixture so
// jobDelivery reads that row's delivery column. The fixture seeds exactly
// one live batch root incarnation, standing in for the rerunCallID job;
// tests that assert exact voided sets read it from f.batchSnap.
func seedRerunBatchRoot(t *testing.T, f *rerunHandlerFx) session.BatchSnapshot {
	t.Helper()
	res, err := session.NewBatchStore(f.a.DB()).Reserve(context.Background(), session.BatchReservation{
		Owner: f.sessionID, Host: rerunBatchFxHost, RunnerID: "rerun-handler-test",
		ToolCallID: rerunCallID, InputHash: "hash-" + rerunCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "running", ControlState: "none",
		}},
	})
	require.NoError(t, err)
	f.batchRootID = res.Snapshot.Root.RootID
	return res.Snapshot
}

// batchStopCoordinator wraps cancellableHoldCoordinator with the optional
// agent.BatchRerunStopper surface: it records every detached
// BatchRerunStopVoided call plus the exact voided incarnations, and closes
// joined when the call returns so a test can join the handler's detached
// goroutine through a channel instead of a sleep.
type batchStopCoordinator struct {
	cancellableHoldCoordinator

	mu     sync.Mutex
	calls  int
	voided []session.BatchRerunVoidedNode
	joined chan struct{}
}

// BatchRerunStopVoided implements agent.BatchRerunStopper.
func (m *batchStopCoordinator) BatchRerunStopVoided(_ context.Context, _ string, voided []session.BatchRerunVoidedNode) {
	m.mu.Lock()
	m.calls++
	m.voided = append(m.voided, voided...)
	m.mu.Unlock()
	if m.joined != nil {
		close(m.joined)
	}
}

// stopVoided returns the recorded call count and a copy of the voided set.
func (m *batchStopCoordinator) stopVoided() (int, []session.BatchRerunVoidedNode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, append([]session.BatchRerunVoidedNode(nil), m.voided...)
}
