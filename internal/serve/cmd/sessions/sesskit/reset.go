package sesskit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchResetRefusedError turns a *session.BatchResetRefusal into the same
// output shape the legacy reset refusal uses, naming the offending root and
// surfacing the cancelling = "stopped but awaiting termination" case.
func BatchResetRefusedError(sessionID string, refusal *session.BatchResetRefusal) error {
	state := refusal.State
	if state == "cancelling" {
		state = "cancelling (stopped but awaiting termination)"
	}
	return fmt.Errorf("session %s is still being worked on (batch root %s tool call %s is %s); "+
		"nothing was reset. Stop it first (`rush sessions cancel %s` / `rush batch stop`), then reset again",
		sessionID, refusal.RootID, refusal.ToolCallID, state, sessionID)
}

// resetLiveLoopRefusal is the reset's live-loop gate: the batch
// reset's atomic refusal covers batch work, but a live `rush run` loop
// between turns holds no batch root and no lock, so `reset --force` would
// otherwise wipe a session a loop still drives (the R8A-3 bug). Refuses
// under --force too.
func ResetLiveLoopRefusal(ctx context.Context, a *app.App, sessionID string) error {
	act, actErr := a.SessionActivity(ctx, sessionID)
	if actErr != nil {
		return ResetRefusedError(sessionID, "live-work state could not be read (fail closed)")
	}
	if act.Facts.Driver != nil {
		return ResetRefusedError(sessionID, "a live `rush run` loop still drives it")
	}
	return nil
}

// batchResetSessionHistory is the reset path: the R10c batch reset
// (check + delete in ONE writer transaction; refusal writes nothing). Its
// atomic refusal of any nonterminal batch root IS the live-work gate -- the
// only predecessor step is H4 W2's best-effort owner-scope dead-host
// recovery (BatchRecoverOwnerScope), which never touches the legacy
// ledger. The plain-wipe fallback
// exists only for a configless App (nil DB).
func BatchResetSessionHistory(ctx context.Context, a *app.App, sessionID string) (session.BatchResetOutcome, error) {
	if a.DB() == nil {
		// No database can hold batch rows: the plain wipe is all there is.
		if err := a.Messages.DeleteSessionMessages(ctx, sessionID); err != nil {
			return session.BatchResetOutcome{}, fmt.Errorf("failed to reset session %s: %w", sessionID, err)
		}
		return session.BatchResetOutcome{}, nil
	}
	// H4 W2: the legacy reset's RecoverOwnerScope twin
	// recovers dead-host roots before its refusal: a dead host's running
	// roots become interrupted/host_lost FIRST, so the atomic refusal below
	// judges only work a live host still holds. Best-effort: a failed
	// recovery keeps the conservative refusal. Accepted without a budget:
	// an explicit user command, not a hot path.
	if _, err := session.NewBatchStore(a.DB()).BatchRecoverOwnerScope(ctx, sessionID, session.BatchSharedHostProbe(a.DataDirectory())); err != nil {
		slog.Debug("sessions reset: batch dead-host recovery failed; refusal stays conservative", "session_id", sessionID, "err", err)
	}
	out, err := session.NewBatchStore(a.DB()).BatchResetOwnerHistory(ctx, a.Messages, sessionID)
	var refusal *session.BatchResetRefusal
	if errors.As(err, &refusal) {
		return session.BatchResetOutcome{}, BatchResetRefusedError(sessionID, refusal)
	}
	if errors.Is(err, session.ErrBatchResetRefused) {
		return session.BatchResetOutcome{}, ResetRefusedError(sessionID, err.Error())
	}
	if err != nil {
		return session.BatchResetOutcome{}, fmt.Errorf("failed to reset session %s: %w", sessionID, err)
	}
	return out, nil
}

// resetRefusedError is the shared reset-refusal shape: the batch reset's
// atomic refusal and the live-loop gate render their "still being worked
// on" reason through it.
func ResetRefusedError(sessionID, what string) error {
	return fmt.Errorf("session %s is still being worked on (%s); nothing was reset. "+
		"Stop it first (`rush sessions cancel %s`: a waiting rush run exits at its next wake; a hung one can be ended by its PID, shown by `rush sessions jobs %s`, "+
		"or `rush sessions kill %s` while a turn holds the session lock), then reset again "+
		"(--force only kills the process holding the session lock; it does not stop a loop between turns or running jobs)", sessionID, what, sessionID, sessionID, sessionID)
}
