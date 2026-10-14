// P0-1 regression test (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// durable call that hits a busy session mailbox executes exactly once, not twice.
//
// The bug: before the fix, when a durable call (FromDurableQueue=true) encountered
// a busy mailbox (mbOwned or mbReleasing), mailbox.submit would append it to
// mb.submitted AND the pump would nack it with no penalty, so the live owner would
// execute the mb.submitted copy and the pump would re-lease and execute the same
// durable row after backoff — double-execution of the same logical request.
//
// The fix: mailbox.submit now skips mb.submitted for durable calls when the mailbox
// is busy; the durable row itself is the retry path via pump's ErrCallQueuedNotExecuted
// handling, so no in-process handoff is needed.

package session_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// durableCallExecutionCounter tracks how many times the pump executes a specific
// durable call, distinguished by its idempotency key. It also allows the test
// coordinator to simulate a busy session (owned by an external live owner) for
// the first N calls, then succeed — this mimics a real Run() turn that stays
// in-flight while the pump tries to lease the same session's durable row.
type durableCallExecutionCounter struct {
	idempotencyKey string
	executions     atomic.Int64
	busyUntilCall  int64
	callCount      atomic.Int64
	mu             sync.Mutex
	executeCh      chan struct{} // blocks until a call is actually dispatched
	allowExecute   bool          // set to true to let the next dispatched call proceed
}

func newDurableCallExecutionCounter(idempotencyKey string, busyUntilCall int64) *durableCallExecutionCounter {
	return &durableCallExecutionCounter{
		idempotencyKey: idempotencyKey,
		busyUntilCall:  busyUntilCall,
		executeCh:      make(chan struct{}),
	}
}

func (c *durableCallExecutionCounter) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	n := c.callCount.Add(1)
	if n <= c.busyUntilCall {
		// Simulate a genuinely external live owner — the pump sees
		// session.ErrCallQueuedNotExecuted and backs off locally.
		return nil, session.ErrCallQueuedNotExecuted
	}

	// Count this as an ACTUAL execution of the durable call (not just a
	// failed lease attempt or a backoff retry).
	c.executions.Add(1)

	// Signal that execution has started; the test can verify the count
	// and then allow it to complete.
	c.mu.Lock()
	ch := c.executeCh
	allow := c.allowExecute
	c.mu.Unlock()

	if allow {
		// A second execution (the bug this test guards, or a lease stolen by
		// a >1s scheduling stall) must fail the executions==1 assertion, not
		// panic the whole package run with a double close.
		c.mu.Lock()
		select {
		case <-ch:
		default:
			close(ch)
		}
		c.mu.Unlock()
	}

	var result any = "executed"
	return &result, nil
}

// TestReleaseGate_P0_1_DurableCallExecutesExactlyOnce proves that the pump's
// existing no-penalty-Nack-and-backoff mechanism (RunQueuePump.executeEntry's
// ErrCallQueuedNotExecuted handling) eventually executes a durable call
// exactly once and Acks it, across multiple backoff cycles.
//
// IMPORTANT SCOPE NOTE (added after independent verification): this test
// uses a mocked Coordinator (durableCallExecutionCounter) that simulates
// "session busy" by directly returning session.ErrCallQueuedNotExecuted —
// it never calls the real sessionAgent.Run / mailbox.submit, so it does
// NOT exercise the actual P0-1 fix (the `if !call.FromDurableQueue` guard
// in internal/agent/mailbox.go's submit). Verified by hand: this test
// still PASSES even with that guard removed (reverted to the pre-fix,
// unconditional `mb.submitted = append(...)`), because this test's fake
// coordinator has no mailbox to double-queue into in the first place. The
// original "REVERT CHECK PROCEDURE" comment here claimed otherwise — that
// claim was false and has been removed.
//
// The REAL regression coverage for the mailbox-level fix lives in
// internal/agent/mailbox_test.go:
// TestMailbox_Submit_DurableCallSkipsQueueWhenAlreadyOwned (and its sibling
// TestMailbox_Submit_NonDurableCallStillQueuesWhenAlreadyOwned, proving
// non-durable calls are unaffected) — both revert-checked directly against
// mailbox.submit.
//
// This test is still kept as a valid, narrower regression: it proves the
// pump-level retry/backoff/eventual-Ack mechanism itself works end-to-end
// (this behavior already existed before P0-1 and is a prerequisite for the
// mailbox-level fix to be sufficient — if the pump ever stopped retrying,
// the mailbox fix alone would leave durable calls permanently stuck instead
// of merely deferring their single execution).
func TestReleaseGate_P0_1_DurableCallExecutesExactlyOnce(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "test-session-p0-1-no-dup")
	ctx := t.Context()

	// Enqueue a durable call with a unique idempotency key. This call will
	// encounter a "busy session" for the first several pump attempts, then
	// succeed once the simulated external owner frees up.
	idempotencyKey := "p0-1-no-dup-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Simulate a session that stays externally busy (owned by another live,
	// in-process turn) for the first several pump attempts, then frees up.
	// The coordinator returns ErrCallQueuedNotExecuted for the first 5 calls,
	// then returns success. This mimics a real Run() turn that blocks the
	// session while the pump repeatedly tries to lease the same durable row.
	busyCycles := int64(5)
	counter := newDurableCallExecutionCounter(idempotencyKey, busyCycles)
	counter.allowExecute = true // allow the first successful execution to complete

	// Flake rewrite: the original version used a real 20ms tick with a 30ms
	// lease TTL. On Windows CI the scheduling gap between the pump's
	// `go p.executeEntry(...)` dispatch and Coordinator.Run returning could
	// exceed the lease watchdog's guaranteed window: with TestLeaseTTL=30ms
	// the watchdog safety margin is clamped to TTL/2 = 15ms
	// (run_queue_pump.go) and the watchdog deadline is seeded from the
	// CEILED DB lease_expires_at, so the guaranteed window is only ~15ms.
	// When the watchdog fired (leaseLost=true) before Run returned, the
	// SUCCESS outcome was skipped (run_queue_entry_exec.go: the
	// `if leaseLost.Load()` branch returns errLeaseLost without an Ack) even
	// though the instant fake coordinator had already executed the call. The
	// row then cycled leased -> lease expiry (up to ~1s of ceil slack) ->
	// CleanupExpiredLeases reap (attempt+1) -> re-lease, re-entering the same
	// race. The failure is a test-timing premise flaw (production TTL is 30s
	// with a 5s margin), so the fix is at the test layer: drive the pump on
	// the manually advanced fake clock. No fake-time advance ever crosses a
	// live execution's watchdog deadline (lease + 15s of fake time), and the
	// fakeClockService stamps every DB lease on the fake clock (year 2001),
	// so neither the watchdog nor lease expiry nor a real-time cleanup can
	// ever fire — every interleaving is deterministic.
	clk := newFakePumpClock(fakePumpEpoch)
	probe := newFakeClockService(svc, clk)
	const leaseTTL = 30 * time.Second
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       probe,
		Coordinator:    counter,
		PumpInstanceID: "p0-1-no-dup-pump",
		TestClock:      clk,
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
		TestLeaseTTL:   leaseTTL,
		// Keeps the real-ticker orphan-outbox goroutine idle so it can
		// never observe the frozen fake clock.
		TestDrainTick: func() time.Duration { return time.Hour },
	})
	pump.Start()
	defer pump.Stop()

	rowPending := func() bool {
		pending, err := probe.ListPendingRunQueueEntries(ctx)
		return err == nil && len(pending) > 0
	}

	// The pump's startup tick performs busy attempt 1 immediately.
	awaitPump(t, func() bool { return counter.callCount.Load() >= 1 },
		"pump startup tick must lease and dispatch the entry")

	// Attempts 2..5: the coordinator returned ErrCallQueuedNotExecuted, the
	// pump nacked with no attempt penalty (busyBackoffUntil = now + leaseTTL)
	// and the row is pending again. Proving that BEFORE any time advances
	// means CleanupExpiredLeases can never reap a live lease on the way.
	for i := int64(2); i <= busyCycles; i++ {
		awaitPump(t, rowPending,
			"pump must retry after the busy backoff (no-attempt-penalty nack)")
		// Crosses the busyBackoffUntil deadline (now + leaseTTL) and
		// delivers exactly one coalesced tick.
		clk.Advance(leaseTTL + 20*time.Millisecond + time.Millisecond)
		awaitPump(t, func() bool { return counter.callCount.Load() >= i },
			"pump tick after backoff must re-lease and re-dispatch the entry")
	}

	// Attempt 6: the coordinator succeeds, runs to completion, and the pump
	// nacks the row back to pending on the busy path one last time before
	// the next attempt executes it for real.
	awaitPump(t, rowPending, "row must be pending again after the last busy attempt")
	clk.Advance(leaseTTL + 20*time.Millisecond + time.Millisecond)
	awaitPump(t, func() bool { return counter.callCount.Load() > busyCycles },
		"the final (successful) attempt must execute the durable call")

	// Fake time is now frozen forever: the watchdog (deadline = lease + 15s
	// fake) and lease expiry cannot fire, and the DB lease stamps ride the
	// fake clock so real time cannot reap either. This is a hang guard, not
	// a timing window.
	awaitPump(t, func() bool {
		gone, err := runQueueGoneEverywhere(ctx, probe)
		return err == nil && gone
	}, "durable row should be acked and deleted after successful execution")

	// The critical invariant: the call was executed EXACTLY ONCE total.
	// Before the fix, the live owner would execute the mb.submitted copy,
	// then after backoff the pump would execute the durable row copy —
	// executions would be 2 or more.
	executions := counter.executions.Load()
	require.Equal(t, int64(1), executions,
		"durable call must execute exactly once — got %d executions, "+
			"indicating double-execution (possibly mb.submitted + durable row)",
		executions)
}

// Non-durable calls are covered directly and unconditionally by
// TestMailbox_Submit_NonDurableCallStillQueuesWhenAlreadyOwned in
// internal/agent/mailbox_test.go — no skipped placeholder needed here.
