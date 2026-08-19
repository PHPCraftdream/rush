//go:build !windows

package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

	result := KillRegisteredChildGroups(dataDir, sessionID, lockPath)
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

// TestChildGroupRegistry_StaleAfterPIDReuse_KillsNothing is the regression
// test the coordinator's review asked for directly: a stale registry from
// a dead crush must kill nothing. It reproduces the exact vulnerability
// found in the previous (pid-keyed, world-writable-tmp-dir) version of
// this file: a crush process registers a group then crashes -- skipping
// UnregisterChildGroup/RemoveChildGroupRegistry -- leaving a registry
// entry with a generation token that can never again match a live lock.
// True OS-level pid reuse cannot be reproduced from user code (the kernel
// controls it), so this models it at the level the registry actually
// checks: no live generation currently on disk for the session at all,
// which is exactly the observable state true pid/session reuse produces.
func TestChildGroupRegistry_StaleAfterPIDReuse_KillsNothing(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-stale-after-pid-reuse"
	lockPath := SessionLockPath(dataDir, sessionID)

	leader, waited := spawnGroupChild(t, true)
	staleGeneration := "99999-1234567890"
	RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, staleGeneration)

	require.Empty(t, ReadLockGeneration(lockPath))

	result := KillRegisteredChildGroups(dataDir, sessionID, lockPath)
	require.Zero(t, result.Killed)
	require.True(t, result.GenerationMismatch)

	require.False(t, isProcessDone(waited))
	require.True(t, isProcessGroupLeaderAlive(leader.Process.Pid))

	_, statErr := os.Stat(childGroupRegistryPath(dataDir, sessionID))
	require.True(t, os.IsNotExist(statErr))
}

// TestChildGroupRegistry_StaleAfterNewOwnerReacquired covers the other
// shape of the same bug: the SESSION ID is reused across two live crush
// processes in sequence (old one released, or died and was reaped; a new
// one later acquired the same id), producing a genuinely different
// generation token. A registry entry from the OLD generation must not let
// a sessions kill against the NEW owner reach a process group that has
// nothing to do with it.
func TestChildGroupRegistry_StaleAfterNewOwnerReacquired(t *testing.T) {
	dataDir := t.TempDir()
	sessionID := "childgroup-stale-after-new-owner"
	lockPath := SessionLockPath(dataDir, sessionID)

	lk1, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	oldGeneration := ReadLockGeneration(lockPath)
	require.NotEmpty(t, oldGeneration)

	leader, waited := spawnGroupChild(t, true)
	RegisterChildGroup(dataDir, sessionID, leader.Process.Pid, oldGeneration)

	require.NoError(t, lk1.Release())
	lk2, err := TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	defer lk2.Release()
	newGeneration := ReadLockGeneration(lockPath)
	require.NotEmpty(t, newGeneration)
	require.NotEqual(t, oldGeneration, newGeneration)

	result := KillRegisteredChildGroups(dataDir, sessionID, lockPath)
	require.Zero(t, result.Killed)
	require.True(t, result.GenerationMismatch)
	require.False(t, isProcessDone(waited))
	require.True(t, isProcessGroupLeaderAlive(leader.Process.Pid))
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

	result := KillRegisteredChildGroups(dataDir, sessionID, lockPath)
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
