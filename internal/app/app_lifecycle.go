// Lifecycle tail end: graceful/forced Shutdown — agent cancellation,
// run-queue pump stop, bounded cleanup, DB release.

package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/shell"
)

// ShutdownResult reports how an App shutdown completed.
type ShutdownResult struct {
	// Forced is true when at least one agent Run goroutine or the
	// run-queue pump was still busy after the grace period. On a forced
	// shutdown the database is deliberately NOT released, to avoid
	// closing it under live writers; the CLI reclaims the handle by
	// exiting, but a long-lived host process must decide its own
	// follow-up (e.g. db.ReleaseAll once it knows no writers remain).
	//
	// On the ShutdownAfterDrain path (the SDK's Close) the same holds,
	// with one refinement: a caller-side drain that stalls past its
	// grace period is cancelled first, and Forced reflects whether work
	// was STILL busy after that cancellation — a run that unwinds once
	// cancelled keeps the shutdown graceful.
	Forced bool
	// CleanupErrors carries the errors returned by the parallel cleanup
	// goroutines (registered cleanup funcs) and by the per-reference
	// db.Release calls on the graceful path. Each error is also logged
	// by Shutdown; they are surfaced here so library callers can react.
	// When the outer cleanup timeout fires, goroutines abandoned past
	// it may still log later — the returned slice is a snapshot taken
	// before returning and never aliases live state.
	CleanupErrors []error
}

// CancelAgents signals cancellation to every agent Run/Summarize
// goroutine the coordinator is tracking and joins them, bounded by the
// coordinator's grace period (agent.DefaultCancelAllGrace, unless the
// agent was built with an explicit CancelAllGrace). It returns whether
// agent work was still busy once that grace period expired — the verdict
// that decides a forced shutdown. On an idle App it returns immediately:
// CancelAll does not burn its grace period when there is nothing to join.
//
// This is the ONLY code path that cancels in-flight agent work (R3-2): a
// shutdown that merely waits for its callers' work to finish can block
// forever on a run stuck in a non-cancellable provider or tool call, so
// any shutdown that bounds its own duration must call this — before it
// starts releasing resources.
func (app *App) CancelAgents() bool {
	if app.AgentCoordinator != nil {
		return app.AgentCoordinator.CancelAll()
	}
	return false
}

// ShutdownAfterDrain performs the full shutdown — agent cancellation,
// run-queue pump stop, bounded parallel cleanup, DB release — and, when
// drained is non-nil, first gives the CALLER's already-admitted work a
// chance to finish, cancelling it only once it stalls:
//
//   - drained == nil (the CLI shape, what ShutdownWithResult does):
//     cancel first, then release — unchanged behavior.
//
//   - drained != nil (library hosts, the SDK's Close): admitted work
//     gets one agent.DefaultCancelAllGrace window to finish against the
//     fully live App — a call that finishes inside that window is never
//     cancelled and never touches a released resource. Work still
//     running when the window expires is cancelled immediately, while
//     every resource is still open, so a run stuck on a non-cancellable
//     provider or tool call unwinds instead of blocking the shutdown
//     forever. Cancellation's own join is bounded by the same grace
//     policy; work that ignores cancellation makes the shutdown forced
//     and the drain is abandoned rather than waited on. After agent work
//     has fully unwound, the residual wait for the caller's remaining
//     admitted calls is unbounded — cancellation cannot reach non-agent
//     calls (a store read), so a host needing a total bound must cancel
//     the contexts it handed to its own calls.
//
// Resources are released only once the drain completed or was
// force-abandoned, under the graceful/forced policy documented in the
// release phase's body. Returns a ShutdownResult describing how it went.
func (app *App) ShutdownAfterDrain(drained <-chan struct{}) ShutdownResult {
	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()

	return app.releaseResources(app.cancelAgentsBeforeRelease(drained))
}

// cancelAgentsBeforeRelease is ShutdownAfterDrain's first phase: cancel
// agent work, respecting the caller's drain up to the shared grace
// policy, and return the still-busy verdict the release phase needs. The
// ordering is the whole point (R3-2): cancellation — the only thing that
// can unblock a stuck run — must fire while the App and its resources
// are still fully live, never after the drain has been waited on to
// completion.
func (app *App) cancelAgentsBeforeRelease(drained <-chan struct{}) bool {
	if drained == nil {
		// No caller-side drain to respect: cancel immediately, the CLI
		// shape ShutdownWithResult has always had.
		return app.CancelAgents()
	}

	// First window: give admitted work one grace period to finish
	// against the fully live App.
	timer := time.NewTimer(agent.DefaultCancelAllGrace)
	defer timer.Stop()
	select {
	case <-drained:
		// Everything drained cooperatively. Cancellation still runs:
		// for the drained calls it is a formality, but it joins any
		// background agent work (title generation, cache keep-alive
		// replays) that no admitted call was blocked on — the same
		// single CancelAll the drain-less path performs.
		return app.CancelAgents()
	case <-timer.C:
		slog.Warn("Shutdown: drain did not finish within grace period - cancelling agent work while the app is still live")
	}

	// Second window: cancellation is now signalled — a stuck run can
	// unwind and even flush its state, because nothing has been released
	// yet — and CancelAgents' grace-bounded join decides whether
	// uncooperative work makes the shutdown forced.
	if app.CancelAgents() {
		// Work ignored cancellation and is still running after the
		// join's grace period: proceed WITHOUT waiting for the drain;
		// the forced release below leaves live writers' resources open.
		return true
	}

	// Agent work fully unwound. Whatever still holds the drain open is
	// either unwinding imminently or a non-agent call cancellation
	// cannot reach; wait for it without a second bound (see
	// ShutdownAfterDrain).
	<-drained
	return false
}

// releaseResources is ShutdownAfterDrain's second phase: run-queue pump
// stop, bounded parallel cleanup, and the conditional DB release. It
// runs only once cancelAgentsBeforeRelease has returned — no resource is
// touched while agent work may still be using it, except on the forced
// path, where live writers' DB and in-memory handles are deliberately
// LEFT OPEN (see the policy comment below).
func (app *App) releaseResources(stillBusy bool) ShutdownResult {
	var result ShutdownResult

	// Stop the run queue pump (task #340 P0-3). This must complete before DB
	// close to ensure no pump goroutines are writing when we close the connection.
	// Pump.Stop() returns true if shutdown was forced (workers still running after grace).
	if app.RunQueuePump != nil {
		pumpStillBusy := app.RunQueuePump.Stop()
		stillBusy = stillBusy || pumpStillBusy
		slog.Info("app: stopped run queue pump")
	}
	result.Forced = stillBusy

	// Shutdown policy: distinguish between graceful and forced shutdown.
	//
	// Graceful shutdown (stillBusy=false): All Run() goroutines finished cleanly.
	// We can safely close resources including the DB, waiting synchronously for cleanup.
	//
	// Forced shutdown (stillBusy=true): Some Run() goroutines did not finish within
	// the 5-second grace period. Closing the DB under live writers risks corruption,
	// so we log a warning and SKIP DB cleanup. The OS will reclaim file descriptors
	// when the process exits (which CLI callers do immediately after Shutdown()).
	// This is the policy choice: bounded shutdown time over perfect cleanup.
	//
	// For library/server callers who want to combine cancellation with a
	// custom release policy, CancelAgents returns the same still-busy
	// verdict Shutdown uses; ShutdownAfterDrain is the ready-made
	// drain-aware variant.
	if stillBusy {
		slog.Warn("Shutdown: some agents did not finish within grace period - proceeding with forced shutdown (DB will NOT be closed, in-progress work may be incomplete)")
	} else {
		slog.Debug("Shutdown: all agents finished gracefully - closing resources")
	}

	// Shared shutdown context for all timeout-bounded cleanup.
	// In forced-shutdown mode we still give non-DB cleanup a bounded window,
	// but skip DB cleanup entirely.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fork merge note: upstream 6938dedd added FlushAll for its debounced
	// message-update layer. We removed that layer (see message/message.go);
	// Update() writes synchronously, so there is nothing to drain here.

	// Collect cleanup errors from the parallel goroutines into the
	// result. A mutex-guarded slice is enough: the goroutines run
	// concurrently under wg, and Shutdown snapshots the slice under the
	// same mutex before returning.
	var errMu sync.Mutex
	var collected []error
	recordCleanupError := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		collected = append(collected, err)
		errMu.Unlock()
	}

	// Now run remaining cleanup tasks in parallel with an overall bounded timeout.
	var wg sync.WaitGroup

	// Kill all background shells.
	wg.Go(func() {
		shell.GetBackgroundShellManager().KillAll(shutdownCtx)
	})

	// Call all cleanup functions.
	for _, cleanup := range app.cleanupFuncs {
		if cleanup != nil {
			wg.Go(func() {
				if err := cleanup(shutdownCtx); err != nil {
					slog.Error("Failed to cleanup app properly on shutdown", "error", err)
					recordCleanupError(err)
				}
			})
		}
	}

	// Wait for all cleanup with an independent outer timeout to guarantee
	// bounded shutdown even if a cleanup goroutine ignores shutdownCtx.
	// 10 seconds is generous: shutdownCtx already gives each cleanup 5 seconds,
	// and we want to ensure the overall Shutdown() never hangs indefinitely.
	waitTimeout := time.After(10 * time.Second)
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		// All cleanup completed within timeout.
	case <-waitTimeout:
		slog.Error("Shutdown: cleanup did not complete within outer timeout - exiting anyway (some resources may not be fully released)")
	}

	// DB cleanup: handled here, not in cleanupFuncs, so we can distinguish
	// graceful from forced shutdown. The policy is:
	// - Graceful shutdown (stillBusy=false): Close the DB synchronously.
	// - Forced shutdown (stillBusy=true): Skip DB close to avoid corrupting
	//   live writers; the OS will reclaim file descriptors on process exit.
	if app.dataDir != "" {
		if !stillBusy {
			// Graceful shutdown: wait synchronously for db.Release.
			slog.Debug("Shutdown: closing database (graceful shutdown)")
			for i := 0; i < app.dbReleasesNeeded; i++ {
				if err := db.Release(app.dataDir); err != nil {
					slog.Error("Shutdown: failed to release database connection", "error", err)
					recordCleanupError(err)
				}
			}
		} else {
			// Forced shutdown: skip DB close. Live writers may still be active.
			slog.Warn("Shutdown: skipping database close due to forced shutdown (live writers may still be active; OS will reclaim resources on process exit)")
		}
	}

	// Snapshot under the mutex: if the outer timeout fired, abandoned
	// cleanup goroutines may still append after this returns, and the
	// caller must never observe that live slice.
	errMu.Lock()
	result.CleanupErrors = append([]error(nil), collected...)
	errMu.Unlock()
	return result
}

// ShutdownWithResult performs the full shutdown — agent cancellation,
// run-queue pump stop, bounded parallel cleanup, DB release — and returns
// a ShutdownResult describing how it went: whether the shutdown was
// forced (agents still busy, DB release skipped) and which cleanup
// errors occurred. See the policy comment in releaseResources' body.
//
// Cancellation comes first, with no caller-side drain to respect — see
// ShutdownAfterDrain for the drain-aware variant library hosts use.
func (app *App) ShutdownWithResult() ShutdownResult {
	return app.ShutdownAfterDrain(nil)
}

// Shutdown performs the same shutdown as ShutdownWithResult and discards
// the result. Kept as the call-site-compatible entry point for the CLI's
// many `defer a.Shutdown()` sites.
func (app *App) Shutdown() {
	app.ShutdownWithResult()
}

// The update check used to live here. It is gone rather than fixed,
// because there is nothing left for it to tell.
//
// Upstream's version (afc8fd0b) ended by publishing a
// pubsub.UpdateAvailableMsg, which the Bubble Tea TUI rendered. This fork
// removed that TUI (7ff2292e) and the message type with it, leaving a
// function that spent a 30-second-bounded HTTP request on every startup
// and then discarded the answer — and doing so against the real network,
// which is why a test had to isolate around it (see
// p348_p0_1_pump_coordinator_wiring_test.go).
//
// Restoring the feature means routing it to the web UI, which is a design
// decision and a piece of work, not a one-line repair. Filed separately.
