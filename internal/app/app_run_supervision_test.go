package app

// Supervision (task #1043, docs/plans/2026-09-27-async-structured-
import (
	"testing"
)

// TestRunNonInteractiveExitsPromptlyWithSupervisionArmedAndNoOtherWork is the
// black-box confirmation at the layer `rush run` actually runs at: a run whose
// background work arms supervision (on by default) still exits promptly once
// that work and the resulting turn finish, nowhere near the 5-minute default
// supervision interval. The batch runtime is unconditional, so the fixture is
// svRunSupervisionBatchSynchronousCommand
// (app_run_supervision_batch_test.go); the batch scope's own design keeps the
// supervision timer entirely outside the live batch work map, so the CLI exit
// path is unaffected by it by construction.
//
// Revert-check: not performed by neutering production code (that would
// require literally making supervision hold the batch work scope open,
// which supervision.go's design makes structurally impossible without a
// much larger, unrelated rewrite -- see that file's own doc for why). The
// property this test checks is instead already covered by internal/agent's
// TestSupervision_StateRemovedOnScopeClose (revert-checked there against the
// scope-close cleanup hook) at the exact mechanism this test's CLI-level
// pass depends on.
func TestRunNonInteractiveExitsPromptlyWithSupervisionArmedAndNoOtherWork(t *testing.T) {
	svRunSupervisionBatchSynchronousCommand(t)
}
