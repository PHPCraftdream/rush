package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunAgentTurnRecovered_Panic proves that a panic raised anywhere inside
// the function passed as runFn (standing in for app.AgentCoordinator.Run,
// which — per coordinator.go's runSubAgent and the fantasy library's fully
// synchronous tool-dispatch loop — can panic on behalf of ANY tool call
// executed during the turn, including a sub-agent delegation) does NOT crash
// the test process and instead surfaces as a single, clear agentTurnResponse
// on done, exactly once. Without runAgentTurnRecovered's recover(), this
// panic would propagate out of the goroutine and terminate the whole
// process via Go's default handler — this is precisely the "rush run died
// with zero log lines" failure mode being closed here.
func TestRunAgentTurnRecovered_Panic(t *testing.T) {
	done := make(chan agentTurnResponse, 1)

	panicking := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		panic("boom: simulated tool-call panic deep inside AgentCoordinator.Run")
	}

	// Run on its own goroutine, same as production, so an unrecovered panic
	// would take down the test binary rather than just this function.
	go runAgentTurnRecovered(t.Context(), "sess-1", "do the thing", panicking, done)

	select {
	case resp := <-done:
		require.Error(t, resp.err, "a recovered panic must still produce a non-nil error result")
		assert.Nil(t, resp.result)
		assert.Contains(t, resp.err.Error(), "panicked")
		assert.Contains(t, resp.err.Error(), "boom: simulated tool-call panic")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for done — panic was not recovered/reported, goroutine likely died silently")
	}
}

// TestRunAgentTurnRecovered_NormalError verifies that runAgentTurnRecovered
// leaves EXISTING, expected error handling completely untouched: a normal Go
// error returned by runFn (not a panic) must be wrapped and reported exactly
// as before, with no interference from the new recover().
func TestRunAgentTurnRecovered_NormalError(t *testing.T) {
	done := make(chan agentTurnResponse, 1)

	sentinel := assert.AnError
	erroring := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return nil, sentinel
	}

	runAgentTurnRecovered(t.Context(), "sess-2", "prompt", erroring, done)

	resp := <-done
	require.Error(t, resp.err)
	assert.ErrorIs(t, resp.err, sentinel)
	assert.False(t, strings.Contains(resp.err.Error(), "panicked"),
		"a normal error must not be mislabeled as a panic")
}

// TestRunAgentTurnRecovered_NilResultNoError pins the corrected behaviour
// for a nil result with a nil error: sessionAgent.Run's ONLY (nil, nil)
// return is the R1-4 legacy queueing path (mailbox.submit's "caller queues
// and returns nil" branch) — every fail-fast busy rejection already wraps
// agent.ErrSessionBusy in a non-nil err, caught by the branch above this
// one. The previous version of this test pinned the OLD, buggy mapping to
// agent.ErrSessionBusy, which made a legacy (FailIfSessionBusy=false)
// caller that genuinely landed in the mid-turn queueing window fail hard
// instead of queueing — round-3 review finding, reproduced by
// TestExecuteRunSameSessionLegacyQueueingStillQueuesBehindOwner failing
// under CI load (a fast idle machine almost always races the second call
// in AFTER the owner releases, masking the bug).
func TestRunAgentTurnRecovered_NilResultNoError(t *testing.T) {
	done := make(chan agentTurnResponse, 1)

	nilResult := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return nil, nil
	}

	runAgentTurnRecovered(t.Context(), "sess-3", "prompt", nilResult, done)

	resp := <-done
	require.NoError(t, resp.err, "a legacy-queued call must not be reported as a failure")
	assert.Nil(t, resp.result)
}

// TestRunAgentTurnRecovered_Success verifies the plain happy path still
// reports the result with no error.
func TestRunAgentTurnRecovered_Success(t *testing.T) {
	done := make(chan agentTurnResponse, 1)

	want := &fantasy.AgentResult{}
	success := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return want, nil
	}

	runAgentTurnRecovered(t.Context(), "sess-4", "prompt", success, done)

	resp := <-done
	require.NoError(t, resp.err)
	assert.Same(t, want, resp.result)
}
