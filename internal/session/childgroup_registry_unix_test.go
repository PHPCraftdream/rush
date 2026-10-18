//go:build !windows

package session

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
	"github.com/stretchr/testify/require"
)

// TestChildGroupRegistry_RegisterReadKill_RealLock is the end-to-end happy
// path against a REAL session lock (not a hand-rolled generation string):
// acquire, register a real process-group leader, sweep, and confirm both
// the kill and the registry cleanup.
func TestChildGroupRegistry_RegisterReadKill_RealLock(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-register-read-kill"

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()

	lockPath := SessionLockPath(dataDir, sessionID)
	generation := ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	leader, waited := spawnGroupChild(t, true)
	RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, generation)

	result := KillRegisteredChildGroups(dataDir, sessionID, generation)
	require.Equal(t, 1, result.Killed)
	require.False(t, result.GenerationMismatch)
	require.Zero(t, result.Implausible)

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the registered process group survived KillRegisteredChildGroups")
	}

	_, statErr := os.Stat(childGroupRegistryPath(dataDir, sessionID))
	require.True(t, os.IsNotExist(statErr))
}

// TestChildGroupRegistry_UnrelatedGeneration_KillsNothingAndIsRetained is
// the regression test for a registry entry that does not belong to the
// generation a caller is sweeping for. Renamed and rewritten (2026-08-19
// static-follow-up review, P0-2 fix) from the original
// TestChildGroupRegistry_StaleAfterPIDReuse_KillsNothing: that version
// called KillRegisteredChildGroups with the session own lock PATH, letting
// the function re-derive "current generation" internally -- exactly the
// defect this task fixes. The caller now supplies an explicit
// victimGeneration token (captured at the moment it proved contention with
// the process it is about to kill, see internal/cmd/sessions_kill.go), so
// this test must supply one too: a generation that is NOT the one recorded
// in the registry, modeling either a genuinely stale entry from a crashed
// instance (true OS-level pid/generation reuse cannot be constructed from
// user code) or simply a sweep invoked for a different victim than the one
// that registered this entry.
//
// The entry must be left completely untouched: not signalled, and --
// unlike the pre-fix behavior, which treated ANY generation mismatch as
// "confirmed permanently stale, safe to drop" -- also RETAINED in the
// registry rather than discarded, since a mismatch against THIS caller's
// target proves nothing about whether the entry might still be validly
// reachable by a future sweep for its own generation.
func TestChildGroupRegistry_UnrelatedGeneration_KillsNothingAndIsRetained(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-unrelated-generation"

	leader, waited := spawnGroupChild(t, true)
	registeredGeneration := "11111-1111111111"
	RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, registeredGeneration)

	unrelatedVictimGeneration := "99999-9999999999"
	require.NotEqual(t, registeredGeneration, unrelatedVictimGeneration)

	result := KillRegisteredChildGroups(dataDir, sessionID, unrelatedVictimGeneration)
	require.Zero(t, result.Killed)
	require.True(t, result.GenerationMismatch)

	require.False(t, isProcessDone(waited))
	require.True(t, isProcessGroupLeaderAlive(leader.Process.Pid))

	// The registry entry must SURVIVE: it was never proven to be the
	// current sweep own target, so it is not this sweep call decision to
	// make. A sweep invoked with registeredGeneration as ITS OWN victim
	// target, later, must still be able to find and act on it.
	entries := readChildGroupFileLockedForTest(t, dataDir, sessionID)
	require.Len(t, entries, 1, "an entry that does not match this sweep victim generation must be retained, not dropped")
	require.Equal(t, leader.Process.Pid, entries[0].PGID)
	require.Equal(t, registeredGeneration, entries[0].Generation)
}

// TestChildGroupRegistry_NewOwnerReacquired_VictimGenerationReachesOldEntry
// is the DIRECT regression test for task #591 blocker P0-2 (2026-08-19
// static-follow-up review, reproduced by running on real Linux). Renamed
// and rewritten from TestChildGroupRegistry_StaleAfterNewOwnerReacquired,
// whose ORIGINAL assertion -- that a sweep must NOT reach the old owner
// entry once a new owner has reacquired the session -- was itself the bug:
// the pre-fix KillRegisteredChildGroups took the session own lock PATH and
// re-read "current" generation internally, which after a new owner
// reacquires means the NEW owner generation, not the dead victim one. That
// treated the actual victim entry -- the live orphan process-group tree
// the whole feature exists to reach -- as "confirmed stale, safe to drop",
// while (per the sibling defect proven in internal/cmd) a sweep running in
// this exact window would separately have gone on to signal the NEW
// owner own live process group instead, having read ITS generation as
// "current".
//
// The fix requires the caller (internal/cmd/sessions_kill.go) to capture
// victimGeneration -- here, oldGeneration -- at the moment it proves
// contention with the OLD holder, BEFORE the kill, and pass that exact
// value down. This test proves both directions with the SAME two-owner
// setup:
//
//  1. Sweeping with victimGeneration = oldGeneration (what the FIXED
//     caller actually does) reaches and kills the old owner own
//     registered entry -- the corrected behavior.
//  2. Sweeping with victimGeneration = newGeneration (what the OLD, BUGGY
//     re-read-current-generation code was equivalent to doing) finds
//     nothing under that generation and leaves the entry retained -- shown
//     as a second, independent registration+sweep pair using a second
//     process-group leader, so this half cannot be satisfied merely by
//     the first sweep having already consumed the only entry.
func TestChildGroupRegistry_NewOwnerReacquired_VictimGenerationReachesOldEntry(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-new-owner-reacquired"
	lockPath := SessionLockPath(dataDir, sessionID)

	lk1, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	oldGeneration := ReadLockGeneration(lockPath)
	require.NotEmpty(t, oldGeneration)

	oldLeader, oldWaited := spawnGroupChild(t, true)
	RegisterChildGroup(dataDir, sessionID, oldLeader.Process.Pid, oldGeneration)

	require.NoError(t, lk1.Release())
	lk2, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk2.Release()
	newGeneration := ReadLockGeneration(lockPath)
	require.NotEmpty(t, newGeneration)
	require.NotEqual(t, oldGeneration, newGeneration)

	// Part 1: the FIXED caller behavior -- sweep with the captured victim
	// generation (oldGeneration) -- reaches and kills the actual victim
	// entry, exactly the process-group tree a real "sessions kill" run in
	// this window is supposed to reach.
	result := KillRegisteredChildGroups(dataDir, sessionID, oldGeneration)
	require.Equal(t, 1, result.Killed, "the FIXED sweep must reach the old owner own entry when given its true victim generation")
	require.False(t, result.GenerationMismatch)

	select {
	case <-oldWaited:
	case <-time.After(2 * time.Second):
		t.Fatal("the old owner registered process group survived a sweep targeting its own victim generation")
	}

	// Part 2: the OLD, BUGGY equivalent -- sweeping with whatever
	// generation happens to be "current" (here modeled directly as
	// newGeneration, the new owner own token) -- must NOT reach an entry
	// registered under a DIFFERENT generation, and must retain it rather
	// than discard it. Uses a second, independent leader+registration so
	// this assertion is not vacuously satisfied by part 1 having already
	// removed the only entry in the registry.
	newLeader, newWaited := spawnGroupChild(t, true)
	t.Cleanup(func() { _ = newLeader.Process.Kill() })
	RegisterChildGroup(dataDir, sessionID, newLeader.Process.Pid, oldGeneration)

	result2 := KillRegisteredChildGroups(dataDir, sessionID, newGeneration)
	require.Zero(t, result2.Killed, "sweeping with the NEW owner generation as target must not reach an entry registered under the OLD generation")
	require.True(t, result2.GenerationMismatch)
	require.False(t, isProcessDone(newWaited))
	require.True(t, isProcessGroupLeaderAlive(newLeader.Process.Pid))

	entries := readChildGroupFileLockedForTest(t, dataDir, sessionID)
	require.Len(t, entries, 1, "the mismatched entry must be retained in the registry, not dropped")
	require.Equal(t, newLeader.Process.Pid, entries[0].PGID)
	require.Equal(t, oldGeneration, entries[0].Generation)
}

// TestChildGroupRegistry_ImplausibleEntry_NotSignalled: a generation-valid
// entry whose pgid does not pass verifyGroupStillPlausible (here: a pgid
// that plausibly never existed) must be counted as Implausible, not
// signalled, and not counted as Killed.
func TestChildGroupRegistry_ImplausibleEntry_NotSignalled(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-implausible-entry"
	lockPath := SessionLockPath(dataDir, sessionID)

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()
	generation := ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	const bogusPGID = 999999
	RegisterChildGroup(dataDir, sessionID, bogusPGID, generation)

	result := KillRegisteredChildGroups(dataDir, sessionID, generation)
	require.Zero(t, result.Killed)
	require.Equal(t, 1, result.Implausible)
	require.False(t, result.GenerationMismatch)
}

// TestChildGroupRegistry_UnregisterRemovesEmptyFile pins the cleanup
// contract: once the last entry is unregistered, the sidecar file itself
// is removed, not left behind empty.
func TestChildGroupRegistry_UnregisterRemovesEmptyFile(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-unregister-empties-file"

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()
	generation := ReadLockGeneration(SessionLockPath(dataDir, sessionID))
	require.NotEmpty(t, generation)

	leader, _ := spawnGroupChild(t, true)
	RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, generation)

	path := childGroupRegistryPath(dataDir, sessionID)
	_, statErr := os.Stat(path)
	require.NoError(t, statErr)

	UnregisterChildGroup(dataDir, sessionID, leader.Process.Pid)

	_, statErr = os.Stat(path)
	require.True(t, os.IsNotExist(statErr))
}

// TestChildGroupRegistryPath_IsUnderLocksDirNextToTheLockFile pins the
// SECURITY property the coordinator's review required: the registry file
// lives in the SAME per-user/per-project locks directory as the session
// lock, not a shared, world-writable temp directory.
func TestChildGroupRegistryPath_IsUnderLocksDirNextToTheLockFile(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-path-shape"

	got := childGroupRegistryPath(dataDir, sessionID)
	want := filepath.Join(dataDir, "locks")
	require.Equal(t, want, filepath.Dir(got))
}

// TestStartTimeToken_SelfProcess is a smoke check on THIS test binary's
// own pid: two reads must agree, and availability must not flap. Does not
// assert true unconditionally -- this codebase also runs on macOS
// (starttime_other_unix.go), where it is correctly always false; on Linux
// (starttime_linux.go) it is expected true, UNVERIFIED from this Windows
// development machine -- see the task notes for the Linux run that would
// confirm it.
func TestStartTimeToken_SelfProcess(t *testing.T) {
	t1, ok1 := startTimeToken(os.Getpid())
	t2, ok2 := startTimeToken(os.Getpid())
	require.Equal(t, ok1, ok2)
	if ok1 {
		require.Equal(t, t1, t2)
		require.NotEmpty(t, t1)
	}
}

func isProcessDone(waited <-chan error) bool {
	select {
	case <-waited:
		return true
	default:
		return false
	}
}

func isProcessGroupLeaderAlive(pgid int) bool {
	return verifyGroupStillPlausible(pgid, "")
}

// TestChildGroupRegistry_LeaderReaped_GroupStillSwept is the regression
// test for the 2026-10-16 review's P2-1: a registered group whose leader
// exited AND was reaped while a grandchild still lives must be killed by
// the sweep. The pre-fix Getpgid-only check in
// verifyGroupStillPlausibleOutcome reported such a group
// plausibilityConfirmedGone (Getpgid on a reaped leader returns ESRCH) and
// the sweep dropped the registry entry without signalling anything --
// exactly the "leader gone implies group gone" assumption.
func TestChildGroupRegistry_LeaderReaped_GroupStillSwept(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	dataDir := t.TempDir()
	sessionID := "childgroup-leader-reaped-group-swept"

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk.Release()

	lockPath := SessionLockPath(dataDir, sessionID)
	generation := ReadLockGeneration(lockPath)
	require.NotEmpty(t, generation)

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	// The leader spawns a grandchild into its own (already new) process
	// group and exits. 41.29 is deliberately non-round, like
	// spawnGroupChild's 43.17: cleanup that matches on command line must
	// not catch other packages' concurrent sleeps.
	script := `sleep 41.29 & p=$!; echo "$p" > '` + pidFile + `'; exit 0`
	cmd := platform.Command(t.Context(), "bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// writer is an *os.File, so exec spawns no copy goroutines and
	// cmd.Wait returns at leader exit even while the grandchild still
	// holds the inherited write end.
	cmd.Stdout = writer
	require.NoError(t, cmd.Start())

	grandpid := 0
	waited := false
	t.Cleanup(func() {
		if grandpid > 0 {
			_ = syscall.Kill(grandpid, syscall.SIGKILL)
		}
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = writer.Close()
		_ = reader.Close()
	})

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
			t.Fatalf("grandchild pid file never became readable: %v", readErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NotZero(t, grandpid)

	// Reap the leader -- the exact P2-1 precondition, not merely a
	// zombie.
	require.NoError(t, cmd.Wait())
	waited = true

	// Premise guards: the leader is fully reaped, yet the grandchild
	// keeps the registered group alive.
	_, pgErr := syscall.Getpgid(cmd.Process.Pid)
	require.Error(t, pgErr, "premise: the leader must be fully reaped (Getpgid fails) before the sweep runs")
	require.NoError(t, syscall.Kill(-cmd.Process.Pid, 0), "premise: the grandchild must keep the registered group alive after the leader is reaped")

	RegisterChildGroup(dataDir, sessionID, cmd.Process.Pid, generation)
	result := KillRegisteredChildGroups(dataDir, sessionID, generation)
	require.Equal(t, 1, result.Killed, "the sweep must signal a registered group whose leader is reaped while members still live -- the pre-fix Getpgid-only check dropped it as implausible")
	require.Zero(t, result.Implausible)
	require.False(t, result.GenerationMismatch)

	// Death oracle via the inherited pipe, NOT pid liveness: a zombie
	// keeps kill(pid, 0) alive.
	require.NoError(t, reader.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 1)
	_, readErr := reader.Read(buf)
	require.ErrorIs(t, readErr, io.EOF, "the grandchild survived the sweep of its registered group")

	_, statErr := os.Stat(childGroupRegistryPath(dataDir, sessionID))
	require.True(t, os.IsNotExist(statErr), "all entries reached a confirmed outcome, so the registry file must be removed")
}
