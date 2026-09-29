// The 60s recheck pass and its ticker lifecycle (docs/plans/2026-09-28-
// async-phase4-durable-core.md sec.3.4 rule (b)/sec.3.5, step 4 review).
package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestRecheckSet_SessionLockBusyGoesIntoRecheckSet_RetriedByPass pins doc
// sec.3.4 rule (b) and sec.6's "orphaned Drain"/"sessions locks holding the
// lock at check time" scenarios: a cross-process session-lock-busy refusal
// (session.SessionLockBusyError -- exactly what `sessions locks/kill/reap`
// holding the lock produces) is never a settle-by-failure event; the session
// goes into the recheck set instead, and the next RecheckPass retries it --
// once the lock clears, the wake is delivered, not lost.
//
// REVERT CHECK: removed `c.addToRecheckSet(job.owner)` from
// recordDrainOutcome's turnAttemptRefused branch (coordinator_drain_policy.go)
// -- this test's `require.True(t, inSet, ...)` FAILED (the set stayed
// empty) and the subsequent RecheckPass found nothing to retry (requests
// stayed 0 forever instead of eventually reaching 1). Restored the call;
// re-ran, passed.
func TestRecheckSet_SessionLockBusyGoesIntoRecheckSet_RetriedByPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "recheck-set-lock-busy")
	f.claimAndFinish(t, ctx, "call-1")
	_, err := f.store.PullJobNotices(ctx, f.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	visible, err := f.store.VisibleReactionDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, visible)

	// Simulate "another process holds the session lock right now" by
	// wrapping f.sa in a driver that fails ONCE with SessionLockBusyError,
	// then behaves normally.
	var attempts int
	wrapped := &lockBusyThenOKAgent{SessionAgent: f.sa, failFirst: &attempts}
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: wrapped, call: SessionAgentCall{SessionID: f.sessID}})

	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.Error(t, err)
	require.Zero(t, f.requests.Load(), "the refused attempt must never reach the provider")

	f.coord.recheckMu.Lock()
	_, inSet := f.coord.recheckSet[f.sessID]
	f.coord.recheckMu.Unlock()
	require.True(t, inSet, "a session-lock-busy refusal must land in the recheck set, not be forgotten")

	debtStillOpen, err := f.store.ReactionDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, debtStillOpen, "an admission refusal must never close the debt")

	// The next RecheckPass retries it; the lock has "cleared" (the wrapper
	// now delegates to the real agent), so this time it succeeds.
	f.coord.RecheckPass(ctx)
	require.Eventually(t, func() bool { return f.requests.Load() > 0 }, 2*time.Second, 10*time.Millisecond,
		"the 60s pass must retry a recheck-set session and eventually deliver the wake")
}

// lockBusyThenOKAgent fails the FIRST Run with session.SessionLockBusyError
// (an admission refusal, doc sec.3.4 rule (b)/(c)), then delegates every
// call (Run included, from the 2nd onward) to the embedded real agent --
// interface embedding means every OTHER SessionAgent method (Cancel,
// IsSessionBusy, ...) already just works without restating its signature.
type lockBusyThenOKAgent struct {
	SessionAgent
	failFirst *int
}

func (a *lockBusyThenOKAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	*a.failFirst++
	if *a.failFirst == 1 {
		return nil, &session.SessionLockBusyError{Path: "test.lock", HolderPID: 999}
	}
	return a.SessionAgent.Run(ctx, call)
}

// TestRecheckTicker_StopsCleanly_NoGoroutineLeak confirms StartRecheckTicker's
// goroutine actually exits once stopped (via CancelAll, production's own
// shutdown path) rather than leaking -- doc sec.3.5's 60s pass must not
// outlive the coordinator.
//
// REVERT CHECK: commented out the `c.StopRecheckTicker()` call inside
// CancelAll (coordinator_interrupt.go) -- this test's `<-done` select
// FAILED (timed out after 2s: the ticker goroutine was still alive, parked
// on its own ctx that CancelAll no longer cancelled). Restored the call
// (byte-identical diff confirmed); re-ran, passed.
func TestRecheckTicker_StopsCleanly_NoGoroutineLeak(t *testing.T) {
	coord := &coordinator{currentAgent: &mockSessionAgent{}}

	coord.StartRecheckTicker()
	coord.StartRecheckTicker() // idempotent: must not spawn a second goroutine
	require.NotNil(t, coord.recheckStop)
	done := coord.recheckDone
	require.NotNil(t, done)

	select {
	case <-done:
		t.Fatal("the ticker goroutine must not have exited before it was ever stopped")
	default:
	}

	coord.CancelAll() // production's shutdown path
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the ticker goroutine must exit once CancelAll stops it -- no leak")
	}

	// A second CancelAll (idempotent shutdown) must not panic or hang.
	coord.CancelAll()
}
