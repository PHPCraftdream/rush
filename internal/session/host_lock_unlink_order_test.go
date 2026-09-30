package session

// R3A T1 (docs/reviews/2026-09-30-async-phase4-round3.md): DUR-5's "a remover
// unlinks only while holding the lock" is the POSIX ordering
// unlinkBeforeUnlock=true in RemoveDeadHostFile. Nothing else pins it: flip the
// constant and every other test still passes.

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRemoveDeadHostFile_POSIX_UnlinksWhileStillHoldingTheLock observes the
// lock at the instant before the unlink: a second open of the file must see
// it held. Were the lock released first, a registrant creating and locking the
// same path in that gap could verify its lock against the very inode the
// remover is about to unlink (acquireFreshHostLock), and end up holding a lock
// on a file no probe can find.
//
// POSIX only. Windows cannot unlink an open file, so its RemoveDeadHostFile
// must close first (unlinkBeforeUnlock=false) and this ordering does not exist
// there: the test is skipped on Windows and runs on Linux/macOS CI only.
//
// Revert-check: set unlinkBeforeUnlock=false in file_lock_unix.go (release,
// then remove) -> the probe at the unlink instant reads dead, not alive. On
// Windows, where the constant is already false, the same body with the skip
// removed fails the same way.
func TestRemoveDeadHostFile_POSIX_UnlinksWhileStillHoldingTheLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only: Windows cannot unlink an open file (unlinkBeforeUnlock=false there); runs on Linux/macOS CI only")
	}
	dataDir := t.TempDir()
	lockPath := HostLockPath(dataDir, "dead-host-order")
	seed, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, seed.Release())
	status, held, err := ProbeHostLock(lockPath)
	require.NoError(t, err)
	require.Equal(t, HostStatusDead, status)
	require.NotNil(t, held)

	var (
		fired    bool
		atUnlink HostLockStatus
	)
	removeDeadHostFileBeforeRemoveSeam = func(path string) {
		fired = true
		atUnlink, _ = ProbeHostLockShared(path)
	}
	t.Cleanup(func() { removeDeadHostFileBeforeRemoveSeam = nil })

	require.NoError(t, RemoveDeadHostFile(lockPath, held))

	require.True(t, fired, "the seam must fire before the unlink")
	require.Equal(t, HostStatusAlive, atUnlink, "the lock must still be held when the file is unlinked")
	_, statErr := os.Stat(lockPath)
	require.True(t, errors.Is(statErr, os.ErrNotExist), "the file is removed")
}
