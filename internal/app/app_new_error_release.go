// Package-PE fix: App.New installs the batch runtime (and with it the
// host identity -- the hosts/<uuid>.lock OS lock) BEFORE InitCoderAgent
// runs. When New then fails (InitCoderAgent error, MCP owner acquisition
// error, or the install failing half-way after the host was already
// ensured), the error paths returned (nil, err) without ever stopping the
// installed runtime or releasing the host identity: there is no *App to
// Shutdown, so the lock file stays held and the runtime goroutines keep
// running for the life of the process. Invisible to CLI callers (the
// process exits), fatal for long-lived processes (tests, the web server):
// a flag-ON shadow run reported TempDir RemoveAll failing on
// hosts/<uuid>.lock ("being used by another process").
//
// This file gives New's error paths the SAME teardown primitives
// releaseResources (app_lifecycle.go) uses on Shutdown, with one
// ownership difference: the batch RUNTIME and the HOST IDENTITY are New's,
// but the DB writer reference (conn) is the CALLER's (task #778 ownership
// split) and the ConnectRead reference (readConn) is released by New's own
// error path right after calling into here. So this teardown drains the
// Runner and releases the host identity only -- it NEVER releases any DB
// reference. Like the graceful Shutdown path it is bounded (drain timeout
// = appBatchDrainTimeout) and idempotent; on a forced verdict (runner
// still draining at the bound, or a fault fence) it retains the host
// identity exactly like the forced Shutdown path (CloseKeepLock) and
// leaves the release to process exit.

package app

import (
	"context"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// peNewErrorReleaseTimeout bounds the Runner drain on New's error path,
// so a wedged Runner can never hang New. Same budget as the ordinary
// graceful Shutdown drain (appBatchDrainTimeout, the per-cleanup
// shutdownCtx budget of releaseResources).
const peNewErrorReleaseTimeout = appBatchDrainTimeout

// peReleaseBatchRuntimeOnNewError tears down everything installBatchRuntime
// gave this (about-to-be-discarded) App, EXCEPT DB references: stop the
// fault watcher and the dead-host maintenance goroutine, drain the Runner
// (bounded), and release the host identity. Call it from New's error paths
// after the flagged install block -- including the install-failure branch
// itself, where the host may already be ensured even though no runtime
// was built (installBatchRuntime ensures the host first, then builds the
// factory; a factory failure leaves the host registered). Safe to call
// when nothing was installed: every step is nil-safe or a documented
// no-op on a store that never ensured a host.
func (app *App) peReleaseBatchRuntimeOnNewError(ctx context.Context) {
	// H4 W2: stop and join the dead-host maintenance goroutine first, so
	// no maintenance pass runs against a host identity being released.
	app.stopBatchMaintenance()

	rt := app.batchRuntime
	if rt == nil {
		// Half-installed (or never installed): the host identity may
		// already be ensured. HostStore.Close and AsyncJobStore.Close are
		// documented no-ops without an ensured host, so release
		// unconditionally.
		app.peReleaseNewErrorHostIdentity(ctx)
		// rt.Stop() normally stops the timeout wake dispatcher via
		// cfg.TimeoutCheckin; with no runtime that never ran.
		if app.batchTimeoutWake != nil {
			app.batchTimeoutWake.Stop()
			app.batchTimeoutWake = nil
		}
		return
	}

	// Disarm the held-question inject bridge first, exactly like
	// releaseBatchRuntimeGate: no answer may be routed into a Runner that
	// is about to drain. (app.AgentCoordinator is nil on this path --
	// InitCoderAgent failed -- and SetCoordinatorBatchHeldAnswerSink is
	// nil-safe.)
	if app.batchHeldAnswerSink != nil {
		app.batchHeldAnswerSink.Close()
	}
	agent.SetCoordinatorBatchHeldAnswerSink(app.AgentCoordinator, nil)
	app.batchHeldAnswerSink = nil

	// Disarm the fault watcher (and, through cfg.TimeoutCheckin, the
	// timeout wake dispatcher). Idempotent.
	rt.Stop()

	app.peDrainRunnerAndReleaseHost(ctx, rt)

	// Mark the runtime consumed so a re-entry can never double-drain or
	// double-release through the same fields.
	app.batchRuntime = nil
	app.batchInstall = nil
}

// peDrainRunnerAndReleaseHost drains the Runner bounded by
// peNewErrorReleaseTimeout and then releases the host identity -- the
// graceful DrainAndRelease order (Runner, then host) minus the writer
// release, which stays the caller's (conn) and New's own (readConn). A
// drain timeout or a faulted Runner retains the host identity
// (CloseKeepLock), exactly like the forced Shutdown path: the lock is
// pinned until process exit and this helper must never hang New.
func (app *App) peDrainRunnerAndReleaseHost(ctx context.Context, rt *BatchRuntime) {
	runner := rt.Runner()
	if runner == nil {
		// Dormant runtime (runnerless install): nothing to drain; the
		// host identity is still ours to release.
		app.peReleaseNewErrorHostIdentity(ctx)
		return
	}
	closeCtx, cancel := context.WithTimeout(ctx, peNewErrorReleaseTimeout)
	defer cancel()
	if err := runner.Close(closeCtx); err != nil {
		slog.Warn("app: batch runner did not drain on New's error path - host identity retained until process exit", "error", err)
		app.peRetainNewErrorHostIdentity()
		return
	}
	app.peReleaseNewErrorHostIdentity(ctx)
}

// peReleaseNewErrorHostIdentity releases the host identity with the same
// primitive the graceful Shutdown path uses (HostStore.Close /
// AsyncJobStore.Close): OS lock release plus row cleanup. The DB writer is
// still open here -- New's error path releases its own ConnectRead only
// AFTER this returns, and the caller's conn reference is never New's to
// release. drainResidualConstructions mirrors the ReleaseHostIdentity seam
// wired in installBatchRuntime: residual construction scopes close before
// the host release.
func (app *App) peReleaseNewErrorHostIdentity(ctx context.Context) {
	app.drainResidualConstructions()
	if batchCutoverOn() {
		if err := app.batchHostRelease(ctx); err != nil {
			slog.Warn("app: failed to release batch host identity after New failed", "error", err)
		}
		return
	}
	if app.asyncJobStore != nil {
		if err := app.asyncJobStore.Close(ctx); err != nil {
			slog.Warn("app: failed to release legacy async host identity after New failed", "error", err)
		}
	}
}

// peRetainNewErrorHostIdentity is the forced counterpart (CloseKeepLock):
// forget the in-memory handle, pin the OS lock until process exit. Same
// graceful/forced split as releaseResources.
func (app *App) peRetainNewErrorHostIdentity() {
	if batchCutoverOn() {
		app.batchHostReleaseKeepLock()
		return
	}
	if app.asyncJobStore != nil {
		app.asyncJobStore.CloseKeepLock()
	}
}
