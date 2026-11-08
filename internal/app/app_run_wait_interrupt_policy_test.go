package app

import (
	"context"
	"errors"
	"testing"

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

func TestWaitingInterruptReadErrorIsNotIgnored(t *testing.T) {
	failure := errors.New("interrupt store unavailable")
	source := &waitingInterruptProbe{err: failure}
	l := nextStepLoop(source)
	step, _, err := l.nextStep()
	require.Equal(t, stepExit, step)
	require.ErrorIs(t, err, failure)
	require.Equal(t, 1, source.polls)
}
