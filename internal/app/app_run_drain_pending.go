package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

const drainPendingMaxAttempts = 3

var drainPendingAfterAttemptSeam func(string)

// drainPendingBeforeRun executes outstanding durable rows before the new turn.
func (app *App) drainPendingBeforeRun(ctx context.Context, sessionID string) error {
	if app.RunQueuePump == nil || app.Sessions == nil {
		return nil
	}

	for attempt := 1; attempt <= drainPendingMaxAttempts; attempt++ {
		outstanding, err := app.Sessions.HasOutstandingRunQueueEntriesForSession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("run: failed to check pending durable work before the turn: %w", err)
		}
		if !outstanding {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("run: canceled before pending durable work drained: %w", err)
		}

		slog.Info("run: executing pending durable work for this session before the new turn", "session_id", sessionID)
		result, drainErr := app.RunQueuePump.DrainSessionNow(ctx, sessionID)
		if drainPendingAfterAttemptSeam != nil {
			drainPendingAfterAttemptSeam(sessionID)
		}

		localBusy := app.AgentCoordinator != nil && app.AgentCoordinator.IsSessionBusy(sessionID)
		if drainErr != nil {
			contention := errors.Is(drainErr, session.ErrCallQueuedNotExecuted) || errors.Is(drainErr, session.ErrDrainIncomplete)
			if !contention {
				return fmt.Errorf("run: pending durable work drain incomplete (%s): %w", result, drainErr)
			}
		}

		if localBusy {
			if err := app.waitLocalOwnerReleased(ctx, sessionID); err != nil {
				return fmt.Errorf("run: canceled while waiting for the current session owner: %w", err)
			}
		}

		outstanding, err = app.Sessions.HasOutstandingRunQueueEntriesForSession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("run: failed to recheck pending durable work after drain: %w", err)
		}
		if !outstanding {
			if drainErr != nil {
				return fmt.Errorf("run: pending durable work drain incomplete (%s): %w", result, drainErr)
			}
			slog.Info("run: pending durable work drained", "session_id", sessionID, "result", result)
			return nil
		}
		if attempt == drainPendingMaxAttempts {
			break
		}
		if !localBusy {
			timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("run: canceled while waiting for pending durable work: %w", ctx.Err())
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("run: pending durable work remains after %d drain attempts", drainPendingMaxAttempts)
}

// waitLocalOwnerReleased waits until this process no longer owns the session.
func (app *App) waitLocalOwnerReleased(ctx context.Context, sessionID string) error {
	if app.AgentCoordinator == nil || !app.AgentCoordinator.IsSessionBusy(sessionID) {
		return nil
	}
	slog.Info("run: waiting for this process's current turn on the session to finish", "session_id", sessionID)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for app.AgentCoordinator.IsSessionBusy(sessionID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}
