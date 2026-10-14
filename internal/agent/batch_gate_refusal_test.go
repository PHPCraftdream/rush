package agent

// R12-E round 6 (fix 1a): a batch-policy refusal (the launch gate is shut
// over visible BATCH debt) must NOT clear the launch gate. Before the fix
// the no-turn facts carried CommitRefused=false on the ON path (att.snapshot
// stays zero there), so decideAccount fell through to "no debt: gate reset"
// and executeAccountVerdict reset a gate the attempt produced no evidence
// about. Production accounting path: accountDrainAttempt -> facts.Attempt
// (drain_attempt.go, drainNoTurn case) -> decideAccount -> A3' "commit
// refused: gate untouched" -> executeAccountVerdict leaves the gate.

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchGateRefusalUntouchedON drives the REAL production accounting
// no-turn path with the flag ON over a cutoverEnv DB: a paced gate, visible
// batch debt, a no-turn leg with a nonempty batch snapshot -> the gate must
// stay paced (A3': the attempt is no evidence about the gate).
func TestBatchGateRefusalUntouchedON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// Real visible batch debt: a settled, DELIVERED detached root.
	env.invokeDetached("call-gate", "view", leaf)
	rootID := env.rootID("call-gate")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	env.requireBatchPull(1)

	// The gate refusal: one paid-unreacted pace with RetryAt in the future
	// (turn_arbiter_state.go pace: g.RetryAt = now+wait; Paced is derived
	// as !RetryAt.IsZero on the snapshot). decideDrainTurn's
	// arbiterVerdict then returns gateShut's rule-9 defer "retry pause
	// after an unreacted attempt" (HintOpens false), so the launch verdict
	// is a refusal while the batch debt stays.
	coord.arb.pace(env.sessionID, 0, time.Minute, false, pacePaidUnreacted)

	// The production ON capture (agent_turn.go): fills att.batchSnapshot,
	// leaves att.snapshot zero.
	bfacts := coordinatorBatchCutoverFacts(coord)
	require.NotNil(t, bfacts)
	bctx, bcancel := context.WithTimeout(env.ctx, drainAccountBudget())
	bsnap, berr := bfacts.CaptureVisibleDebt(bctx, env.sessionID)
	bcancel()
	require.NoError(t, berr)
	require.False(t, bsnap.Empty(), "the delivered root is visible batch debt")

	att := &drainAttempt{
		sessionID:     env.sessionID,
		outcome:       drainNoTurn,
		commitNo:      drainVerdict{kind: drainPaced, recheck: true, reason: "retry pause after an unreacted attempt"},
		batchSnapshot: bsnap,
		batchCaptured: true,
	}
	coord.accountDrainAttempt(env.ctx, att, nil)

	// FIRST ASSERTION (static trace in the report): the gate is UNTOUCHED.
	// Trace: accountDrainAttempt -> drainNoTurn case -> batchRefused :=
	// att.batchCaptured && !att.batchSnapshot.Empty() = true ->
	// CommitRefused=true -> decideAccount A3' -> Verdict{VNone, "commit
	// refused: gate untouched"} -> executeAccountVerdict's A3' branch: no
	// resetGate, no pace (drain_attempt.go).
	gate := coord.arb.snapshot(env.sessionID).Gate
	require.True(t, gate.Paced, "the paced gate is untouched by the refusal accounting")
	require.True(t, gate.RetryAt.After(time.Now()), "the pause is still in the future")
	// Behavioral oracle through the launch side: the paced refusal stands.
	dv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, drainPaced, dv.kind, "the launch verdict is still the paced refusal")
	require.Equal(t, "retry pause after an unreacted attempt", dv.reason)
}

// TestBatchGateLegacyTwinsOFF keeps the OFF behavior byte-identical: a
// nonempty LEGACY snapshot refuses (gate untouched); an empty one resets the
// gate ("no debt: gate reset").
func TestBatchGateLegacyTwinsOFF(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "")
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)
	coord := newCutoverCoordinator(t, env)
	require.Nil(t, coordinatorBatchCutoverFacts(coord), "flag off: no batch facts (the inline ledger path)")

	// OFF refusal twin: visible legacy debt + a paced gate -> the gate must
	// stay paced (the pre-fix predicate already covered this).
	coord.arb.pace(env.sessionID, 0, time.Minute, false, pacePaidUnreacted)
	attRefused := &drainAttempt{
		sessionID: env.sessionID,
		outcome:   drainNoTurn,
		commitNo:  drainVerdict{kind: drainPaced, recheck: true, reason: "retry pause after an unreacted attempt"},
		snapshot:  session.DebtSnapshot{Jobs: []session.DebtJobRef{{ToolCallID: "legacy-1"}}},
	}
	coord.accountDrainAttempt(env.ctx, attRefused, nil)
	gate := coord.arb.snapshot(env.sessionID).Gate
	require.True(t, gate.Paced, "OFF refusal: the paced gate stays")
	require.True(t, gate.RetryAt.After(time.Now()))

	// OFF reset twin: no debt at all -> "no debt: gate reset" opens the
	// gate (executeAccountVerdict's reset branch).
	attReset := &drainAttempt{
		sessionID: env.sessionID,
		outcome:   drainNoTurn,
		commitNo:  drainVerdict{kind: drainDeferred, reason: "no visible debt"},
	}
	coord.accountDrainAttempt(env.ctx, attReset, nil)
	gate = coord.arb.snapshot(env.sessionID).Gate
	require.False(t, gate.Paced, "OFF no-debt: the gate is reset")
	require.True(t, gate.RetryAt.IsZero())
	require.Zero(t, gate.PaidStreak, "OFF no-debt: the streaks are cleared")
}
