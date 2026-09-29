// B12/C14 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): the 60s
// pass's own recheck of parked delegations must call recheckChild with the
// CHILD session id (recheckChild indexes l.byChild by child id) -- not the
// PARENT id parkedParentSessions() returns, which was always a miss.
package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
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
	coord, messages := newParkedOutcomeCoordinatorWithMessages(t, func(completion AsyncCompletion) { delivered <- completion })

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

	// B3/C6 fix follow-up: childScopeDrained now also waits for the child's
	// OWN cross-process reaction debt to clear. Write that reaction directly
	// (simulateChildReactionWithoutTrigger, NOT simulateChildReaction) so the
	// only thing that re-evaluates the delegation is RecheckPass itself, not
	// an in-process run-ended trigger this test deliberately never fires.
	simulateChildReactionWithoutTrigger(t, coord, messages, parkedChildSession, "child reacted", message.FinishReasonEndTurn)

	coord.RecheckPass(context.Background())

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "the 60s pass must deliver the parked delegation once the child's scope is actually drained")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.False(t, coord.asyncJobs.hasParked())
}

// TestRecheckPass_RecheckSetWakesRunConcurrentlyAndDetached pins the B12/C14
// fix and its follow-up: the recheck-set wakes run concurrently (all in
// flight before any finishes) and DETACHED (RecheckPass returns while they are
// still blocked), so a real Drain turn (seconds to minutes) neither serialises
// behind the others nor holds the ticker.
//
// Revert-check performed: changed the launch back to a plain synchronous
// `c.wakeSession(...)` loop -- the pass never returned while the first wake
// blocked; with goroutines but a trailing WaitGroup.Wait() in RecheckPass, the
// "must not wait for them" assertion FAILED (the pass blocked until the wakes
// were released).
func TestRecheckPass_RecheckSetWakesRunConcurrentlyAndDetached(t *testing.T) {
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	coord.asyncJobs = newWorkLedger(coord.notifyAsyncCompletion)

	release := make(chan struct{})
	var entered atomic.Int32
	wakeSessionAttemptSeam = func() {
		entered.Add(1)
		<-release
	}
	t.Cleanup(func() { wakeSessionAttemptSeam = nil })

	coord.addToRecheckSet("sess-a")
	coord.addToRecheckSet("sess-b")

	passDone := make(chan struct{})
	go func() {
		coord.RecheckPass(context.Background())
		close(passDone)
	}()
	select {
	case <-passDone:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("RecheckPass returned only after the wakes finished: it must not wait for them")
	}
	require.Eventually(t, func() bool { return entered.Load() == 2 }, 5*time.Second, 5*time.Millisecond,
		"both wakes must be in flight at once")
	close(release)
	coord.waitRecheckWakes()
}

// TestRecheckPass_SessionAlreadyBeingWoken_NotLaunchedTwice: a session whose
// recheck wake is still in flight is not launched a second time by a later
// pass; it stays in the recheck set and is woken again once the first wake
// has finished.
//
// Revert-check performed: dropped the recheckWakeInFlight guard in
// launchRecheckWake -- the second pass started a second wake (entered == 2
// while the first was still blocked).
func TestRecheckPass_SessionAlreadyBeingWoken_NotLaunchedTwice(t *testing.T) {
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	coord.asyncJobs = newWorkLedger(coord.notifyAsyncCompletion)

	release := make(chan struct{})
	var entered atomic.Int32
	wakeSessionAttemptSeam = func() {
		entered.Add(1)
		<-release
	}
	t.Cleanup(func() { wakeSessionAttemptSeam = nil })

	coord.addToRecheckSet("sess-a")
	coord.RecheckPass(context.Background())
	require.Eventually(t, func() bool { return entered.Load() == 1 }, 5*time.Second, 5*time.Millisecond)

	coord.addToRecheckSet("sess-a")
	coord.RecheckPass(context.Background())
	require.Never(t, func() bool { return entered.Load() > 1 }, 200*time.Millisecond, 10*time.Millisecond,
		"a session already being woken must not get a second concurrent wake")
	coord.recheckMu.Lock()
	_, kept := coord.recheckSet["sess-a"]
	coord.recheckMu.Unlock()
	require.True(t, kept, "the skipped session must stay in the recheck set for the next pass")

	close(release)
	coord.waitRecheckWakes()
	coord.RecheckPass(context.Background())
	coord.waitRecheckWakes()
	require.EqualValues(t, 2, entered.Load(), "once the first wake finished the session is woken again")
}

// TestRecheckPass_WakeConcurrencyIsBounded: more sessions than
// maxConcurrentRecheckWakes never run more than the bound at once; the rest
// stay in the recheck set and are woken by later passes.
//
// Revert-check performed: removed the len(recheckWakeInFlight) bound -- all
// sessions entered at once (entered > maxConcurrentRecheckWakes).
func TestRecheckPass_WakeConcurrencyIsBounded(t *testing.T) {
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	coord.asyncJobs = newWorkLedger(coord.notifyAsyncCompletion)

	release := make(chan struct{})
	var entered atomic.Int32
	wakeSessionAttemptSeam = func() {
		entered.Add(1)
		<-release
	}
	t.Cleanup(func() { wakeSessionAttemptSeam = nil })

	const total = maxConcurrentRecheckWakes + 2
	for i := range total {
		coord.addToRecheckSet(fmt.Sprintf("sess-%d", i))
	}
	coord.RecheckPass(context.Background())
	require.Eventually(t, func() bool { return entered.Load() == maxConcurrentRecheckWakes }, 5*time.Second, 5*time.Millisecond)
	require.Never(t, func() bool { return entered.Load() > maxConcurrentRecheckWakes }, 200*time.Millisecond, 10*time.Millisecond,
		"no more than maxConcurrentRecheckWakes wakes may run at once")
	coord.recheckMu.Lock()
	waiting := len(coord.recheckSet)
	coord.recheckMu.Unlock()
	require.Equal(t, total-maxConcurrentRecheckWakes, waiting, "sessions over the bound wait in the recheck set")

	close(release)
	coord.waitRecheckWakes()
	coord.RecheckPass(context.Background())
	coord.waitRecheckWakes()
	require.EqualValues(t, total, entered.Load(), "the waiting sessions are woken by the next pass")
}

// TestRecheckTicker_SlowWakeDoesNotDelayNextSweep drives the REAL ticker: a
// wake in the recheck set that blocks (a Drain turn taking minutes) must not
// stop the following ticks from running their sweep/purge/recheck.
//
// Revert-check performed: made RecheckPass wait for its wakes again
// (WaitGroup.Wait() at the end) -- sweeps stayed at 1 while the wake was
// blocked and the Eventually below timed out.
func TestRecheckTicker_SlowWakeDoesNotDelayNextSweep(t *testing.T) {
	coord := &coordinator{currentAgent: &mockSessionAgent{}, subAgentDrivers: newSubAgentDriverRegistry()}
	coord.asyncJobs = newWorkLedger(coord.notifyAsyncCompletion)

	release := make(chan struct{})
	wakeEntered := make(chan struct{}, 1)
	wakeSessionAttemptSeam = func() {
		select {
		case wakeEntered <- struct{}{}:
		default:
		}
		<-release
	}
	var sweeps atomic.Int32
	sweepSeam := func() { sweeps.Add(1) }
	recheckPassSweepSeam.Store(&sweepSeam)
	oldInterval := recheckPassIntervalNS.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() {
		wakeSessionAttemptSeam = nil
		recheckPassSweepSeam.Store(nil)
		recheckPassIntervalNS.Store(oldInterval)
	})

	coord.addToRecheckSet("sess-slow")
	coord.StartRecheckTicker()
	done := coord.recheckDone
	t.Cleanup(func() {
		close(release)
		coord.CancelAll()
		<-done
		coord.waitRecheckWakes()
	})

	select {
	case <-wakeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the recheck wake never started")
	}
	base := sweeps.Load()
	require.Eventually(t, func() bool { return sweeps.Load() >= base+3 }, 5*time.Second, 5*time.Millisecond,
		"later ticks must keep sweeping while a wake is still blocked")
}
