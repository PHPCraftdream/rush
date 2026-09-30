// R2C-10: a forced shutdown pins the phase-4 host lock for the process's life
// (A12). That is the documented contract (ShutdownResult.Forced, Shutdown,
// AsyncJobStore.CloseKeepLock): a library caller must exit its process. This
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
	store := h.app.asyncJobStore
	_, err := store.Claim(ctx, session.ClaimParams{
		Owner: h.sessionID, ToolCallID: "stuck-1", Kind: session.JobKindCommand, Input: "sleep", ToolName: "bash",
	})
	require.NoError(t, err)
	hostID := store.HostID()
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

	// A new App/store in the same process cannot recover the row: the host is
	// still alive as far as it can tell.
	second := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 4242, "second-app")
	require.Equal(t, session.HostStatusAlive, second.HostLiveness(hostID))
	second.RecoverOwnerScope(ctx, h.sessionID, nil)
	row, err := second.Get(ctx, h.sessionID, "stuck-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "rows of a forced-shutdown host stay running until the process exits")
}
