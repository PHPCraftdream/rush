//go:build !windows

package cliprovider

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestTrackChildTree_RegistersGroupWithReapedLeaderAndLivingGrandchild is
// the regression test for the 2026-10-16 review's P3-5: trackChildTree
// runs right after Start, and a CLI leader that exits immediately can
// already be reaped by then -- the old isProcessGroupLeader gate
// (Getpgid(pid) == pid) reported ESRCH and skipped the durable
// RegisterChildGroup entirely, so a "sessions kill" in another process
// lost the only pointer to a group whose grandchildren still lived.
func TestTrackChildTree_RegistersGroupWithReapedLeaderAndLivingGrandchild(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	dataDir := t.TempDir()
	sessionID := "cliprovider-trackchildtree-reaped-leader"

	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()

	generation := session.ReadLockGeneration(session.SessionLockPath(dataDir, sessionID))
	require.NotEmpty(t, generation)

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	// The leader spawns a grandchild into its own (already new) process
	// group and exits. 43.31 is deliberately non-round and unique in
	// this package, like grandchildSleepDuration's 37.91: cleanup that
	// matches on command line must not catch other tests' concurrent
	// sleeps.
	script := `sleep 43.31 & p=$!; echo "$p" > '` + pidFile + `'; exit 0`
	cmd := platform.Command(t.Context(), "bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// writer is an *os.File, so exec spawns no copy goroutines and
	// cmd.Wait returns at leader exit even while the grandchild still
	// holds the inherited write end.
	cmd.Stdout = writer
	require.NoError(t, cmd.Start())
	// P2-1 (2026-10-19 review): cleanup armed IMMEDIATELY after the
	// successful Start, BEFORE the fallible writer.Close() assertion, so a
	// failure there cannot leak the started group or the pipe ends.
	grandpid := 0
	waited := false
	// F3 (2026-10-18 review, finding 3): registered UNCONDITIONALLY, before
	// anything that can fail. The previous cleanup killed the sleeper only
	// under `if grandpid > 0`, so a PID-publication failure (grandpid still
	// 0) leaked the grandchild for its full sleep.
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		session.UntrackProcessTree(cmd.Process.Pid)
		session.UnregisterChildGroup(dataDir, sessionID, cmd.Process.Pid)
		_ = writer.Close()
		_ = reader.Close()
	})
	// The child now owns the only write end: keeping the parent's copy open
	// would make the EOF death oracle below unreachable.
	require.NoError(t, writer.Close())

	deadline := time.Now().Add(10 * time.Second)
	for {
		data, readErr := os.ReadFile(pidFile)
		// An unparseable or not-yet-positive read (bash has open(O_TRUNC)-ed
		// the file but its write has not landed yet) is "not ready", not an
		// error: keep polling until the deadline, like
		// kill_grandchild_test.go's pidfile poll.
		if readErr == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				grandpid = pid
				break
			}
		}
		if time.Now().After(deadline) {
			// Errorf+return, not Fatalf: the unconditional F3 cleanup still
			// runs either way, but this shape keeps the failure attributable
			// and matches the F3 sibling tests.
			t.Errorf("grandchild pid file never became readable: %v", readErr)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NotZero(t, grandpid)

	// Reap the leader -- the exact P3-5 precondition, not merely a
	// zombie.
	require.NoError(t, cmd.Wait())
	waited = true

	// Premise guards: the leader is fully reaped, yet the grandchild
	// keeps the group alive.
	_, pgErr := syscall.Getpgid(cmd.Process.Pid)
	require.Error(t, pgErr, "premise: the leader must be reaped before trackChildTree runs, or this test does not exercise P3-5")
	require.NoError(t, syscall.Kill(-cmd.Process.Pid, 0), "premise: the grandchild must keep the group alive after the leader is reaped")

	childPid := trackChildTree(cmd.Process, dataDir, sessionID)
	require.Equal(t, cmd.Process.Pid, childPid)

	// Drive the REAL cross-path consumer: the same sweep "rush sessions
	// kill" runs in another process.
	result := session.KillRegisteredChildGroups(dataDir, sessionID, generation)
	if result.Killed == 0 && result.Retained == 1 && runtime.GOOS != "linux" {
		// The leader-gone branch now requires a registration-time member
		// witness; on platforms with no member-enumeration facility the
		// entry is deliberately retained, not signalled. Skip rather than
		// fail there (the cliprovider package cannot inspect the recorded
		// witness list directly, so the outcome pair is the observable).
		t.Skip("no member witnesses available on this platform: leader-gone identity is unverifiable and the entry is retained by design")
	}
	require.Equal(t, 1, result.Killed, "trackChildTree must register the group durably even though its leader exited and was reaped before the call -- the pre-fix Getpgid gate skipped registration and the sweep found nothing")
	require.Zero(t, result.Implausible)

	// Death oracle via the inherited pipe, NOT pid liveness: a zombie
	// keeps kill(pid, 0) alive.
	require.NoError(t, reader.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 1)
	_, readErr := reader.Read(buf)
	require.ErrorIs(t, readErr, io.EOF, "the grandchild survived the sweep -- the group was never registered (P3-5) or never signalled")
}
