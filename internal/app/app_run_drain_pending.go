package app

import (
	"context"
	"log/slog"
	"time"
)

// drainPendingBeforeRun executes this session's outstanding durable run-queue
// rows synchronously before ExecuteRun starts its own turn. DrainSessionNow
// either runs a pending row itself or waits for the one this process's pump
// already admitted, so our turn never queues behind a local pump turn that
// our own exit would then cancel. Failures are logged, never fatal: the
// caller's own turn still runs.
func (app *App) drainPendingBeforeRun(ctx context.Context, sessionID string) {
	if app.RunQueuePump == nil || app.Sessions == nil {
		return
	}
	outstanding, err := app.Sessions.HasOutstandingRunQueueEntriesForSession(ctx, sessionID)
	if err != nil {
		slog.Warn("run: could not check pending durable work before the turn", "session_id", sessionID, "err", err)
		return
	}
	if !outstanding {
		return
	}
	slog.Info("run: executing pending durable work for this session before the new turn", "session_id", sessionID)
	result, drainErr := app.RunQueuePump.DrainSessionNow(ctx, sessionID)
	// The drained turn may still be unwinding (lease lost, watchdog cancel).
	defer app.waitLocalOwnerReleased(ctx, sessionID)
	if drainErr != nil {
		slog.Warn("run: pending durable work did not drain cleanly; continuing with the new turn", "session_id", sessionID, "result", result, "err", drainErr)
		return
	}
	slog.Info("run: pending durable work drained", "session_id", sessionID, "result", result)
}

// waitLocalOwnerReleased blocks until no turn in THIS process owns sessionID
// (or ctx ends). A drain can return while the turn it drained is still
// unwinding (lease lost mid-execution, watchdog cancel): starting our turn
// then would queue behind it, exit "queued", and cancel it on the way out.
func (app *App) waitLocalOwnerReleased(ctx context.Context, sessionID string) {
	if app.AgentCoordinator == nil || !app.AgentCoordinator.IsSessionBusy(sessionID) {
		return
	}
	slog.Info("run: waiting for this process's current turn on the session to finish", "session_id", sessionID)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for app.AgentCoordinator.IsSessionBusy(sessionID) {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
