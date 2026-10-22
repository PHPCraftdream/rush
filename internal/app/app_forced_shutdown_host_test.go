// R2C-10: a forced shutdown pins the phase-4 host lock for the process's life
// (A12). That is the documented contract (ShutdownResult.Forced, Shutdown,
// HostStore.CloseKeepLock): a library caller must exit its process. This
// pins it, so a change that starts releasing the lock without a provable
// "no writers remain" point turns it red and forces the docs to be revisited.
package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check: replacing CloseKeepLock with Close on the forced path in
// releaseResources releases the lock and unmarks the id -- the probe reads
// dead, the own-id assertion fails and the second store recovers the row.
func TestForcedShutdown_PinsHostLockAndRowsStayRunning(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "answer", 11, 3)
	})
	ctx := context.Background()
	// Batch fixture: a live, announced one-task root owned by the loop's
	// session under the App's own registered host identity (the f6 shape).
	root, _ := f6ReserveDelegation(t, h.app, h.sessionID, "stuck-1", "", "command", "running")
	hostID := h.app.BatchHostStore().HostID()
	require.NotEmpty(t, hostID)
	t.Cleanup(func() {
		session.ReleaseRetainedHostLocksForTest(hostID)
		_ = db.ReleaseAll(h.dataDir)
	})

	h.app.AgentCoordinator = &mockCoordinatorForShutdown{cancelAllFunc: func() bool { return true }}
	res := h.app.ShutdownWithResult()
	require.True(t, res.Forced)

	require.True(t, session.IsOwnHostID(hostID), "the own-host id stays marked after a forced shutdown")
	status, lock, probeErr := session.ProbeHostLock(session.HostLockPath(h.dataDir, hostID))
	if lock != nil {
		_ = lock.Release()
	}
	require.NoError(t, probeErr)
	require.Equal(t, session.HostStatusAlive, status, "the host lock stays held after a forced shutdown")

	// A new host identity in the same process cannot take the lock: the host
	// is still alive as far as it can tell.
	_, _, err := session.ProbeHost(h.dataDir, hostID)
	require.ErrorIs(t, err, session.ErrProbeOwnHost, "the forced-shutdown host id is still this process's own")

	// The live rows of a forced-shutdown host stay running until the process
	// exits: no recovery may interrupt them through the still-alive lock.
	var state string
	require.NoError(t, h.app.DB().QueryRowContext(ctx,
		`SELECT state FROM batch_nodes WHERE node_id = ?`, root).Scan(&state))
	require.Equal(t, "running", state, "rows of a forced-shutdown host stay running until the process exits")
}
