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
