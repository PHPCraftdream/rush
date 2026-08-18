// Lifecycle tail end: graceful/forced Shutdown (agent cancellation,
// run-queue pump stop, bounded cleanup, DB release) and the background
// update check launched from New.

package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/update"
	"github.com/charmbracelet/crush/internal/version"
)

// Shutdown performs a graceful shutdown of the application.
func (app *App) Shutdown() {
	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()

	// First, cancel all agents and wait for them to finish. This must complete
	// before closing the DB so agents can finish writing their state.
	// CancelAll now returns whether agents are still busy after the grace period.
	var stillBusy bool
	if app.AgentCoordinator != nil {
		stillBusy = app.AgentCoordinator.CancelAll()
	}

	// Stop the run queue pump (task #340 P0-3). This must complete before DB
	// close to ensure no pump goroutines are writing when we close the connection.
	// Pump.Stop() returns true if shutdown was forced (workers still running after grace).
	if app.RunQueuePump != nil {
		pumpStillBusy := app.RunQueuePump.Stop()
		stillBusy = stillBusy || pumpStillBusy
		slog.Info("app: stopped run queue pump")
	}

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
	// For library/server callers who want graceful shutdown, they should call
	// CancelAll separately and check the return value before invoking Shutdown()
	// with a custom policy.
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

	// Now run remaining cleanup tasks in parallel with an overall bounded timeout.
	var wg sync.WaitGroup

	// Send exit event
	wg.Go(func() {
	})

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
				}
			}
		} else {
			// Forced shutdown: skip DB close. Live writers may still be active.
			slog.Warn("Shutdown: skipping database close due to forced shutdown (live writers may still be active; OS will reclaim resources on process exit)")
		}
	}
}

// checkForUpdates checks for available updates.
func (app *App) checkForUpdates(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := update.Check(checkCtx, version.Version, update.Default)
	if err != nil || !info.Available() {
		return
	}
}
