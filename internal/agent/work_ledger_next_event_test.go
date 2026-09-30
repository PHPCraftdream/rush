package agent

// Phase 3, step 0 (docs/plans/2026-09-28-async-phase3-spec.md §9): empirical
// proof that the ledger's wakeup primitive is signal-driven, never poll-driven.
// Originally proved against workLedger.next() (the in-memory ready-queue
// wait). Phase 4 step 4 deletes next()/the ready queue entirely (doc sec.3.5:
// "the loop waits for a hint channel plus the 60s pass, not the memory
// queue") -- waitForHint/bumpHint are the primitive the CLI loop (via
// coordinator.WaitForHint) actually depends on now, so this test moved to
// pin THEIR wakeup latency instead of next()'s.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWorkLedger_WaitForHintUnblocksOnSignalNotPoll holds a hint back for
// well past any plausible polling interval before bumping it, then asserts
// waitForHint returns within single-digit milliseconds of that bump -- not
// aligned to any coarse polling boundary.
//
// Revert-check: this test cannot regress by reverting a single line
// (waitForHint predates nothing here -- it is the phase-4 replacement being
// pinned) -- its purpose is to fail LOUDLY if a future change reintroduces
// polling into waitForHint's blocking branch. Confirmed the assertion is
// meaningful by temporarily inserting a `time.Sleep(120 * time.Millisecond)`
// immediately before waitForHint's `return true` on a real hint (simulating
// a poll-granularity delay): the "unblock within 50ms of the hint"
// assertion failed as expected. Reverted.
func TestWorkLedger_WaitForHintUnblocksOnSignalNotPoll(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	const owner = "root"
	since := l.hintSeqOf(owner)

	const hold = 500 * time.Millisecond
	hintedAt := make(chan time.Time, 1)
	go func() {
		time.Sleep(hold)
		l.bumpHint(owner)
		hintedAt <- time.Now()
	}()

	start := time.Now()
	ok := l.waitForHint(context.Background(), owner, since)
	unblockedAt := time.Now()
	require.True(t, ok, "waitForHint must report a real hint, not a ctx/close exit")

	require.GreaterOrEqual(t, unblockedAt.Sub(start), hold,
		"waitForHint must not return before the hint actually fired")
	hintTime := <-hintedAt
	require.Less(t, unblockedAt.Sub(hintTime), 50*time.Millisecond,
		"waitForHint must unblock within milliseconds of bumpHint, not on a "+
			"coarse poll boundary")
}
