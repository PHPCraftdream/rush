package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

type waitingInterruptProbe struct {
	agent.ReactionDebtSource
	polls int
	err   error
}

func (p *waitingInterruptProbe) PollExternalInterrupt(_ context.Context, _ string, beforeDelivery func() error) (string, error) {
	if beforeDelivery != nil {
		if err := beforeDelivery(); err != nil {
			return "", err
		}
	}
	p.polls++
	return "", p.err
}

func TestWaitingInterruptStopAndContextTakePriority(t *testing.T) {
	for _, cancelledContext := range []bool{false, true} {
		t.Run(map[bool]string{false: "operator_stop", true: "context_cancel"}[cancelledContext], func(t *testing.T) {
			source := &waitingInterruptProbe{}
			l := nextStepLoopCancel(source, !cancelledContext)
			if cancelledContext {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				l.ctx = ctx
			}
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("interrupt polls=%d cancelled_context=%v", source.polls, cancelledContext)
				}
			})
			step, _, err := l.nextStep()
			require.Error(t, err)
			if cancelledContext {
				require.Equal(t, stepExit, step)
			} else {
				require.Equal(t, stepCanceled, step)
			}
			require.Zero(t, source.polls, "a stopped run must not consume an operator interrupt")
		})
	}
}

// A poll that keeps failing is not ignored: the loop retries it inside the
// DB-read retry bound and then ends the run with the poll error.
//
// Revert-check: exiting on the first poll error (the pre-fix code) makes the
// transient test below red; dropping the bound makes this one hang.
func TestWaitingInterruptReadErrorIsNotIgnored(t *testing.T) {
	prev := cliDBErrorRetryOverallLimit
	cliDBErrorRetryOverallLimit = 700 * time.Millisecond
	t.Cleanup(func() { cliDBErrorRetryOverallLimit = prev })
	failure := errors.New("interrupt store unavailable")
	source := &waitingInterruptProbe{err: failure}
	l := nextStepLoop(source)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("interrupt polls=%d limit=%s pause=%s", source.polls, cliDBErrorRetryOverallLimit, cliDBRetryPause)
		}
	})
	step, _, err := l.nextStep()
	require.Equal(t, stepExit, step)
	require.ErrorIs(t, err, failure)
	require.GreaterOrEqual(t, source.polls, 2, "a failing poll must be retried before the run gives up")
}

// transientInterruptSource fails its first interrupt polls (a store busy past
// the poll deadline) and then answers; its scope reports nothing open.
type transientInterruptSource struct {
	scriptedScopeSource
	failures int
	polls    int
}

func (s *transientInterruptSource) PollExternalInterrupt(context.Context, string, func() error) (string, error) {
	s.polls++
	if s.polls <= s.failures {
		return "", context.DeadlineExceeded
	}
	return "", nil
}

// A poll that times out once must not end the run (it killed long agent runs
// with "pending interrupt: context deadline exceeded" while the store was
// busy): the loop retries and goes on to read the scope.
func TestWaitingInterruptTransientPollErrorIsRetried(t *testing.T) {
	source := &transientInterruptSource{
		scriptedScopeSource: scriptedScopeSource{script: []scopeAnswer{{state: agent.CLIScopeState{}}}},
		failures:            1,
	}
	l := nextStepLoop(source)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("interrupt polls=%d scope calls=%d", source.polls, source.calls.Load())
		}
	})
	step, _, err := l.nextStep()
	require.NoError(t, err)
	require.Equal(t, stepExit, step, "nothing open: the scope closes the run normally")
	require.Equal(t, 2, source.polls, "one failed poll, then one that answers")
	require.EqualValues(t, 1, source.calls.Load(), "the scope is read after the retried poll")
}
