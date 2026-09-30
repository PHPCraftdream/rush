package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// A `sessions cancel` flag (sessions.cancel_requested) is a ONE-SHOT request
// (R8A-2/R8C-2): it is cleared when it is honoured and when a human-initiated
// turn starts, so one cancel never aborts every later turn of the session.
//   - honoured: the in-turn abort (enforceRunawayCaps) and, for a session the
//     `rush run` loop drives, the loop itself (it reads the flag between turns
//     and after an aborted turn, so a turn of such a session never clears it);
//   - human turn: a web/SDK Run (runInternal, no per-call options, not a Drain)
//     and a delegation resuming a child (runSubAgent). The CLI's own turns clear
//     in ExecuteRun where they are meant to; a Drain never clears at its start:
//     the loop must keep seeing the operator's request.

// clearCancelRequest spends sessionID's cancel flag, best effort on a context
// detached from the turn's: a failure leaves the flag for the next human turn
// to clear.
func clearCancelRequest(ctx context.Context, sessions session.Service, sessionID string) {
	if sessions == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := sessions.ClearCancelRequest(ctx, sessionID); err != nil {
		slog.Warn("could not clear the cancel-requested flag", "session_id", sessionID, "err", err)
	}
}
