// B12/C14 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): the 60s
// pass's own recheck of parked delegations must call recheckChild with the
// CHILD session id (recheckChild indexes l.byChild by child id) -- not the
// PARENT id parkedParentSessions() returns, which was always a miss.
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestParkedChildSessions_ReturnsChildIdsNotParentIds is the direct, narrow
// proof of the underlying bug: parkedParentSessions() and
// parkedChildSessions() must report DIFFERENT ids for the same armed
// delegation.
func TestParkedChildSessions_ReturnsChildIdsNotParentIds(t *testing.T) {
	t.Parallel()
	coord := newParkedOutcomeCoordinator(t, func(AsyncCompletion) {})
	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, false)
	parkDelegation(t, coord, parkedChildSession, "still working")
	require.True(t, coord.asyncJobs.hasParked())

	require.Equal(t, []string{parkedParentSession}, coord.asyncJobs.parkedParentSessions())
	require.Equal(t, []string{parkedChildSession}, coord.asyncJobs.parkedChildSessions())
}

// TestRecheckPass_DeliversParkedDelegationViaChildRecheck reproduces B12's
// scenario: a child's owned job finishes (e.g. via another process's dead-
// host recovery, which deliberately never wakes/delivers -- DUR-6) WITHOUT
// the normal in-process "child run ended" trigger (noteSubAgentChildRunEnded)
// ever firing. The delegation stays parked until SOMETHING re-evaluates it;
// the 60s pass is that fallback.
//
// Revert-check performed: changed RecheckPass's loop back to `for _, parent
// := range c.parkedParentSessions() { c.asyncJobs.recheckChild(parent) }` --
// this test's `require.Len(t, drainCompletions(delivered), 1)` FAILED (0
// items: recheckChild(parentSession) looked up the empty
// byChild[parentSession] and did nothing). Restored parkedChildSessions();
// re-ran, passed.
func TestRecheckPass_DeliversParkedDelegationViaChildRecheck(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(t, func(completion AsyncCompletion) { delivered <- completion })

	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, false)
	parkDelegation(t, coord, parkedChildSession, "still working")
	require.True(t, coord.asyncJobs.hasParked())
	require.Empty(t, drainCompletions(delivered))

	// The child's owned job completes, but the normal "child run ended"
	// trigger is deliberately NOT invoked here -- simulating a completion
	// this process's own in-process triggers never got a chance to react to
	// (e.g. dead-host recovery in a DIFFERENT process, which never wakes or
	// delivers by design).
	coord.asyncJobs.finish(parkedChildSession, parkedChildJob, jobResult{content: "gate ok"})
	select {
	case <-delivered:
	default:
		t.Fatal("the child's own job result must still wake the child in-memory")
	}
	require.True(t, coord.asyncJobs.hasParked(), "precondition: the delegation itself must still be parked (no run-ended trigger fired)")

	coord.RecheckPass(context.Background())

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "the 60s pass must deliver the parked delegation once the child's scope is actually drained")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.False(t, coord.asyncJobs.hasParked())
}

// TestRecheckPass_RecheckSetWakesRunConcurrentlyNotSerially pins the second
// B12/C14 fix: each recheck-set wake must run in its own goroutine, so N
// slow wakes finish in roughly ONE wake's duration, not their sum -- a real
// Drain turn (which can take seconds) must never delay the sweep/purge of
// the NEXT tick, or the processing of the OTHER sessions in this same pass,
// by running serially.
//
// Revert-check performed: changed the recheck-set loop back to a plain
// synchronous `for _, sessionID := range c.drainRecheckSet() {
// c.wakeSession(...) }` (no goroutine, no WaitGroup) -- this test's
// `require.Less(t, elapsed, 2*perWakeDelay)` FAILED (elapsed was
// ~2*perWakeDelay: the two wakes ran back to back). Restored the
// goroutine+WaitGroup shape; re-ran, elapsed dropped back to ~1*perWakeDelay.
func TestRecheckPass_RecheckSetWakesRunConcurrentlyNotSerially(t *testing.T) {
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	ledger := newWorkLedger(coord.notifyAsyncCompletion)
	coord.asyncJobs = ledger

	const perWakeDelay = 150 * time.Millisecond
	wakeSessionAttemptSeam = func() { time.Sleep(perWakeDelay) }
	t.Cleanup(func() { wakeSessionAttemptSeam = nil })

	coord.addToRecheckSet("sess-a")
	coord.addToRecheckSet("sess-b")

	start := time.Now()
	coord.RecheckPass(context.Background())
	elapsed := time.Since(start)

	require.Less(t, elapsed, 2*perWakeDelay,
		"two recheck-set wakes must run concurrently, not serially (elapsed %s, per-wake delay %s)", elapsed, perWakeDelay)
}
