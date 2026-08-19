package session_test

// Regression coverage for the orphan-outbox poison-row finding in the
// 2026-08-18 release-readiness review (task #556), corrected by the
// 2026-08-19 review (task #571) after that review CONFIRMED, by deleting
// the production call and watching the suite stay green, that the original
// version of this file was vacuous:
//
//   - The original test called svc.RecordOrphanOutboxFailure directly and
//     never reached processOrphanOutboxEntry (run_queue_orphan_drain.go) at
//     all -- deleting the recordOrphanOutboxDrainFailure call at that
//     function's only production call site left the suite green, the exact
//     revert the test's own "Revert-check" comment claimed would fail.
//   - The row it built was also not actually poison: []byte("this is not
//     json") for a VALID session ID drains into session_run_queue just
//     fine, because DrainOrphanOutboxEntry carries call_data across as an
//     opaque TEXT column with no format validation. Nothing in the
//     original suite constructed a row the drain's own INSERT would
//     actually keep failing on.
//
// This version drives the real pump (Start() -> drainOrphanOutbox ->
// processOrphanOutboxEntry -> DrainOrphanOutboxEntry) against a row that IS
// genuinely un-enqueueable: the INSERT into session_run_queue enforces
// `session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE`
// (20260809000001_add_session_run_queue.sql), so a row whose session_id
// does not exist in `sessions` fails that INSERT with a real SQLite
// FOREIGN KEY constraint violation every single time, forever -- no amount
// of retrying changes the outcome, because the row's OWN data is what is
// wrong.
//
// That specific shape cannot be produced through WriteToOrphanOutbox: the
// orphan_call_outbox table carries the IDENTICAL
// `session_id ... REFERENCES sessions (id) ON DELETE CASCADE` constraint
// (20260812000001_add_orphan_call_outbox.sql), so WriteToOrphanOutbox
// itself already rejects a bogus session_id with the same FK violation
// (confirmed directly: session.Service.WriteToOrphanOutbox against a real
// db.Connect()-opened database, foreign_keys=ON as connect.go's own pragma
// map sets it, returns "constraint failed: FOREIGN KEY constraint failed"
// for a nonexistent session_id) -- and a session that legitimately existed
// and was later deleted cascades its outbox row away in the SAME
// transaction as the delete (session.Delete's own single BeginTx/Commit),
// so there is no window where a real session deletion leaves a stale
// outbox row behind either. This test therefore builds the poison row by
// inserting it directly against the raw *sql.DB with foreign_keys
// temporarily OFF for that one statement -- a faithful simulation of a row
// that reached the table through a path other than the validated
// WriteToOrphanOutbox API (a future write path lacking the same check, a
// restored backup, manual DB surgery) rather than a claim about how likely
// that path is. Once such a row exists, DrainOrphanOutboxEntry's own INSERT
// has no way to know the difference, and retries it exactly as it would
// retry any other pending entry.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// This test opens its database through the SAME db.Connect path production
// uses (not a hand-rolled inline schema), so foreign_keys=ON and every
// other pragma in connect.go's own map actually apply -- required for the
// poison row below to behave as a genuine, permanent FK violation rather
// than silently succeeding the way it would against the package's other
// test harnesses (run_queue_round2_test.go's setupTestSession and
// p1_2_lease_loss_test.go's setupTestSessionWithDB both open their DB via a
// bare sql.Open with an inline CREATE TABLE, which never issues `PRAGMA
// foreign_keys = ON` -- confirmed directly: a bogus session_id INSERT
// against either of those succeeds with no error at all, since SQLite
// defaults foreign key enforcement to OFF per-connection).
func TestOrphanOutbox_PoisonEntryQuarantinesInsteadOfRetryingForever(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	dataDir := t.TempDir()
	ctx := context.Background()

	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })

	q := db.New(conn)
	svc := session.NewService(q, conn)

	_, err = svc.Create(ctx, "orphan-poison")
	require.NoError(t, err)

	// A genuinely un-enqueueable row: session_id references no row in
	// `sessions` at all. session_run_queue's FK makes
	// DrainOrphanOutboxEntry's inner INSERT fail this way every time -- see
	// this file's header for why WriteToOrphanOutbox itself cannot be used
	// to build it (its own identical FK rejects it immediately) and why a
	// real session delete cannot produce it either (CASCADE removes the
	// outbox row atomically in the same transaction). Built by inserting
	// directly against the raw connection with foreign_keys temporarily OFF
	// for this one statement, then restored immediately after -- the row is
	// invalid DATA, not a side effect of a weakened connection; everything
	// downstream (DrainOrphanOutboxEntry's own transaction) still runs
	// under full FK enforcement.
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		`INSERT INTO orphan_call_outbox (id, session_id, call_data, status, attempts, max_attempts, created_at, updated_at) VALUES (?, ?, ?, 'pending', 0, 5, ?, ?)`,
		"poison-1", "session-that-does-not-exist", `{"SessionID":"session-that-does-not-exist","Prompt":"x"}`, 1000, 1000)
	require.NoError(t, err, "constructing the poison row directly must succeed (foreign_keys is off for this one statement)")
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)

	pending, err := svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the entry must start out pending")
	budget := pending[0].MaxAttempts
	require.Greater(t, budget, int64(0), "the schema must define a retry budget for this test to mean anything")

	// Drive the REAL pump through drainOrphanOutbox -> processOrphanOutboxEntry
	// -> DrainOrphanOutboxEntry, one simulated tick per iteration, exactly as
	// production does -- not svc.RecordOrphanOutboxFailure called directly.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    nil, // no main-queue coordinator needed; this only exercises the orphan drain
		PumpInstanceID: "test-pump-poison",
		TestTick:       func() time.Duration { return time.Hour }, // keep the main tick out of the way
		TestDrainTick:  func() time.Duration { return 20 * time.Millisecond },
	})
	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	// Each drain tick attempts the row, gets a real FK violation from
	// DrainOrphanOutboxEntry, and counts one attempt against the budget.
	// Wait for the row to leave the pending scan (quarantined) rather than
	// asserting on a fixed number of ticks -- the exact tick count needed
	// depends only on budget/DrainTick, but Eventually is the correct
	// primitive for "N background ticks must have fired" regardless.
	require.Eventually(t, func() bool {
		pending, err := svc.ListPendingOrphanOutboxEntries(ctx)
		return err == nil && len(pending) == 0
	}, 5*time.Second, 20*time.Millisecond,
		"a genuinely un-enqueueable entry must eventually leave the pending scan (be quarantined), or the drain tick retries it forever")

	// But it is parked, not deleted -- an operator can still see it and why.
	stored, err := svc.GetOrphanOutboxEntry(ctx, "poison-1")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, "failed", stored.Status)
	require.True(t, stored.LastError.Valid, "the reason must be recorded on the row, not only in the log")
	require.Contains(t, stored.LastError.String, "FOREIGN KEY", "the recorded failure must be the real DrainOrphanOutboxEntry error, not a synthetic message")
	require.GreaterOrEqual(t, stored.Attempts, budget, "quarantine must not fire before the budget is actually spent")

	// And it cannot be counted again or resurrected by a later tick.
	pump.Stop()
	after, err := svc.RecordOrphanOutboxFailure(ctx, "poison-1", "another tick")
	require.NoError(t, err)
	require.True(t, after.AlreadyTerminal, "a terminal row must not be re-counted")
}

// transientBusySessions wraps a real session.Service and makes
// DrainOrphanOutboxEntry return a GENUINE, driver-issued SQLITE_BUSY error
// (not a synthetic one -- see TestIsTransientOrphanOutboxDrainError_SQLiteBusyIsTransient's
// own doc for why a real one is used everywhere in this round) for the
// first N calls, then delegates. Used to prove the transient/permanent
// classification (task #571 part c) end to end: a run of SQLITE_BUSY
// failures across drain ticks must NOT count against max_attempts, unlike
// the permanent FK-violation case covered above.
type transientBusySessions struct {
	session.Service
	busyErr error

	// failuresLeft is written by the pump's background goroutine, read by
	// the test's polling goroutine -- must be atomic. It gates which call
	// returns the injected transient error vs falls through to the real
	// service, but it is NOT a safe thing for a test to poll on by itself
	// (see transientFailuresIssued below): Add(-1) going negative only
	// means the 9th (real) call has STARTED, not that the 8 injected
	// failures have all been durably recorded and nothing else has
	// happened yet -- by the time a poller observes that transition, the
	// real 9th call may already have completed and moved the row out of
	// the orphan outbox entirely, which is a perfectly correct outcome
	// but not the intermediate state a poller keyed on failuresLeft alone
	// would expect to observe.
	failuresLeft atomic.Int32

	// transientFailuresIssued counts calls that actually returned the
	// injected transient error, incremented BEFORE the return so it is
	// only ever observed once that specific call's error is already on
	// its way back to the caller. Task #583 (second attempt): the
	// original test polled failuresLeft<=0 directly and, once in roughly
	// 1-in-3 full-package -race runs, observed the row already gone
	// (drained by the real 9th call, which is a legitimate outcome of
	// that same failuresLeft transition) instead of still pending with
	// zero attempts -- both are valid depending on exactly when the
	// racing 9th call's own transaction lands relative to the test's
	// read, so failuresLeft<=0 was never a safe signal for "the 8
	// injected failures happened AND nothing else has yet" in the first
	// place. Waiting for this counter to reach the injected failure count
	// is satisfied strictly BEFORE the 9th (non-intercepted) call can
	// even begin, closing that window instead of just documenting it.
	transientFailuresIssued atomic.Int32

	// releaseNinthCall, when non-nil, is closed by the test to let the
	// first non-intercepted (9th) DrainOrphanOutboxEntry call proceed.
	// Read once failuresLeft has gone negative (i.e. exactly when this
	// call would otherwise immediately fall through to the real
	// service), so the row is guaranteed to still reflect only the 8
	// injected failures for as long as the test chooses to hold the gate
	// shut. NEEDED because transientFailuresIssued alone is only a lower
	// bound on when the 9th call could start, not a guarantee about when
	// a polling goroutine actually observes it -- confirmed directly:
	// switching this test from polling failuresLeft to polling
	// transientFailuresIssued (task #583's first fix attempt) narrowed
	// the original race but did not close it, and the SAME "Expected
	// value not to be nil" failure reappeared under full-package -race
	// load with that fix alone in place: the 9th call can start AND
	// finish in the gap between two polls of a scheduling-starved test
	// goroutine. This channel is the actual fix -- a real synchronization
	// point instead of a poll racing wall-clock scheduling.
	releaseNinthCall chan struct{}
}

func (s *transientBusySessions) DrainOrphanOutboxEntry(ctx context.Context, id string) (bool, error) {
	if s.failuresLeft.Add(-1) >= 0 {
		s.transientFailuresIssued.Add(1)
		return false, fmt.Errorf("enqueueing to main queue: %w", s.busyErr)
	}
	if s.releaseNinthCall != nil {
		<-s.releaseNinthCall
	}
	return s.Service.DrainOrphanOutboxEntry(ctx, id)
}

// TestOrphanOutbox_TransientBusyDoesNotCountAgainstBudget is the P1 fix
// this round adds (task #571 part c): max_attempts=5
// (20260812000001_add_orphan_call_outbox.sql) with no backoff between
// ticks means five ordinary SQLITE_BUSY hits -- exactly what several
// concurrent `crush run` processes each running an immediate
// Start()-time drain would produce -- used to permanently quarantine a
// perfectly healthy, still-queued user call. This drives MORE than
// max_attempts consecutive transient failures through the real pump and
// asserts the row is still pending (not quarantined, attempts still 0)
// afterward, then lets the next attempt succeed to prove the row was
// never actually broken.
func TestOrphanOutbox_TransientBusyDoesNotCountAgainstBudget(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "orphan-transient-busy")
	ctx := context.Background()

	callData := []byte(`{"SessionID":"` + sess.ID + `","Prompt":"continue"}`)
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, "transient-1", sess.ID, callData))

	busyErr := realSQLiteBusyErrorForTest(t)
	// More than max_attempts (5) consecutive transient failures: if these
	// were being counted, the row would quarantine well before this many
	// ticks. failuresLeft is generous specifically to make that failure
	// mode visible rather than borderline.
	wrapped := &transientBusySessions{Service: svc, busyErr: busyErr, releaseNinthCall: make(chan struct{})}
	wrapped.failuresLeft.Store(8)

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       wrapped,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-transient-busy",
		TestTick:       func() time.Duration { return time.Hour },
		TestDrainTick:  func() time.Duration { return 10 * time.Millisecond },
	})
	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	// Wait for EXACTLY the 8 injected transient failures to have been
	// ISSUED (their error already returned to the caller). This alone
	// only bounds the EARLIEST point the 9th (real) call could start --
	// releaseNinthCall (held shut until this test explicitly closes it,
	// below) is what actually keeps that 9th call from touching the row
	// before the read below runs, no matter how long this goroutine
	// takes to get scheduled. See releaseNinthCall's own doc for why a
	// poll on transientFailuresIssued alone (task #583's first fix
	// attempt here) was not sufficient by itself.
	require.Eventually(t, func() bool {
		return wrapped.transientFailuresIssued.Load() >= 8
	}, 3*time.Second, 10*time.Millisecond, "all injected transient failures must have been issued")

	// The row must STILL be pending with NO attempts counted -- transient
	// failures are excluded from the budget entirely, not merely slow to
	// exhaust it. Safe to read now: releaseNinthCall is still shut, so the
	// 9th (real, non-intercepted) call is blocked inside
	// DrainOrphanOutboxEntry and cannot have touched the row yet.
	stored, err := svc.GetOrphanOutboxEntry(ctx, "transient-1")
	require.NoError(t, err)
	require.NotNil(t, stored, "the row must not have been quarantined or lost")
	require.Equal(t, "pending", stored.Status, "repeated transient DB contention must never quarantine a healthy row (last_error=%q)", stored.LastError.String)
	require.Equal(t, int64(0), stored.Attempts, "transient failures must not be counted against max_attempts at all (last_error=%q)", stored.LastError.String)

	// Now let the 9th call (and the row's genuine drain to the main
	// queue) proceed -- the assertions above have already observed the
	// state they need to, so there is nothing left to protect.
	close(wrapped.releaseNinthCall)

	// And now that the injected failures are exhausted, the row drains
	// normally on the very next tick -- proving it was never actually
	// broken, only unlucky with DB contention.
	require.Eventually(t, func() bool {
		pending, err := svc.ListPendingOrphanOutboxEntries(ctx)
		return err == nil && len(pending) == 0
	}, 3*time.Second, 10*time.Millisecond, "the row must drain normally once transient contention clears")
}

// realSQLiteBusyErrorForTest is the session_test-package counterpart of
// realSQLiteBusyError (package session, p571_transient_classifier_test.go)
// -- duplicated rather than shared because the two live in different Go
// packages (session vs session_test) and this file has no access to an
// unexported helper across that boundary. Produces a genuine, driver-issued
// *sqlite.Error with code SQLITE_BUSY by racing two independent raw
// connections to the same on-disk file with the second's busy_timeout set
// to 0, so it fails immediately with no sleep/retry loop.
func realSQLiteBusyErrorForTest(t *testing.T) error {
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

// TestOrphanOutbox_HealthyEntryStillDrains is the control. Quarantine must
// not have made the ordinary path harder to reach: an entry that CAN be
// enqueued still moves to the main run queue untouched by any of this.
func TestOrphanOutbox_HealthyEntryStillDrains(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "orphan-healthy")
	ctx := context.Background()

	callData := []byte(`{"SessionID":"` + sess.ID + `","Prompt":"continue"}`)
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, "healthy-1", sess.ID, callData))

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-orphan-healthy",
		TestTick:       func() time.Duration { return time.Hour },
		TestDrainTick:  func() time.Duration { return 5 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	require.Eventually(t, func() bool {
		pending, err := svc.ListPendingOrphanOutboxEntries(ctx)
		return err == nil && len(pending) == 0
	}, 5*time.Second, 20*time.Millisecond, "a healthy entry must still drain out of the outbox")

	// GetOrphanOutboxEntry reports a missing row as (nil, nil), not as an
	// error — so the assertion is on the value, not on err.
	stored, err := svc.GetOrphanOutboxEntry(ctx, "healthy-1")
	require.NoError(t, err)
	require.Nil(t, stored, "a drained entry is deleted from the outbox, not quarantined")
}
