// Package-agent file for the psv supervision-arming restoration: the batch
// delivery seam is the work-STARTED boundary (the deleted asyncTool launch
// path's replacement), so arming points are created at the batch delivery's
// STARTED boundaries. The delivery answers with the started response exactly
// when live work escaped to the background — the same condition the legacy
// path armed on — and never for an inline-answered completion, a sync
// answer, a denied/failed invocation or a lost started election.
package agent

import "context"

// psvArmSupervision invokes deps.ArmSupervision when present. Nil-safe and
// owner-safe: an install without arming wiring (flag-OFF, existing tests)
// and an empty owner are silent no-ops. Called strictly after the delivery's
// started election returned successfully and never under a lock the
// delivery holds (it holds none), preserving supervision.go's
// lock-ordering note: workLedger.mu and supervisionRegistry.mu are never
// nested with any delivery lock.
func (b *batchDeliveryImpl) psvArmSupervision(ctx context.Context, owner string) {
	if b == nil || b.deps.ArmSupervision == nil || owner == "" {
		return
	}
	b.deps.ArmSupervision(ctx, owner)
}

// PsvBatchSupervisionArmingLate builds the production batch delivery
// arming func for an app-level wiring site whose coordinator is assigned
// AFTER the batch install is built (get is re-read on every call). The
// returned func calls the late-bound coordinator's noteWorkStarted, which
// already: exempts delegated children
// (subAgentDrivers), resolves config + per-call CallOptions
// (--no-supervision / --supervision-interval) from the PASSED ctx, is
// idempotent for an already-armed session, and resumes a paused session
// with fresh backoff (legacy new-work resume semantics). A not-yet-bound
// or non-concrete coordinator is a silent no-op, matching
// PscCoordinatorChildReactionFactsLate's late-binding discipline.
func PsvBatchSupervisionArmingLate(get func() Coordinator) func(context.Context, string) {
	return func(ctx context.Context, owner string) {
		if get == nil || owner == "" {
			return
		}
		cc, ok := get().(*coordinator)
		if !ok || cc == nil {
			return
		}
		cc.noteWorkStarted(ctx, owner)
	}
}
