// Watcher wake-up and fence-channel tests (P1 fix): SetRunner after Start,
// fence without the watcher, refusal of a second different Runner.

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWatcherWakesOnSetRunnerAfterStart: Start while dormant, establish THIS
// runtime's dormant boundary via its instance-local park seam, SetRunner,
// then a fault — the watcher must wake on runnerSet and exit 70 exactly once
// after the injected containment wait.
func TestWatcherWakesOnSetRunnerAfterStart(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, nil) // dormant: no runner yet
	// Instance-local dormant boundary: the parked channel belongs to THIS
	// BatchRuntime, unlike a process-wide goroutine-stack check that a
	// parallel test's own watcher could satisfy.
	parked := make(chan struct{}, 1)
	rt.testParked = parked
	rt.Start()
	defer rt.Stop()

	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("this runtime's watcher never parked in the dormant runnerSet select")
	}

	rt.SetRunner(runner)
	require.Equal(t, runner, rt.Runner())

	// Instance-local arrival: watchFaults parks in its live select by
	// READING the attached Runner's Faulted channel, so a read observed on
	// THIS fake proves THIS watcher observed the runner. Mutation oracle
	// (jb-strengthen#1): if SetRunner's runnerSet signal
	// (app_batch_runtime.go SetRunner) were removed, the watcher — already
	// parked in the dormant select above — is never woken, it never
	// re-reads rt.runner, Faulted is never called, and THIS assertion times
	// out instead of the test passing on some other instance's watcher.
	require.Eventually(t, func() bool { return runner.faultedReads.Load() > 0 },
		5*time.Second, time.Millisecond,
		"this runtime's watcher never read this runner's Faulted channel: the SetRunner wake is missing")

	runner.FireFault(errors.New("late runner fault"))

	require.Eventually(t, func() bool { return len(exits.called()) == 1 },
		5*time.Second, time.Millisecond, "watcher must wake on SetRunner and exit once")
	require.Equal(t, []int{BatchFaultExitCode}, exits.called())
	require.True(t, rt.Fenced())
	assertNoReleases(t, rec, "fault path must not release")
}

// TestFencedWithoutWatcherStart: the fault latched but Start was never
// called — DrainAndRelease must still retain everything (Forced, no
// releases, no exit), deterministically via the closed Faulted channel.
func TestFencedWithoutWatcherStart(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)

	runner.FireFault(errors.New("fault before start"))
	require.True(t, rt.Fenced(), "Fenced must observe the closed fault channel without Start")

	res := rt.DrainAndRelease(context.Background(), time.Second)
	require.True(t, res.Forced)
	require.Empty(t, rec.snapshot(), "fenced drain without watcher must release nothing")
	require.Empty(t, exits.called(), "exit belongs to the watcher, which never ran")
	require.EqualValues(t, 0, runner.closeCalls.Load())
}

// TestStoppedWatcherStillFences: Start then Stop, then a fault with a
// containment wait that never fires — Fenced is true via the channel, the
// drain retains everything, and no exit can happen.
func TestStoppedWatcherStillFences(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)
	rt.Start()
	rt.Stop()

	never := make(chan time.Time)
	rt.cfg.WaitFunc = func(time.Duration) <-chan time.Time { return never }

	runner.FireFault(errors.New("fault after stop"))
	require.True(t, rt.Fenced(), "Fenced does not depend on the watcher having run")

	res := rt.DrainAndRelease(context.Background(), time.Second)
	require.True(t, res.Forced)
	require.Empty(t, rec.snapshot(), "retained after stop: no releases")
	require.Empty(t, exits.called(), "a stopped watcher must never exit")
}

// TestSecondSetRunnerRefused: first SetRunner wins; a different runner is
// refused (Runner keeps the first) and faulting the FIRST runner still
// exits 70 exactly once.
func TestSecondSetRunnerRefused(t *testing.T) {
	t.Parallel()
	first := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, first)
	rt.Start()
	defer rt.Stop()

	second := newFakeRunner()
	rt.SetRunner(second)
	require.Equal(t, first, rt.Runner(), "first SetRunner must win")

	rt.SetRunner(first) // same pointer: no-op
	require.Equal(t, first, rt.Runner())

	first.FireFault(errors.New("first runner fault"))
	require.Eventually(t, func() bool { return len(exits.called()) == 1 },
		5*time.Second, time.Millisecond)
	require.Equal(t, []int{BatchFaultExitCode}, exits.called())
	assertNoReleases(t, rec, "refusal must not affect retention")
}
