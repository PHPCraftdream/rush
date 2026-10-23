// Own-scope recovery (doc sec.3.5/3.7, step 5): CLIScope recovers its
// owner's dead-host rows to 'interrupted' before answering, so a row whose
// host just died is treated as recoverable, never as open scope forever.
//
// The deleted newTestAsyncJobStoreWithDataDir helper and this file's OFF
// twins (the async_jobs ClaimAsyncJob/MarkAsyncJobAnnounced seeds and their
// store.Get readbacks) were obsolete contracts: the batch rows are the only
// live-work authority, so both fixtures seed and read through batch_nodes
// (b1SeedBatchRootOnHost / batchScopeRootState) over a one-migration store.
package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestCLIScope_RecoversDeadHostRowBeforeAnswering pins the CLI-loop wiring
// (app_run_async.go's nextStep calls CLIScope between turns): a running
// batch root on a host that has since died must be recovered to 'interrupted'
// as part of answering the scope question, not merely skipped by HostNotDead
// and left 'running' forever.
func TestCLIScope_RecoversDeadHostRowBeforeAnswering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, dataDir := batchFixtureConn(t)

	// Fabricate the dead host: a lock file that exists but is not held --
	// exactly what a crashed process leaves behind.
	seed, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, "dead-host-x"))
	require.NoError(t, err)
	require.NoError(t, seed.Release())

	batchRoot, batchChild := b1SeedBatchRootOnHost(t, conn, "sess-1", "batch-call-1", "dead-host-x", "running")

	c := newBatchRuntimeCoordinator()
	// Standalone coordinator: an explicit node-facts binding is required, or
	// a CLIScope answers through unknownBatchCutoverFacts ("batch facts:
	// node source unavailable").
	bindBatchNodeFacts(t, c, conn, dataDir)

	st, err := c.CLIScope(ctx, "sess-1")
	require.NoError(t, err)
	require.False(t, st.WorkOpen, "an interrupted root (wake=0) with nothing else outstanding must close the scope")

	state, outcome := batchScopeRootState(t, conn, batchRoot)
	require.Equal(t, "interrupted", state)
	require.Equal(t, "host_lost", outcome)
	count, wake := batchScopeSelectedNotices(t, conn, batchRoot)
	require.Equal(t, 1, count)
	require.Zero(t, wake)
	var childState, childOutcome string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT state, outcome_code FROM batch_nodes WHERE node_id = ?`, batchChild).Scan(&childState, &childOutcome))
	require.Equal(t, "interrupted", childState)
	require.Equal(t, "host_lost", childOutcome)
}

// TestCLIScope_LiveHostRunningRowKeepsScopeOpen is the negative twin: a
// running root on a LIVE host must still report scope open, unrecovered.
func TestCLIScope_LiveHostRunningRowKeepsScopeOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, dataDir := batchFixtureConn(t)

	// A live host holds its lock for the whole test.
	holder, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, "live-host-x"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })

	batchRoot, _ := b1SeedBatchRootOnHost(t, conn, "sess-1", "batch-call-1", "live-host-x", "running")

	c := newBatchRuntimeCoordinator()
	// Explicit facts binding for the standalone coordinator.
	bindBatchNodeFacts(t, c, conn, dataDir)

	st, err := c.CLIScope(ctx, "sess-1")
	require.NoError(t, err)
	require.True(t, st.WorkOpen, "a running root on a live host must keep the scope open")

	state, outcome := batchScopeRootState(t, conn, batchRoot)
	require.Equal(t, "running", state, "a live host's root must never be recovered")
	require.Empty(t, outcome, "a live host's root must never be recovered")
}
