package session

// R3A-1 (docs/reviews/2026-09-30-async-phase4-round3.md): only a data dir that
// cannot host an OS lock file degrades the driver marker to "unavailable".
// A database or context error while registering the host must fail the claim.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const abortHostInsertTrigger = `CREATE TRIGGER abort_async_hosts_insert BEFORE INSERT ON async_hosts
BEGIN SELECT RAISE(ABORT, 'injected: async_hosts insert refused'); END`

// TestClaimSessionDriver_HostInsertDBError_FailsClaim: a SQLite trigger aborts
// INSERT INTO async_hosts (the stand-in for SQLITE_BUSY/FULL). The claim must
// return that error, not ErrDriverMarkerUnavailable (which callers swallow),
// must not memoize the failure, and must write no marker. Dropping the trigger
// lets the very next claim succeed.
//
// Revert-check: wrap every ensureHost error as ErrDriverMarkerUnavailable (the
// pre-fix code) -> ErrorIs/NotErrorIs below fail.
func TestClaimSessionDriver_HostInsertDBError_FailsClaim(t *testing.T) {
	t.Parallel()
	stores, _, ctx := nStores(t, 1, "s1")
	st := stores[0]
	_, err := st.sqlDB.ExecContext(ctx, abortHostInsertTrigger)
	require.NoError(t, err)

	err = st.ClaimSessionDriver(ctx, "s1")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrDriverMarkerUnavailable, "a DB error must not look like a lock-incapable data dir")
	require.NotErrorIs(t, err, ErrHostLockUnavailable)
	require.ErrorContains(t, err, "async_hosts insert refused", "the DB error itself must reach the caller")
	require.Empty(t, st.HostID(), "a failed registration publishes no host")
	_, ok := driverRow(t, ctx, st, "s1")
	require.False(t, ok, "no marker may be written")

	_, err = st.sqlDB.ExecContext(ctx, `DROP TRIGGER abort_async_hosts_insert`)
	require.NoError(t, err)
	require.NoError(t, st.ClaimSessionDriver(ctx, "s1"), "the failure is not sticky")
	row, ok := driverRow(t, ctx, st, "s1")
	require.True(t, ok)
	require.Equal(t, st.HostID(), row.HostID)
}

// TestClaimSessionDriver_CancelledCtx_FailsClaim: a cancelled ctx is a plain
// error, never "marker unavailable".
//
// Revert-check: as above.
func TestClaimSessionDriver_CancelledCtx_FailsClaim(t *testing.T) {
	t.Parallel()
	stores, _, _ := nStores(t, 1, "s1")
	st := stores[0]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := st.ClaimSessionDriver(ctx, "s1")
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrDriverMarkerUnavailable)
}

// TestRegisterHost_ErrorClasses: ErrHostLockUnavailable is returned for lock
// capability failures ONLY -- a missing/unwritable hosts dir -- and not for a
// DB insert failure or for registration that loses every retry to an active
// peer.
//
// Revert-check: return ErrHostLockUnavailable for every acquire/insert failure
// -> the db and exhausted-peer subtests fail (and TestClaimSessionDriver_*
// above fail too).
func TestRegisterHost_ErrorClasses(t *testing.T) {
	t.Run("hosts_dir_is_a_file", func(t *testing.T) {
		store, q, ctx := newTestStore(t)
		require.NoError(t, os.WriteFile(HostsDir(store.dataDir), []byte("x"), 0o644))
		_, err := RegisterHost(ctx, store.dataDir, 1, "x", q)
		require.ErrorIs(t, err, ErrHostLockUnavailable)
	})

	t.Run("db_insert_failure", func(t *testing.T) {
		store, q, ctx := newTestStore(t)
		_, err := store.sqlDB.ExecContext(ctx, abortHostInsertTrigger)
		require.NoError(t, err)
		_, err = RegisterHost(ctx, store.dataDir, 1, "x", q)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrHostLockUnavailable)
		require.ErrorContains(t, err, "db insert")
	})

	t.Run("every_attempt_loses_to_a_peer", func(t *testing.T) {
		store, q, ctx := newTestStore(t)
		var held []*FileLock
		registerHostAfterCreateSeam = func(lockPath string) {
			if filepath.Dir(lockPath) != HostsDir(store.dataDir) {
				return
			}
			status, lock, err := ProbeHostLock(lockPath)
			require.NoError(t, err)
			require.Equal(t, HostStatusDead, status)
			held = append(held, lock)
		}
		t.Cleanup(func() {
			registerHostAfterCreateSeam = nil
			for _, l := range held {
				_ = l.Release()
			}
		})
		_, err := RegisterHost(ctx, store.dataDir, 1, "x", q)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrHostLockUnavailable, "contention with live peers is not a lock-incapable data dir")
		require.Len(t, held, registerHostAttempts)
	})
}
