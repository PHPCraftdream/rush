//go:build !windows

package session

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/kit/sys/proctrack"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/stretchr/testify/require"
)

// spawnGroupChild starts a long-lived child, optionally as its own process
// group leader, and returns it with a reaper already waiting.
func spawnGroupChild(t *testing.T, ownGroup bool) (*exec.Cmd, <-chan error) {
	t.Helper()
	// Deliberately not a round duration: other packages' tests run
	// concurrently under the pre-push hook and also sleep, and a cleanup
	// that matches on command line must not catch theirs.
	// CommandContext, not Command: the linter forbids the context-less form,
	// and the test context is the right one — if the test ends early the
	// child dies with it instead of outliving the run for 43 seconds.
	cmd := platform.Command(t.Context(), "sleep", "43.17")
	if ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	require.NoError(t, cmd.Start())

	waited := make(chan error, 1)
	// reaped is closed after Wait returns, whether or not anyone read the
	// result. Cleanup waits on THAT, not on `waited`: a test whose body
	// already drained `waited` would otherwise find the channel empty and
	// sit out the full timeout every time — measured at 2.08s for
	// TestKillAllTrackedTrees_KillsTheGroupAndForgetsIt against 0.08-0.16s
	// for its siblings, all of it spent waiting for a process that had
	// already been reaped.
	reaped := make(chan struct{})
	go func() {
		waited <- cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-reaped:
		case <-time.After(2 * time.Second):
		}
	})
	// Let the exec complete so Getpgid sees the final group.
	time.Sleep(80 * time.Millisecond)
	return cmd, waited
}

// TestFinalizationTrackedProcessHoldCountTracksUnixGroups: the exported
// counter is a live snapshot of the proctrack group registry -- registering
// one spawned group raises it by exactly one and untracking returns it to
// baseline.
//
// Revert-check: a counter that snapshots a copy at init time (or reads the
// map without the mutex) misses entries registered after init, so a live
// tracked group would not defer any owner's seal.
func TestFinalizationTrackedProcessHoldCountTracksUnixGroups(t *testing.T) {
	leader, _ := spawnGroupChild(t, true)
	pid := leader.Process.Pid
	t.Cleanup(func() { proctrack.UntrackProcessTree(pid) })

	baseline := CountFinalizationTrackedProcessHolds()
	require.NoError(t, proctrack.RegisterSpawnedGroup(pid))
	require.Equal(t, baseline+1, CountFinalizationTrackedProcessHolds(),
		"one registered group must raise the process hold count by one")

	proctrack.UntrackProcessTree(pid)
	require.Equal(t, baseline, CountFinalizationTrackedProcessHolds(),
		"physical teardown returns the count to baseline")
}
