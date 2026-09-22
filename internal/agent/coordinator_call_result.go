// Per-invocation call-result identity reporting for internal/app's terminal
// reconciliation (R8-3, round-8 audit).
//
// Terminal reconciliation used to accept ANY assistant row newer than its
// baseline snapshot as this call's own result. A committed A can release
// its session, then a legitimately later B claims it and finishes its OWN
// turn before A's event loop gets around to reconciling -- baseline alone
// cannot tell A's own row apart from B's, since both are "new" relative to
// A's snapshot taken before A's turn started. Scoping reconciliation to the
// assistant message ID THIS runInternal invocation actually produced closes
// that window; the caller's baseline stays an additional filter; this
// closes a different window than R3-1/R7-1's own-ID fixes, which protect
// retry classification inside runInternal, not the app-layer consumer of
// its result.
package agent

import (
	"context"
	"sync"
)

// CallResultRecorder captures the assistant message ID that ONE runInternal
// invocation (Run/RunWithOverrides/RunWithCredentials, they all funnel
// through it) ultimately produced. Exported because internal/app -- the
// only intended caller -- arms one per ExecuteRun phase and reads it after
// the phase's turn goroutine has returned.
type CallResultRecorder struct {
	mu sync.Mutex
	id string
}

// NewCallResultRecorder returns a recorder that has not yet captured an ID.
func NewCallResultRecorder() *CallResultRecorder { return &CallResultRecorder{} }

// record overwrites the captured ID. Called only by runInternal, once,
// right before it returns, with whatever curAttempt.resolve() produced for
// the attempt actually being returned -- the same value its own retry
// classifiers use to scope themselves to this call (R3-1).
func (r *CallResultRecorder) record(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.id = id
}

// Resolve reads the captured ID ("" if runInternal never reached the point
// of recording one, e.g. an early refusal before any message was created).
// internal/app calls this only after the turn goroutine that was handed
// this recorder's ctx has returned -- the channel send that reports that
// completion is itself a happens-before edge, so no additional
// synchronization beyond the mutex is required.
func (r *CallResultRecorder) Resolve() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id
}

type callResultRecorderCtxKey struct{}

// WithCallResultRecorder arms rec as the call-result recorder for the
// DIRECT runInternal invocation ctx is handed to. internal/app is the only
// intended caller.
func WithCallResultRecorder(ctx context.Context, rec *CallResultRecorder) context.Context {
	return context.WithValue(ctx, callResultRecorderCtxKey{}, rec)
}

// callResultRecorderFrom returns the recorder armed on ctx, or nil when the
// caller did not arm one (every non-ExecuteRun caller: interrupt replay,
// compaction, sub-agent delegation).
func callResultRecorderFrom(ctx context.Context) *CallResultRecorder {
	rec, _ := ctx.Value(callResultRecorderCtxKey{}).(*CallResultRecorder)
	return rec
}

// withoutCallResultRecorder detaches any recorder from ctx so a nested
// runInternal reached through this turn's tools (a sub-agent's child
// session shares ctx) observes nil and cannot overwrite the top-level
// caller's own captured ID with a child session's unrelated message.
func withoutCallResultRecorder(ctx context.Context) context.Context {
	return context.WithValue(ctx, callResultRecorderCtxKey{}, (*CallResultRecorder)(nil))
}
