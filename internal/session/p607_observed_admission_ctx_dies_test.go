package session_test

// Regression coverage for the SECOND of task #607/P0's two ctx-death exit
// sites in DrainSessionNow: the admission-wait branch's `case <-ctx.Done()`
// (the sibling of the top-of-loop `if ctx.Err() != nil` check pinned by
// p607_ctx_dies_after_success_test.go). Both sites shared the exact same
// defect -- a bare `ledger.verdict(false)` call that trusted the ledger's
// pre-existing state instead of recording THIS call's own reason for
// giving up -- and both are fixed by routing through the SAME
// rowLedger.verdictOnCtxDone helper. This file exists because the task's
// revert-check requirement is per-site: reverting site 1 alone must not
// also fail via site 2's independent coverage, and vice versa.
//
// Scenario, driven entirely through TestAfterAdmissionRefusal (the same
// deterministic seam p587_admission_aba_test.go uses, rather than racing a
// background tick or a spin-polling goroutine against DrainSessionNow's own
// loop -- both were tried and found genuinely racy: DrainSessionNow's own
// post-wait retry reliably wins a same-process admission race against
// anything not synchronized through this exact hook):
//
//  1. The test admits the session for itself BEFORE calling DrainSessionNow
//     (hostage "A"), so DrainSessionNow's OWN first admission attempt (for
//     row A) is refused and the hook fires.
//  2. Inside the hook's FIRST call: release hostage A with a nil outcome
//     (a clean, confirmed success) -- DrainSessionNow's wait on A resolves
//     to success and the ledger records it. Then, still synchronously
//     inside the same hook call (before DrainSessionNow's own loop can
//     possibly get back around to its next admission attempt), admit a
//     REPLACEMENT hostage "B" for the same session and hold it.
//  3. DrainSessionNow loops back, tries to admit row B, is refused AGAIN
//     (hostage B is already in place -- no race window exists, because the
//     hostage was taken on DrainSessionNow's own call stack via the hook
//     before it returned control). The hook's SECOND call does nothing
//     further -- hostage B is deliberately never released.
//  4. The test cancels DrainSessionNow's own ctx while it is parked
//     waiting on hostage B's (never-closing) done channel -- forcing the
//     `case <-ctx.Done():` branch specifically.

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestDrainSessionNow_ObservedAdmission_CtxDiesAfterOneSuccess_NeverReportsCleanSuccess(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p607-observed-ctx-dies")

	coord := &twoRowFailThenSucceedCoordinator{firstFn: func() (*any, error) { return nil, nil }}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p607-observed-ctx-dies",
	})
	enqueueTwoRows(t, "p607-observed-ctx-dies", svc, sess.ID)

	// Step 1: take hostage A before DrainSessionNow ever runs.
	releaseA, admittedA := pump.AdmitSessionForTest(sess.ID)
	require.True(t, admittedA, "test must be able to admit the session before DrainSessionNow starts")

	var hookCalls int
	var bAdmitted bool
	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		if sessionID != sess.ID {
			return
		}
		hookCalls++
		if hookCalls != 1 {
			// Second (and any further) refusal: hostage B is already held
			// from inside the first call below. Nothing further to do --
			// DrainSessionNow is now parked waiting on hostage B, which
			// this test never releases.
			return
		}
		// Step 2: resolve hostage A as a clean, confirmed success --
		// DrainSessionNow's wait on A will observe this and recordSuccess
		// it into the ledger.
		releaseA(nil)

		// Step 2 (continued): immediately claim hostage B for the SAME
		// session, still on DrainSessionNow's own refusal-handling call
		// stack -- before it can possibly get back around to its own next
		// admission attempt. No race window exists here (see p587's own
		// ABA test for the identical technique).
		_, bAdmitted = pump.AdmitSessionForTest(sess.ID)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Give DrainSessionNow time to be refused for row A, observe hostage
	// A's release (success), fold it into the ledger, loop back, and be
	// refused a second time for row B (now held by hostage B, claimed
	// above with no race window). Then kill this call's own ctx.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after its own ctx was cancelled while waiting on hostage B's admission")
	}

	require.Equal(t, 0, coord.calls, "DrainSessionNow's own Coordinator.Run must never be called at all -- both row A and row B's admission were held hostage by the test throughout")
	require.GreaterOrEqual(t, hookCalls, 2, "DrainSessionNow must have been refused admission twice -- once for row A (resolved via the wait, as a success) and once for row B (left hanging on hostage B) -- otherwise this test does not exercise the admission-wait ctx-death branch it claims to")
	require.True(t, bAdmitted, "the test's hostage B claim must have succeeded -- otherwise DrainSessionNow's row-B admission attempt was never actually refused, and this test proves nothing about the wait branch")
	require.False(t, result == session.DrainComplete && drainErr == nil,
		"MUST NOT report (DrainComplete, nil): row A succeeded (observed via the wait) but THIS call's own ctx died while waiting on hostage B's admission -- row B's actual outcome was never learned")
	require.Error(t, drainErr, "the call stopped because its OWN ctx ended while waiting on another admission holder -- that must surface as an error, not silence")
	require.ErrorIs(t, drainErr, context.Canceled, "the representative error must trace back to this call's own ctx cancellation")
}
