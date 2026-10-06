package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// This file is the abort layer of the turn-stall policy (chunk 2). The
// sampler (turn_stall_clock.go) only warns and dumps; the policy installed
// here acts on a fire: provider phases get the stuck request cancelled and
// the step re-issued (at most stallMaxRetries times), internal phases abort
// the turn immediately. abortTimeout == 0 never aborts — warn+dump still
// happen. Principle: cancel the stuck request, retry twice, then die with
// a clear reason.

// stallMaxRetries is how many times a stalled provider request is cancelled
// and re-issued before the turn is aborted with causeStall.
const stallMaxRetries = 2

// ErrTurnStalled is joined into the turn error when the stall abort fires,
// so the CLI can map the exit to exit_reason "stalled".
var ErrTurnStalled = errors.New("turn aborted: no durable progress past the stall timeout")

// turnStallPolicyInstall registers turnStallAbortPolicy exactly once, on
// the first armTurnStall — the least invasive startup hook (no coordinator
// or app wiring needed).
var turnStallPolicyInstall sync.Once

// installTurnStallPolicy lazily arms the abort policy; called from
// armTurnStall.
func installTurnStallPolicy() {
	turnStallPolicyInstall.Do(func() { SetTurnStallPolicy(turnStallAbortPolicy) })
}

// turnStallAbortPolicy reacts to one sampler fire. Identifiers and
// durations only — never prompt text or tool input. Runs on the sampler
// goroutine: every action it takes is a cancel or a non-blocking closure.
func turnStallAbortPolicy(sessionID string, phase turnPhase, sinceProgress, sinceByte time.Duration) {
	clock, ok := armedStallClocks.Load(sessionID)
	if !ok {
		return
	}
	sc, _ := clock.(*stallClock)
	if sc == nil {
		return
	}
	// Detaching a stalled synchronous tool is not destructive: it wakes the
	// model at the warn threshold (the sampler only calls past it), whatever
	// the abort timeout. Delegations and background/async jobs are excluded
	// inside stallToolStalled; a non-detachable call is left running.
	if phase.kind == "tool" {
		stallToolStalled(sessionID, phase.callID, sinceProgress)
		return
	}
	// Off switch: abortTimeout == 0 disables abort only.
	if sc.abortTimeout <= 0 || sinceProgress < sc.abortTimeout {
		return
	}
	switch phase.kind {
	case "provider":
		// A re-issued request gets a full warn window to show progress
		// before the next retry or the abort.
		if last := sc.lastStallRetryNanos.Load(); last != 0 && stallNow().UnixNano()-last < int64(streamIdleTimeoutDefault) {
			return
		}
		sc.lastStallRetryNanos.Store(stallNow().UnixNano())
		n := sc.stallRetries.Add(1)
		if n <= stallMaxRetries {
			slog.Warn("turn-stall policy: cancelling stalled provider request, retrying",
				"session_id", sessionID,
				"retry", n,
				"max_retries", stallMaxRetries,
				"since_progress", sinceProgress.Truncate(time.Second),
			)
			sc.cancelAttempt()
			return
		}
		slog.Warn("turn-stall policy: provider retries exhausted, aborting turn",
			"session_id", sessionID,
			"retries", n,
			"since_progress", sinceProgress.Truncate(time.Second),
		)
		sc.stallAbort(sinceProgress)
	case "internal":
		// No retry for internal phases: one fire past abortTimeout aborts.
		slog.Warn("turn-stall policy: internal phase stalled, aborting turn",
			"session_id", sessionID,
			"phase", phase.name,
			"since_progress", sinceProgress.Truncate(time.Second),
		)
		sc.stallAbort(sinceProgress)
	}
}

// cancelAttempt cancels the in-flight provider attempt, if one is armed.
func (sc *stallClock) cancelAttempt() {
	if cancel, ok := sc.attemptCancel.Load().(context.CancelFunc); ok && cancel != nil {
		cancel()
	}
}

// stallAbort invokes the turn's abort closure (handleWatchdogFire with
// causeStall + genCtx cancel), if one is armed.
func (sc *stallClock) stallAbort(elapsed time.Duration) {
	if abort, ok := sc.abortFn.Load().(func(time.Duration)); ok && abort != nil {
		abort(elapsed)
	}
}

// armAttemptCancel stores (or clears, with nil) the live attempt's cancel.
func (sc *stallClock) armAttemptCancel(cancel context.CancelFunc) {
	sc.attemptCancel.Store(cancel)
}

// armStallAbort stores the turn's abort closure.
func (sc *stallClock) armStallAbort(abort func(elapsed time.Duration)) {
	sc.abortFn.Store(abort)
}

// runStallRetried runs attempt until it succeeds or the stall policy's
// retries are exhausted. A context.Canceled return counts as a stall retry
// only when the policy recorded a new retry for THIS attempt (stallRetries
// advanced past try) — an operator cancel leaves the counter alone and
// returns immediately. onRetry (which flags the cut attempt's partial
// content stale, mirroring fantasy's OnRetry) runs before each re-issue.
func runStallRetried(stall *stallClock, attempt func() error, onRetry func(retry int)) error {
	if stall != nil {
		defer stall.armAttemptCancel(nil)
	}
	for try := 0; ; try++ {
		// Attribute the cancel: only a retry the POLICY issued for THIS
		// attempt (counter advanced past base) is re-issued; an operator
		// cancel leaves the counter alone and returns immediately.
		var base int32
		if stall != nil {
			base = stall.stallRetries.Load()
		}
		err := attempt()
		if err == nil || !errors.Is(err, context.Canceled) {
			return err
		}
		if stall == nil || stall.stallRetries.Load() <= base || try >= stallMaxRetries {
			return err
		}
		onRetry(try + 1)
	}
}

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
