// task #611 Part 2: TestP1_2_LeaseLossCancelCauseIsRenewalNotWatchdog proves
// that when the renewal loop's own `!ok` branch is the mechanism that fires
// (not the independent watchdog), it is that branch — not the watchdog
// racing ahead of it — that performs the cancellation. The existing lease-
// loss tests (p1_2_lease_loss_test.go) only assert that cancellation
// happened at all, which several mechanisms can produce; the eighth review
// noted they can pass purely because the watchdog fires first, without ever
// exercising the `!ok` branch's own cancellation path.
//
// This test uses a long TestLeaseTTL (so the watchdog's own deadline is far
// away and cannot plausibly win the race) and steals the lease via direct
// SQL, same as the existing lease-loss tests, then asserts the reported
// cancelCause is exactly cancelCauseRenewalNotOK.
package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestP1_2_LeaseLossCancelCauseIsRenewalNotWatchdog verifies that a stolen
// lease is detected and cancelled by the renewal loop's own `!ok` branch,
// not merely by the independent watchdog happening to fire around the same
// time.
//
// REVERT CHECK (task #611), and it is worth stating what it actually
// measured rather than what one would expect. Neutralising exactly the
// `!ok` branch's cancellation — dropping its `leaseLost.Store(true)`,
// its cancelCauseAtomic CAS and its `execCancel()`, leaving the log line —
// and running the whole `TestP1_2_` family in one invocation gave:
//
//	--- PASS: TestP1_2_OutcomeWriteSkippedOnLeaseLoss (0.50s)
//	--- PASS: TestP1_2_ExecCtxCanceledOnLeaseLoss     (0.51s)
//	--- FAIL: TestP1_2_LeaseLossCancelCauseIsRenewalNotWatchdog (15.40s)
//	    coordinator did not observe context cancellation within 15s
//
// So the eighth review's suspicion is confirmed on this codebase: the two
// pre-existing lease-loss tests stay green with the branch they were
// written for entirely disabled, and in half a second — fast enough that
// whatever cancels there is not even the watchdog. Only an assertion on
// the cause notices. Note the failure surfaces as the 15s cancellation
// timeout above rather than as a cancelCauseNone mismatch: with nothing
// cancelling, the coordinator never unblocks and the test never reaches
// the cause assertion.
func TestP1_2_LeaseLossCancelCauseIsRenewalNotWatchdog(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc, sqlDB := setupTestSessionWithDB(t, "test-session-p1-2-cancel-cause")
	ctx := t.Context()

	idempotencyKey := "p1-2-cancel-cause-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	coord := newContextAwareCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	causeResultCh := make(chan session.CancelCauseForTest, 1)

	// TestLeaseTTL is deliberately LARGE relative to the test's own steal +
	// observation window: the watchdog's own deadline (TTL - margin, and
	// margin is clamped to at most TTL/2) sits far in the future, so it
	// cannot plausibly be the mechanism that fires first. Renewal ticks
	// still happen every TTL/3, so the stolen lease is still detected
	// promptly by the renewal loop's own `!ok` branch.
	const testTTL = 30 * time.Second

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "p1-2-cancel-cause-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   testTTL,
		TestOnCancelCause: func(cause session.CancelCauseForTest) {
			select {
			case causeResultCh <- cause:
			default:
			}
		},
	})
	pump.Start()
	t.Cleanup(func() { pump.Stop() })

	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond,
		"coordinator must be called (worker started)")

	// Wait for at least one renewal tick to land (TTL/3 ~= 10s, but the
	// pump's own first renewal can happen well before that on its own
	// ticker cadence) so the row is genuinely leased_by this pump before we
	// steal it. A short fixed wait is sufficient and matches the existing
	// lease-loss tests' own approach.
	time.Sleep(50 * time.Millisecond)

	stealCtx, stealCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stealCancel()
	_, err = sqlDB.ExecContext(stealCtx,
		"UPDATE session_run_queue SET leased_by = ? WHERE id = ?",
		"thief-owner", idempotencyKey)
	require.NoError(t, err, "stealing the lease via direct SQL update should succeed")

	select {
	case <-coord.canceledCh:
		// Expected: coordinator observed context cancellation.
	case <-time.After(15 * time.Second):
		t.Fatal("coordinator did not observe context cancellation within 15s")
	}

	select {
	case cause := <-causeResultCh:
		require.Equal(t, session.CancelCauseRenewalNotOKForTest, cause,
			"cancellation must be attributed to the renewal loop's own !ok branch, not the watchdog or no cause at all (got cause=%v)", cause)
	case <-time.After(5 * time.Second):
		t.Fatal("TestOnCancelCause was never invoked")
	}

	close(blockCh)

	nackCtx, nackCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer nackCancel()
	err = svc.NackRunQueueEntryNoAttemptPenalty(nackCtx, idempotencyKey, "thief-owner", "test cleanup")
	require.NoError(t, err)
}
