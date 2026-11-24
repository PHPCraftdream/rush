// Drain attempt accounting (docs/reviews/2026-09-30-async-phase4-round2-
// attempts-design.md): ONE accounting point for a Drain leg, run by the turn
// loop that ran it -- exactly once, after the leg ends and before the mailbox
// is released -- from the DB's own verdict (are the rows this attempt could
// see still unreacted?). Launchers never account: a queued or merged Drain has
// no launcher, and the release hook fires inside Run before a launcher
// regains control. Launch decisions read the per-session gate this file
// writes (work_ledger_reaction.go, coordinator_drain_policy.go).
//
// The implementation moved to drain_component.go (the drainComponent host
// pattern, like wake_component.go); this file keeps the thin coordinator
// delegates.
package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
)

func (c *coordinator) accountDrainAttempt(ctx context.Context, att *drainctl.DrainAttempt, turnErr error) {
	newDrainComponent(c).accountDrainAttempt(ctx, att, turnErr)
}

func (c *coordinator) noteDrainRefused(sessionID string, cause error) {
	newDrainComponent(c).noteDrainRefused(sessionID, cause)
}

func (c *coordinator) noteTurnFailed(sessionID string) {
	newDrainComponent(c).noteTurnFailed(sessionID)
}

func (c *coordinator) drainRefused(ctx context.Context, sessionID string, err error) error {
	return newDrainComponent(c).drainRefused(ctx, sessionID, err)
}
