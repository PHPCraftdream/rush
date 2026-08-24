package session_test

// Regression coverage for task #587/P0-1 of the 2026-08-19 release-readiness
// follow-up review: admitSession's refusal branch used to return
// (nil, nil, false), discarding the entry it had just observed under
// inFlightMu. DrainSessionNow then re-read the CURRENT entry via a SEPARATE
// waitForAdmission call, in its own, later critical section. Between those
// two critical sections an ABA sequence was possible:
//
//  1. background execution A holds the session's admission entry.
//  2. DrainSessionNow's admitSession call is refused (loses the race to A).
//  3. A finishes, closes A.done, and deletes itself from p.inFlight.
//  4. A DIFFERENT execution B is admitted for the SAME session before
//     DrainSessionNow's follow-up waitForAdmission call runs.
//  5. That follow-up call reads B, not A, so DrainSessionNow waits for
//     and classifies B's outcome as if it were A's.
//
// The fix folds the observed entry into admitSession's refusal return value
// itself, read under the SAME lock as the refusal, so there is no window in
// which a second critical section could observe a replacement. This test
// deterministically provokes the exact interleaving above (via the
// TestAfterAdmissionRefusal hook) and proves DrainSessionNow's wait resolves
// against A, not B, which is engineered to NEVER release for the duration
// of the test.
//
// A's outcome is deliberately chosen to be ErrCallQueuedNotExecuted (the
// busy outcome), the ONLY classifyBackgroundOutcome category with
// stopNow=true: the only one where DrainSessionNow returns IMMEDIATELY
// after observing it, without looping to check for more work. Every other
// outcome (including a terminal failure) has stopNow=false and loops back
// to retry admission, which would immediately hit B (still held, never
// released) on the very next iteration regardless of whether the fix is
// present, making the test pass or fail for the wrong reason. A
// timing-based reproduction (DrainSessionNow returns promptly vs. hangs
// until ctx's deadline) is therefore the correct and only signal here: the
// fixed code returns in well under a second (A's outcome was already
// resolved before the wait even began); the pre-fix code hangs on B's
// never-closed done channel until ctx's own deadline.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestDrainSessionNow_AdmissionRefusal_NeverObservesReplacementEntry is the
// ABA regression itself: it forces the exact interleaving task #587/P0-1
// describes and proves DrainSessionNow's wait branch resolves against the
// SAME entry admitSession's refusal handed it, even when that original
// entry is released and REPLACED for the same session before the wait
// begins.
//
// Revert-check (see task prompt): reintroducing the pre-fix "refuse, then
// separately re-look-up the current entry" shape makes this test fail:
// DrainSessionNow ends up waiting on B (which this test deliberately never
// releases) instead of A, so the call blocks until ctx's deadline. The
// VERBATIM failure captured while validating this is recorded in this
// file's trailing comment block.
func TestDrainSessionNow_AdmissionRefusal_NeverObservesReplacementEntry(t *testing.T) {
	sess, svc := setupTestSession(t, "p587-admission-aba")

	gateA := newGatedCoordinator()
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    gateA,
		PumpInstanceID: "test-pump-p587-aba",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	callDataA, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "first"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p587-aba-idem-A", sess.ID, callDataA))

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	// Confirm the background tick is genuinely EXECUTING A (not merely
	// leased) before proceeding, otherwise the race below proves nothing
	// about the interleaving under test.
	select {
	case <-gateA.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing A -- test setup is broken, proves nothing about the race under test")
	}

	// Grab an independent handle on A's own entry, confirming A currently
	// holds admission. This handle lets the hook below wait for A's actual
	// release deterministically, with no timing guess involved.
	entryA, admittedA := pump.AdmitSessionObservedEntryForTest(sess.ID)
	require.False(t, admittedA, "A must already be holding admission for this session")

	// hookRan proves TestAfterAdmissionRefusal actually fired for this
	// call. Without this, a bug that skipped invoking the hook entirely
	// would make the test pass vacuously (DrainSessionNow would just wait
	// on A directly, coincidentally matching the fixed behavior, but for
	// the wrong reason -- the ABA interleaving would never actually have
	// been provoked).
	hookRan := make(chan struct{})
	var bAdmitted bool

	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		defer close(hookRan)
		if sessionID != sess.ID {
			return
		}
		// Step 3: release A with the busy outcome (ErrCallQueuedNotExecuted),
		// the only classifyBackgroundOutcome result with stopNow=true, so a
		// correct wait on A returns IMMEDIATELY once this closes A's done
		// channel, without looping back into a second admission attempt
		// that would hit B regardless of which entry the fix actually
		// waited on.
		gateA.release <- session.ErrCallQueuedNotExecuted

		// Deterministically wait for A's release to have fully landed
		// (done closed, map entry deleted) before admitting B, using
		// entryA's own Done() channel rather than a sleep.
		<-entryA.Done()

		// Step 4: admit a REPLACEMENT entry B for the SAME session, before
		// DrainSessionNow (paused right here, mid-refusal-handling, by this
		// very hook) gets a chance to look anything up again. B is
		// DELIBERATELY never released for the rest of this test -- if
		// DrainSessionNow's wait ends up resolving against B instead of A,
		// it will hang until ctx's own deadline instead of returning
		// promptly, making misattribution observable as a hang/timeout
		// rather than a silent pass.
		_, admittedB := pump.AdmitSessionForTest(sessionID)
		bAdmitted = admittedB
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	start := time.Now()
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	select {
	case <-hookRan:
	case <-time.After(10 * time.Second):
		t.Fatal("TestAfterAdmissionRefusal hook never fired -- DrainSessionNow did not take the refusal branch, proves nothing about the race under test")
	}
	require.True(t, bAdmitted, "B must have been successfully admitted for the same session right after A released -- otherwise step 4 of the ABA sequence never actually happened")

	select {
	case <-drainDone:
	case <-time.After(4 * time.Second):
		t.Fatal("DrainSessionNow did not return within its own ctx deadline -- if this fires, it is itself evidence DrainSessionNow ended up waiting on B (never released) instead of A")
	}
	elapsed := time.Since(start)

	require.NoError(t, drainErr, "A's outcome was busy (ErrCallQueuedNotExecuted), which classifies to a nil error -- a non-nil error here would itself be evidence of an unexpected outcome source")
	require.Equal(t, session.DrainNoWork, result, "A's outcome was busy -- nothing actually ran for it, so the result must be DrainNoWork")
	require.Less(t, elapsed, 3*time.Second, "DrainSessionNow must resolve promptly against A's already-closed done channel -- taking anywhere near ctx's 5s deadline means it waited on B (never released) instead of A, the exact ABA misattribution this test exists to catch")
}

// VERBATIM revert-check output, captured by temporarily reintroducing the
// pre-fix "refuse, then separately call a waitForAdmission-shaped lookup"
// behavior (admitSession's refusal ignoring the entry it observed, and
// DrainSessionNow re-reading p.inFlight[sessionID] in a second, later
// critical section, AFTER the TestAfterAdmissionRefusal hook had already
// released A and admitted B in its place):
//
//	=== RUN   TestDrainSessionNow_AdmissionRefusal_NeverObservesReplacementEntry
//	2026/08/19 18:23:51 INFO run_queue_pump: started instance_id=test-pump-p587-aba
//	    p587_admission_aba_test.go:157: DrainSessionNow did not return within its own ctx deadline -- if this fires, it is itself evidence DrainSessionNow ended up waiting on B (never released) instead of A
//	2026/08/19 18:23:55 WARN run_queue_pump: list pending failed err="context canceled" instance_id=test-pump-p587-aba
//	2026/08/19 18:23:55 INFO run_queue_pump: stopped gracefully instance_id=test-pump-p587-aba
//	    run_queue_round2_test.go:329: pump.Stop() graceful shutdown after 733.9us
//	--- FAIL: TestDrainSessionNow_AdmissionRefusal_NeverObservesReplacementEntry (4.35s)
//	FAIL
//	FAIL	github.com/PHPCraftdream/rush/internal/session	4.512s
//	FAIL
//
// (DrainSessionNow's own ctx timed out at ~4.35s waiting on B's
// admissionEntry, which this test never releases -- exactly the ABA
// misattribution the fix closes. The fix was then restored and the test
// re-run to confirm it passes again -- see the PASS run captured while
// developing this test, 0.31s elapsed, well under the 3s threshold.)
