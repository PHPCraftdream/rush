package hooks

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// wedgeRunShell replaces the shell executor with one that never yields to
// ctx cancellation, forcing every runOne onto the abandon path. Workers
// block until the returned release func is called; t.Cleanup closes the
// release channel (idempotently) and waits for every invoked worker to
// return so the test exits cleanly under -race.
//
// It returns the close-once release func.
//
// Tests using this helper are deliberately NOT t.Parallel: they swap the
// package-level runShell seam and assert the global abandonedWorkers
// counter, neither of which tolerates concurrent hook runs from other
// tests in the package.
func wedgeRunShell(t *testing.T) func() {
	t.Helper()

	origRunShell := runShell
	t.Cleanup(func() { runShell = origRunShell })

	release := make(chan struct{})
	// invoked counts how many times the fake executor was entered;
	// returned is signalled once per invocation that has unwound. The
	// pair lets cleanup drain a dynamically-sized set of workers without
	// the Add-after-Wait race of a plain WaitGroup.
	var invoked atomic.Int64
	returned := make(chan struct{}, 128)

	runShell = func(_ context.Context, _ shell.RunOptions) error {
		invoked.Add(1)
		defer func() { returned <- struct{}{} }()
		<-release
		return nil
	}

	var closeOnce sync.Once
	releaseFunc := func() {
		closeOnce.Do(func() { close(release) })
	}
	t.Cleanup(func() {
		releaseFunc()
		for n := invoked.Load(); n > 0; n-- {
			<-returned
		}
		// <-returned only confirms the fake runShell call returned, not
		// that runner.go's abandoned-worker bookkeeping has decremented
		// AbandonedWorkers() yet -- that happens in the abandoning
		// goroutine, a step after runShell returns. Under CI scheduling
		// pressure that gap can outlast this cleanup, leaking a nonzero
		// count into the NEXT test in this file (they deliberately run
		// sequentially and share the package-level counter, see this
		// function's own doc above) -- confirmed as the cause of a real
		// flake: TestRunnerAbandonHardKillSeam's hook run got rejected
		// outright ("too many abandoned hook workers") because an
		// earlier test's workers hadn't finished decrementing yet.
		require.Eventually(t, func() bool { return AbandonedWorkers() == 0 },
			5*time.Second, 10*time.Millisecond,
			"abandoned-worker count must drain to zero before the next test runs")
	})
	return releaseFunc
}

// TestRunnerAbandonedWorkersBounded proves the abandoned-worker count is
// bounded. Wave 1 spawns exactly maxAbandonedWorkers simultaneously-wedged
// hooks: they all pass the saturation check while the counter is still
// zero, abandon at ~timeout+abandonGrace, and land the counter exactly on
// the cap — never above it. Wave 2 then fires more wedged hook runs than
// there is head-room; every one must be rejected before spawning a worker
// (fast return, no counter growth). Total wedged spawn attempts across
// both waves exceed the cap, while the tracked count never does.
//
// The counter can only grow via abandonments and there are at most
// wave-1 workers alive to abandon (wave 2 spawns none), so the Equal
// assertions below are deterministic — no sampling needed.
func TestRunnerAbandonedWorkersBounded(t *testing.T) {
	release := wedgeRunShell(t)

	const wave2 = 20
	wave1 := maxAbandonedWorkers // Exactly the cap: never above it.

	hookCfg := config.HookConfig{
		Command: "wedged-hook",
		Timeout: 1,
	}
	r := NewRunner([]config.HookConfig{hookCfg}, t.TempDir(), t.TempDir())

	// Wave 1: cap many simultaneous wedged hooks.
	var waveWG sync.WaitGroup
	waveWG.Add(wave1)
	results := make([]AggregateResult, wave1)
	errs := make([]error, wave1)
	start := time.Now()
	for i := 0; i < wave1; i++ {
		go func(i int) {
			defer waveWG.Done()
			results[i], errs[i] = r.Run(
				context.Background(), EventPreToolUse, "sess", "bash", `{}`,
			)
		}(i)
	}
	waveWG.Wait()
	wave1Elapsed := time.Since(start)

	for i := 0; i < wave1; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, DecisionNone, results[i].Decision,
			"an abandoned hook must stay non-blocking")
	}
	// The caller timing contract must survive the abandonment
	// bookkeeping: all wave-1 callers returned by ~timeout+abandonGrace.
	require.Less(t, wave1Elapsed, 1*time.Second+abandonGrace+3*time.Second,
		"wedged hooks must not block their callers past timeout+abandonGrace")

	// The boundedness core: exactly cap wedged workers are tracked,
	// and the tracked count never exceeds the cap.
	require.Equal(t, int64(wave1), AbandonedWorkers(),
		"every wedged hook past timeout+grace must be tracked")
	require.LessOrEqual(t, AbandonedWorkers(), int64(maxAbandonedWorkers),
		"tracked abandoned workers must never exceed the cap")

	// Wave 2: over the cap, new hook runs must be rejected outright
	// instead of piling on more abandoned workers.
	for i := 0; i < wave2; i++ {
		runStart := time.Now()
		res, err := r.Run(context.Background(), EventPreToolUse, "sess", "bash", `{}`)
		require.NoError(t, err)
		require.Equal(t, DecisionNone, res.Decision)
		require.Less(t, time.Since(runStart), 1200*time.Millisecond,
			"a hook run past the abandoned-worker cap must be rejected before spawning, "+
				"not run to its own timeout+grace (~2s)")
		require.Equal(t, int64(wave1), AbandonedWorkers(),
			"a rejected run must not grow the abandoned-worker count")
	}

	// Releasing the wedged workers must drain the counter back to zero:
	// the decrement fires when an abandoned worker eventually finishes,
	// even though nobody was waiting on it anymore.
	release()
	require.Eventually(t, func() bool { return AbandonedWorkers() == 0 },
		5*time.Second, 10*time.Millisecond,
		"abandoned-worker count must drain to zero once the workers finish")
}

// TestRunnerAbandonedDoesNotBlockCaller pins the deliberate existing
// behaviour: a wedged hook does not block its caller past
// timeout+abandonGrace, and the abandoned worker becomes observable in
// the abandoned-workers counter.
func TestRunnerAbandonedDoesNotBlockCaller(t *testing.T) {
	wedgeRunShell(t)

	hookCfg := config.HookConfig{
		Command: "wedged-hook",
		Timeout: 1,
	}
	r := NewRunner([]config.HookConfig{hookCfg}, t.TempDir(), t.TempDir())

	start := time.Now()
	res, err := r.Run(context.Background(), EventPreToolUse, "sess", "bash", `{}`)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, DecisionNone, res.Decision)
	require.Less(t, elapsed, 1*time.Second+abandonGrace+1500*time.Millisecond,
		"a wedged hook must not block its caller past timeout+abandonGrace")
	require.Equal(t, int64(1), AbandonedWorkers(),
		"the abandoned worker must be tracked so the count is bounded")
}

// TestRunnerAbandonHardKillSeam proves the abandon path attempts a hard
// kill of exactly the processes the wedged worker registered, without
// blocking the caller: the seam replaces session.KillProcess, so no real
// pid is ever signalled.
func TestRunnerAbandonHardKillSeam(t *testing.T) {
	origRunShell := runShell
	origSeam := abandonSeam
	t.Cleanup(func() {
		runShell = origRunShell
		abandonSeam = origSeam
	})

	release := make(chan struct{})
	var closeOnce sync.Once
	releaseFunc := func() {
		closeOnce.Do(func() { close(release) })
	}
	t.Cleanup(releaseFunc)

	seamPids := make(chan []int, 1)
	const fakePID = 424242

	abandonSeam = func(pids []int) { seamPids <- pids }
	runShell = func(_ context.Context, opts shell.RunOptions) error {
		// Simulate the interpreter having started one child process.
		if opts.RegisterProcess != nil {
			opts.RegisterProcess(fakePID)
		}
		<-release
		return nil
	}

	hookCfg := config.HookConfig{
		Command: "wedged-with-child",
		Timeout: 1,
	}
	r := NewRunner([]config.HookConfig{hookCfg}, t.TempDir(), t.TempDir())

	start := time.Now()
	res, err := r.Run(context.Background(), EventPreToolUse, "sess", "bash", `{}`)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, DecisionNone, res.Decision)
	require.Equal(t, int64(1), AbandonedWorkers(),
		"the abandoned worker must be tracked")

	// The hard-kill attempt fires asynchronously (off the caller's
	// goroutine) with exactly the pids the worker registered.
	select {
	case pids := <-seamPids:
		require.Equal(t, []int{fakePID}, pids,
			"the hard kill must target the worker's registered pids")
	case <-time.After(5 * time.Second):
		t.Fatal("abandonSeam was not invoked on the abandon path")
	}
	require.Less(t, elapsed, 1*time.Second+abandonGrace+1500*time.Millisecond,
		"the hard-kill attempt must not block the caller past timeout+abandonGrace")

	releaseFunc()
	require.Eventually(t, func() bool { return AbandonedWorkers() == 0 },
		5*time.Second, 10*time.Millisecond,
		"abandoned-worker count must drain to zero once the worker finishes")
}
