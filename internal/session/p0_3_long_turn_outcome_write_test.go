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

	// The coordinator's Run() sleeps for 1s — deliberately longer than
	// the 500ms TestDBWriteTimeout below. Before the fix, dbCtx was created
	// (with this same 500ms budget) BEFORE Run() was called, so it would
	// already be expired by the time the post-Run Ack ran.
	//
	// Why 500ms/1s and not the original 50ms/150ms: a fresh per-write DELETE
	// can itself exceed a 50ms budget under a loaded runner (observed ~20% of
	// runs as "ack failed after success — context deadline exceeded"), which
	// is the same wall-clock dependency that failed this test on CI; 500ms
	// gives the fresh write ample headroom while a reverted single dbCtx
	// created before Run is still guaranteed expired (its deadline passes
	// 500ms after entry, Run returns at ~1s).
	coord := &sleepingCoordinator{sleep: 1 * time.Second}

	// Run on the fake pump clock (pump_fake_clock_test.go). The pump's run
	// loop performs an immediate initial p.tick() on startup
	// (run_queue_lifecycle.go:146), which can lease and dispatch the entry
	// BEFORE this test's single clk.Advance(100ms) — awaitPump only proves
	// the scan ticker is registered, not that the initial tick hasn't
	// already leased. With a short TTL (e.g. 200ms) the lease is stamped
	// (whole-Unix-seconds, floor) at the fake epoch and the watchdog
	// deadline (lease_expires_at - min(margin, TTL/2) = epoch - 100ms) is
	// already at or before the post-Advance fake time, so the single Advance
	// fires the pre-created watchdog fake ticker and it cancels the
	// execution: Run returns ctx.Err, the row is Nacked back to pending,
	// and with fake time frozen no later tick ever re-leases it — a
	// scheduling race on how fast the initial tick ran, not the
	// outcome-write property under test. With the production 30s TTL the
	// watchdog deadline is lease + 25s of fake time and the renewal interval
	// is 10s of fake time; both fake tickers stay silent under ANY
	// scheduling (whether the lease lands before or after the single
	// Advance), so the test no longer depends on wall-clock speed. The
	// outcome-write property is unchanged: Run still sleeps 1s of real
	// time against the 500ms TestDBWriteTimeout.
	clk := newFakePumpClock(fakePumpEpoch)
	probe := newFakeClockService(svc, clk)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:           probe,
		Coordinator:        coord,
		PumpInstanceID:     "p0-3-long-turn-pump",
		TestTick:           func() time.Duration { return 100 * time.Millisecond },
		TestLeaseTTL:       30 * time.Second,
		TestDBWriteTimeout: 500 * time.Millisecond,
		TestClock:          clk,
	})
	pump.Start()
	// t.Cleanup so pump.Stop() still runs if the require below fails and
	// unwinds early — same rationale as
	// TestP1_1_WatchdogCancelsBeforeExpiry's identical comment.
	t.Cleanup(func() { pump.Stop() })

	// Wait for the pump's scan ticker to exist, then advance exactly one
	// scan tick of fake time to lease and dispatch the entry (if the
	// startup's initial tick hasn't already done so). With the production
	// 30s TTL the watchdog deadline (lease + 25s of fake time) and the
	// renewal interval (10s of fake time) are both far beyond the single
	// Advance, so neither fake ticker can fire while the 1s real-time
	// Run sleep is in flight, under any scheduling.
	awaitPump(t, func() bool { return clk.liveTickers() >= 1 },
		"scan ticker must be registered")
	clk.Advance(100 * time.Millisecond)

	// Wait for the entry to be Acked (deleted) — poll for the row's
	// disappearance rather than a fixed sleep, since exact scheduling
	// timing varies.
	awaitPump(t, func() bool {
		var exists bool
		row := sqlDB.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM session_run_queue WHERE id = ?)", idempotencyKey)
		if err := row.Scan(&exists); err != nil {
			return false
		}
		return !exists
	},
		"entry must be Acked (deleted) once the long turn completes, even though "+
			"Run() outlived the per-write DB context budget")

	pump.Stop()
}
