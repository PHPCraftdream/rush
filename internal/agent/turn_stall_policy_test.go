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
//   - TestTurnStallAbortClosureFiresCauseStall guards the runTurn abort
//     closure: forceStall, causeStall stored, genCtx cancelled.
//   - TestTurnStallAbortJoinsErrTurnStalled guards handleStreamFailure's
//     ErrTurnStalled join for causeStall aborts.
//   - TestExitMapsStallAbort guards app's exit mapping of ErrTurnStalled
//     to exit_reason "stalled".

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
