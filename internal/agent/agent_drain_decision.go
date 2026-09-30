// The Drain call's turn-start decision (docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.4, step 4 review fix P1). Split out of agent_turn.go
// to keep that file under the repo's 1000-line limit.
package agent

import (
	"context"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/session"
)

// decideDrainTurn reports whether a Drain call (agent_turn.go's runTurn,
// after its own turn-start pull) should reach the provider at all. snap is
// the visible (delivery='done') debt captured AFTER that pull: a pull that
// never succeeds leaves rows 'pending', so it can never force a chain of
// empty-prompt provider turns (doc sec.6). With visible debt the session
// policy decides; the verdict says why a turn is refused.
func (a *sessionAgent) decideDrainTurn(ctx context.Context, sessionID string, snap session.DebtSnapshot) (bool, drainVerdict) {
	if snap.Empty() {
		return false, drainVerdict{kind: drainDeferred, reason: "no visible debt"}
	}
	if a.asyncJobs == nil || a.asyncJobs.coord == nil {
		return true, drainVerdict{kind: drainAllow}
	}
	allowed, _, polErr := a.asyncJobs.coord.sessionDrainPolicy(ctx, sessionID)
	if polErr != nil {
		slog.Warn("drain turn: session policy check failed; allowing the turn",
			"session_id", sessionID, "err", polErr)
		return true, drainVerdict{kind: drainAllow}
	}
	if !allowed {
		return false, drainVerdict{kind: drainDeferred, reason: "session policy"}
	}
	return true, drainVerdict{kind: drainAllow}
}

// visibleDebtSnapshot reads owner's visible (delivery='done') debt row set.
func (a *sessionAgent) visibleDebtSnapshot(ctx context.Context, sessionID string) (session.DebtSnapshot, error) {
	if a.asyncJobs == nil {
		return session.DebtSnapshot{}, nil
	}
	return a.asyncJobs.captureDebtSnapshot(ctx, sessionID)
}

// pendingDebtLeft reports whether pending-inclusive debt remains after a
// no-turn Drain (its pull keeps failing). An unreadable answer counts as
// "remains": the session goes to the re-check set instead of being forgotten.
func (a *sessionAgent) pendingDebtLeft(ctx context.Context, sessionID string) bool {
	if a.asyncJobs == nil {
		return false
	}
	debt, err := a.asyncJobs.reactionDebtExists(ctx, sessionID)
	if err != nil {
		slog.Warn("drain turn: pending debt check failed; treating the debt as remaining",
			"session_id", sessionID, "err", err)
		return true
	}
	return debt
}
