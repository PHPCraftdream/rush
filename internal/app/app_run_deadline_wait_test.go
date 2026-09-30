package app

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// expiredCtx reports its cancellation as a deadline expiry: what `--timeout`
// and the default wall-clock cap (R6C-5) hand the loop.
type expiredCtx struct{ context.Context }

func (c expiredCtx) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

// R6C-5: the deadline of a `rush run` -- `--timeout`, or the default cap that
// replaced the old os.Exit(124) backstop -- reaching a loop that waits on a
// job that never finishes ends it through exitWait: one envelope, exit reason
// "canceled", the deadline error, the last completed answer; not an error
// exit and not a process kill.
//
// Revert-check: dropping context.DeadlineExceeded from exitWait's errors.Is
// makes the exit reason "error".
func TestRunNonInteractive_DeadlineWhileWaitingOnHeldJob_EndsThroughExitWait(t *testing.T) {
	rh := newReviewerAsyncHarness(t, true)
	base, cancel := context.WithCancel(loopCtx(t))
	defer cancel()
	var out syncBuffer
	stderr := &cancelOnWrite{substr: "still has open work", cancel: cancel}
	l := r4LoopWith(expiredCtx{base}, rh, driverSource(t, rh.app), &out, stderr)

	res, err := l.run()

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Contains(t, res.Error, "deadline exceeded")
	require.Equal(t, r4Waiting, res.FinalText, "the last completed turn is the answer")
	require.Equal(t, 1, strings.Count(out.String(), `"final_text"`), "one envelope is flushed")
}
