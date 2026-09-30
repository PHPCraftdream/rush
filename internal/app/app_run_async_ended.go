package app

import (
	"context"
	"errors"
	"log/slog"
)

// persistEndedReason is the ONLY writer of the session row's ended_reason for a
// `rush run` loop (R8A-1): the column is empty while the run is in progress
// (ExecuteRun's per-turn write is suppressed for the loop's turns, loopTurn)
// and holds the run's exit reason afterwards -- the envelope's exit_reason the
// loop is about to flush. Every loop exit (Ctrl-C or --timeout while waiting,
// a stuck debt, a cap or cancel caught by the precheck or while waiting, an
// unreadable DB, the ordinary close) goes through exit or flushed, which call
// it. The write runs on a context detached from the run's (cancelled on those
// exits) and is skipped when the row already holds the reason. A run that
// crashed (kill -9) never reaches it and keeps the empty value.
//
// Without an envelope (the launched first turn returned none) the reason is
// derived from the error; a first turn that never launched (refused, lock
// busy) wrote nothing and records nothing.
func (l *cliLoop) persistEndedReason(final *RunResult, err error) {
	reason := l.endedReasonFor(final, err)
	if reason == "" || l.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	if l.app.DB != nil {
		var current string
		qErr := l.app.DB().QueryRowContext(ctx,
			`SELECT COALESCE(ended_reason, '') FROM sessions WHERE id = ?`, l.sessionID).Scan(&current)
		if qErr == nil && current == reason {
			return
		}
	}
	if setErr := l.app.Sessions.SetEndedReason(ctx, l.sessionID, reason); setErr != nil {
		slog.Warn("Failed to persist ended_reason at loop exit", "session_id", l.sessionID, "reason", reason, "err", setErr)
	}
}

// endedReasonFor is the value persistEndedReason records ("" = record nothing).
func (l *cliLoop) endedReasonFor(final *RunResult, err error) string {
	switch {
	case l.refusedByOwner:
		return ""
	case final != nil && final.ExitReason != "":
		return final.ExitReason
	case final != nil:
		return "done"
	case !l.firstSubmitted || err == nil:
		return ""
	case exitedCanceled(nil, err) || l.ctx.Err() != nil:
		return "canceled"
	}
	var inc *runIncompleteError
	if errors.As(err, &inc) && inc.reason != "" {
		return inc.reason
	}
	return "error"
}
