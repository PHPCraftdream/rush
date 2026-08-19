package session

// Regression coverage for the transient-vs-permanent classification finding
// in the 2026-08-19 release-readiness review (task #571, part c):
// recordOrphanOutboxDrainFailure charged EVERY DrainOrphanOutboxEntry
// failure -- including plain SQLite lock contention (SQLITE_BUSY /
// SQLITE_LOCKED), which says nothing about whether the row's data is
// valid -- against the same fixed, backoff-free max_attempts=5 budget a
// genuinely permanent failure (a session_id that fails session_run_queue's
// FK check) uses. The file's own header already documents that two pump
// instances can double-count the same failed attempt; combined with no
// backoff between ticks, five ordinary SQLITE_BUSY hits across drain ticks
// -- easily reached with several concurrent `crush run` processes each
// running an immediate Start()-time drain -- permanently quarantines a
// perfectly healthy, still-queued user call with nothing left to recover
// it. Before this fix the row retried until the DB recovered; the fix
// keeps that property for transient failures while leaving the bounded
// quarantine intact for genuinely permanent ones (see
// TestOrphanOutbox_PoisonEntryQuarantinesInsteadOfRetryingForever in
// p556_orphan_outbox_quarantine_test.go for that half).
//
// This file lives in package session (not session_test) specifically to
// reach the unexported isTransientOrphanOutboxDrainError classifier
// directly.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	sqlitedriver "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

// realSQLiteBusyError deterministically produces a genuine, driver-issued
// *sqlite.Error with code SQLITE_BUSY, by racing two independent raw
// connections to the same on-disk file: the second connection has
// busy_timeout=0 (fails immediately rather than waiting), so this needs no
// sleep/retry loop and returns in microseconds. WAL mode is required for
// this to be the specific lock SQLite reports as BUSY rather than blocking
// silently under the default rollback journal.
//
// This is deliberately NOT a synthetic/hand-built error: *sqlite.Error's
// fields (msg, code) are unexported and modernc.org/sqlite exposes no
// public constructor, so there is no way to fabricate one that isn't
// itself produced by the real driver -- which is also the more faithful
// test in any case, since it proves the classifier against the exact
// concrete type and Code() value production will actually see.
func realSQLiteBusyError(t *testing.T) error {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "busy-probe.db")

	db1, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)")
	require.NoError(t, err)
	db1.SetMaxOpenConns(1)
	t.Cleanup(func() { db1.Close() })
	_, err = db1.Exec(`CREATE TABLE t (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)

	db2, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)")
	require.NoError(t, err)
	db2.SetMaxOpenConns(1)
	t.Cleanup(func() { db2.Close() })

	tx1, err := db1.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { tx1.Rollback() })
	_, err = tx1.Exec(`INSERT INTO t (id) VALUES ('a')`)
	require.NoError(t, err)
	// tx1 now holds SQLite's single write lock, uncommitted.

	tx2, err := db2.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { tx2.Rollback() })
	_, busyErr := tx2.Exec(`INSERT INTO t (id) VALUES ('b')`)
	require.Error(t, busyErr, "the second writer must be rejected -- if this passes, the two connections were not actually contending for the same lock and this test proves nothing")

	var sqliteErr *sqlitedriver.Error
	require.ErrorAs(t, busyErr, &sqliteErr, "must be a genuine *sqlite.Error for this test to exercise the real classification path")
	require.Equal(t, 5, sqliteErr.Code()&0xff, "must genuinely be SQLITE_BUSY (5), not some other contention code, or this test is not testing what it claims")

	return busyErr
}

// TestIsTransientOrphanOutboxDrainError_SQLiteBusyIsTransient is the
// positive case: a genuine SQLITE_BUSY from the driver must be classified
// as transient.
//
// Revert-check: change the `case 5, 6:` in isTransientOrphanOutboxDrainError
// to `case 6:` (drop SQLITE_BUSY) and this fails.
func TestIsTransientOrphanOutboxDrainError_SQLiteBusyIsTransient(t *testing.T) {
	t.Parallel()
	busyErr := realSQLiteBusyError(t)
	require.True(t, isTransientOrphanOutboxDrainError(busyErr), "a real SQLITE_BUSY must be classified as transient")
}

// TestIsTransientOrphanOutboxDrainError_WrappedSQLiteBusyIsTransient proves
// the classifier sees through fmt.Errorf("...: %w", err) wrapping --
// exactly how DrainOrphanOutboxEntry's own call sites (session_orphan_outbox.go)
// return their errors ("enqueueing to main queue: %w", "beginning
// transaction: %w", etc.), never the bare driver error.
func TestIsTransientOrphanOutboxDrainError_WrappedSQLiteBusyIsTransient(t *testing.T) {
	t.Parallel()
	busyErr := realSQLiteBusyError(t)
	wrapped := fmt.Errorf("enqueueing to main queue: %w", busyErr)
	require.True(t, isTransientOrphanOutboxDrainError(wrapped), "a wrapped SQLITE_BUSY must still be classified as transient -- production errors always arrive wrapped")
}

// TestIsTransientOrphanOutboxDrainError_ConstraintViolationIsPermanent is
// the negative case: a genuine SQLITE_CONSTRAINT (the FK violation shape
// used by TestOrphanOutbox_PoisonEntryQuarantinesInsteadOfRetryingForever)
// must NOT be classified as transient -- it is exactly the failure this
// fix must continue to charge against the budget, or the quarantine this
// file's sibling test covers would stop working.
func TestIsTransientOrphanOutboxDrainError_ConstraintViolationIsPermanent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "constraint-probe.db")
	rawDB, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(ON)")
	require.NoError(t, err)
	t.Cleanup(func() { rawDB.Close() })
	_, err = rawDB.Exec(`CREATE TABLE parent (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = rawDB.Exec(`CREATE TABLE child (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL REFERENCES parent(id))`)
	require.NoError(t, err)

	_, fkErr := rawDB.Exec(`INSERT INTO child (id, parent_id) VALUES ('c1', 'does-not-exist')`)
	require.Error(t, fkErr)
	var sqliteErr *sqlitedriver.Error
	require.ErrorAs(t, fkErr, &sqliteErr)
	require.Equal(t, 19, sqliteErr.Code()&0xff, "must genuinely be SQLITE_CONSTRAINT (19)")

	require.False(t, isTransientOrphanOutboxDrainError(fkErr), "a genuine constraint violation must NOT be classified as transient -- it is a permanent, this-row-can-never-succeed failure")
}

// TestIsTransientOrphanOutboxDrainError_NonSQLiteErrorIsPermanent covers
// the documented safe-direction fallback: an error that is not a
// *sqlite.Error at all (a context error, a generic error from a
// non-SQLite Sessions implementation, or a bug in this classifier's own
// reasoning) must default to "permanent" (counted), not "transient"
// (ignored) -- the safe direction per isTransientOrphanOutboxDrainError's
// own doc, since the false-transient direction would let a genuinely
// permanent failure retry forever.
func TestIsTransientOrphanOutboxDrainError_NonSQLiteErrorIsPermanent(t *testing.T) {
	t.Parallel()
	require.False(t, isTransientOrphanOutboxDrainError(context.DeadlineExceeded))
	require.False(t, isTransientOrphanOutboxDrainError(errors.New("plain failure")))
}
