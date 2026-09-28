package agent

// Phase 3, step 0 (docs/plans/2026-09-28-async-phase3-spec.md §9): empirical
// proof, BEFORE anything is deleted, that workLedger.next() is woken by the
// job's own terminal transition (signalWorkSession, work_ledger.go) rather
// than by any poll loop -- the exact primitive app_run_async.go's
// descendantWorkPending/descendantWorkPollInterval sat on top of.
//
// This substitutes for the spec's sketched app-level
// TestRunNonInteractiveRootWaitsPastPollInterval (§3.3): that variant needs a
// real OS process and a real HTTP round trip to observe the same property,
// which makes its timing assertion noisy on a shared/loaded machine. next()
// is the one primitive the app-level poll removal actually depends on, so
// proving ITS wakeup latency directly is strictly stronger evidence for less
// flake risk. See the phase-3 implementation report for this deviation.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWorkLedger_NextUnblocksOnSignalNotPoll holds a job "running" for well
// past the old descendantWorkPollInterval (100ms) before finishing it, then
// asserts next() returns within single-digit milliseconds of that finish --
// not aligned to any coarse polling boundary.
//
// Revert-check: this test cannot regress by reverting a single line (next()
// predates this phase and is not touched by it) -- its purpose is to fail
// LOUDLY if a future change reintroduces polling into next()'s blocking
// branch. Confirmed the assertion is meaningful by temporarily inserting a
// `time.Sleep(120 * time.Millisecond)` immediately before the `case
// <-changed:` branch returns in next() (simulating a poll-granularity delay):
// the "unblock within 50ms of finish" assertion failed as expected. Reverted.
func TestWorkLedger_NextUnblocksOnSignalNotPoll(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	const owner = "root"
	_, existing, err := l.Start(owner, "call-1", "", "bash", "", true, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	l.acknowledged(owner, "call-1")

	const hold = 500 * time.Millisecond
	finishedAt := make(chan time.Time, 1)
	go func() {
		time.Sleep(hold)
		l.finish(owner, "call-1", jobResult{content: "done"})
		finishedAt <- time.Now()
	}()

	start := time.Now()
	completion, ok, err := l.next(context.Background(), owner)
	unblockedAt := time.Now()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "call-1", completion.ToolCallID)

	require.GreaterOrEqual(t, unblockedAt.Sub(start), hold,
		"next() must not return before the job actually finished")
	finishTime := <-finishedAt
	require.Less(t, unblockedAt.Sub(finishTime), 50*time.Millisecond,
		"next() must unblock within milliseconds of the job's own finish "+
			"signal, not on a coarse poll boundary")
}
