package session

// R2A-1 (docs/reviews/2026-09-30-async-phase4-round2.md): a host that is
// still registering must never lose its lock file to a reaper. Registration
// is create-then-lock; a reaper that wins the lock on the fresh file in
// between used to unlink it (POSIX), or make the registrant's lock attempt
// fail (any OS). acquireFreshHostLock/RegisterHost now retry under a new id
// and verify the path after locking.

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// setRegisterSeam installs registerHostAfterCreateSeam for one test, scoped
// to dataDir (other tests in the package register hosts too) and one-shot: fn
// runs for the FIRST registration attempt only, so the retry is undisturbed.
// Restores nil on cleanup.
func setRegisterSeam(t *testing.T, dataDir string, fn func(lockPath string)) {
	t.Helper()
	var once sync.Once
	registerHostAfterCreateSeam = func(lockPath string) {
		if filepath.Dir(lockPath) != HostsDir(dataDir) {
			return
		}
		once.Do(func() { fn(lockPath) })
	}
	t.Cleanup(func() { registerHostAfterCreateSeam = nil })
}

// TestRegisterHost_ReaperHoldsLockInCreateWindow_RetriesUnderNewID: a peer
// (a reaper mid-probe) holds the exclusive lock on the fresh file when the
// registrant tries to lock it. Registration must retry under a NEW id and
// succeed, not fail the whole claim with ErrLockContended.
//
// REVERT CHECK: RegisterHost's retry loop reduced to a single
// acquireFreshHostLock attempt (the pre-fix behaviour, one TryAcquireFileLock)
// -> RegisterHost returned "file lock contended" and require.NoError failed.
func TestRegisterHost_ReaperHoldsLockInCreateWindow_RetriesUnderNewID(t *testing.T) {
	store, q, ctx := newTestStore(t)
	dataDir := store.dataDir

	var firstPath string
	var held *FileLock
	setRegisterSeam(t, dataDir, func(lockPath string) {
		firstPath = lockPath
		status, lock, err := ProbeHostLock(lockPath)
		require.NoError(t, err)
		require.Equal(t, HostStatusDead, status, "the fresh unlocked file must look dead to a prober")
		held = lock
	})

	h, err := RegisterHost(ctx, dataDir, 1, "race", q)
	if held != nil {
		require.NoError(t, held.Release())
	}
	require.NoError(t, err, "losing the create-to-lock race must cost a retry, not the registration")
	t.Cleanup(func() { _ = h.Close(ctx, q) })

	require.NotEmpty(t, firstPath)
	require.NotEqual(t, firstPath, HostLockPath(dataDir, h.ID), "the retry must use a fresh id")
	status, err := ProbeHostLockShared(HostLockPath(dataDir, h.ID))
	require.NoError(t, err)
	require.Equal(t, HostStatusAlive, status, "the registered host's file must be held")
}

// TestRegisterHost_ReaperUnlinksFreshFile_RegistrantDoesNotKeepUnlinkedInode:
// the reaper (purgeOrphanHostLockFiles: "file with no async_hosts row")
// wins the lock on the fresh file and unlinks it before the registrant locks.
// The registrant must notice and retry: afterwards its file must exist at the
// path and be held, so every probe of the id says alive.
//
// POSIX only: Windows cannot unlink a file another handle has open (Go opens
// without FILE_SHARE_DELETE), so the window does not exist there.
//
// REVERT CHECK: the post-lock SameFile re-check in acquireFreshHostLock
// removed -> RegisterHost returned an id whose lock file was gone (probes saw
// ENOENT = dead) and the ProbeHostShared assertion failed.
func TestRegisterHost_ReaperUnlinksFreshFile_RegistrantDoesNotKeepUnlinkedInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an open lock file cannot be unlinked on Windows; the create-to-lock unlink window does not exist")
	}
	store, q, ctx := newTestStore(t)
	dataDir := store.dataDir
	reaper := NewAsyncJobStore(store.sqlDB, dataDir, 998, "reaper")

	setRegisterSeam(t, dataDir, func(string) {
		require.NoError(t, reaper.purgeOrphanHostLockFiles(ctx))
	})

	h, err := RegisterHost(ctx, dataDir, 1, "race", q)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close(ctx, q) })

	_, statErr := os.Stat(HostLockPath(dataDir, h.ID))
	require.NoError(t, statErr, "a registered host's lock file must exist at its path")
	status, err := ProbeHostLockShared(HostLockPath(dataDir, h.ID))
	require.NoError(t, err)
	require.Equal(t, HostStatusAlive, status, "the reaper's unlink must not leave the host looking dead")
}
