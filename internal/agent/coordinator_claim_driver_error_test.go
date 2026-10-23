// R3A-1 (docs/reviews/2026-09-30-async-phase4-round3.md): ClaimExternalDriver
// degrades to the in-memory marker ONLY for a data dir that cannot host an OS
// lock file. A database error registering the host fails the claim, so the
// `rush run` loop stops before it touches the session it would have driven.
//
// Batch world: the durable marker is the bound session.HostStore
// (BatchDriverClaimer, batch_cutover_driver.go); the in-memory marker is
// coord.driverHints.
package agent

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func claimDriverFixture(t *testing.T) (*coordinator, string, *sql.DB) {
	t.Helper()
	conn, dataDir := batchFixtureConn(t)
	coord := newBatchRuntimeCoordinator()
	coord.batchDriverClaimer = session.NewHostStore(conn, dataDir, os.Getpid(), "coord")
	t.Cleanup(coord.StopRecheckTicker)
	return coord, dataDir, conn
}

// TestClaimExternalDriver_HostInsertDBError_FailsClaim: a trigger aborts
// INSERT INTO async_hosts. ClaimExternalDriver must return the error and set
// nothing: no in-memory marker, no durable marker.
//
// Revert-check: wrap every ensureHost error as ErrDriverMarkerUnavailable in
// ClaimSessionDriver (pre-fix) -> ClaimExternalDriver logs a Warn, returns nil
// and sets the in-memory marker: require.Error fails.
func TestClaimExternalDriver_HostInsertDBError_FailsClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coord, _, conn := claimDriverFixture(t)
	_, err := conn.ExecContext(ctx, `CREATE TRIGGER abort_async_hosts_insert BEFORE INSERT ON async_hosts
BEGIN SELECT RAISE(ABORT, 'injected: async_hosts insert refused'); END`)
	require.NoError(t, err)

	err = coord.ClaimExternalDriver(ctx, "cli-root")
	require.Error(t, err)
	require.NotErrorIs(t, err, session.ErrDriverMarkerUnavailable)
	require.ErrorContains(t, err, "async_hosts insert refused")
	require.False(t, coord.driverHints.isExternalDriver("cli-root"), "a failed claim must not set the in-memory marker")
	var n int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_drivers`).Scan(&n))
	require.Zero(t, n)
}

// TestClaimExternalDriver_NoLockCapableDataDir_DegradesToInMemory: the one
// remaining degraded outcome -- the hosts dir cannot be created -- keeps the
// in-memory marker and returns nil.
func TestClaimExternalDriver_NoLockCapableDataDir_DegradesToInMemory(t *testing.T) {
	t.Parallel()
	coord, dataDir, _ := claimDriverFixture(t)
	require.NoError(t, os.WriteFile(session.HostsDir(dataDir), []byte("x"), 0o644))

	require.NoError(t, coord.ClaimExternalDriver(context.Background(), "cli-root"))
	require.True(t, coord.driverHints.isExternalDriver("cli-root"))
}
