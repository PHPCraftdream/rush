package session

// R5A-2: purgeEmptyDeadHostFiles decides with the shared probe, deletes the
// host row holding no lock, and takes the exclusive lock only for the unlink
// (like RecoverDeadHost, R3A-3). Installs a package-global seam: not parallel.

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPurgeEmptyDeadHostFiles_MidPurge_HostStillProbesDead parks the reaper
// between its dead verdict and the row delete. The host must read dead to every
// other process (a reader, the foreign-driver check, a claim of a session whose
// marker names it); afterwards the row and the file are gone.
//
// Revert-check: replace the shared probe with ProbeHost and keep its lock until
// the loop iteration ends (the pre-fix behaviour) -> HostLiveness reads alive
// and ClaimSessionDriver is refused with *ErrSessionDrivenElsewhere.
func TestPurgeEmptyDeadHostFiles_MidPurge_HostStillProbesDead(t *testing.T) {
	stores, q, ctx := nStores(t, 2, "s1")
	reaper, other := stores[0], stores[1]
	_, err := other.ensureHost(ctx) // its registration sweep must not reap the fixture host
	require.NoError(t, err)

	const dead = "dead-host-r5a2"
	fabricateDeadHost(t, ctx, q, reaper.dataDir, dead)
	claimMarker(t, ctx, q, "s1", dead)

	paused, gate := make(chan struct{}), make(chan struct{})
	purgeEmptyDeadHostBeforeDeleteSeam = func(host string) {
		if host != dead {
			return
		}
		close(paused)
		<-gate
	}
	t.Cleanup(func() { purgeEmptyDeadHostBeforeDeleteSeam = nil })

	done := make(chan error, 1)
	go func() { done <- reaper.purgeEmptyDeadHostFiles(ctx) }()
	<-paused

	require.Equal(t, HostStatusDead, other.HostLiveness(dead), "a host being reaped is still dead")
	_, foreign, err := other.ForeignLiveDriver(ctx, "s1")
	require.NoError(t, err)
	require.False(t, foreign, "a crashed driver's marker is not foreign while its host is being reaped")
	require.NoError(t, other.ClaimSessionDriver(ctx, "s1"), "the crashed driver's session is claimable mid-reap")

	close(gate)
	require.NoError(t, <-done)
	_, err = q.GetAsyncHost(ctx, dead)
	require.ErrorIs(t, err, sql.ErrNoRows, "the empty dead host's row is reaped")
	_, statErr := os.Stat(HostLockPath(reaper.dataDir, dead))
	require.True(t, os.IsNotExist(statErr), "and its lock file is unlinked")
}
