// The Drain session policy: the implementation moved to
// drain_policy_component.go (the drainComponent host pattern); this file
// keeps the thin coordinator delegates.
package agent

import (
	"context"
)

func (c *coordinator) arbiterVerdict(ctx context.Context, sessionID string, site LaunchSite) (Verdict, TurnFacts, error) {
	return newDrainComponent(c).arbiterVerdict(ctx, sessionID, site)
}

func (c *coordinator) drainPolicy(ctx context.Context, sessionID string, spent bool) drainVerdict {
	return newDrainComponent(c).drainPolicy(ctx, sessionID, spent)
}

func (c *coordinator) drainPermitted(ctx context.Context, sessionID string, spent bool) drainVerdict {
	return newDrainComponent(c).drainPermitted(ctx, sessionID, spent)
}

func (c *coordinator) drainDecision(ctx context.Context, sessionID string, spent bool) (debt bool, v drainVerdict, err error) {
	return newDrainComponent(c).drainDecision(ctx, sessionID, spent)
}

func (c *coordinator) isDurableDelegationChild(ctx context.Context, sessionID string) (bool, error) {
	return newDrainComponent(c).isDurableDelegationChild(ctx, sessionID)
}

func (c *coordinator) sessionDebtIsBGShellOnly(ctx context.Context, sessionID string) (bgOnly bool, err error) {
	return newDrainComponent(c).sessionDebtIsBGShellOnly(ctx, sessionID)
}
