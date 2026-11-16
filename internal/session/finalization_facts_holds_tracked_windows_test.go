//go:build windows

package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/kit/sys/procinfo"
	"github.com/PHPCraftdream/rush/internal/kit/sys/proctrack"
	"github.com/stretchr/testify/require"
)

// spawnLiveVictim starts a live, untracked process that plays the role
// of whatever unrelated process the OS later assigned a pid that a
// leaked tracked-job entry still keys. Returns the pid and registers
// the cleanup kill.
func spawnLiveVictim(t *testing.T) int {
	t.Helper()
	cmd := platform.Command(context.Background(), "ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn victim process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	require.True(t, procinfo.IsProcessAlive(cmd.Process.Pid), "victim must be alive after Start")
	return cmd.Process.Pid
}

// TestFinalizationTrackedProcessHoldCountTracksWindowsJobs: the exported
// counter is a live snapshot of the proctrack job registry -- tracking one
// live process raises it by exactly one and untracking returns it to baseline.
//
// Revert-check: a counter that snapshots a copy at init time (or reads the
// map without the mutex) misses entries tracked after init, so a live
// tracked tree would not defer any owner's seal.
func TestFinalizationTrackedProcessHoldCountTracksWindowsJobs(t *testing.T) {
	pid := spawnLiveVictim(t)
	baseline := CountFinalizationTrackedProcessHolds()

	require.NoError(t, proctrack.TrackProcessTree(pid), "TrackProcessTree")
	t.Cleanup(func() { proctrack.UntrackProcessTree(pid) })
	require.Equal(t, baseline+1, CountFinalizationTrackedProcessHolds(),
		"one tracked entry must raise the process hold count by one")

	proctrack.UntrackProcessTree(pid)
	require.Equal(t, baseline, CountFinalizationTrackedProcessHolds(),
		"physical teardown returns the count to baseline")
}
