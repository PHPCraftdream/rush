// Fault handler tests (decision D4): exit 70, containment bound, fence
// semantics, fault/shutdown race in both orders, exactly-once exit.

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// TestBatchFaultExits70AfterContainment: the fault watcher observes the
// Runner fault, waits the (injected) containment bound, then calls the exit
// function with 70 exactly once, with the fence set before exit.
func TestBatchFaultExits70AfterContainment(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)
	rt.Start()
	defer rt.Stop()

	runner.FireFault(errors.New("commit hook stuck"))

	require.Eventually(t, func() bool {
		return len(exits.called()) == 1
	}, 5*time.Second, time.Millisecond, "exit must be called once")
	require.Equal(t, []int{BatchFaultExitCode}, exits.called(), "exit code must be 70")
	require.True(t, rt.Fenced(), "fence must be set on fault")
	require.Equal(t, []string{"retain-host"}, rec.snapshot(),
		"fault path retains the host identity handle and never releases")
}

// TestBatchFaultFenceBeforeContainmentWait: the fence is set synchronously
// with fault observation, before the containment wait — a shutdown starting
// after the fault can never release. Driven without sleeps by starting the
// drain only after the fault fired and observing that no release happened.
func TestBatchFaultFenceBeforeContainmentWait(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)

	// Slow containment: the exit is NOT called during this test's window,
	// but the fence is already active.
	blocked := make(chan time.Time)
	rt.cfg.WaitFunc = func(time.Duration) <-chan time.Time { return blocked }
	rt.Start()
	defer rt.Stop()

	runner.FireFault(errors.New("fault"))
	require.Eventually(t, func() bool { return rt.Fenced() },
		5*time.Second, time.Millisecond, "fence set before containment wait ends")

	res := rt.DrainAndRelease(context.Background(), time.Second)
	require.True(t, res.Forced, "drain after fault must report Forced")
	require.Empty(t, rec.snapshot(), "fenced drain must retain host identity and writer")
	require.Empty(t, exits.called(), "exit belongs to the fault watcher, once")
	blocked <- time.Now()
	require.Eventually(t, func() bool { return len(exits.called()) == 1 },
		5*time.Second, time.Millisecond)
	require.Equal(t, []int{BatchFaultExitCode}, exits.called())
}

// TestFaultRacingShutdownFaultFirst: fault starts shutdown — the gate retains
// lock/writer and the process exits 70 exactly once.
func TestFaultRacingShutdownFaultFirst(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)
	rt.Start()
	defer rt.Stop()

	runner.FireFault(errors.New("commit failed"))
	res := rt.DrainAndRelease(context.Background(), time.Second)
	require.True(t, res.Forced)
	assertNoReleases(t, rec, "no release after fault")
	require.Eventually(t, func() bool { return len(exits.called()) == 1 },
		5*time.Second, time.Millisecond, "exit 70 exactly once")
	require.Equal(t, []int{BatchFaultExitCode}, exits.called(), "exit 70 exactly once")
	require.EqualValues(t, 0, runner.closeCalls.Load(), "drain after fault must not Close the Runner")
}

// TestFaultRacingShutdownFaultDuringDrain: a fault during shutdown still
// exits 70 and stops the release sequence. Close blocks on closeRelease; the
// test fires the fault after Close has entered, then lets Close return.
func TestFaultRacingShutdownFaultDuringDrain(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)
	rt.Start()
	defer rt.Stop()

	done := make(chan BatchDrainResult, 1)
	go func() { done <- rt.DrainAndRelease(context.Background(), 10*time.Second) }()

	<-runner.closeEntered // Close is in flight, nothing released yet
	require.Empty(t, rec.snapshot())

	runner.FireFault(errors.New("fault during drain"))
	runner.finishClose(nil) // drain "succeeds" — but the fence wins

	res := <-done
	require.True(t, res.Forced, "fault during drain forces the result")
	assertNoReleases(t, rec, "fault during drain retains lock and writer")
	require.Eventually(t, func() bool { return len(exits.called()) == 1 },
		5*time.Second, time.Millisecond, "exit 70 exactly once")
	require.Equal(t, []int{BatchFaultExitCode}, exits.called())
}

// TestShutdownRacingFaultReleaseAbortsMidway: the fence is re-checked between
// release steps, so a fault firing between the two releases prevents the
// writer release.
func TestShutdownRacingFaultReleaseAbortsMidway(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)

	rt.cfg.ReleaseHostIdentity = func(context.Context) error {
		rec.record("release-host")
		runner.FireFault(errors.New("fault between releases"))
		return nil
	}
	rt.Start()
	defer rt.Stop()

	res := rt.DrainAndRelease(context.Background(), time.Second)
	require.True(t, res.Forced, "fault during release forces the result")
	require.Equal(t, []string{"release-host"}, rec.snapshot(),
		"writer release must be skipped once the fence is set")
	require.Eventually(t, func() bool { return len(exits.called()) == 1 },
		5*time.Second, time.Millisecond)
	require.Equal(t, []int{BatchFaultExitCode}, exits.called())
}

// TestBatchFaultNoSyntheticSettlement: the fault path never touches the
// Runner's nodes — no Close, no Stop, no retry; the exit function is the
// only side effect beyond the fence and retention.
func TestBatchFaultNoSyntheticSettlement(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)
	rt.Start()
	defer rt.Stop()

	runner.FireFault(errors.New(string(jobbatch.RecordClosed))) // any fault
	require.Eventually(t, func() bool { return len(exits.called()) == 1 },
		5*time.Second, time.Millisecond)
	require.Equal(t, []string{"retain-host"}, rec.snapshot(),
		"fault retains the handle; it never releases")
	require.EqualValues(t, 0, runner.closeCalls.Load(), "fault path must not Close or retry")
}

// assertNoReleases fails when any host/writer RELEASE was recorded; the
// retain event is allowed on fault and timeout paths.
func assertNoReleases(t *testing.T, rec *releaseRecorder, msg string) {
	t.Helper()
	for _, ev := range rec.snapshot() {
		require.NotEqual(t, "release-host", ev, msg)
		require.NotEqual(t, "release-writer", ev, msg)
	}
}
