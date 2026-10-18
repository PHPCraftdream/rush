//go:build !windows

package cliprovider

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestRegisterSpawnedGroup_SweepsReapedLeaderGroup is the F1 regression
// test (2026-10-18 review, finding 1): a group this code spawned with
// Setpgid whose LEADER exits and is reaped while a grandchild lives must
// still be recorded by session.RegisterSpawnedGroup and swept by
// session.KillAllTrackedTrees. The old code path (session.TrackProcessTree)
// refuses a pid whose Getpgid fails -- a reaped leader -- and recorded
// nothing, so the last-resort in-process sweep could not reach the tree.
//
// Production mutations this catches:
//
//   - implementing RegisterSpawnedGroup as a delegate to TrackProcessTree
//     (or equivalently reverting procgroup_unix.go's trackChildTree to call
//     TrackProcessTree): the reaped leader is refused, KillAllTrackedTrees
//     returns 0, and the grandchild survives -- both assertions below fail.
//   - removing TrackProcessTree's non-leader rejection: the negative half
//     (grandpid refusal) would register the grandchild's pid and sweep a
//     group by a non-leader numeric id; here the count assertion fails.
func TestRegisterSpawnedGroup_SweepsReapedLeaderGroup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	// The leader spawns a grandchild into its own (already new) process
	// group and exits. 45.67 is deliberately non-round and unique in this
	// package, like the other tests' sleeps: cleanup that matches on
	// command line must not catch other tests' concurrent sleeps.
	script := `sleep 45.67 & p=$!; echo "$p" > '` + pidFile + `'; exit 0`
	cmd := platform.Command(t.Context(), "bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// writer is an *os.File, so exec spawns no copy goroutines and
	// cmd.Wait returns at leader exit even while the grandchild still
	// holds the inherited write end.
	cmd.Stdout = writer
	require.NoError(t, cmd.Start())
	require.NoError(t, writer.Close())

	// F3-style unconditional cleanup: the group kill must not depend on
	// the pidfile poll having succeeded.
	waited := false
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = writer.Close()
		_ = reader.Close()
	})

	deadline := time.Now().Add(10 * time.Second)
	grandpid := 0
	for {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				grandpid = pid
				break
			}
		}
		if time.Now().After(deadline) {
			// Errorf+return, not Fatalf, so the unconditional cleanup
			// still runs (F3 shape).
			t.Errorf("grandchild pid file never became readable: %v", readErr)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NotZero(t, grandpid)

	// Reap the leader -- the exact finding-1 precondition.
	require.NoError(t, cmd.Wait())
	waited = true
	_, pgErr := syscall.Getpgid(cmd.Process.Pid)
	require.Error(t, pgErr, "premise: the leader must be fully reaped (Getpgid fails)")
	require.NoError(t, syscall.Kill(-cmd.Process.Pid, 0), "premise: the grandchild must keep the group alive")

	// NEGATIVE half: TrackProcessTree's generic non-leader rejection must
	// be untouched. grandpid is a non-leader member of the spawned group
	// (its group leader is cmd's pid), so refusing it is provable here
	// without risking a kill(-pid) against rush's own group. If the
	// rejection were gone, the sweep below would signal a non-leader pid's
	// group and the count assertion would fail.
	require.NoError(t, session.TrackProcessTree(grandpid))
	require.Zero(t, session.KillAllTrackedTrees(),
		"TrackProcessTree must still refuse a non-leader pid: sweeping it would kill(-pid) an unrelated group")

	// POSITIVE half: the spawned leader, though reaped, IS registered by
	// the new spawn-identity entry point.
	require.NoError(t, session.RegisterSpawnedGroup(cmd.Process.Pid))
	n := session.KillAllTrackedTrees()
	require.Equal(t, 1, n,
		"KillAllTrackedTrees must sweep exactly the registered spawned group -- the pre-F1 TrackProcessTree path recorded nothing for a reaped leader")

	// Death oracle via the inherited pipe, NOT pid liveness: a zombie
	// keeps kill(pid, 0) alive.
	require.NoError(t, reader.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 1)
	_, readErr := reader.Read(buf)
	require.ErrorIs(t, readErr, io.EOF,
		"the grandchild survived KillAllTrackedTrees -- the reaped leader's group was not registered (finding 1)")
}
