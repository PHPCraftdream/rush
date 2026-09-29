// B5/C3 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): a
// cancellation/deadline error, or AwaitingAnswerError, is NOT evidence a
// Drain's own provider attempt failed -- there is no attempt outcome to
// classify. Before this fix, settleOrRetryDrainFailure fed these straight
// into classifyProviderError, which treats context.Canceled/DeadlineExceeded
// as terminal (documented as safe only for its OTHER caller, shouldRetryTurn,
// which first gates on owning the attempt's own assistant row with a real
// error finish -- a gate this function never had). A user Stop during a
// Drain, CancelAll/shutdown with a Drain in flight, Ctrl-C/--timeout, a
// watchdog stall, or the agent legitimately asking a question all closed the
// debt as a permanent failure and wrote a visible wake-failed marker.
package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func (f *settleFixture) markerKinds(t *testing.T) []string {
	t.Helper()
	notices, err := f.store.ListSessionNotices(context.Background(), f.sessID)
	require.NoError(t, err)
	kinds := make([]string, 0, len(notices))
	for _, n := range notices {
		kinds = append(kinds, n.Kind)
	}
	return kinds
}

func (f *settleFixture) inRecheckSet(t *testing.T) bool {
	t.Helper()
	f.coord.recheckMu.Lock()
	defer f.coord.recheckMu.Unlock()
	_, ok := f.coord.recheckSet[f.sessID]
	return ok
}

// TestSettleByFailure_ContextCanceled_DoesNotSettle covers Stop/CancelAll/
// shutdown/Ctrl-C/--timeout/a watchdog stall -- all of which surface as
// context.Canceled or context.DeadlineExceeded, never a real provider
// classification.
//
// Revert-check performed: removed the `errors.Is(runErr, context.Canceled)
// || errors.Is(runErr, context.DeadlineExceeded) || errors.As(runErr,
// &awaiting)` guard from settleOrRetryDrainFailure (coordinator_drain_
// policy.go) -- this test's `require.True(t, f.debtExists(t))` (after the
// call) FAILED: the debt was closed (settled) and a wake_failed marker was
// written on a bare context.Canceled. Restored the guard; re-ran, passed.
func TestSettleByFailure_ContextCanceled_DoesNotSettle(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "cancel-during-drain")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, context.Canceled
	}
	require.True(t, f.debtExists(t), "precondition: debt must be visible before the canceled wake")

	_ = f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)

	require.True(t, f.debtExists(t), "a cancellation must not settle the debt")
	require.NotContains(t, f.markerKinds(t), "wake_failed", "a cancellation must not write a wake-failed marker")
	require.True(t, f.inRecheckSet(t), "a canceled Drain attempt must land in the 60s recheck set, never be forgotten")
}

// TestSettleByFailure_DeadlineExceeded_DoesNotSettle is the sibling for
// context.DeadlineExceeded (e.g. `rush run --timeout` firing mid-turn).
func TestSettleByFailure_DeadlineExceeded_DoesNotSettle(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "deadline-during-drain")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, context.DeadlineExceeded
	}
	require.True(t, f.debtExists(t))

	_ = f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)

	require.True(t, f.debtExists(t), "a deadline error must not settle the debt")
	require.NotContains(t, f.markerKinds(t), "wake_failed")
	require.True(t, f.inRecheckSet(t))
}

// TestSettleByFailure_AwaitingAnswer_DoesNotSettle: the agent legitimately
// asked the user a question mid-Drain -- not a failure, must not close the
// debt or lose the question by marking it a permanent wake failure.
func TestSettleByFailure_AwaitingAnswer_DoesNotSettle(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "awaiting-answer-during-drain")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, &AwaitingAnswerError{Question: "which environment?", SessionID: f.sessID}
	}
	require.True(t, f.debtExists(t))

	_ = f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)

	require.True(t, f.debtExists(t), "an awaiting-answer outcome must not settle the debt")
	require.NotContains(t, f.markerKinds(t), "wake_failed")
	require.True(t, f.inRecheckSet(t))
}
