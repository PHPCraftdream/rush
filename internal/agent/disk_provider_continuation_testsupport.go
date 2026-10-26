package agent

// Test-support seam for the durable continuation owner (R12-H2b §2.4).
// This file is additive only: it delegates to the production hold helper
// without changing any production behavior.

// HoldDurableContinuationOwnerForTest exposes the production row-keyed
// Continuation hold to test fixtures that enqueue durable rows without the
// production producers. The instance MUST be the exact run-queue row id the
// later enqueue uses, so the pump rebuild's own release
// (releaseDurableContinuationOwner) lands on the same key. ok=false means the
// binding is not live; the fixture must fail rather than enqueue a dead row.
func HoldDurableContinuationOwnerForTest(caps *DiskProviderRegistry, bindingID, rowKey string) (release func(), ok bool) {
	return holdDurableContinuationOwner(caps, bindingID, rowKey)
}
