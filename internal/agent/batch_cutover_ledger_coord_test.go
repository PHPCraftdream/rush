// Batch-world replacement of the deleted batch_cutover_facts_test.go's
// ledger coordinator fixture: the workLedger is gone, so the "cheap
// coordinator fixture" is the current batch-runtime coordinator (arbiter,
// driver hints, supervision, timeouts, sub-agent drivers all initialized)
// with NO sessions service -- the callers bind node facts themselves via
// cutoverBindFacts. COMPILE-ONLY IN THIS WORKTREE.
package agent

import "testing"

// newCutoverLedgerCoordinator builds the cheap coordinator fixture for the
// per-consumer cutover-facts/accounting tests.
func newCutoverLedgerCoordinator(t *testing.T) *coordinator {
	t.Helper()
	return newBatchRuntimeCoordinator()
}
