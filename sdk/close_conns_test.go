package sdk

// Close-vs-ephemeral-handle lifecycle tests (review round-1 finding
// R1-3): Client.Close must honour app.ShutdownWithResult's
// forced-shutdown policy for the in-memory handles of library-mode
// ephemeral clients too. app itself never releases its DB on a forced
// shutdown (live writers may still hold it); the old Close closed
// closeConns unconditionally, which pulled an ephemeral in-memory
// database out from under those writers -- a policy the file-backed
// path respected but the ephemeral path broke.
//
// These tests live inside package sdk (not sdk_test) because they build
// bare Clients around real openMemoryDB handle pairs plus spy
// coordinators -- the same shape the sdk_test close tests use one
// package up, but with access to the unexported pieces. The busy spy is
// the controlled stand-in for a live writer: exactly like the package's
// other close tests, the spy decides CancelAll's verdict, and every
// wait below is bounded (PingContext on a healthy single-connection
// pool returns immediately; no unbounded blocking anywhere).

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/stretchr/testify/require"
)

// busySpyCoordinator reports busy on every CancelAll, so
// ShutdownWithResult takes the forced path (DB deliberately not
// released). The embedded nil Coordinator is never reached: shutdown
// touches no other Coordinator method.
type busySpyCoordinator struct {
	agent.Coordinator
	cancelAllCalls atomic.Int32
}

func (c *busySpyCoordinator) CancelAll() bool {
	c.cancelAllCalls.Add(1)
	return true
}

// idleSpyCoordinator reports graceful (not busy) on every CancelAll.
type idleSpyCoordinator struct {
	agent.Coordinator
}

func (c *idleSpyCoordinator) CancelAll() bool {
	return false
}

// newEphemeralTestClient opens a real in-memory database pair (keeper +
// main, migrations included -- exactly what openLibrary hands a real
// ephemeral client) and wraps it in a bare Client driven by coord.
func newEphemeralTestClient(t *testing.T, coord agent.Coordinator) (*Client, *sql.DB) {
	t.Helper()
	closeConns, conn, err := openMemoryDB(context.Background())
	require.NoError(t, err)
	require.Len(t, closeConns, 2)
	return &Client{app: &app.App{AgentCoordinator: coord}, closeConns: closeConns}, conn
}

// TestClientCloseForcedKeepsEphemeralConnsUntilReclaim pins the forced
// contract: a forced Close leaves the in-memory handles usable (the
// database survives under live writers), stays idempotent without
// flipping to a close, and only the explicit CloseEphemeralConnsForced
// actually releases the handles.
func TestClientCloseForcedKeepsEphemeralConnsUntilReclaim(t *testing.T) {
	ctx := context.Background()
	coord := &busySpyCoordinator{}
	client, conn := newEphemeralTestClient(t, coord)

	res := client.Close()
	require.True(t, res.Forced, "the spy coordinator stages a forced shutdown")
	require.Equal(t, int32(1), coord.cancelAllCalls.Load())

	// Forced shutdown must NOT have closed the in-memory handles: the
	// database stays usable for still-live writers.
	require.NoError(t, conn.PingContext(ctx), "forced Close must leave the in-memory handles open")

	// A repeat Close stays idempotent and must not close the handles
	// after the fact either.
	require.Equal(t, res, client.Close())
	require.NoError(t, conn.PingContext(ctx))

	// Explicit reclaim: the host releases the handles once it knows
	// the writers are gone. database/sql reports any later use of a
	// closed DB as "sql: database is closed".
	require.NoError(t, client.CloseEphemeralConnsForced())
	require.Error(t, conn.PingContext(ctx), "the explicit reclaim must actually close the handles")

	// The reclaim is idempotent.
	require.NoError(t, client.CloseEphemeralConnsForced())
}

// TestClientCloseGracefulClosesEphemeralConnsAndReclaimIsNoop pins the
// graceful contract: Close itself closes the in-memory handles, and the
// explicit reclaim afterwards is a harmless no-op.
func TestClientCloseGracefulClosesEphemeralConnsAndReclaimIsNoop(t *testing.T) {
	ctx := context.Background()
	client, conn := newEphemeralTestClient(t, &idleSpyCoordinator{})

	res := client.Close()
	require.False(t, res.Forced)
	require.Empty(t, res.CleanupErrors)
	require.Error(t, conn.PingContext(ctx), "graceful Close must close the in-memory handles itself")

	// After a graceful Close the explicit reclaim is a no-op.
	require.NoError(t, client.CloseEphemeralConnsForced())
}

// TestCloseEphemeralConnsForcedWithoutInMemoryHandles pins that the
// forced reclaim is a no-op for clients without in-memory handles
// (application mode, Wrap).
func TestCloseEphemeralConnsForcedWithoutInMemoryHandles(t *testing.T) {
	client := &Client{app: &app.App{AgentCoordinator: &idleSpyCoordinator{}}}
	require.NoError(t, client.CloseEphemeralConnsForced())
}
