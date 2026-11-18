// The reaction chain guard (#1113): the implementation moved to
// reaction_component.go (the drainComponent host pattern); this file keeps
// the thin coordinator delegates.
package agent

import (
	"context"
)

func (c *coordinator) reactionChainCount(sessionID string) int {
	return newDrainComponent(c).reactionChainCount(sessionID)
}

func (c *coordinator) chainGuardMarker(ctx context.Context, sessionID string) {
	newDrainComponent(c).chainGuardMarker(ctx, sessionID)
}

func (c *coordinator) reactionChainHasClaim(sessionID, claim string) bool {
	return newDrainComponent(c).reactionChainHasClaim(sessionID, claim)
}
