package session

// R2A-2 (docs/reviews/2026-09-30-async-phase4-round2.md): lock-order
// inversion between the first host registration (a writer-connection DB call)
// and a writer transaction that reads store state. The writer connection is
// single (SetMaxOpenConns(1)); ensureHost used to hold s.mu across
// RegisterHost, while a pull transaction holding the connection took s.mu
// (HostID) -- a permanent deadlock of every DB write in the process.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// holdWriterAndStartRegistration takes the store's only writer connection with
// a transaction, starts ensureHost on another goroutine and waits until that
// registration is genuinely blocked on the connection (sql.DBStats.WaitCount).
// The returned commit releases the connection; registered receives
// ensureHost's result.
func holdWriterAndStartRegistration(t *testing.T, store *AsyncJobStore) (commit func(), registered <-chan error) {
	t.Helper()
	ctx := context.Background()
	tx, err := store.sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	released := false
	commit = func() {
		if !released {
			released = true
			require.NoError(t, tx.Commit())
		}
	}
	t.Cleanup(commit)

	ch := make(chan error, 1)
	go func() {
		_, err := store.ensureHost(ctx)
		ch <- err
	}()
	require.Eventually(t, func() bool { return store.sqlDB.Stats().WaitCount >= 1 }, 10*time.Second, 5*time.Millisecond,
		"the registration must reach its writer-connection DB call")
	return commit, ch
}

// TestEnsureHost_RegistrationNeverBlocksWriterTransactionReadingStoreState is
// the real interleaving, no fault injection: registration is blocked on the
// writer connection a transaction holds; that transaction then reads exactly
// what pullOneSessionNotice's void condition reads (HostID via HostNotDead)
// plus the reader-pool accessor. All of it must return while registration is
// still in flight -- with s.mu held across the DB call they wait for a lock
// whose owner waits for this transaction (deadlock, bounded here by the
// timeout).
//
// REVERT CHECK: ensureHost made to hold s.mu across RegisterHost again and
// HostID to take s.mu (the pre-fix shape) -> the probe goroutine never
// finished and the test failed with "deadlock" after the timeout.
func TestEnsureHost_RegistrationNeverBlocksWriterTransactionReadingStoreState(t *testing.T) {
	store, _, _ := newTestStore(t)
	commit, registered := holdWriterAndStartRegistration(t, store)

	probed := make(chan struct{})
	go func() {
		defer close(probed)
		_ = store.HostID()
		_ = store.HostNotDead("some-other-host")
		_ = store.readQuerier()
	}()
	select {
	case <-probed:
	case <-time.After(3 * time.Second):
		commit() // unblock the leaked goroutines before failing
		<-probed
		t.Fatal("deadlock: a writer transaction reading store state waited on the in-flight host registration")
	}

	commit()
	require.NoError(t, <-registered)
	require.NotEmpty(t, store.HostID(), "the registration must complete once the connection is free")
}

// TestClose_WaitsForInFlightRegistrationAndTearsItDown: a Close racing the
// first registration must not return before the registration finishes and
// leave the freshly published host (and its held lock) behind.
//
// REVERT CHECK: takeHost without regMu -> Close returned at once with no
// host, ensureHost then published one, and HostID stayed non-empty with its
// lock file held.
func TestClose_WaitsForInFlightRegistrationAndTearsItDown(t *testing.T) {
	store, _, ctx := newTestStore(t)
	commit, registered := holdWriterAndStartRegistration(t, store)

	closed := make(chan error, 1)
	go func() { closed <- store.Close(ctx) }()
	select {
	case <-closed:
		commit()
		<-registered
		t.Fatal("Close returned while a registration was still in flight")
	case <-time.After(300 * time.Millisecond):
	}

	commit()
	require.NoError(t, <-registered)
	require.NoError(t, <-closed)
	require.Empty(t, store.HostID(), "Close must tear down the host the racing registration published")
}
