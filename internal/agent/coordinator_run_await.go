// runAwaitingAdmission: a blocking helper for callers that need the REAL
// outcome of a Run() call even when the mailbox was busy and this specific
// invocation only queued it (#1036).
package agent

import (
	"context"
	"sync"

	"charm.land/fantasy"
)

// runAwaitingAdmission runs call on agent and, if THIS invocation queues the
// call instead of executing it (mailbox busy, non-fail-fast), blocks
// (bounded by ctx) until the queued call's own eventual turn -- run by
// whichever goroutine drains the mailbox -- resolves, and returns THAT
// turn's outcome instead of (nil, nil). Blocking (not a returned future) is
// deliberate: every existing caller that needs this (runSubAgent) already
// calls Run synchronously and immediately consumes *fantasy.AgentResult, so
// a blocking helper needs no restructuring of that code, and per the design
// doc's own structured-concurrency principle, waiting on live work is
// correct behavior here, not a hang -- the queued turn WILL end.
//
// queued reports whether this call's own turn was admitted immediately
// (false) or had to wait behind another owner (true) -- callers that only
// need OBSERVABILITY (wakeSession's failure-visibility bookkeeping) read
// this without blocking by NOT calling this helper at all: they call
// agent.Run directly with a turnAdmission armed on ctx and check
// wasQueued() themselves -- queued is an EXPECTED, non-failure outcome for a
// wake (the notice is already durable), so there is nothing to await there.
func (c *coordinator) runAwaitingAdmission(ctx context.Context, agent SessionAgent, call SessionAgentCall) (result *fantasy.AgentResult, err error, queued bool) {
	type outcome struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan outcome, 1)
	// resolveOnce guards the hook itself, independent of the channel's own
	// buffering: this call's outcome must be delivered exactly once even if
	// something upstream (a future call site, not any today) ever invokes
	// onQueueResolved more than once for the same call.
	var resolveOnce sync.Once
	call.onQueueResolved = func(r *fantasy.AgentResult, e error) {
		resolveOnce.Do(func() {
			select {
			case done <- outcome{r, e}:
			default:
			}
		})
	}
	admission := newTurnAdmission()
	result, err = agent.Run(withTurnAdmission(ctx, admission), call)
	if !(result == nil && err == nil && admission.wasQueued()) {
		return result, err, false
	}
	select {
	case o := <-done:
		return o.result, o.err, true
	case <-ctx.Done():
		return nil, ctx.Err(), true
	}
}
