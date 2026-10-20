// R11F (M2) flagged batch-runtime installation: App.New builds the batch
// runtime via the M1 factory only when RUSH_BATCH_RUNTIME parses true, and
// the shutdown path replaces the "release host identity and writer on
// agent-idle" sites with the R11E drain gate (BatchRuntime.DrainAndRelease).
//
// Flag OFF: none of this runs — no runtime object, no watcher goroutine, no
// store write, and app_lifecycle.go keeps its original blocks verbatim.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/google/uuid"
)

// appBatchDrainTimeout bounds the Runner drain inside an ordinary
// (graceful) App shutdown. A Close that outlives it makes the gate retain
// the still-draining host identity and writer and report Forced — the same
// verdict the agent CancelAll grace produces, and the same consequence:
// the caller must exit its process. 5s matches the per-cleanup shutdownCtx
// budget of releaseResources.
const appBatchDrainTimeout = 5 * time.Second

// batchRuntimeConfigHook is a test-only seam applied to the runtime config
// right after the factory build and BEFORE the fault watcher starts: tests
// inject WaitFunc/ExitFunc to exercise fault/timeout paths in-process. Nil
// in production; production code must never set it.
var batchRuntimeConfigHook func(*BatchRuntimeConfig)

// batchInstallDepsHook is a test-only, per-install dependency decorator.
// Nil in production; tests restore it immediately after New returns.
var batchInstallDepsHook func(*agent.BatchInstallDeps)

// arms its fault watcher. It fails closed: any missing resource surfaces as
// a New error — the process never silently continues with a half-installed
// runtime. Caller must have checked the flag and non-nil asyncJobStore.
func (app *App) installBatchRuntime(ctx context.Context) error {
	if app.batchRuntime != nil {
		// First wins: one install, one authority per App. A second flag-on
		// call (only possible from tests/re-entry) never swaps the Runner.
		return nil
	}
	// R11F (M3): the commit-hook identity must exist BEFORE the first Claim,
	// so the flagged install registers the host up front (HostID() is empty
	// until the lazy ensureHost registration).
	// R12-F: with the flag ON the identity is R12-D's job-independent
	// HostStore -- async_jobs is never read (the table may not even exist).
	var host string
	var err error
	if batchCutoverOn() {
		host, err = app.batchHostEnsure(ctx)
	} else {
		host, err = app.asyncJobStore.EnsureHost(ctx)
	}
	if err != nil {
		return err
	}
	deps := agent.BatchInstallDeps{
		// R11F round-2 (P1-1) decision — delivery ownership: the install
		// carries NO fixed owner. Owner=host was an FK violation on the
		// first Submit (batch_nodes.owner_session_id REFERENCES
		// sessions(id); async_hosts rows are not sessions). The commit hook
		// resolves every Submit's owner from the INVOKING session carried
		// in the spec payload (BatchCommitIdentity.OwnerFor); deliveries
		// and elections fence by the invocation's session
		// (batchDeliveryImpl.owner); Host stays ONLY the fence identity.
		Host: host, RunnerID: uuid.NewString(),
		DiskBindings: agent.NewDiskProviderRegistry(),
	}
	if batchInstallDepsHook != nil {
		batchInstallDepsHook(&deps)
	}
	factory, err := newBatchRuntimeFactory(app, deps)
	if err != nil {
		return err
	}
	if batchCutoverOn() {
		app.batchTimeoutWake = newBatchTimeoutWakeDispatcher()
		factory.wake = app.batchTimeoutWake.Wake
	}
	if factory.deps.Profile == nil {
		profile, perr := agent.NewBatchProfileDefault()
		if perr != nil {
			if app.batchTimeoutWake != nil {
				app.batchTimeoutWake.Stop()
				app.batchTimeoutWake = nil
			}
			return perr
		}
		factory.deps.Profile = profile
	}
	if factory.deps.Registries == nil {
		factory.deps.Registries = agent.NewBatchLeafRegistryStore()
	}
	rt, install, err := factory.build(ctx)
	if err != nil {
		if app.batchTimeoutWake != nil {
			app.batchTimeoutWake.Stop()
			app.batchTimeoutWake = nil
		}
		return err
	}
	// R11F (M3): the exported one-authority ingress seam over the installed
	// runtime; InitCoderAgent binds it to the coordinator.
	app.batchWorkIngress, err = agent.NewBatchWorkIngressWithDiskBindings(install.Runtime, factory.deps.Registries, install.DiskBindings)
	if err != nil {
		if app.batchTimeoutWake != nil {
			app.batchTimeoutWake.Stop()
			app.batchTimeoutWake = nil
		}
		return err
	}
	// R12-H2 P1-2/P1-4: bind the ingress into the runtime so batchLeafEnv
	// carries it — without this, provider-bound delegation fails closed.
	agent.BindRuntimeIngress(install.Runtime, app.batchWorkIngress)
	app.batchModelControl = agent.NewBatchModelControl(install.RunnerHandle, install.Delivery, factory.deps.Store, factory.live, app.Sessions, factory.deps.Host, factory.deps.RunnerID, factory.deps.Profile, factory.deps.Resolver, install.RootLeases)
	app.batchLiveReader = factory.live
	// Wire the R11E release seams onto the gate BEFORE the watcher starts:
	// RetainHostIdentity mirrors the flag-off forced path's CloseKeepLock,
	// ReleaseHostIdentity the graceful Close, and ReleaseWriter the
	// generation-safe per-reference DB release of releaseResources.
	if batchCutoverOn() {
		// R12-F: host release goes through the HostStore -- CloseKeepLock
		// pins the OS lock, Close deletes via DeleteAsyncHostIfNoNodes,
		// never async_jobs.
		rt.cfg.RetainHostIdentity = app.batchHostReleaseKeepLock
		rt.cfg.ReleaseHostIdentity = func(ctx context.Context) error {
			// R12-H2 §1.3: residual construction scopes close AFTER the
			// disposal drain and BEFORE host/writer release (the gate's
			// release order is host, then writer). Forced shutdown and the
			// fault fence never reach this seam, so the manager's remaining
			// owners are retained until process exit.
			app.drainResidualConstructions()
			return app.batchHostRelease(ctx)
		}
	} else {
		rt.cfg.RetainHostIdentity = app.asyncJobStore.CloseKeepLock
		rt.cfg.ReleaseHostIdentity = func(ctx context.Context) error {
			app.drainResidualConstructions()
			return app.asyncJobStore.Close(ctx)
		}
	}
	rt.cfg.ReleaseWriter = func(ctx context.Context) error {
		return app.releaseBatchWriter()
	}
	if batchRuntimeConfigHook != nil {
		batchRuntimeConfigHook(&rt.cfg)
	}
	if batchCutoverOn() {
		rt.cfg.TimeoutCheckin = batchTimeoutWakeRuntime{checkin: rt.cfg.TimeoutCheckin, dispatch: app.batchTimeoutWake}
	}
	app.batchRuntime = rt
	app.batchInstall = install
	// Start arms the fault watcher (idempotent; SetRunner already ran inside
	// build — Start and SetRunner are order-independent).
	rt.Start()
	// H4 W2: arm the App-owned dead-host maintenance (startup pass + 60s
	// periodic pass) alongside the fault watcher. Covers CLI, server and
	// SDK hosts; stopped and joined by releaseResources.
	app.startBatchMaintenance()
	slog.Info("app: batch runtime installed", "host", host)
	return nil
}

// drainResidualConstructions closes residual construction scopes on the
// graceful path only: construction records whose only remaining owners are
// their Construction pins are dropped (R12-H2 §1.3) — the forced path never
// calls this, retaining the manager and its owners until process exit.
func (app *App) drainResidualConstructions() {
	if app.batchInstall != nil {
		app.batchInstall.DrainDiskBindings()
	}
}

// releaseBatchWriter releases this App's pooled DB references exactly like
// the graceful branch of releaseResources' DB block (per Connect/ConnectRead
// call, or the legacy dbReleasesNeeded count for hand-assembled Apps).
func (app *App) releaseBatchWriter() error {
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(app.dbConns) > 0 {
		for _, conn := range app.dbConns {
			record(db.ReleaseConn(conn))
		}
		return firstErr
	}
	for i := 0; i < app.dbReleasesNeeded; i++ {
		record(db.Release(app.dataDir))
	}
	return firstErr
}

// releaseBatchRuntimeGate is the flag-on replacement for the "release host
// identity and writer on agent-idle" block of releaseResources: the Runner
// drains first, then host identity, then writer — all inside
// DrainAndRelease. It returns the (possibly promoted) stillBusy verdict.
//
//   - Graceful (stillBusy=false): drain bounded by appBatchDrainTimeout;
//     a Close timeout or fault fence retains the draining resources and
//     promotes the shutdown to Forced — the DB block below then skips the
//     release exactly like the flag-off forced path.
//   - Forced (stillBusy=true): same retention policy as flag-off — disarm
//     the watcher, keep the host lock (CloseKeepLock), never touch the DB.
func (app *App) releaseBatchRuntimeGate(ctx context.Context, stillBusy bool, record func(error)) bool {
	rt := app.batchRuntime
	if rt == nil {
		if app.batchTimeoutWake != nil {
			app.batchTimeoutWake.Stop()
			app.batchTimeoutWake = nil
		}
		// Defensive: a flag-off App must never take this path; keep the
		// flag-off retention semantics if it somehow does. Flag ON: the
		// HostStore twin.
		if batchCutoverOn() {
			app.batchHostReleaseKeepLock()
		} else if app.asyncJobStore != nil {
			app.asyncJobStore.CloseKeepLock()
		}
		return stillBusy
	}
	if stillBusy {
		rt.Stop()
		if batchCutoverOn() {
			app.batchHostReleaseKeepLock()
		} else if app.asyncJobStore != nil {
			app.asyncJobStore.CloseKeepLock()
		}
		return true
	}
	rt.Stop()
	res := rt.DrainAndRelease(ctx, appBatchDrainTimeout)
	if res.Err != nil {
		record(res.Err)
	}
	if res.Forced {
		slog.Warn("app: batch drain forced - host identity and writer retained; caller must exit the process")
		return true
	}
	return false
}

// batchRuntimeFlagEnabled re-exports the shared parser for the app package
// (single flag definition in internal/config).
func batchRuntimeFlagEnabled() bool {
	return config.BatchRuntimeEnabled()
}
