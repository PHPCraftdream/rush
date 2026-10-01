// P0-3 follow-up regression test, found during the closing review of the
// 2026-08-11 release-readiness round: executeEntry created a single dbCtx
// (30s absolute deadline) at function entry, BEFORE calling
// Coordinator.Run, then reused that same context for the post-Run outcome
// write (Ack/Nack/TerminalFail). Any turn longer than 30s hit an already-
// expired context on its outcome write — the write failed with "context
// deadline exceeded", the row stayed leased/pending instead of being
// deleted, and the next tick re-leased and re-executed the same call. That
// is exactly the duplicate-execution bug P0-3's workerWg join was
// introduced to prevent, reintroduced by the same commit via its own new
// dbCtx.
//
// The fix creates the DB write's context fresh, AFTER Coordinator.Run
// returns, via a per-call newDBCtx() closure — see executeEntry's own doc
// comment. TestDBWriteTimeout lets this test force that budget down to a
// few tens of milliseconds so a "long turn" can be simulated with a short
// sleep instead of a real 30+ second wait.
//
// REVERT CHECK: reverting to a single dbCtx created at function entry (the
// original P0-3 shape) makes this test fail with "ack failed after
// success ... context deadline exceeded" and the row still present in the
// database afterward.

package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// sleepingCoordinator is a minimal Coordinator that sleeps for a fixed
// duration before returning success — long enough to outlive a
// deliberately tiny TestDBWriteTimeout, simulating a real LLM turn that
// outlives the 30s production budget without an actual 30+ second wait.
type sleepingCoordinator struct {
	sleep time.Duration
}

func (c *sleepingCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	select {
	case <-time.After(c.sleep):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var result any = "executed"
	return &result, nil
}

// TestP0_3_LongTurnOutcomeWriteSurvivesDBWriteTimeout verifies that a turn
// whose Coordinator.Run call outlives the per-write DB context budget still
// gets Acked successfully — i.e. the budget is applied to the outcome write
// itself, not to the whole executeEntry call including Run().
func TestP0_3_LongTurnOutcomeWriteSurvivesDBWriteTimeout(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc, sqlDB := setupTestSessionWithDB(t, "test-session-p0-3-long-turn")
	ctx := t.Context()

	idempotencyKey := "p0-3-long-turn-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// The coordinator's Run() sleeps for 150ms — deliberately longer than
	// the 50ms TestDBWriteTimeout below. Before the fix, dbCtx was created
	// (with this same 50ms budget) BEFORE Run() was called, so it would
	// already be expired by the time the post-Run Ack ran.
	coord := &sleepingCoordinator{sleep: 150 * time.Millisecond}

	// Run on the fake pump clock (pump_fake_clock_test.go). With the real
	// clock, the lease watchdog's margin defaults to production 5s clamped
	// to TTL/2 = 100ms, and its deadline is seeded from the whole-Unix-
	// seconds lease_expires_at column — up to ~1s later than a 200ms TTL —
	// so the watchdog fired on REAL time as early as ~100ms after the
	// lease, racing this test's 150ms Run sleep depending on the arbitrary
	// wall-clock phase the lease landed on: a mid-sleep cancellation made
	// Run return ctx.Err, the row was Nacked back to pending and re-leased,
	// and the row-deletion wait below timed out. That was a watchdog-vs-
	// test race, not the outcome-write property this test protects. On the
	// fake clock the deadline sits at lease + 100ms of FAKE time and fake
	// time never advances past the single lease tick, so the watchdog is
	// deterministically silent while the Run sleep and the Ack proceed on
	// real time (only the pump's scheduling decisions run on the fake
	// clock).
	clk := newFakePumpClock(fakePumpEpoch)
	probe := newFakeClockService(svc, clk)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:           probe,
		Coordinator:        coord,
		PumpInstanceID:     "p0-3-long-turn-pump",
		TestTick:           func() time.Duration { return 100 * time.Millisecond },
		TestLeaseTTL:       200 * time.Millisecond,
		TestDBWriteTimeout: 50 * time.Millisecond,
		TestClock:          clk,
	})
	pump.Start()
	// t.Cleanup so pump.Stop() still runs if the require below fails and
	// unwinds early — same rationale as
	// TestP1_1_WatchdogCancelsBeforeExpiry's identical comment.
	t.Cleanup(func() { pump.Stop() })

	// Wait for the pump's scan ticker to exist, then advance exactly one
	// scan tick of fake time to lease and dispatch the entry; fake time
	// never moves again, so the watchdog (lease + TTL - margin = lease +
	// 100ms of fake time) cannot fire while the 150ms real-time Run sleep
	// is in flight.
	awaitPump(t, func() bool { return clk.liveTickers() >= 1 },
		"scan ticker must be registered")
	clk.Advance(100 * time.Millisecond)

	// Wait for the entry to be Acked (deleted) — poll for the row's
	// disappearance rather than a fixed sleep, since exact scheduling
	// timing varies.
	require.Eventually(t, func() bool {
		var exists bool
		row := sqlDB.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM session_run_queue WHERE id = ?)", idempotencyKey)
		if err := row.Scan(&exists); err != nil {
			return false
		}
		return !exists
	}, 5*time.Second, 20*time.Millisecond,
		"entry must be Acked (deleted) once the long turn completes, even though "+
			"Run() outlived the per-write DB context budget")

	pump.Stop()
}
