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

// installBatchRuntime builds the batch runtime from App-owned resources and
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
	host, err := app.asyncJobStore.EnsureHost(ctx)
	if err != nil {
		return err
	}
	factory, err := newBatchRuntimeFactory(app, agent.BatchInstallDeps{
		// R11F (M3) decision — delivery ownership: the commit-hook identity
		// (Host/RunnerID/Owner) stays HOST-scoped, one Runner and one owner
		// per process (session.BatchCommitIdentity contract). The ingress
		// rebinds per-invocation ownership at the boundary:
		// BatchInvocation.SessionID is the CALLING session (attach fence +
		// singleflight key) while durable elections and deliveries remain
		// fenced by the install Owner (the delivery seam's fixed deps.Owner).
		Host: host, RunnerID: uuid.NewString(), Owner: host,
		// R11F (M3) coordinator seams: the agent executor family resolves
		// its pinned leaf through the process binding; MCP leaves admit as
		// non-reentrant (reentrant ones fail closed).
		AgentDeps: agent.CoordinatorBatchAgentDeps(),
		Resolver:  agent.BatchNonReentrantMCPResolver(),
	})
	if err != nil {
		return err
	}
	if factory.deps.Profile == nil {
		profile, perr := agent.NewBatchProfileDefault()
		if perr != nil {
			return perr
		}
		factory.deps.Profile = profile
	}
	if factory.deps.Registries == nil {
		factory.deps.Registries = agent.NewBatchLeafRegistryStore()
	}
	rt, install, err := factory.build(ctx)
	if err != nil {
		return err
	}
	// R11F (M3): the exported one-authority ingress seam over the installed
	// runtime; InitCoderAgent binds it to the coordinator.
	app.batchWorkIngress, err = agent.NewBatchWorkIngress(install.Runtime, factory.deps.Registries)
	if err != nil {
		return err
	}
	// Wire the R11E release seams onto the gate BEFORE the watcher starts:
	// RetainHostIdentity mirrors the flag-off forced path's CloseKeepLock,
	// ReleaseHostIdentity the graceful Close, and ReleaseWriter the
	// generation-safe per-reference DB release of releaseResources.
	rt.cfg.RetainHostIdentity = app.asyncJobStore.CloseKeepLock
	rt.cfg.ReleaseHostIdentity = app.asyncJobStore.Close
	rt.cfg.ReleaseWriter = func(context.Context) error {
		return app.releaseBatchWriter()
	}
	if batchRuntimeConfigHook != nil {
		batchRuntimeConfigHook(&rt.cfg)
	}
	app.batchRuntime = rt
	// Start arms the fault watcher (idempotent; SetRunner already ran inside
	// build — Start and SetRunner are order-independent).
	rt.Start()
	slog.Info("app: batch runtime installed", "host", host)
	return nil
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
		// Defensive: a flag-off App must never take this path; keep the
		// flag-off retention semantics if it somehow does.
		if app.asyncJobStore != nil {
			app.asyncJobStore.CloseKeepLock()
		}
		return stillBusy
	}
	if stillBusy {
		rt.Stop()
		if app.asyncJobStore != nil {
			app.asyncJobStore.CloseKeepLock()
		}
		return true
	}
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
