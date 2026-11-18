// ReactionDebtSource implementations moved to reaction_component.go (the
// drainComponent host pattern); this file keeps the thin coordinator
// delegates.
package agent

import (
	"context"
	"time"
)

func (c *coordinator) ClaimExternalDriver(ctx context.Context, sessionID string) error {
	return newDrainComponent(c).ClaimExternalDriver(ctx, sessionID)
}

func (c *coordinator) ReleaseExternalDriver(ctx context.Context, sessionID string) {
	newDrainComponent(c).ReleaseExternalDriver(ctx, sessionID)
}

func (c *coordinator) ReactionDebtExists(ctx context.Context, sessionID string) (bool, error) {
	return newDrainComponent(c).ReactionDebtExists(ctx, sessionID)
}

func (c *coordinator) WaitForHint(ctx context.Context, sessionID string, until time.Time) {
	newDrainComponent(c).WaitForHint(ctx, sessionID, until)
}

func (c *coordinator) RunMaintenanceSweep(ctx context.Context) {
	newDrainComponent(c).RunMaintenanceSweep(ctx)
}

func (c *coordinator) CLIScope(ctx context.Context, sessionID string) (CLIScopeState, error) {
	return newDrainComponent(c).CLIScope(ctx, sessionID)
}

func (c *coordinator) runningWorkOpen(ctx context.Context, sessionID string) (bool, error) {
	return newDrainComponent(c).runningWorkOpen(ctx, sessionID)
}
