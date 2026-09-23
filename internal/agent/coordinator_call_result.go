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
	mu         sync.Mutex
	id         string
	ids        map[string]struct{}
	generation uint64
	sealed     bool
}

// NewCallResultRecorder returns a recorder that has not yet captured an ID.
func NewCallResultRecorder() *CallResultRecorder { return &CallResultRecorder{} }

// BeginCall clears identity accumulated by an earlier call using the same
// context, as can happen when a durable drain executes several queue rows.
func (r *CallResultRecorder) BeginCall() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.id = ""
	r.ids = nil
	r.generation++
	r.sealed = false
	r.mu.Unlock()
}

// Seal rejects callbacks that arrive after this call has completed.
func (r *CallResultRecorder) Seal() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.sealed = true
	r.mu.Unlock()
}

// Capture returns a callback restricted to the current call generation.
func (r *CallResultRecorder) Capture() func(string) {
	if r == nil {
		return func(string) {}
	}
	r.mu.Lock()
	generation := r.generation
	r.mu.Unlock()
	return func(id string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if id == "" || r.generation != generation || r.sealed {
			return
		}
		r.id = id
		if r.ids == nil {
			r.ids = make(map[string]struct{})
		}
		r.ids[id] = struct{}{}
	}
}

// Owns reports whether id was created by this recorded call.
func (r *CallResultRecorder) Owns(id string) bool {
	if r == nil || id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ids[id]
	return ok
}

// IDs returns the assistant rows created by the current call.
func (r *CallResultRecorder) IDs() map[string]struct{} {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make(map[string]struct{}, len(r.ids))
	for id := range r.ids {
		ids[id] = struct{}{}
	}
	return ids
}

// RecordConfirmed adopts an identity confirmed by a durable queue execution.
func (r *CallResultRecorder) RecordConfirmed(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids == nil {
		r.ids = make(map[string]struct{})
	}
	r.id = id
	r.ids[id] = struct{}{}
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
