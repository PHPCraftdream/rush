// Per-invocation admission outcome reporting for coordinator_run.go's
// retry loop (R7-1, round-7 audit).
//
// runInternal must know whether THIS agent.Run invocation executed a turn
// or was merely queued behind a current owner, BEFORE it classifies retry
// evidence. The (nil, nil) queued return cannot carry that distinction
// (internal/app detects the queued case by exactly that shape at two
// boundaries: app_run_turn.go and app.go's pump adapter), and a sentinel
// error would have to be converted back at every internal Run call site
// (coordinator_interrupt.go's RunSessionAgentCall, agent_compaction.go's
// compaction drain, coordinator_subagents.go's sub-agent run) -- each a
// silent behavior change if forgotten. So the status travels beside the
// contract: runInternal arms a recorder on the ctx it hands to Run;
// sessionAgent.Run's queueing branch marks the recorder it finds there
// and still returns (nil, nil).

package agent

import (
	"context"
	"sync"
)

// turnAdmission records whether ONE agent.Run invocation executed a turn
// or was queued behind a current owner. The flag is mutex-guarded because
// the recorder is written inside Run and read only after Run has returned
// to its caller; keep it cheap, since every Run invocation through
// coordinator_run.go arms one.
type turnAdmission struct {
	mu     sync.Mutex
	queued bool
}

// newTurnAdmission returns a recorder whose invocation has not yet
// reported any admission outcome.
func newTurnAdmission() *turnAdmission { return &turnAdmission{} }

// markQueued records that this invocation queued the call instead of
// executing it. Called ONLY by sessionAgent.Run's queueing branch, and
// only on the direct caller's recorder (Run detaches the recorder from
// ctx before anything downstream runs, so nested Runs -- a sub-agent's
// child session, a queue drain -- can never mark an ancestor's recorder).
func (t *turnAdmission) markQueued() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.queued = true
}

// wasQueued reports whether the invocation this recorder was armed for
// queued its call instead of executing a turn.
func (t *turnAdmission) wasQueued() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.queued
}

// turnAdmissionCtxKey is the unexported key turnAdmission values travel
// under, the same ctx-carried pattern as callOptions and
// reservedOwnership.
type turnAdmissionCtxKey struct{}

// withTurnAdmission arms a as the admission recorder for the DIRECT Run
// invocation this ctx is handed to. Only one recorder is ever reachable
// through a ctx chain: withoutTurnAdmission detaches it before anything
// downstream runs.
func withTurnAdmission(ctx context.Context, a *turnAdmission) context.Context {
	return context.WithValue(ctx, turnAdmissionCtxKey{}, a)
}

// turnAdmissionFrom returns the recorder armed on ctx, or nil when the
// caller did not arm one (every non-coordinator Run caller).
func turnAdmissionFrom(ctx context.Context) *turnAdmission {
	a, _ := ctx.Value(turnAdmissionCtxKey{}).(*turnAdmission)
	return a
}

// withoutTurnAdmission detaches any recorder from ctx so a nested Run
// reached through this turn (sub-agent delegation, queue-drain) observes
// nil and cannot mark an ancestor invocation's admission outcome.
func withoutTurnAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnAdmissionCtxKey{}, (*turnAdmission)(nil))
}
