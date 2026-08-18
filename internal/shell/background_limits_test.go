// MaxBackgroundJobs limit semantics: the concurrent Start race, the cap
// when all jobs are active, and slot reuse once jobs complete.
package shell

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBackgroundShellManager_Start_AtomicLimitCheck proves the
// check-then-insert sequence in Start (Len() >= MaxBackgroundJobs, then
// Set()) can't be overshot by concurrent callers racing past the check
// before either has inserted. Runs many concurrent Start calls against a
// manager whose limit is effectively the real MaxBackgroundJobs constant, and
// asserts the resulting count never exceeds it.
func TestBackgroundShellManager_Start_AtomicLimitCheck(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	const attempts = MaxBackgroundJobs + 20
	var wg sync.WaitGroup
	var succeeded atomic.Int64
	for range attempts {
		wg.Go(func() {
			// Use a long-running command so started jobs STAY active for the
			// whole test. The limit is enforced on ACTIVE (not total/retained)
			// jobs: if any completed mid-test they would legitimately free
			// their slot, and the "never overshoot" invariant could no longer
			// be expressed as succeeded <= MaxBackgroundJobs.
			_, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
			if err == nil {
				succeeded.Add(1)
			}
		})
	}
	wg.Wait()

	require.LessOrEqual(t, int(succeeded.Load()), MaxBackgroundJobs,
		"concurrent Start calls must never overshoot MaxBackgroundJobs")
	require.Equal(t, manager.ActiveJobs(), int(succeeded.Load()),
		"all retained jobs are still active here, so active count must equal succeeded")
	require.LessOrEqual(t, manager.shells.Len(), MaxBackgroundJobs)

	// Clean up any still-tracked shells.
	manager.KillAll(t.Context())
}

// TestBackgroundShellManager_LimitIgnoresCompletedJobs proves the
// MaxBackgroundJobs limit counts only ACTIVE jobs: starting MORE than
// MaxBackgroundJobs jobs SEQUENTIALLY — each completing before the next starts
// — must succeed for every one of them, because finished jobs free their
// concurrency slot immediately. On the OLD Len()-based limit this test fails
// at the 51st Start: the 50 finished (but retained) jobs kept shells.Len() at
// 50, so the 51st was rejected even though nothing was running.
func TestBackgroundShellManager_LimitIgnoresCompletedJobs(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	const n = MaxBackgroundJobs + 5 // 55: comfortably past the limit
	for i := range n {
		bg, err := manager.Start(t.Context(), workingDir, nil, "echo hi", "")
		require.NoError(t, err,
			"job %d must start even though %d completed jobs are retained", i, i)
		// Wait for THIS job to finish before starting the next, so at most one
		// is active at a time and its slot is freed before the next Start.
		require.Eventually(t, bg.IsDone, 5*time.Second, 20*time.Millisecond,
			"job %d should complete so its slot is freed", i)
	}

	// No job is active after the loop; the retained (completed) entries are
	// still in the map, which is exactly the regression condition the old
	// Len()-based check choked on.
	require.Zero(t, manager.ActiveJobs(),
		"no jobs should be active after all completed")
	require.Equal(t, n, manager.shells.Len(),
		"completed jobs are retained in the map for querying")
}

// TestBackgroundShellManager_LimitBlocksWhenAllActive proves the limit still
// holds for genuinely concurrent jobs: with MaxBackgroundJobs long-running jobs
// all active at once, the next Start must be rejected. The active-counter fix
// must not weaken the real concurrency cap.
func TestBackgroundShellManager_LimitBlocksWhenAllActive(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	for range MaxBackgroundJobs {
		_, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
		require.NoError(t, err)
	}
	require.Equal(t, MaxBackgroundJobs, manager.ActiveJobs(),
		"all started jobs are active")

	// The (MaxBackgroundJobs+1)th job must be rejected.
	_, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "maximum number of background jobs")

	// Clean up the long-running jobs.
	manager.KillAll(t.Context())
	require.Zero(t, manager.ActiveJobs())
}
