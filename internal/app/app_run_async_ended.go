package app

import (
	"context"
	"log/slog"
)

// persistEndedReason makes the session row's ended_reason match the envelope
// the loop is about to flush (R6C-6). ExecuteRun writes it once per real turn,
// so a loop exit that changes the outcome after the last turn -- Ctrl-C or
// --timeout while waiting, a stuck debt, a cap or cancel caught by the
// precheck, an unreadable DB -- would otherwise leave the last turn's value
// ("end_turn") next to an envelope that says "canceled". The write runs on a
// context detached from the run's (cancelled on those exits) and is skipped
// when the row already holds the reason, which is the common case: the last
// turn wrote it. A turn that ran nothing (refused, queued) never wrote, so
// the row still holds the reason of the answer the envelope carries.
func (l *cliLoop) persistEndedReason(final *RunResult) {
	if final == nil || final.ExitReason == "" || l.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	if l.app.DB != nil {
		var current string
		err := l.app.DB().QueryRowContext(ctx,
			`SELECT COALESCE(ended_reason, '') FROM sessions WHERE id = ?`, l.sessionID).Scan(&current)
		if err == nil && current == final.ExitReason {
			return
		}
	}
	if setErr := l.app.Sessions.SetEndedReason(ctx, l.sessionID, final.ExitReason); setErr != nil {
		slog.Warn("Failed to persist ended_reason at loop exit", "session_id", l.sessionID, "reason", final.ExitReason, "err", setErr)
	}
}
