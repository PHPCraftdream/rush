package session

// Regression coverage for the outbox-quarantine-polarity finding in the
// 2026-08-19 release-readiness static follow-up review (task #589),
// correcting #571 part c: the previous classifier (isTransientOrphanOutboxDrainError)
// treated context.Canceled/context.DeadlineExceeded and every unrecognised
// error as PERMANENT by default, on the theory that a false "permanent" only
// cost one extra point off a five-attempt budget. That theory is wrong for a
// shutdown-shaped cancellation specifically: an ordinary Stop() landing
// mid-transaction, or the drain's own 10s budget expiring on a slow disk, is
// not rare DB corruption -- it is routine operation -- and five of them (or
// fewer, since two pump instances can double-count, see this package's other
// orphan-outbox tests) durably quarantines perfectly healthy, already-
// accepted user work.
//
// This file covers both halves of the fix:
//
//   - isPermanentOrphanOutboxDrainError (the renamed, polarity-inverted
//     classifier in run_queue_orphan_drain.go) must positively recognise
//     ONLY a genuine SQLITE_CONSTRAINT and treat everything else --
//     SQLITE_BUSY, a context error, an unrecognised error type -- as
//     "not permanent", i.e. not counted.
//   - processOrphanOutboxEntry itself must never even reach that classifier
//     for a p.ctx-caused cancellation: it short-circuits on p.ctx.Err()
//     BEFORE isPermanentOrphanOutboxDrainError runs, so a p.ctx cancellation
//     never calls RecordOrphanOutboxFailure at all. This is the part a
//     classifier-only unit test cannot prove -- it has to be driven through
//     processOrphanOutboxEntry with a real cancelled p.ctx, exactly the
//     shape task #589 (and this session's verification-discipline note)
//     asked for, after a previous test in this same file made the mistake
//     of calling the production function directly and never actually
//     exercising the call site being regression-tested.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	sqlitedriver "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

// newTestServiceForCancellationTest opens a real database via db.Connect
// (production's own connect path, so foreign_keys and every other pragma
// connect.go sets actually apply) and creates one session -- the minimal
// fixture the tests below need. Package-local counterpart of
// run_queue_round2_test.go's setupTestSession (session_test package): this
// file lives in package session (needed for unexported access to
// isPermanentOrphanOutboxDrainError, pump.cancel, and
// pump.processOrphanOutboxEntry), which cannot import the session_test
// package's helper.
func newTestServiceForCancellationTest(t *testing.T) (*Session, Service) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()

	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })

	q := db.New(conn)
	svc := NewService(q, conn)

	sess, err := svc.Create(ctx, "orphan-cancellation-probe")
	require.NoError(t, err)
	return &sess, svc
}

// realSQLiteBusyErrorForCancellationTest produces a genuine, driver-issued
// *sqlite.Error with code SQLITE_BUSY, by racing two independent raw
// connections to the same on-disk file. This file replaces the old
// p571_transient_classifier_test.go (which tested the pre-#589
// isTransientOrphanOutboxDrainError name/polarity) entirely, so it is kept
// self-contained rather than sharing a helper with a file that no longer
// exists.
func realSQLiteBusyErrorForCancellationTest(t *testing.T) error {
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

	tx2, err := db2.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { tx2.Rollback() })
	_, busyErr := tx2.Exec(`INSERT INTO t (id) VALUES ('b')`)
	require.Error(t, busyErr, "the second writer must be rejected, or this test proves nothing")

	return busyErr
}

// realConstraintErrorForCancellationTest produces a genuine, driver-issued
// *sqlite.Error with primary code SQLITE_CONSTRAINT (19), the one cause
// isPermanentOrphanOutboxDrainError positively recognises as permanent.
// Used by TestProcessOrphanOutboxEntry_PCtxCancellationDoesNotCountAttempt
// specifically so that test's outcome depends on the p.ctx short-circuit,
// not on the classifier alone (see that test's own doc for why a plain
// context error would make the test pass regardless of the short-circuit).
func realConstraintErrorForCancellationTest(t *testing.T) error {
	t.Helper()
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
	require.Error(t, fkErr, "the constraint violation must be rejected, or this test proves nothing")
	var sqliteErr *sqlitedriver.Error
	require.ErrorAs(t, fkErr, &sqliteErr)
	require.Equal(t, 19, sqliteErr.Code()&0xff, "must genuinely be SQLITE_CONSTRAINT (19)")

	return fkErr
}

// TestIsPermanentOrphanOutboxDrainError_ConstraintViolationIsPermanent pins
// the one positively-recognised permanent cause.
//
// Revert-check: change `case 19:` to `case 20:` (or any other code) in
// isPermanentOrphanOutboxDrainError and this fails.
func TestIsPermanentOrphanOutboxDrainError_ConstraintViolationIsPermanent(t *testing.T) {
	t.Parallel()
	limitParallel(t)
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

	require.True(t, isPermanentOrphanOutboxDrainError(fkErr), "a genuine constraint violation must be classified as permanent -- it is the one proven this-row-can-never-succeed failure")
}

// TestIsPermanentOrphanOutboxDrainError_SQLiteBusyIsNotPermanent is the
// polarity check for ordinary DB contention: SQLITE_BUSY says nothing about
// the row's own data and must not be classified as permanent.
//
// Revert-check: add `case 5:` to isPermanentOrphanOutboxDrainError's switch
// and this fails.
func TestIsPermanentOrphanOutboxDrainError_SQLiteBusyIsNotPermanent(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	busyErr := realSQLiteBusyErrorForCancellationTest(t)
	require.False(t, isPermanentOrphanOutboxDrainError(busyErr), "a real SQLITE_BUSY must NOT be classified as permanent")
}

// TestIsPermanentOrphanOutboxDrainError_WrappedSQLiteBusyIsNotPermanent
// proves the classifier still sees through fmt.Errorf wrapping in the new
// polarity, exactly as it did before the fix.
func TestIsPermanentOrphanOutboxDrainError_WrappedSQLiteBusyIsNotPermanent(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	busyErr := realSQLiteBusyErrorForCancellationTest(t)
	wrapped := fmt.Errorf("enqueueing to main queue: %w", busyErr)
	require.False(t, isPermanentOrphanOutboxDrainError(wrapped))
}

// TestIsPermanentOrphanOutboxDrainError_ContextErrorsAreNotPermanent is the
// core polarity fix: this is exactly what the pre-#589 classifier got
// backwards. context.Canceled and context.DeadlineExceeded (the two ways
// p.ctx-derived cancellation surfaces) must NOT be classified as permanent.
//
// Revert-check below reproduces the ORIGINAL bug shape directly against
// this classifier and shows it would have failed this exact assertion.
func TestIsPermanentOrphanOutboxDrainError_ContextErrorsAreNotPermanent(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	require.False(t, isPermanentOrphanOutboxDrainError(context.Canceled),
		"context.Canceled (an ordinary Stop() mid-transaction) must not be classified as permanent")
	require.False(t, isPermanentOrphanOutboxDrainError(context.DeadlineExceeded),
		"context.DeadlineExceeded (the drain's own 10s budget expiring) must not be classified as permanent")
	require.False(t, isPermanentOrphanOutboxDrainError(fmt.Errorf("beginning transaction: %w", context.Canceled)),
		"a wrapped context.Canceled must also not be classified as permanent")
}

// TestIsPermanentOrphanOutboxDrainError_UnknownErrorIsNotPermanent covers
// the other half of the polarity fix: an error this classifier has never
// seen (not a *sqlite.Error at all) must default to "not permanent" (not
// counted, left pending with a loud diagnostic already logged at the call
// site), the OPPOSITE of the pre-#589 default. Losing automatic recovery of
// accepted user work is worse than one more permanent-looking row being
// retried an extra few times.
func TestIsPermanentOrphanOutboxDrainError_UnknownErrorIsNotPermanent(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	require.False(t, isPermanentOrphanOutboxDrainError(errors.New("plain failure")))
}

// fakePumpSessionsForCancellationTest wraps a real session.Service (embedded
// via the unexported Service interface, reachable here because this file is
// package session) and forces DrainOrphanOutboxEntry to fail with a
// caller-supplied error while recording whether RecordOrphanOutboxFailure
// was ever invoked -- the actual, callsite-level observable this test needs
// (attempts must stay at 0 / the recorder must never be called), rather than
// re-deriving the answer from the classifier in isolation.
type fakePumpSessionsForCancellationTest struct {
	Service
	drainErr           error
	recordFailureCalls atomic.Int32
}

func (f *fakePumpSessionsForCancellationTest) DrainOrphanOutboxEntry(ctx context.Context, id string) (bool, error) {
	return false, f.drainErr
}

func (f *fakePumpSessionsForCancellationTest) RecordOrphanOutboxFailure(ctx context.Context, id, lastError string) (OrphanOutboxFailureOutcome, error) {
	f.recordFailureCalls.Add(1)
	return f.Service.RecordOrphanOutboxFailure(ctx, id, lastError)
}

// TestProcessOrphanOutboxEntry_PCtxCancellationDoesNotCountAttempt is the
// test task #589's verification discipline specifically asked for: driven
// through processOrphanOutboxEntry (the real production call site), not by
// calling isPermanentOrphanOutboxDrainError or RecordOrphanOutboxFailure
// directly. A prior test in this exact package (p556's original version,
// see that file's own header) was found vacuous specifically because it
// bypassed the call site and tested the recorder in isolation -- deleting
// the production call left the suite green.
//
// Deliberately uses a GENUINE SQLITE_CONSTRAINT error (the one cause
// isPermanentOrphanOutboxDrainError positively recognises as permanent) as
// the injected DrainOrphanOutboxEntry failure, NOT a context error -- if a
// plain context.Canceled were used here, the classifier's own type check
// (errors.As against *sqlite.Error) would already return "not permanent" on
// its own, and this test would pass whether or not the p.ctx short-circuit
// exists, which is exactly the kind of test-doesn't-exercise-what-it-claims
// gap flagged in this session's verification-discipline note. Using a
// constraint-shaped error means the ONLY thing standing between this error
// and a counted attempt is the p.ctx.Err() short-circuit -- so removing
// that short-circuit makes this test fail via the classifier legitimately
// classifying the error as permanent and counting it. See the Revert-check
// below.
//
// Shape: build a real RunQueuePump (so p.ctx/p.cancel/dbWriteTimeout are
// all the genuine production fields, not stand-ins), cancel p.ctx up front
// (simulating Stop() having already fired, or the drain's own 10s budget
// having already expired -- both surface identically as p.ctx.Err() != nil
// at the point processOrphanOutboxEntry checks it), then call
// processOrphanOutboxEntry directly with a Sessions fake whose
// DrainOrphanOutboxEntry returns a genuine constraint violation wrapped the
// same way DrainOrphanOutboxEntry's real EnqueueRunQueueEntry call site
// wraps it ("enqueueing to main queue: %w"). This models the review's exact
// scenario: p.ctx is cancelled (Stop() mid-transaction, or the 10s budget
// expiring) at the same moment the DB layer reports SOME error -- and per
// the review, "cancellation caused by p.ctx must not count at all",
// regardless of what shape the resulting DB error happens to take. Assert
// RecordOrphanOutboxFailure is NEVER called -- not just that the row's
// attempts happen to stay 0, which the AlreadyTerminal/no-op paths could
// also produce for unrelated reasons.
//
// Revert-check: comment out (or hardcode to false) the
// `if p.ctx.Err() != nil { ...; return }` block in
// run_queue_orphan_drain.go's processOrphanOutboxEntry, immediately after
// the slog.Error("...failed to drain orphan outbox entry...") call. Rerun
// this test: it now FAILS, because isPermanentOrphanOutboxDrainError
// legitimately classifies the injected SQLITE_CONSTRAINT error as
// permanent and processOrphanOutboxEntry calls
// p.recordOrphanOutboxDrainFailure, incrementing attempts to 1 -- proving
// the short-circuit (not the classifier) is what this test is pinning.
func TestProcessOrphanOutboxEntry_PCtxCancellationDoesNotCountAttempt(t *testing.T) {
	sess, svc := newTestServiceForCancellationTest(t)
	ctx := context.Background()

	callData := []byte(`{"SessionID":"` + sess.ID + `","Prompt":"continue"}`)
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, "ctx-cancel-1", sess.ID, callData))

	fake := &fakePumpSessionsForCancellationTest{
		Service:  svc,
		drainErr: fmt.Errorf("enqueueing to main queue: %w", realConstraintErrorForCancellationTest(t)),
	}

	pump := NewRunQueuePump(RunQueuePumpConfig{
		Sessions:       fake,
		PumpInstanceID: "test-pump-ctx-cancel",
	})
	// Simulate Stop() already having fired (or the 10s drainCtx deadline
	// already having expired -- both cancel the SAME p.ctx field and are
	// indistinguishable to processOrphanOutboxEntry's p.ctx.Err() check),
	// WITHOUT running the real Stop() shutdown sequence -- this test only
	// needs p.ctx cancelled, not a full graceful-shutdown drain.
	pump.cancel()

	entry, err := svc.GetOrphanOutboxEntry(ctx, "ctx-cancel-1")
	require.NoError(t, err)
	require.NotNil(t, entry)

	pump.processOrphanOutboxEntry(ctx, *entry)

	require.Equal(t, int32(0), fake.recordFailureCalls.Load(),
		"a p.ctx-caused cancellation must never reach RecordOrphanOutboxFailure at all -- it is not merely uncounted, the call site must not even attempt to count it")

	after, err := svc.GetOrphanOutboxEntry(ctx, "ctx-cancel-1")
	require.NoError(t, err)
	require.NotNil(t, after, "the row must still exist and be untouched")
	require.Equal(t, "pending", after.Status, "a shutdown-caused failure must leave the row pending, never quarantined")
	require.Equal(t, int64(0), after.Attempts, "a shutdown-caused failure must not consume any of the retry budget")
}

// TestProcessOrphanOutboxEntry_NonCtxPermanentErrorStillCounts is the
// control for the above: once p.ctx is NOT cancelled, a genuinely permanent
// error (constraint violation) must still reach RecordOrphanOutboxFailure
// exactly as before -- the p.ctx short-circuit must not have accidentally
// swallowed the real quarantine path too.
func TestProcessOrphanOutboxEntry_NonCtxPermanentErrorStillCounts(t *testing.T) {
	sess, svc := newTestServiceForCancellationTest(t)
	ctx := context.Background()

	callData := []byte(`{"SessionID":"` + sess.ID + `","Prompt":"continue"}`)
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, "ctx-live-1", sess.ID, callData))

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

	fake := &fakePumpSessionsForCancellationTest{
		Service:  svc,
		drainErr: fmt.Errorf("enqueueing to main queue: %w", fkErr),
	}

	pump := NewRunQueuePump(RunQueuePumpConfig{
		Sessions:       fake,
		PumpInstanceID: "test-pump-ctx-live",
	})
	t.Cleanup(func() { pump.cancel() })
	// p.ctx is deliberately left live (not cancelled) here.

	entry, err := svc.GetOrphanOutboxEntry(ctx, "ctx-live-1")
	require.NoError(t, err)
	require.NotNil(t, entry)

	pump.processOrphanOutboxEntry(ctx, *entry)

	require.Equal(t, int32(1), fake.recordFailureCalls.Load(),
		"a genuine permanent error with a live p.ctx must still reach RecordOrphanOutboxFailure")

	after, err := svc.GetOrphanOutboxEntry(ctx, "ctx-live-1")
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, int64(1), after.Attempts, "the permanent failure must be counted against the budget")
}
