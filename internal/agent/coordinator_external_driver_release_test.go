// C4 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): a
// NON-persistent coordinator (`rush run`) must keep its external-driver
// marker for the entire life of the process instead of releasing it before
// App.Shutdown()/CancelAll actually runs -- see ReleaseExternalDriver's own
// doc for the full race this closes.
package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReleaseExternalDriver_NonPersistentCoordinator_NeverReleases pins the
// fix directly.
//
// Revert-check performed: removed the `if !c.persistentMode.Load() {
// return }` guard from ReleaseExternalDriver (coordinator_reaction_source.go)
// -- this test's `require.True(t, ledger.isExternalDriver(...))` (post-
// release) FAILED (isExternalDriver was false). Restored the guard; re-ran,
// passed.
func TestReleaseExternalDriver_NonPersistentCoordinator_NeverReleases(t *testing.T) {
	coord := &coordinator{}
	ledger := newWorkLedger(nil)
	ledger.coord = coord
	coord.asyncJobs = ledger
	// persistentMode left at its zero value (false) -- matches `rush run`,
	// which never calls SetPersistentMode(true).

	const sessID = "cli-root-session"
	require.NoError(t, coord.ClaimExternalDriver(context.Background(), sessID))
	require.True(t, ledger.isExternalDriver(sessID))

	coord.ReleaseExternalDriver(context.Background(), sessID)
	require.True(t, ledger.isExternalDriver(sessID),
		"a non-persistent (CLI) coordinator must never release its external-driver marker")
}

// TestReleaseExternalDriver_PersistentCoordinator_StillReleases is the
// regression guard: the web server's coordinator (persistentMode true) must
// keep releasing normally.
func TestReleaseExternalDriver_PersistentCoordinator_StillReleases(t *testing.T) {
	coord := &coordinator{}
	ledger := newWorkLedger(nil)
	ledger.coord = coord
	coord.asyncJobs = ledger
	coord.SetPersistentMode(true)
	t.Cleanup(coord.StopRecheckTicker)

	const sessID = "web-tab-session"
	require.NoError(t, coord.ClaimExternalDriver(context.Background(), sessID))
	require.True(t, ledger.isExternalDriver(sessID))

	coord.ReleaseExternalDriver(context.Background(), sessID)
	require.False(t, ledger.isExternalDriver(sessID),
		"a persistent (web) coordinator must still release its external-driver marker normally")
}
