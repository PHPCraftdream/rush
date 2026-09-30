package session

import (
	"bytes"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// syncBuffer is a bytes.Buffer safe for slog's concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureWarnLogs routes slog to a buffer at Warn level for the test.
func captureWarnLogs(t *testing.T) *syncBuffer {
	t.Helper()
	out := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return out
}

// R6A-1: a listed orphan lock file that another reaper (or RecoverDeadHost)
// deleted before this entry's probe is a no-op, not a "remove lock file
// failed" Warn: ProbeHost reports dead with no lock and no error for it.
//
// Revert-check: dropping the `lock == nil` guard in reapOrphanHostLock makes
// RemoveDeadHostFile fail with "nil lock" and the Warn assertion goes red.
// Not parallel: it swaps the process-wide slog default.
func TestReapOrphanHostLock_FileDeletedBeforeProbe_NoWarn(t *testing.T) {
	store, _, ctx := newTestStore(t)
	logs := captureWarnLogs(t)

	const hostID = "orphan-gone-before-probe"
	lockPath := HostLockPath(store.dataDir, hostID)
	seed, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, seed.Release())
	require.NoError(t, os.Remove(lockPath)) // the other reaper won between listing and probe

	store.reapOrphanHostLock(ctx, hostID)

	require.NotContains(t, logs.String(), "purge orphan host lock files", "a vanished file is not a failure")
}

// The same guard must not stop the real reap: a listed, dead, rowless file is
// removed and nothing is logged.
func TestReapOrphanHostLock_DeadRowlessFile_RemovedWithoutWarn(t *testing.T) {
	store, _, ctx := newTestStore(t)
	logs := captureWarnLogs(t)

	const hostID = "orphan-dead-rowless"
	lockPath := HostLockPath(store.dataDir, hostID)
	seed, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, seed.Release())

	store.reapOrphanHostLock(ctx, hostID)

	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "the dead rowless file is reaped")
	require.NotContains(t, logs.String(), "purge orphan host lock files")
}
