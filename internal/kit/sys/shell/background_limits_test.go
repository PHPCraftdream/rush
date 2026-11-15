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
	// Lower THIS manager's cap. What is under test is the atomicity of the
	// check-and-insert, not the production value: racing cap+20 real
	// processes to prove a mutex holds costs more than the property it
	// demonstrates, and on Windows the survivors block TempDir cleanup.
	// Per-manager, so t.Parallel stays safe.
	manager.SetMaxJobs(10)
	cap := manager.MaxJobs()

	attempts := cap + 20
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

	// Bound against THIS manager's cap, not the package constant. Lowering
	// the cap to 10 while still asserting "<= 50" made the test vacuous:
	// only 30 goroutines race, so 50 could never be exceeded and deleting
	// startMu entirely would not have failed it. The cap is what the
	// atomicity is protecting.
	require.LessOrEqual(t, int(succeeded.Load()), cap,
		"concurrent Start calls must never overshoot the cap")
	require.Equal(t, manager.ActiveJobs(), int(succeeded.Load()),
		"all retained jobs are still active here, so active count must equal succeeded")
	require.LessOrEqual(t, manager.shells.Len(), cap)

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
	// Same reasoning as above: the regression — completed jobs still
	// counting against the cap — reproduces at any cap, and each iteration
	// here waits for its job to finish.
	manager.SetMaxJobs(10)

	n := manager.MaxJobs() + 5 // comfortably past the limit
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
	// Pin THIS manager's cap. newBackgroundShellManager honours
	// RUSH_MAX_BACKGROUND_JOBS, so asserting against the bare constant
	// made the test fail on any host that had set the knob — measured:
	// RUSH_MAX_BACKGROUND_JOBS=200 turned the rejection assertion into
	// "An error is expected but got nil". A low fixed cap also keeps the
	// fill loop from spawning 50 real processes to prove one rejection.
	manager.SetMaxJobs(6)
	cap := manager.MaxJobs()

	for range cap {
		_, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
		require.NoError(t, err)
	}
	require.Equal(t, cap, manager.ActiveJobs(),
		"all started jobs are active")

	// The (cap+1)th job must be rejected.
	_, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "maximum number of background jobs")

	// Clean up the long-running jobs.
	manager.KillAll(t.Context())
	require.Zero(t, manager.ActiveJobs())
}

// The cap is the operator's to raise, and the env var is how.
//
// The default stays at 50 because raising it was measured to destabilise
// this repository's own suite (internal/shell 5s -> 149s, timing-sensitive
// siblings failing), so the escape hatch has to actually work — otherwise
// the answer to "50 killed my session" is still "edit the source".
func TestMaxJobsFromEnv(t *testing.T) {
	t.Run("unset uses the default", func(t *testing.T) {
		t.Setenv("RUSH_MAX_BACKGROUND_JOBS", "")
		require.Equal(t, MaxBackgroundJobs, maxJobsFromEnv())
	})

	t.Run("a positive value is honoured", func(t *testing.T) {
		t.Setenv("RUSH_MAX_BACKGROUND_JOBS", "500")
		require.Equal(t, 500, maxJobsFromEnv())
	})

	t.Run("a new manager picks it up", func(t *testing.T) {
		t.Setenv("RUSH_MAX_BACKGROUND_JOBS", "7")
		require.Equal(t, 7, newBackgroundShellManager().MaxJobs())
	})

	// Falling back rather than failing is deliberate: this is a convenience
	// knob, and a typo in it must not stop rush from starting.
	for _, bad := range []string{"nonsense", "0", "-5", "3.5"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			t.Setenv("RUSH_MAX_BACKGROUND_JOBS", bad)
			require.Equal(t, MaxBackgroundJobs, maxJobsFromEnv(),
				"a malformed value must fall back to the default, not disable the limit")
		})
	}
}
