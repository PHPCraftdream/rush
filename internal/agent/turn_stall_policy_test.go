package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Each test names the single production behavior it must catch (revert-
// check doc):
//
//   - TestTurnStallPolicyProviderStallRetriesThenAborts guards
//     turnStallAbortPolicy's provider branch: cancel the stuck attempt and
//     retry at most stallMaxRetries times, then abort via the armed
//     closure (runStallRetried re-issues the step; the final failure goes
//     through the causeStall fire path).
//   - TestTurnStallPolicyInternalPhaseAbortsImmediately guards the
//     internal-phase branch: no retry, one fire past abortTimeout aborts.
//   - TestTurnStallPolicyAbortDisabled guards the off switch:
//     abortTimeout == 0 never aborts (warn+dump still happen upstream).
//   - TestTurnStallPolicyProgressPreventsAbort guards that durable
//     progress (text deltas mark lastProgress) keeps the policy silent.
//   - TestRunStallRetriedAttributesOnlyPolicyCancels guards
//     runStallRetried's retry attribution: an operator cancel (counter
//     unmoved) must NOT be retried.
//   - TestTurnStallAbortClosureFiresCauseStall guards the runTurn abort
//     closure: forceStall, causeStall stored, genCtx cancelled.
//   - TestTurnStallAbortJoinsErrTurnStalled guards handleStreamFailure's
//     ErrTurnStalled join for causeStall aborts.
//   - TestExitMapsStallAbort guards app's exit mapping of ErrTurnStalled
//     to exit_reason "stalled".

// newStallPolicyClock arms a clock under sessionID with fake-clock
// control, an attempt-cancel holder and an abort recorder; restores the
// global registry and policy on cleanup.
func newStallPolicyClock(t *testing.T, abortTimeout time.Duration) (*stallClock, *context.Context, *[]time.Duration, func(time.Time)) {
	t.Helper()
	// Install the real policy (armTurnStall is never called here) and
	// restore whatever seam value the test process had before.
	prev := turnStallPolicy.Load()
	installTurnStallPolicy()
	t.Cleanup(func() { turnStallPolicy.Store(prev) })
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)
	sc := &stallClock{abortTimeout: abortTimeout}
	sc.markProgress()
	sessionID := "stall-policy-test"
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sc.armAttemptCancel(cancel)
	var aborts []time.Duration
	sc.armStallAbort(func(elapsed time.Duration) { aborts = append(aborts, elapsed) })
	// The abort recorder is read by the test after the fire; guard with a
	// pointer so the closure capture stays visible.
	return sc, &ctx, &aborts, set
}

// fireStallSampler advances the fake clock past both the warn threshold
// and the clock's abortTimeout and runs one sampler tick.
func fireStallSampler(t *testing.T, set func(time.Time), from time.Time, advance time.Duration) {
	t.Helper()
	set(from.Add(advance))
	stallSamplerTickOnce()
}

func TestTurnStallPolicyProviderStallRetriesThenAborts(t *testing.T) {
	sc, ctx, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	sc.setPhase("provider", "test-model", "")
	t0 := time.Now()

	// Fire 1: attempt cancelled, retry 1.
	fireStallSampler(t, set, t0, 11*time.Minute)
	select {
	case <-(*ctx).Done():
	default:
		t.Fatal("policy did not cancel the stalled provider attempt on fire 1")
	}
	require.Equal(t, int32(1), sc.stallRetries.Load())
	require.Empty(t, *aborts)

	// Fire 2 (past the warn window): retry 2.
	fireStallSampler(t, set, t0, 11*time.Minute+stallWarnWindow)
	require.Equal(t, int32(2), sc.stallRetries.Load())
	require.Empty(t, *aborts)

	// Fire 3: retries exhausted → causeStall abort with sinceProgress.
	fireStallSampler(t, set, t0, 11*time.Minute+2*stallWarnWindow)
	require.Len(t, *aborts, 1)
}

func TestTurnStallPolicyInternalPhaseAbortsImmediately(t *testing.T) {
	sc, ctx, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	sc.setPhase("internal", "compaction", "")
	t0 := time.Now()

	fireStallSampler(t, set, t0, 11*time.Minute)

	require.Len(t, *aborts, 1)
	require.Equal(t, int32(0), sc.stallRetries.Load(), "internal phases must never retry")
	select {
	case <-(*ctx).Done():
		t.Fatal("attempt cancel must not fire for internal phases")
	default:
	}
}

func TestTurnStallPolicyAbortDisabled(t *testing.T) {
	sc, ctx, aborts, set := newStallPolicyClock(t, 0)
	sc.setPhase("internal", "persist", "")
	t0 := time.Now()

	fireStallSampler(t, set, t0, time.Hour)
	fireStallSampler(t, set, t0, time.Hour+stallWarnWindow)

	require.Empty(t, *aborts)
	require.Equal(t, int32(0), sc.stallRetries.Load())
	select {
	case <-(*ctx).Done():
		t.Fatal("abort must be disabled when abortTimeout is 0")
	default:
	}
	// Warn+dump still happened: the sampler crossed its threshold.
	require.NotZero(t, sc.lastWarnNanos.Load())
}

func TestTurnStallPolicyProgressPreventsAbort(t *testing.T) {
	sc, _, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	t0 := time.Now()

	// A text delta lands a durable progress point just before the tick.
	set(t0.Add(11 * time.Minute))
	sc.markProgress()
	stallSamplerTickOnce()

	require.Empty(t, *aborts)
	require.Equal(t, int32(0), sc.stallRetries.Load())
	require.Zero(t, sc.lastWarnNanos.Load())
}

func TestRunStallRetriedAttributesOnlyPolicyCancels(t *testing.T) {
	sc := &stallClock{}
	attempts := 0
	err := runStallRetried(sc, func() error {
		attempts++
		if attempts <= 2 {
			sc.stallRetries.Add(1) // the policy fired and cancelled this attempt
			return context.Canceled
		}
		return nil
	}, func(retry int) {})
	require.NoError(t, err)
	require.Equal(t, 3, attempts)

	// An operator cancel (counter unmoved) is never retried.
	attempts = 0
	err = runStallRetried(sc, func() error {
		attempts++
		return context.Canceled
	}, func(retry int) {})
	require.True(t, errors.Is(err, context.Canceled))
	require.Equal(t, 1, attempts)
}

func TestTurnStallAbortClosureFiresCauseStall(t *testing.T) {
	a := &sessionAgent{}
	var causeVal atomic.Int32
	var stalled atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	abort := a.stallAbortClosure("sess-abort", &causeVal, time.Minute, time.Minute, Model{}, cancel, func() { stalled.Store(true) })
	abort(42 * time.Minute)

	require.True(t, stalled.Load())
	require.Equal(t, int32(causeStall), causeVal.Load())
	select {
	case <-ctx.Done():
	default:
		t.Fatal("abort closure did not cancel the turn context")
	}
}

func TestTurnStallAbortJoinsErrTurnStalled(t *testing.T) {
	f := newRetryFixture(t, "stall-join")
	var causeVal atomic.Int32
	causeVal.Store(int32(causeStall))
	var stalled atomic.Bool
	stalled.Store(true)
	f.ts.wd = streamWatchdog{stalled: &stalled}
	f.ts.watchdogCauseVal = &causeVal
	_, _, _, err := f.ts.handleStreamFailure(nil, context.Canceled, false)
	require.True(t, errors.Is(err, ErrTurnStalled))

	// A plain watchdog idle-stall (causeIdleStall) must NOT map to stalled.
	causeVal.Store(int32(causeIdleStall))
	_, _, _, err = f.ts.handleStreamFailure(nil, context.Canceled, false)
	require.False(t, errors.Is(err, ErrTurnStalled))
}
