package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"

	"github.com/stretchr/testify/require"
)

// Each test names the single production behavior it must catch (revert-
// check doc):
//
//   - TestTurnStallAbortClosureFiresCauseStall guards the runTurn abort
//     closure: forceStall, watchdog.CauseStall stored, genCtx cancelled.
//   - TestTurnStallAbortJoinsErrTurnStalled guards handleStreamFailure's
//     ErrTurnStalled join for watchdog.CauseStall aborts.
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
	require.Equal(t, int32(watchdog.CauseStall), causeVal.Load())
	select {
	case <-ctx.Done():
	default:
		t.Fatal("abort closure did not cancel the turn context")
	}
}

func TestTurnStallAbortJoinsErrTurnStalled(t *testing.T) {
	f := newRetryFixture(t, "stall-join")
	var causeVal atomic.Int32
	causeVal.Store(int32(watchdog.CauseStall))
	var stalled atomic.Bool
	stalled.Store(true)
	f.ts.wd = watchdog.StreamWatchdog{Stalled: &stalled}
	f.ts.watchdogCauseVal = &causeVal
	_, _, _, err := f.ts.handleStreamFailure(nil, context.Canceled, false)
	require.True(t, errors.Is(err, ErrTurnStalled))

	// A plain watchdog idle-stall (watchdog.CauseIdleStall) must NOT map to stalled.
	causeVal.Store(int32(watchdog.CauseIdleStall))
	_, _, _, err = f.ts.handleStreamFailure(nil, context.Canceled, false)
	require.False(t, errors.Is(err, ErrTurnStalled))
}
