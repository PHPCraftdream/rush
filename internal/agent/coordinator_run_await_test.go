package agent

// §3 tests: runAwaitingAdmission must return the REAL outcome of a queued
// call's own eventual turn instead of (nil, nil) for callers that need it
// (#1036).

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestRunAwaitingAdmission_ImmediateRunPassesThrough: an idle mailbox (this
// fake never marks the admission queued) must pass the direct result
// through unchanged, with queued=false.
// Revert-check performed: forced the function to always return
// (nil, nil, true) -- this test FAILED (queued was true, result was nil
// instead of "direct"). Restored the real logic; re-ran, passed.
func TestRunAwaitingAdmission_ImmediateRunPassesThrough(t *testing.T) {
	agent := &mockSessionAgent{
		runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("direct"), nil
		},
	}
	coord := &coordinator{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result, err, queued := coord.runAwaitingAdmission(ctx, agent, SessionAgentCall{SessionID: "s1", Prompt: "hi"})
	require.NoError(t, err)
	require.False(t, queued)
	require.Equal(t, "direct", result.Response.Content.Text())
}

// TestRunAwaitingAdmission_ReturnsQueuedTurnsResult: a fake agent that marks
// the call queued (as the real sessionAgent.Run does when the mailbox is
// busy) and later fires call.onQueueResolved (as runOwned's dispatch loop
// does once the queued turn actually runs) must make runAwaitingAdmission
// block until that fire and return ITS result, not (nil, nil).
// Revert-check performed: removed the blocking `select` (returned
// `(nil, nil, true)` immediately on wasQueued()) -- this test FAILED
// (result was nil instead of "queued result"). Restored the blocking wait;
// re-ran, passed.
func TestRunAwaitingAdmission_ReturnsQueuedTurnsResult(t *testing.T) {
	var savedCall SessionAgentCall
	captured := make(chan struct{})
	agent := &mockSessionAgent{
		runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if adm := turnAdmissionFrom(ctx); adm != nil {
				adm.markQueued()
			}
			savedCall = call
			close(captured)
			return nil, nil
		},
	}
	coord := &coordinator{}

	type outcome struct {
		result *fantasy.AgentResult
		err    error
		queued bool
	}
	done := make(chan outcome, 1)
	go func() {
		result, err, queued := coord.runAwaitingAdmission(t.Context(), agent, SessionAgentCall{SessionID: "s1", Prompt: "hi"})
		done <- outcome{result, err, queued}
	}()

	select {
	case <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("agent.Run was never invoked")
	}

	select {
	case <-done:
		t.Fatal("runAwaitingAdmission returned before the queued call's own turn resolved")
	case <-time.After(100 * time.Millisecond):
	}

	require.NotNil(t, savedCall.onQueueResolved, "runAwaitingAdmission must arm onQueueResolved on the call it passes to agent.Run")
	savedCall.onQueueResolved(agentResultWithText("queued result"), nil)

	select {
	case o := <-done:
		require.NoError(t, o.err)
		require.True(t, o.queued)
		require.Equal(t, "queued result", o.result.Response.Content.Text())
	case <-time.After(2 * time.Second):
		t.Fatal("runAwaitingAdmission did not return after onQueueResolved fired")
	}
}
