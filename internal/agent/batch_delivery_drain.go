package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// batchDrainPullCap is the default bound on one web notice pull. The pull
// runs under its own control context so a closing caller does not abort a
// half-written drain, and a wedged pull cannot block the web response.
var batchDrainPullCap = 15 * time.Second

var errBatchDrainPullTimeout = errors.New("batch drain: notice pull exceeded its bound")

// batchNoticePull is the drain/notice-pull seam used by the web UI. Pull must
// drain only ALREADY-SELECTED notices (R8 outbox); it must never wait for
// running batch work.
type batchNoticePull interface {
	Pull(ctx context.Context) (int, error)
}

// batchWebDrain performs the nonblocking web Drain: the pull runs detached
// from the caller's context (bounded by cap) and Drain returns as soon as the
// caller goes away or the bound expires — it NEVER blocks on running batch
// work and never waits for a root's Done.
func batchWebDrain(ctx context.Context, pull batchNoticePull, cap time.Duration) (int, error) {
	if pull == nil {
		return 0, errors.New("batch drain: notice pull is required")
	}
	if cap <= 0 {
		cap = batchDrainPullCap
	}
	ctl := context.WithoutCancel(ctx)
	ctl, cancel := context.WithTimeout(ctl, cap)
	defer cancel()
	type drainOutcome struct {
		n   int
		err error
	}
	done := make(chan drainOutcome, 1)
	go func() {
		n, err := pull.Pull(ctl)
		done <- drainOutcome{n, err}
	}()
	// A caller already cancelled before the pull started is not "gone
	// mid-wait": the pull still drains, bounded by its own cap. Only a
	// cancellation that arrives while waiting stops the wait.
	callerGone := ctx.Done()
	if ctx.Err() != nil {
		callerGone = nil
	}
	select {
	case o := <-done:
		return o.n, o.err
	case <-callerGone:
		// Caller gone: stop waiting. The pull keeps its own bound.
		return 0, ctx.Err()
	case <-ctl.Done():
		return 0, fmt.Errorf("%w after %s", errBatchDrainPullTimeout, cap)
	}
}
