package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func batchProbeTestReserve(t *testing.T, s *BatchStore, ctx context.Context, host, runner, hash string, task db.ReserveBatchNodeParams) string {
	t.Helper()
	p := batchStoreTestReservation()
	p.Host, p.RunnerID, p.InputHash = host, runner, hash
	p.Root.Children = []BatchReservationNode{{Values: task}}
	r, err := s.ReserveIdentity(ctx, p)
	require.NoError(t, err)
	return r.Snapshot.Root.RootID
}

func batchProbeTestTask(payload string) db.ReserveBatchNodeParams {
	return db.ReserveBatchNodeParams{Kind: "task", TaskKind: batchStoreTestString("shell"), TaskPayload: batchStoreTestString(payload), State: "pending", ControlState: "none"}
}

func TestBatchProbeLiveAttachesMatchingLiveRoot(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	root := batchProbeTestReserve(t, s, ctx, "host", "runner-1", "hash-1", batchProbeTestTask("1"))
	got, attached, err := s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call", InputHash: "hash-1"})
	require.NoError(t, err)
	require.True(t, attached)
	require.Equal(t, root, got)
}

func TestBatchProbeLiveRefusesMismatchesAsErrors(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	batchProbeTestReserve(t, s, ctx, "host", "runner-1", "hash-1", batchProbeTestTask("1"))
	for name, probe := range map[string]BatchLiveProbe{
		"hash mismatch":   {Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call", InputHash: "other"},
		"foreign host":    {Owner: "owner", Host: "elsewhere", RunnerID: "runner-1", ToolCallID: "call", InputHash: "hash-1"},
		"runner mismatch": {Owner: "owner", Host: "host", RunnerID: "runner-2", ToolCallID: "call", InputHash: "hash-1"},
	} {
		_, attached, err := s.BatchProbeLive(ctx, probe)
		require.Error(t, err, name)
		require.False(t, attached, name)
	}
	_, _, err := s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call", InputHash: "other"})
	require.ErrorIs(t, err, ErrBatchInputMismatch)
	_, _, err = s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "elsewhere", RunnerID: "runner-1", ToolCallID: "call", InputHash: "hash-1"})
	require.ErrorIs(t, err, ErrBatchForeignLiveHost)
	_, _, err = s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner-2", ToolCallID: "call", InputHash: "hash-1"})
	require.ErrorIs(t, err, ErrBatchRunnerMismatch)
}

func TestBatchProbeLiveNoLiveRootIsNotAttached(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	root, attached, err := s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call", InputHash: "hash-1"})
	require.NoError(t, err)
	require.False(t, attached)
	require.Empty(t, root)
}

// The ingress probe hash is the canonical hash of the one-item TASK spec,
// not the commit hook's stored batch-spec hash; the probe must recompute it
// from the reserved task kind and payload.
func TestBatchProbeLiveRecomputesTaskSpecHash(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	batchProbeTestReserve(t, s, ctx, "host", "runner-1", "commit-hook-hash", batchProbeTestTask("123"))
	spec := jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: "shell", Payload: json.RawMessage("123")}}
	_, attached, err := s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call", InputHash: batchCommitSpecHash(spec)})
	require.NoError(t, err)
	require.True(t, attached)
	// The same probe against a task with a different payload is a mismatch
	// even though the stored InputHash column is unrelated.
	other := jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: "shell", Payload: json.RawMessage("456")}}
	_, _, err = s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call", InputHash: batchCommitSpecHash(other)})
	require.ErrorIs(t, err, ErrBatchInputMismatch)
}
