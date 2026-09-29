// W-DRAIN item 2 (C5c, docs/reviews/2026-09-29-async-phase4-round1.md): a
// Drain attempt that reaches the provider and returns success can still
// leave its captured debt unreacted if the reaction write itself silently
// fails (fantasy has no way to surface an OnStepFinish failure as a turn
// error). Unbounded, this relaunches a fresh PAID provider turn every time
// forever -- checkStuckDrainProgress bounds it with the same wake_attempts/
// K=3 counter settle-by-failure uses.
package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestCheckStuckDrainProgress_SuccessfulTurnNeverReacts_SettlesAtKThree is
// the core C5c proof: three consecutive wakeSession calls whose mock Run
// reports SUCCESS (nil error) and provides real per-attempt evidence (a
// committed assistant message with content, a clean finish) but never
// actually marks the captured debt reacted -- exactly the "reaction write
// silently failed" scenario. The debt must survive the first two attempts
// and close, with exactly one marker, on the third.
//
// Revert-check performed: removed the `if !admission.wasQueued() { ...
// checkStuckDrainProgress ... }` block from wakeSession (coordinator_wake.go)
// -- this test's third-attempt assertions FAILED (debt still existed after
// 3 "successful" wakes, zero markers, meaning production would relaunch a
// FOURTH paid provider turn and every one after it, forever). Restored the
// block; re-ran, passed.
func TestCheckStuckDrainProgress_SuccessfulTurnNeverReacts_SettlesAtKThree(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "stuck-progress")
	succeedWithoutReacting := func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		msg, err := f.messages.Create(ctx, f.sessID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "looks done"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
		if call.OnAssistantMessageCreated != nil {
			call.OnAssistantMessageCreated(msg.ID)
		}
		// The reaction write itself is never performed -- simulating
		// fantasy swallowing an OnStepFinish failure: the turn looks
		// successful, but MarkReactedWithMessageUpdate never ran.
		return &fantasy.AgentResult{}, nil
	}
	f.agent.runFunc = succeedWithoutReacting
	require.True(t, f.debtExists(t), "precondition: debt must be visible before the first wake")

	for i := 1; i <= 2; i++ {
		err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
		require.NoError(t, err, "attempt %d: a successful turn must not itself error", i)
		require.True(t, f.debtExists(t), "attempt %d: debt must survive under K=3", i)
		require.Zero(t, f.markerCount(t), "attempt %d: no marker before K=3", i)
	}

	err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.False(t, f.debtExists(t), "the 3rd successful-but-unreacted pass must close the debt")
	require.Equal(t, 1, f.markerCount(t), "exactly one marker at K=3, not a chain of paid turns")
}

// TestCheckStuckDrainProgress_NoEvidence_NeverActs is the regression guard
// for every OTHER settle-by-failure/wakeSession test in this package: a
// mock SessionAgent that reports success WITHOUT ever wiring
// OnAssistantMessageCreated (the overwhelming majority of this package's
// fixtures) must never trigger checkStuckDrainProgress -- no evidence means
// no guess, exactly like the mirror-image fallback in
// settleOrRetryDrainFailure for the failure case.
func TestCheckStuckDrainProgress_NoEvidence_NeverActs(t *testing.T) {
	t.Parallel()
	f := newSettleFixture(t, "no-evidence-success")
	f.agent.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return &fantasy.AgentResult{}, nil
	}

	for i := 1; i <= 5; i++ {
		err := f.coord.wakeSession(context.Background(), jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
		require.NoError(t, err, "attempt %d", i)
	}
	require.True(t, f.debtExists(t), "with no per-attempt evidence at all, checkStuckDrainProgress must never settle")
	require.Zero(t, f.markerCount(t))
}
