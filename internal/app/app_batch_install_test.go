// R11F (M2) flagged-install tests: flag-off golden (no runtime object, no
// DB write), flag-on install through App wiring, and the DrainAndRelease
// gate exercised through the app-level shutdown wiring (graceful order,
// Close-timeout Forced retention, fault-fence retention).

package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// newFullNewApp builds an App through the REAL New entry point — NO
// SkipAgentSetup — with the same offline isolation the other New callers in
// this package use (isolateAppNewTestEnv + a config-only openaicompat probe
// provider that is never called). R11F round-2 P1-7 items 2 and 3: a
// flag-off golden built through SkipAgentSetup or a hand-assembled &App{}
// proves nothing about the installed system.
func newFullNewApp(t *testing.T) *App {
	t.Helper()
	return newFullNewAppAt(t, t.TempDir())
}

// newFullNewAppAt is newFullNewApp over a caller-chosen data directory (the
// R12-F cross-process tests need the subprocess's data dir, not a private
// TempDir).
func newFullNewAppAt(t *testing.T, dataDir string) *App {
	t.Helper()
	isolateAppNewTestEnv(t)
	// The provider probe never needs to be called; its existence satisfies
	// config validation without a network endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID: "openaicompat", Type: openaicompat.Name, BaseURL: srv.URL, APIKey: "probe",
		Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = fmt.Errorf("New: %w", err)
		_ = db.ReleaseConn(conn)
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	return application
}

// TestBatchRuntimeFlagOffGolden (R11F round-2 P1-7 item 2): with
// RUSH_BATCH_RUNTIME unset, "false" or unparseable, an App built through the
// REAL New entry point (NO SkipAgentSetup) has no batch runtime object, no
// ingress seam, and the database carries NO batch rows at all — counted over
// every owner, never through an owner=” query. This is the flag-off
// equivalence pin for the M2 wiring.
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

			app := newFullNewApp(t)
			require.Nil(t, app.batchRuntime, "flag off: no batch runtime object is built")
			require.Nil(t, app.batchWorkIngress, "flag off: no ingress seam is built")

			// No batch surface wrote anything: zero rows over ALL owners.
			var count int
			require.NoError(t, app.DB().QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM batch_nodes`).Scan(&count))
			require.Zero(t, count, "flag off: no batch_nodes row is written for any owner")
		})
	}
}

// TestBatchRuntimeFlagOnInstall: with the flag on, the app-level install
// wires the full runtime — Runner attached, release seams bound to the
// App's R12-F HostStore/DB release path, fault watcher armed — and the
// commit-hook identity uses the HostStore identity (one install, one
// authority), NOT the AsyncJobStore's: async_jobs stays untouched.
func TestBatchRuntimeFlagOnInstall(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	a := batchFactoryApp(t)
	// The host identity registers during the flagged install (R11F M3:
	// EnsureHost precedes the commit-hook identity), so read it after.
	require.NoError(t, a.installBatchRuntime(context.Background()))
	hostID := a.BatchHostStore().HostID()
	require.NotEmpty(t, hostID)
	// R12-F independence pin: the AsyncJobStore identity was never ensured,
	// so the async_jobs-backed registration never ran under the flag.
	require.Empty(t, a.asyncJobStore.HostID())

	release := func() {
		if a.batchRuntime != nil {
			// Stop disarms the watcher; Close drains the factory-built real
			// Runner (Stop alone leaves the engine goroutines running).
			if r := a.batchRuntime.Runner(); r != nil {
				_ = r.Close(context.Background())
			}
			a.batchRuntime.Stop()
		}
		if hs := a.BatchHostStore(); hs != nil {
			// Close (not CloseKeepLock): the test's TempDir cleanup must be
			// able to unlink the host lock on Windows.
			_ = hs.Close(context.Background())
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
	require.NotEmpty(t, a.BatchHostStore().HostID())
}

// TestBatchRuntimeFlagOnInstallFailsClosed: a hand-assembled App without a
// message service must abort New-style installation with an error — never
// continue with a half-installed runtime and never leave a runtime on the
// App.
func TestBatchRuntimeFlagOnInstallFailsClosed(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	a := batchFactoryApp(t)
	a.Messages = nil
	t.Cleanup(func() {
		_ = a.asyncJobStore.Close(context.Background())
		if hs := a.BatchHostStore(); hs != nil {
			_ = hs.Close(context.Background())
		}
	})
	require.Error(t, a.installBatchRuntime(context.Background()))
	require.Nil(t, a.batchRuntime, "fail closed: no partially-installed runtime")
}

// TestAppReleaseResourcesDrainsBeforeRelease (R11F round-2 P1-7 item 4):
// the flag-on SHUTDOWN goes through app.releaseResources — not the gate
// helper directly — and runs DrainAndRelease in order: Runner drain first,
// then host identity, then writer, and the DB block never re-releases the
// writer the gate already released.
func TestAppReleaseResourcesDrainsBeforeRelease(t *testing.T) {
	t.Parallel()
	app := batchFactoryApp(t)
	runner := newFakeRunner()
	rec, _, _ := wireTestBatchRuntimeFull(t, app, runner)
	// Make the DB block observable: under the guard mutation the block
	// re-releases the pooled reference and closes the DB out from under the
	// App even though the gate already released the writer.
	app.dbConns = []*sql.DB{app.DB()}

	result := make(chan ShutdownResult, 1)
	go func() { result <- app.releaseResources(false) }()
	<-runner.closeEntered
	require.Empty(t, rec.snapshot(), "nothing releases while the Runner drains")

	runner.finishClose(nil)
	res := <-result
	require.False(t, res.Forced, "graceful drain keeps the shutdown graceful")
	require.Equal(t, []string{"release-host", "release-writer"}, rec.snapshot(),
		"drain precedes release; host identity precedes writer")
	require.Empty(t, res.CleanupErrors)
	// Exactly-once writer release: the gate released it (the fake seam), so
	// the DB block must have skipped the release on the flag-on graceful path.
	require.NoError(t, app.DB().PingContext(context.Background()),
		"flag-on graceful shutdown releases the writer exactly once (no double release)")
}

// TestAppReleaseResourcesCloseTimeoutForced: a Close timeout observed
// through releaseResources retains the still-draining resources (only
// retention runs, the writer is never released) and promotes the shutdown
// verdict to Forced.
func TestAppReleaseResourcesCloseTimeoutForced(t *testing.T) {
	t.Parallel()
	app := batchFactoryApp(t)
	runner := newFakeRunner()
	rec, _, _ := wireTestBatchRuntimeFull(t, app, runner)
	// The drain bound is production-owned (appBatchDrainTimeout = 5s), so the
	// timeout is simulated the way a REAL Runner.Close reports it (the R11E
	// contract: *jobbatch.CloseTimeoutError, errors.Is ErrCloseTimeout) —
	// Close returns the error early instead of silently returning nil on
	// ctx.Done (which is what lost the Forced verdict: the gate only promotes
	// on the Close error, and a nil one reads as a graceful drain).
	runner.finishClose(&jobbatch.CloseTimeoutError{Nodes: []jobbatch.ID{"root-1"}})

	res := app.releaseResources(false)
	require.True(t, res.Forced, "Close timeout must promote the shutdown to Forced")
	require.Equal(t, []string{"retain-host"}, rec.snapshot(),
		"host identity and writer retained on the forced path (no releases)")
	found := false
	for _, err := range res.CleanupErrors {
		if errors.Is(err, jobbatch.ErrCloseTimeout) {
			found = true
		}
	}
	require.True(t, found, "the Close timeout error is surfaced in the cleanup errors")
}

// TestAppReleaseResourcesFaultFenceRetains: a fault fence observed through
// releaseResources retains everything until exit 70 — no host release, no
// writer release, Forced verdict. handleFault itself records the fence's
// retain (app_batch_fault.go: fence first, log, RetainHostIdentity, exit 70
// exactly once) — the gate then refuses every release (DrainAndRelease
// returns Forced from Fenced() without releasing) — so the recorder shows
// exactly one retain-host and no release at all.
func TestAppReleaseResourcesFaultFenceRetains(t *testing.T) {
	t.Parallel()
	app := batchFactoryApp(t)
	runner := newFakeRunner()
	rec, exits, rt := wireTestBatchRuntimeFull(t, app, runner)
	rt.handleFault(nil)
	require.NotEmpty(t, exits.called(), "fault path exits the process (exit 70)")

	res := app.releaseResources(false)
	require.True(t, res.Forced, "fenced shutdown is Forced")
	require.Equal(t, []string{"retain-host"}, rec.snapshot(),
		"fault fence retains the host identity once and never releases host or writer")
}

// TestAppReleaseResourcesForcedAgentsBusy: when agent work already made the
// shutdown forced, releaseResources keeps the forced retention policy through
// the gate — watcher disarmed, host lock kept (CloseKeepLock), writer
// untouched.
func TestAppReleaseResourcesForcedAgentsBusy(t *testing.T) {
	t.Parallel()
	app := batchFactoryApp(t)
	runner := newFakeRunner()
	rec, _, _ := wireTestBatchRuntimeFull(t, app, runner)

	res := app.releaseResources(true)
	require.True(t, res.Forced)
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
