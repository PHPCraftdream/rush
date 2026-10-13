// Drain gate tests: drain-before-release ordering, Close timeout retention,
// Forced convention, dormant runtime behavior.

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// TestOrdinaryShutdownDrainsBeforeRelease: the Runner Close (drain) must
// complete before the host identity and the writer are released, in that
// order — the call-order recorder proves it without sleeps.
func TestOrdinaryShutdownDrainsBeforeRelease(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)

	done := make(chan BatchDrainResult, 1)
	go func() { done <- rt.DrainAndRelease(context.Background(), 5*time.Second) }()

	// Close is entered but not finished: no release may happen yet.
	<-runner.closeEntered
	require.Empty(t, rec.snapshot(), "nothing releases while the Runner drains")

	runner.finishClose(nil)
	res := <-done
	require.False(t, res.Forced, "ordinary shutdown is graceful")
	require.NoError(t, res.Err)
	require.Equal(t, []string{"release-host", "release-writer"}, rec.snapshot(),
		"drain precedes release; host identity precedes writer")
	require.Empty(t, exits.called(), "graceful shutdown never exits the process")
}

// TestCloseTimeoutRetainsDrainingResources: a Close timeout retains the
// still-draining writer and host identity (never released beneath a draining
// Runner) and reports Forced, like CancelAll's stillBusy convention.
func TestCloseTimeoutRetainsDrainingResources(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)

	done := make(chan BatchDrainResult, 1)
	go func() { done <- rt.DrainAndRelease(context.Background(), 5*time.Second) }()
	<-runner.closeEntered

	runner.finishClose(&jobbatch.CloseTimeoutError{Nodes: []jobbatch.ID{"r1"}})
	res := <-done

	require.True(t, res.Forced, "Close timeout reports Forced")
	require.ErrorIs(t, res.Err, jobbatch.ErrCloseTimeout)
	require.Equal(t, []string{"retain-host"}, rec.snapshot(),
		"only retention runs on the timeout path: host identity and writer retained, never released")
	require.Empty(t, exits.called())
}

// TestCloseTimeoutWithDeadlineExpiry: when the drain context expires before
// Close returns its own CloseTimeoutError, the gate still retains.
func TestCloseTimeoutWithDeadlineExpiry(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, exits, rec := newTestRuntime(t, runner)
	// Close returns nil on ctx.Done in the fake only via closeErr; script the
	// production shape: Close honours ctx and returns the timeout error.
	runner.closeHook = func(ctx context.Context) error {
		<-ctx.Done()
		return &jobbatch.CloseTimeoutError{Nodes: []jobbatch.ID{"r1"}}
	}

	done := make(chan BatchDrainResult, 1)
	go func() { done <- rt.DrainAndRelease(context.Background(), 50*time.Millisecond) }()
	res := <-done

	require.True(t, res.Forced)
	require.ErrorIs(t, res.Err, jobbatch.ErrCloseTimeout)
	require.Equal(t, []string{"retain-host"}, rec.snapshot(), "expired drain retains resources")
	require.Empty(t, exits.called())
}

// TestDormantRuntimeDrainReleasesDirectly: with no Runner attached (flag-off
// shape) the gate keeps today's release order so flag-off behavior is
// unchanged.
func TestDormantRuntimeDrainReleasesDirectly(t *testing.T) {
	t.Parallel()
	rt, exits, rec := newTestRuntime(t, nil)
	res := rt.DrainAndRelease(context.Background(), time.Second)
	require.False(t, res.Forced)
	require.NoError(t, res.Err)
	require.Equal(t, []string{"release-host", "release-writer"}, rec.snapshot())
	require.Empty(t, exits.called())
}

// TestDrainReleaseErrorSurfaces: a host-identity release error is surfaced
// without skipping the writer release (same as app_lifecycle's cleanup error
// collection).
func TestDrainReleaseErrorSurfaces(t *testing.T) {
	t.Parallel()
	rt := NewBatchRuntime(BatchRuntimeConfig{
		ReleaseHostIdentity: func(context.Context) error { return errors.New("host busy") },
		ReleaseWriter:       func(context.Context) error { return nil },
	})
	res := rt.DrainAndRelease(context.Background(), time.Second)
	require.False(t, res.Forced)
	require.ErrorContains(t, res.Err, "host busy")
}

// TestDisposeFailureRetains: dispose failures after a drained Close also
// force and retain, matching Close's DisposeError contract.
func TestDisposeFailureRetains(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	rt, _, rec := newTestRuntime(t, runner)

	done := make(chan BatchDrainResult, 1)
	go func() { done <- rt.DrainAndRelease(context.Background(), time.Second) }()
	<-runner.closeEntered
	runner.finishClose(&jobbatch.DisposeError{Failures: []jobbatch.DisposeFailure{{Node: "r1", Code: "dispose_failed"}}})
	res := <-done

	require.True(t, res.Forced, "dispose failures force the shutdown")
	require.ErrorIs(t, res.Err, jobbatch.ErrDispose)
	require.Equal(t, []string{"retain-host"}, rec.snapshot(),
		"dispose failures retain: handle forgotten, writer kept")
}
