// The Drain call's turn-start decision (docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.4, step 4 review fix P1). Split out of agent_turn.go
// to keep that file under the repo's 1000-line limit.
package agent

import (
	"context"
	"log/slog"
)

// decideDrainTurn reports whether a Drain call (agent_turn.go's runTurn,
// after its own turn-start pull) should reach the provider at all.
//
// Uses VISIBLE debt (delivery='done'), never the plain reaction-debt
// predicate (delivery IN pending,done): a permanently failing pull leaves
// its rows stuck at 'pending' forever, and reacting to 'pending' debt would
// force an endless chain of empty-prompt provider turns with nothing new in
// history to react to (doc sec.6, P1 review fix). anyWake -- this pull's own
// wake bit -- is used ONLY as a fallback when the debt check itself errors,
// never as the primary signal (step 3's interim rule, replaced in step 4).
//
// When there IS visible debt, the session policy is also consulted: a
// policy-check error fails OPEN (the worst case is one turn the policy
// would have refused, not a lost reaction -- the debt stays durable either
// way).
func (a *sessionAgent) decideDrainTurn(ctx context.Context, sessionID string, anyWake bool) bool {
	debt, debtErr := a.asyncJobs.visibleReactionDebtExists(ctx, sessionID)
	if debtErr != nil {
		slog.Warn("drain turn: visible reaction debt check failed; falling back to this pull's own wake bit",
			"session_id", sessionID, "err", debtErr)
		debt = anyWake
	}
	if !debt {
		return false
	}
	if a.asyncJobs == nil || a.asyncJobs.coord == nil {
		return true
	}
	allowed, _, polErr := a.asyncJobs.coord.sessionDrainPolicy(ctx, sessionID)
	if polErr != nil {
		slog.Warn("drain turn: session policy check failed; allowing the turn",
			"session_id", sessionID, "err", polErr)
		return true
	}
	return allowed
}
