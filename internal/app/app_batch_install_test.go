// R11F (M2) flagged-install tests: flag-off golden (no runtime object, no
// DB write), flag-on install through App wiring, and the DrainAndRelease
// gate exercised through the app-level shutdown wiring (graceful order,
// Close-timeout Forced retention, fault-fence retention).

package app

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestBatchRuntimeFlagOffGolden: with RUSH_BATCH_RUNTIME unset, "false" or
// unparseable, the install guard never fires: an App built through New has
// no batch runtime object (hence no watcher goroutine), and the database
// carries no batch rows. This is the flag-off equivalence pin for the M2
// wiring — the flag-off branch of New is exactly the pre-R11F code.
func TestBatchRuntimeFlagOffGolden(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		set         bool
	}{
		{name: "unset", set: false},
		{name: "explicit false", set: true, value: "false"},
		{name: "zero", set: true, value: "0"},
		{name: "garbage means off", set: true, value: "sometimes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("RUSH_BATCH_RUNTIME", tc.value)
			}
			require.False(t, config.BatchRuntimeEnabled())

			isolateAppNewTestEnv(t)
			dataDir := t.TempDir()
			store, err := config.Init(dataDir, dataDir, false)
			require.NoError(t, err)
			conn, err := db.Connect(context.Background(), dataDir)
			require.NoError(t, err)

			app, err := New(context.Background(), conn, store, SkipAgentSetup())
			require.NoError(t, err)
			require.Nil(t, app.batchRuntime, "flag off: no batch runtime object is built")

			// No DB write from any batch surface: no live batch root exists.
			q := db.New(app.DB())
			roots, err := q.ListLiveBatchRootsForOwner(context.Background(), "")
			require.NoError(t, err)
			require.Empty(t, roots, "flag off: no batch root row is written")
			t.Cleanup(func() {
				// Release every pooled reference the App took at New, so the
				// TempDir cleanup can unlink rush.db on Windows.
				for i := 0; i < app.dbReleasesNeeded+2; i++ {
					_ = db.Release(dataDir)
				}
				_ = db.ReleaseConn(conn)
			})
		})
	}
}

// TestBatchRuntimeFlagOnInstall: with the flag on, the app-level install
// wires the full runtime — Runner attached, release seams bound to the
// App's own asyncJobStore/DB release path, fault watcher armed — and the
// commit-hook identity uses the App's host identity (one install, one
// authority).
func TestBatchRuntimeFlagOnInstall(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	a := batchFactoryApp(t)
	// The host identity registers during the flagged install (R11F M3:
	// EnsureHost precedes the commit-hook identity), so read it after.
	require.NoError(t, a.installBatchRuntime(context.Background()))
	hostID := a.asyncJobStore.HostID()
	require.NotEmpty(t, hostID)

	release := func() {
		if a.batchRuntime != nil {
			a.batchRuntime.Stop()
		}
		if a.asyncJobStore != nil {
			// Close (not CloseKeepLock): the test's TempDir cleanup must be
			// able to unlink the host lock on Windows.
			_ = a.asyncJobStore.Close(context.Background())
		}
	}
	t.Cleanup(release)

	rt := a.batchRuntime
	require.NotNil(t, rt)
	require.NotNil(t, rt.Runner(), "flag on: Runner attached at install")
	require.NotNil(t, rt.cfg.ReleaseHostIdentity, "gate release seams wired")
	require.NotNil(t, rt.cfg.ReleaseWriter)
	require.NotNil(t, rt.cfg.RetainHostIdentity)
	require.False(t, rt.Fenced())

	// A second install must refuse (first-wins authority policy): the same
	// App keeps its original runtime and Runner.
	require.NoError(t, a.installBatchRuntime(context.Background()))
	require.Same(t, rt, a.batchRuntime)
	require.NotEmpty(t, a.asyncJobStore.HostID())
}

// TestBatchRuntimeFlagOnInstallFailsClosed: a hand-assembled App without a
// message service must abort New-style installation with an error — never
// continue with a half-installed runtime and never leave a runtime on the
// App.
func TestBatchRuntimeFlagOnInstallFailsClosed(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	a := batchFactoryApp(t)
	a.Messages = nil
	t.Cleanup(func() { _ = a.asyncJobStore.Close(context.Background()) })
	require.Error(t, a.installBatchRuntime(context.Background()))
	require.Nil(t, a.batchRuntime, "fail closed: no partially-installed runtime")
}

// TestAppShutdownGateDrainsBeforeRelease: the app-level shutdown gate runs
// DrainAndRelease — Runner drain first, then host identity, then writer.
func TestAppShutdownGateDrainsBeforeRelease(t *testing.T) {
	t.Parallel()
	app := batchFactoryApp(t)
	runner := newFakeRunner()
	rec, _, _ := wireTestBatchRuntimeFull(t, app, runner)

	result := make(chan bool, 1)
	go func() { result <- app.releaseBatchRuntimeGate(context.Background(), false, func(error) {}) }()
	<-runner.closeEntered
	require.Empty(t, rec.snapshot(), "nothing releases while the Runner drains")

	runner.finishClose(nil)
	require.False(t, <-result, "graceful drain keeps stillBusy false")
	require.Equal(t, []string{"release-host", "release-writer"}, rec.snapshot(),
		"drain precedes release; host identity precedes writer")
}

// TestAppShutdownGateCloseTimeoutForced: a Close timeout through the app
// wiring retains the still-draining resources (only retention runs, the
// writer is never released) and promotes the shutdown to Forced.
func TestAppShutdownGateCloseTimeoutForced(t *testing.T) {
	t.Parallel()
	app := batchFactoryApp(t)
	runner := newFakeRunner()
	rec, _, _ := wireTestBatchRuntimeFull(t, app, runner)

	busy := app.releaseBatchRuntimeGate(context.Background(), false, func(error) {})
	require.True(t, busy, "Close timeout promotes the shutdown to Forced")
	require.Equal(t, []string{"retain-host"}, rec.snapshot(),
		"host identity and writer retained on the forced path")
}

// TestAppShutdownGateFaultFenceRetains: a fault fence observed through the
// app wiring retains everything until exit 70 — no host release, no writer
// release, Forced verdict.
func TestAppShutdownGateFaultFenceRetains(t *testing.T) {
	t.Parallel()
	app := batchFactoryApp(t)
	runner := newFakeRunner()
	rec, exits, rt := wireTestBatchRuntimeFull(t, app, runner)
	rt.handleFault(nil)
	require.NotEmpty(t, exits.called(), "fault path exits the process (exit 70)")

	busy := app.releaseBatchRuntimeGate(context.Background(), false, func(error) {})
	require.True(t, busy, "fenced shutdown is Forced")
	require.Empty(t, rec.snapshot(),
		"fault fence retains host identity and writer until exit 70")
}

// TestAppShutdownGateForcedAgentsBusy: when agent work already made the
// shutdown forced, the gate keeps the flag-off forced retention policy —
// watcher disarmed, host lock kept (CloseKeepLock), writer untouched.
func TestAppShutdownGateForcedAgentsBusy(t *testing.T) {
	t.Parallel()
	app := batchFactoryApp(t)
	runner := newFakeRunner()
	rec, _, _ := wireTestBatchRuntimeFull(t, app, runner)

	busy := app.releaseBatchRuntimeGate(context.Background(), true, func(error) {})
	require.True(t, busy)
	require.Empty(t, rec.snapshot(), "forced shutdown never releases through the gate")
}

// wireTestBatchRuntime swaps the factory-built runtime on the App for one
// carrying the SAME wiring release seams but a fake Runner and injected
// wait/exit — so the drain, timeout and fence paths are observable
// in-process. Returns the release recorder and a channel closed when the
func wireTestBatchRuntimeFull(t *testing.T, app *App, runner *fakeRunner) (*releaseRecorder, *exitRecorder, *BatchRuntime) {
	t.Helper()
	rec := &releaseRecorder{}
	exits := &exitRecorder{}
	rt := NewBatchRuntime(BatchRuntimeConfig{
		ContainmentWait: time.Second,
		WaitFunc:        immediateWaitFunc(),
		ExitFunc:        exits.exit,
		RetainHostIdentity: func() {
			rec.record("retain-host")
		},
		ReleaseHostIdentity: func(context.Context) error {
			rec.record("release-host")
			return nil
		},
		ReleaseWriter: func(context.Context) error {
			rec.record("release-writer")
			return nil
		},
	})
	rt.SetRunner(runner)
	app.batchRuntime = rt
	rt.Start()
	t.Cleanup(rt.Stop)
	return rec, exits, rt
}
