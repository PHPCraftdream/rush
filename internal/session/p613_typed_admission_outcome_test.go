package session_test

// Regression coverage for task #613 (P0) of the 2026-08-20 read-only release
// review, findings F3 and F4. Both share one root cause: admissionEntry used
// to publish only a bare `error` (internal/session/run_queue_admission.go),
// with no row identity and no outcome kind, so a destructive terminal
// deletion was indistinguishable from a harmless early return (F3), and an
// observed outcome had no row ID a later same-row success could use to clear
// it (F4).
//
// F3 — terminal deletion published as "nothing happened": an
// attempts-exhausted row leased and terminal-failed (DELETEd) WITHOUT ever
// calling executeEntrySync used to release admission with the SAME
// errNoExecutionAttempted sentinel every harmless early return uses. A
// waiting DrainSessionNow mapped that to "nothing ran, loop and inspect
// pending work" without recording a failure. If that waiter had already
// observed an earlier row's success, its next empty pending scan reached
// DrainSessionNow's "nothing pending" bottom path and returned
// (DrainComplete, nil) — exit 0 over a row that was actually discarded to
// dead-letter.
//
// F4 — the mirror-image false failure: an observed outcome with no row ID
// was recorded under a synthetic, per-occurrence `__unattributed_N` ledger
// key (recordUnattributed) that a later SAME-ROW success could never clear,
// even when the row ID was perfectly well known to the execution that
// produced the outcome.
//
// The fix (run_queue_admission.go's admissionOutcome/outcomeKind,
// classifyBackgroundOutcome's rewrite in run_queue_drain_session.go) makes
// both share one typed handoff: {rowID, kind, err}. This file pins the three
// scenarios the task explicitly required, including both terminal-write
// sub-cases for scenario 1.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// exhaustedLeaseService wraps a real session.Service so that leasing ONE
// specific row (by idempotency-key-derived ID, captured after enqueue)
// returns it with Attempts already at RunQueueMaxAttempts — deterministically
// forcing processEntry's/DrainSessionNow's attempts-exhausted terminal-fail
// fast path on the FIRST lease, without looping through RunQueueMaxAttempts
// real retries (the pattern p348 uses, correct but far slower and less
// targeted: this file needs ONE specific row terminal-failed while another
// row succeeds in the SAME call/tick, not every row in the session
// exhausted).
//
// termFailErr, if non-nil, is returned by TerminalFailRunQueueEntry for the
// victim row ID specifically — used to cover BOTH sub-cases the task
// requires for scenario 1: a successful terminal DB write (termFailErr nil)
// and a failed one (termFailErr non-nil).
//
// leaseGate, if non-nil, is signaled (best-effort, non-blocking) the instant
// the victim row is leased, then PARKS that lease call until the test sends
// on release — holding the background tick's admission for the victim row
// open long enough for a concurrent DrainSessionNow call to reliably lose
// the admission race and enter its wait branch, instead of racing a bare
// goroutine-scheduling window (flaky) or running DrainSessionNow only AFTER
// the background tick has already fully resolved everything, which would
// prove nothing about the wait path (see this file's own scenario-1
// revision note).
type exhaustedLeaseService struct {
	session.Service
	victimID    string
	termFailErr error
	leaseGate   chan struct{}
	leaseEnter  chan struct{}
}

func (s *exhaustedLeaseService) LeaseRunQueueEntry(ctx context.Context, sessionID, leasedBy string, leaseTTL time.Duration) (*session.RunQueueEntry, error) {
	entry, err := s.Service.LeaseRunQueueEntry(ctx, sessionID, leasedBy, leaseTTL)
	if err != nil || entry == nil {
		return entry, err
	}
	if entry.ID == s.victimID {
		clone := *entry
		clone.Attempts = session.RunQueueMaxAttempts
		if s.leaseGate != nil {
			select {
			case s.leaseEnter <- struct{}{}:
			default:
			}
			<-s.leaseGate
		}
		return &clone, nil
	}
	return entry, nil
}

// ListPendingRunQueueEntries also reports the victim row as exhausted:
// processEntry's own attempts-exhausted check (run_queue_entry_dispatch.go)
// reads entry.Attempts from THIS scan, not from the later lease — tick()
// passes the scanned copy straight into processEntry, which only leases
// AFTER deciding whether to take the attempts-exhausted branch. Without this
// override, the background tick would lease and genuinely EXECUTE the
// victim row instead of terminal-failing it without ever calling
// Coordinator.Run, which is the exact scenario under test.
func (s *exhaustedLeaseService) ListPendingRunQueueEntries(ctx context.Context) ([]session.RunQueueEntry, error) {
	entries, err := s.Service.ListPendingRunQueueEntries(ctx)
	if err != nil {
		return entries, err
	}
	for i := range entries {
		if entries[i].ID == s.victimID {
			entries[i].Attempts = session.RunQueueMaxAttempts
		}
	}
	return entries, nil
}

func (s *exhaustedLeaseService) TerminalFailRunQueueEntry(ctx context.Context, id, leasedBy string) error {
	if id == s.victimID && s.termFailErr != nil {
		return s.termFailErr
	}
	return s.Service.TerminalFailRunQueueEntry(ctx, id, leasedBy)
}

// enqueueRowsAndCaptureIDs enqueues rows A and B (FIFO) for sessionID and
// returns row A's durable ID, captured unambiguously before either row is
// touched by anything else.
func enqueueRowsAndCaptureIDs(t *testing.T, name string, sessions session.Service, sessionID string) (rowAID string) {
	t.Helper()
	callDataA, err := json.Marshal(map[string]any{"SessionID": sessionID, "Prompt": "row-A"})
	require.NoError(t, err)
	require.NoError(t, sessions.EnqueueRunQueueEntry(context.Background(), name+"-row-A", sessionID, callDataA))

	pending, err := sessions.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "row A must be the only pending entry at this point, so its ID can be captured unambiguously")
	rowAID = pending[0].ID

	callDataB, err := json.Marshal(map[string]any{"SessionID": sessionID, "Prompt": "row-B"})
	require.NoError(t, err)
	require.NoError(t, sessions.EnqueueRunQueueEntry(context.Background(), name+"-row-B", sessionID, callDataB))
	return rowAID
}

// runScenario1 is shared by both terminal-write sub-cases: a background
// tick (a DIFFERENT admitted execution, not this test's own DrainSessionNow
// call) terminal-fails exhausted row A while a concurrent DrainSessionNow
// call has already lost the admission race and is genuinely WAITING on A's
// admissionEntry (proven via leaseEnter/leaseGate, not assumed from timing).
// Row B then succeeds — locally leased by this call's own retry, or
// observed via a second wait (either is a valid resolution of the same
// contract, mirroring p592's own framing). The call must NOT return
// DrainComplete: a terminal deletion is a row-scoped failure, not a no-op.
//
// Earlier revision of this test ran DrainSessionNow only AFTER the
// background tick had already fully resolved both rows -- which proved
// nothing about the wait path DrainSessionNow's F3 fix lives in: with
// nothing in flight and nothing pending, DrainSessionNow's own scan
// legitimately returns DrainNoWork without ever observing row A's fate, so
// require.NotEqual(DrainComplete) passed vacuously on BOTH the pre-fix and
// post-fix code, and the terminal-deletion outcome was never exercised
// through classifyBackgroundOutcome at all. The leaseGate below closes that
// gap: it holds the background tick's admission for row A open from the
// instant it is leased until this test has confirmed DrainSessionNow lost
// the admission race and is parked on otherEntry.done.
func runScenario1(t *testing.T, name string, termFailErr error) {
	t.Helper()
	sess, svc := setupTestSession(t, name)
	rowAID := enqueueRowsAndCaptureIDs(t, name, svc, sess.ID)
	wrapped := &exhaustedLeaseService{
		Service:     svc,
		victimID:    rowAID,
		termFailErr: termFailErr,
		leaseGate:   make(chan struct{}),
		leaseEnter:  make(chan struct{}, 1),
	}

	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       wrapped,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-" + name,
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	var refusals atomic.Int64
	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		if sessionID == sess.ID {
			refusals.Add(1)
		}
	})

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	// Wait for the background tick to have leased row A and parked on
	// leaseGate, holding admission for the session open.
	select {
	case <-wrapped.leaseEnter:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never leased row A — test setup is broken, proves nothing about the scenario under test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Confirm THIS call was genuinely refused admission (forced to wait on
	// row A's admissionEntry) before releasing the background tick to
	// proceed with the terminal-fail write. Without this, DrainSessionNow
	// might not yet be waiting when leaseGate is released, and would then
	// race the background tick for row B directly instead of observing row
	// A's outcome via the wait path this scenario is about.
	require.Eventually(t, func() bool {
		return refusals.Load() >= 1
	}, 5*time.Second, 5*time.Millisecond, "DrainSessionNow was never refused admission for row A — test setup is broken, proves nothing about the wait path under test")

	close(wrapped.leaseGate) // let the background tick proceed to terminal-fail row A, then execute row B

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after the background tick resolved row A and row B")
	}

	require.NotEqual(t, session.DrainComplete, result, "row A was terminally discarded (dead-lettered) by a DIFFERENT admitted execution — a waiting DrainSessionNow reporting DrainComplete here is exit 0 over a destructively lost row, the exact F3 defect")
	require.Error(t, drainErr, "a terminal deletion is a row-scoped failure, not a no-op")

	// ListPendingRunQueueEntries only scans status='pending', so it reads
	// empty in BOTH sub-cases: when the terminal DELETE succeeds, row A is
	// gone entirely; when the terminal DELETE itself fails, row A is still
	// physically present but stuck 'leased' (F3's "unconfirmed, not no-op"
	// case), which is equally invisible to a pending-only scan. Either way,
	// row B is acked (gone). This assertion only confirms nothing is left
	// PENDING, not that row A no longer exists in the termFailErr != nil
	// sub-case -- see runScenario1's own doc.
	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "row B is acked and row A (terminal-failed or stuck leased) is not 'pending' either way -- nothing left PENDING for this session")
}

// TestDrainSessionNow_F3_TerminalDeleteSucceeds_ForeignExhaustedRow_NeverReportsComplete
// is scenario 1's first sub-case: the terminal DELETE write itself succeeds.
//
// Revert-check: publish noRowTouched() instead of
// terminalDeletedOutcome(leased.ID, termErr) at the attempts-exhausted
// release site in run_queue_entry_dispatch.go (processEntry's background
// path) — this test fails on the require.NotEqual below with DrainComplete
// reported.
func TestDrainSessionNow_F3_TerminalDeleteSucceeds_ForeignExhaustedRow_NeverReportsComplete(t *testing.T) {
	runScenario1(t, "p613-f3-term-ok", nil)
}

// TestDrainSessionNow_F3_TerminalDeleteFails_ForeignExhaustedRow_NeverReportsComplete
// is scenario 1's second sub-case: the terminal DELETE write ITSELF fails —
// F3's "second bad branch". The row's fate is unconfirmed, not a no-op.
//
// Revert-check: same site as above — the pre-fix code published the SAME
// errNoExecutionAttempted default regardless of whether the terminal write
// succeeded or failed, discarding the DB error entirely.
func TestDrainSessionNow_F3_TerminalDeleteFails_ForeignExhaustedRow_NeverReportsComplete(t *testing.T) {
	runScenario1(t, "p613-f3-term-fail", errors.New("p613: disk on fire during row A's terminal fail"))
}

// runScenario1b is scenario 1's SECOND admitted-actor shape: instead of the
// background tick's processEntry terminal-failing exhausted row A (covered
// by runScenario1 above), a DIFFERENT, concurrent DrainSessionNow call
// leases and terminal-fails row A directly through DrainSessionNow's OWN
// attempts-exhausted branch (run_queue_drain_session.go, the
// `if leased.Attempts >= RunQueueMaxAttempts` block a few lines after the
// lease attempt) -- the task's "another admitted drain" half of "another
// admitted drain / background worker terminally valid[sic] exhausted B".
// This is a DISTINCT release site from processEntry's own, and was found
// uncovered by any existing or newly-added test until this scenario was
// added specifically to close that gap -- see this file's own task report
// for the revert-check that caught it.
//
// No background tick runs at all here (TestTick is left at its zero value,
// i.e. production interval, and Start() is never called) -- both
// DrainSessionNow calls race each other directly, so admission for the
// session is won by exactly one of them, deterministically forcing the
// OTHER to enter the observed-admission wait branch.
func runScenario1b(t *testing.T, name string, termFailErr error) {
	t.Helper()
	sess, svc := setupTestSession(t, name)
	rowAID := enqueueRowsAndCaptureIDs(t, name, svc, sess.ID)
	wrapped := &exhaustedLeaseService{
		Service:     svc,
		victimID:    rowAID,
		termFailErr: termFailErr,
		leaseGate:   make(chan struct{}),
		leaseEnter:  make(chan struct{}, 1),
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       wrapped,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-" + name,
	})
	stopPumpLoggingForcedShutdown(t, pump)

	var refusals atomic.Int64
	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		if sessionID == sess.ID {
			refusals.Add(1)
		}
	})

	// The FIRST DrainSessionNow call: wins admission, leases row A (the
	// exhausted one -- FIFO order guarantees A is leased before B), and
	// parks inside LeaseRunQueueEntry on leaseGate, holding admission open.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel1()
	firstDone := make(chan struct{})
	var firstResult session.DrainResult
	var firstErr error
	go func() {
		defer close(firstDone)
		firstResult, firstErr = pump.DrainSessionNow(ctx1, sess.ID)
	}()

	select {
	case <-wrapped.leaseEnter:
	case <-time.After(5 * time.Second):
		t.Fatal("first DrainSessionNow call never leased row A — test setup is broken, proves nothing about the scenario under test")
	}

	// The SECOND, concurrent DrainSessionNow call: must lose the admission
	// race against the first and wait on row A's admissionEntry.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	secondDone := make(chan struct{})
	var secondResult session.DrainResult
	var secondErr error
	go func() {
		defer close(secondDone)
		secondResult, secondErr = pump.DrainSessionNow(ctx2, sess.ID)
	}()

	require.Eventually(t, func() bool {
		return refusals.Load() >= 1
	}, 5*time.Second, 5*time.Millisecond, "the second DrainSessionNow call was never refused admission — test setup is broken, proves nothing about the wait path under test")

	close(wrapped.leaseGate) // let the first call proceed to terminal-fail row A, then execute row B

	select {
	case <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("first DrainSessionNow call did not return")
	}
	select {
	case <-secondDone:
	case <-time.After(10 * time.Second):
		t.Fatal("second DrainSessionNow call did not return after observing the first call's outcome")
	}

	// The FIRST call's own return value already carried row A's failure
	// correctly before task #613 (its own ledger records it directly, not
	// through the admission handoff this task fixes) -- the defect under
	// test is specifically whether the SECOND, waiting call learns about it
	// too.
	require.Error(t, firstErr, "the first call's own ledger must report row A's terminal failure regardless of task #613's fix")
	require.NotEqual(t, session.DrainComplete, firstResult)

	require.NotEqual(t, session.DrainComplete, secondResult, "row A was terminally discarded by a DIFFERENT admitted DrainSessionNow call — the second, waiting call reporting DrainComplete here is exit 0 over a destructively lost row (F3, DrainSessionNow's OWN attempts-exhausted release site)")
	require.Error(t, secondErr, "a terminal deletion is a row-scoped failure, not a no-op, even when observed via a wait rather than executed locally")

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "row B is acked and row A (terminal-failed or stuck leased) is not 'pending' either way")
}

// TestDrainSessionNow_F3_OwnTerminalDeleteSucceeds_ObservedByConcurrentDrain_NeverReportsComplete
// is runScenario1b's first sub-case: the terminal DELETE write itself
// succeeds.
//
// Revert-check: replace releaseSession(terminalDeletedOutcome(leased.ID,
// termErr)) with releaseSession(noRowTouched()) at DrainSessionNow's OWN
// attempts-exhausted release site (run_queue_drain_session.go) — this test
// fails on the require.NotEqual(secondResult) below with DrainComplete
// reported for the second, waiting call. This is a DIFFERENT release site
// than runScenario1's (processEntry's own, in run_queue_entry_dispatch.go)
// — reverting THAT site alone does not affect this test, and reverting
// THIS site alone does not affect runScenario1's tests; see the task report
// for both single-site reverts run independently.
func TestDrainSessionNow_F3_OwnTerminalDeleteSucceeds_ObservedByConcurrentDrain_NeverReportsComplete(t *testing.T) {
	runScenario1b(t, "p613-f3-own-term-ok", nil)
}

// TestDrainSessionNow_F3_OwnTerminalDeleteFails_ObservedByConcurrentDrain_NeverReportsComplete
// is runScenario1b's second sub-case: the terminal DELETE write ITSELF
// fails.
func TestDrainSessionNow_F3_OwnTerminalDeleteFails_ObservedByConcurrentDrain_NeverReportsComplete(t *testing.T) {
	runScenario1b(t, "p613-f3-own-term-fail", errors.New("p613: disk on fire during row A's terminal fail (DrainSessionNow's own lease)"))
}

// TestDrainSessionNow_F4_ObservedRetryable_ThenLocalSuccess_ReportsComplete
// is scenario 2: an observed retryable failure for row A (background tick
// executes it, Coordinator.Run returns an ordinary retryable error, A is
// nacked back to pending), followed by A's SAME-ROW success — this test's own
// DrainSessionNow call re-leases and commits A itself. The call must report
// DrainComplete once the queue is genuinely empty: the observed failure must
// have been recorded under row A's OWN id (not a synthetic
// __unattributed_N key), so the later recordSuccess(rowID) for that exact
// row can clear it.
//
// Revert-check: at the observed-admission branch in
// run_queue_drain_session.go's DrainSessionNow (the `outcomeDrained` handling
// right after the wait on otherEntry.done), replace the row-ID-aware
// recordFailure/recordSuccess switch with the pre-#613 unconditional
// recordUnattributed(outcomeErr) — this test then fails on require.Equal
// below with DrainFailed reported instead of DrainComplete, because the
// synthetic key can never be cleared by A's own later success.
func TestDrainSessionNow_F4_ObservedRetryable_ThenLocalSuccess_ReportsComplete(t *testing.T) {
	sess, svc := setupTestSession(t, "p613-f4-retry-then-local-success")
	gate := newSequentialGatedCoordinator(1)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-p613-f4-retry-local",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row-A"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p613-f4-retry-local-row-A", sess.ID, callData))

	// Deterministic proof that THIS call was genuinely refused admission and
	// entered the observed-admission wait branch before row A's retryable
	// failure resolves — the hook below fires ONLY from that branch
	// (run_queue_drain_session.go's !admitted path). Without this wait, the
	// 150ms sleep this test used to rely on could expire before
	// DrainSessionNow even attempted admission, and the call would then lease
	// and execute row A ITSELF after the nack — resolving the queue through
	// the local-execution branch, never observing any outcome, and passing
	// both final assertions while the observed-admission branch never ran at
	// all.
	refusalCh := make(chan struct{}, 8)
	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		if sessionID == sess.ID {
			select {
			case refusalCh <- struct{}{}:
			default:
			}
		}
	})

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing row A — test setup is broken, proves nothing about the scenario under test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Park on the refusal before resolving row A's execution with an ordinary
	// retryable failure (nacked back to pending, not terminal): only now is
	// it guaranteed that this call will observe that failure through the
	// wait on otherEntry.done rather than executing row A locally.
	select {
	case <-refusalCh:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainSessionNow was never refused admission — test setup is broken, proves nothing about the observed-admission path under test")
	}
	retryableErr := errors.New("p613: simulated transient provider failure")
	gate.gates[0] <- retryableErr

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return — row A's observed retryable failure or its own later retry never resolved")
	}

	require.NoError(t, drainErr, "row A's observed retryable failure must be cleared by this SAME call's own later success at retrying it")
	require.Equal(t, session.DrainComplete, result, "the queue is genuinely empty after row A's own retry committed — the observed failure must have been recorded under row A's OWN id, not a synthetic unattributed key that a same-row success can never clear (F4)")

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "row A must be gone (acked by this call's own retry)")
}

// TestDrainSessionNow_F4_ObservedRetryable_ThenObservedSuccess_ReportsComplete
// is scenario 3: an observed retryable failure for row A, followed by a
// LATER observed success for the SAME row A — both resolutions arrive via a
// wait on someone else's admissionEntry (the background tick executes row A
// twice: first a retryable failure, then, once nacked back to pending, the
// tick's own next scan re-leases and commits it), never via this
// DrainSessionNow call's own local lease. The call must still report
// DrainComplete once the queue is genuinely empty.
//
// Revert-check: same site as scenario 2 — the pre-#613 unconditional
// recordUnattributed makes this fail identically, since NEITHER resolution
// carries a row ID under the old code, regardless of whether either arrives
// via a local execution or a wait.
func TestDrainSessionNow_F4_ObservedRetryable_ThenObservedSuccess_ReportsComplete(t *testing.T) {
	sess, svc := setupTestSession(t, "p613-f4-retry-then-observed-success")
	gate := newSequentialGatedCoordinator(2)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-p613-f4-retry-observed",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row-A"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p613-f4-retry-observed-row-A", sess.ID, callData))

	refusalCh := make(chan struct{}, 8)
	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		if sessionID == sess.ID {
			select {
			case refusalCh <- struct{}{}:
			default:
			}
		}
	})

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing row A — test setup is broken, proves nothing about the scenario under test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Confirm this call was genuinely refused admission at least once before
	// resolving row A's first (retryable) attempt — otherwise this call
	// might lease row A itself, which is the scenario-2 contract, not this
	// one.
	select {
	case <-refusalCh:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainSessionNow was never refused admission — test setup is broken, proves nothing about the observed-admission path under test")
	}

	retryableErr := errors.New("p613: simulated transient provider failure (first attempt)")
	gate.gates[0] <- retryableErr

	// Give the background tick's own next scan time to re-lease row A (now
	// back in pending) and this call time to lose the admission race AGAIN
	// before resolving row A's second attempt with a clean success.
	time.Sleep(150 * time.Millisecond)
	gate.gates[1] <- nil

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return — row A's two observed outcomes never resolved")
	}

	require.GreaterOrEqual(t, gate.calls, 2, "the background tick must have executed row A twice: the retryable failure, then the same-row success")
	require.NoError(t, drainErr, "row A's later observed success (same row) must clear its earlier observed retryable failure")
	require.Equal(t, session.DrainComplete, result, "the queue is genuinely empty after row A's second, successful observed execution — both outcomes must have been recorded under row A's OWN id for the later one to supersede the earlier one (F4)")

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "row A must be gone (acked by the background tick's second, successful attempt)")
}
