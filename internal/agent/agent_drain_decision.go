// The Drain call's turn-start decision (docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.4, step 4 review fix P1). Split out of agent_turn.go
// to keep that file under the repo's 1000-line limit.
package agent

import (
	"context"
)

// decideDrainTurn reports whether a Drain call (agent_turn.go's runTurn,
// after its own turn-start pull) should reach the provider at all.
// hasVisibleDebt says the batch debt captured AFTER that pull is non-empty
// (len(batchSnap.Nodes)+len(batchSnap.Notices) > 0; the capture in
// agent_turn.go). A pull that never succeeds leaves rows 'pending', so it
// can never force a chain of empty-prompt provider turns (doc sec.6). With
// visible debt the ONE launch predicate decides (policy, then the launch
// gate); the verdict says why a turn is refused. A Drain queued before a
// failure commits under the now-closed gate and only pulls.
func (a *sessionAgent) decideDrainTurn(ctx context.Context, sessionID string, hasVisibleDebt bool) (bool, drainVerdict) {
	if !hasVisibleDebt {
		return false, drainVerdict{kind: drainDeferred, reason: "no visible debt"}
	}
	if a.coord == nil {
		return true, drainVerdict{kind: drainAllow}
	}
	// The commit compares the bg-shell cap like a re-check (siteCommit: no
	// slot): a QUEUED Drain decides on debt its launch decision never saw (a
	// completion over the cap that landed after the Drain that owned the last
	// slot pulled), and it must not get a sixth automatic turn. A Drain's own
	// slot row is not an over-cap row, so it is never refused for the slot it
	// holds.
	v, _, err := a.coord.arbiterVerdict(ctx, sessionID, siteCommit)
	if err != nil {
		return false, unreadablePolicyInput("launch decision input", sessionID, err)
	}
	dv := drainVerdictOf(v)
	if dv.kind == drainAllow {
		// An allowed drain launch breaks await_tasks' sleep and cancels its
		// max_wait schedule (#1270). The wakeSession funnel (drainPolicy)
		// never sees an externally-driven session's drains — its loop
		// launches those itself — so this is the commit-side half of the
		// "every allowed launch clears" contract.
		a.coord.clearSleepAll(sessionID)
	}
	return dv.kind == drainAllow, dv
}
