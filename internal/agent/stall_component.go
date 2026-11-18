package agent

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
)

type activeStallClock = drainctl.StallClock

func stallSetPhase(sessionID, kind, name string) { drainctl.StallSetPhase(sessionID, kind, name) }

// stallAbortClosure builds the abort closure armed onto the turn's stall
// clock: it marks the watchdog stalled (so handleStreamFailure takes the
// watchdog-stall branch), stores causeStall through the existing fire path
// (finish part written there), and cancels genCtx to unblock the turn.
func (a *sessionAgent) stallAbortClosure(
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel Model,
	cancel context.CancelFunc,
	forceStall func(),
) func(elapsed time.Duration) {
	return func(elapsed time.Duration) {
		if forceStall != nil {
			forceStall()
		}
		a.handleWatchdogFire(causeStall, elapsed, sessionID, watchdogCauseVal, toolMaxDuration, idleTimeout, smartModel)
		cancel()
	}
}
