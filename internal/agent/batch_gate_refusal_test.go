package agent

// R12-E round 7 (fix 1): the detached completion root uses a plain
// countingTool leaf (no gates): its Run returns immediately, so the root
// settles completed with no release bookkeeping and rootTerminal's
// Eventually has nothing to wait on. Production accounting path:
// accountDrainAttempt -> facts.Attempt (drain_attempt.go, drainNoTurn
// case) -> decideAccount -> A3' "commit refused: gate untouched" ->
// executeAccountVerdict leaves the gate.

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
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// Real visible batch debt: a settled, DELIVERED detached root. The
	// countingTool leaf completes instantly (no gate, nothing to release).
	env.invokeDetached("call-gate", "view", fast)
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

// TestBatchGateLegacyTwinsOFF (renamed semantics: the twins are now
// flag-neutral batch policy) pins the refusal/reset accounting: a nonempty
// snapshot refuses (gate untouched); an empty one resets the gate ("no debt:
// gate reset").
func TestBatchGateLegacyTwinsOFF(t *testing.T) {
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverCoordinator(t, env)

	// Refusal twin: visible debt + a paced gate -> the gate must stay paced
	// (the pre-fix predicate already covered this).
	coord.arb.pace(env.sessionID, 0, time.Minute, false, pacePaidUnreacted)
	attRefused := &drainAttempt{
		sessionID:     env.sessionID,
		outcome:       drainNoTurn,
		commitNo:      drainVerdict{kind: drainPaced, recheck: true, reason: "retry pause after an unreacted attempt"},
		batchCaptured: true,
		batchSnapshot: session.BatchDebtSnapshot{Nodes: []session.BatchDebtNodeRef{{RootID: "root-1", NodeID: "node-1"}}},
	}
	coord.accountDrainAttempt(env.ctx, attRefused, nil)
	gate := coord.arb.snapshot(env.sessionID).Gate
	require.True(t, gate.Paced, "refusal: the paced gate stays")
	require.True(t, gate.RetryAt.After(time.Now()))

	// Reset twin: no debt at all -> "no debt: gate reset" opens the
	// gate (executeAccountVerdict's reset branch).
	attReset := &drainAttempt{
		sessionID: env.sessionID,
		outcome:   drainNoTurn,
		commitNo:  drainVerdict{kind: drainDeferred, reason: "no visible debt"},
	}
	coord.accountDrainAttempt(env.ctx, attReset, nil)
	gate = coord.arb.snapshot(env.sessionID).Gate
	require.False(t, gate.Paced, "no-debt: the gate is reset")
	require.True(t, gate.RetryAt.IsZero())
	require.Zero(t, gate.PaidStreak, "no-debt: the streaks are cleared")
}

// TestBatchPendingLeftONNoDebtResetsGate (R12-E round 7 fix 4a): the ON
// no-visible-debt accounting computes pendingLeft from the BATCH
// pending-inclusive source, never from async_jobs -- with the legacy table
// DROPPED, a no-debt no-turn leg still reaches A3 "no debt: gate reset".
func TestBatchPendingLeftONNoDebtResetsGate(t *testing.T) {
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	// The legacy async_jobs table no longer exists in the batch-only schema;
	// the DROP proves the accounting below never reads it.
	_, err := env.conn.ExecContext(env.ctx, `DROP TABLE IF EXISTS async_jobs`)
	require.NoError(t, err)

	// A paced gate the reset must clear.
	coord.arb.pace(env.sessionID, 0, time.Minute, false, pacePaidUnreacted)

	// The production predicate inputs (agent_turn.go att block): the ON
	// capture is empty (fresh env, nothing invoked) and the pending-inclusive
	// batch read is empty too -> pendingLeft=false.
	bfacts := coordinatorBatchCutoverFacts(coord)
	require.NotNil(t, bfacts)
	bctx, bcancel := context.WithTimeout(env.ctx, drainAccountBudget())
	bsnap, berr := bfacts.CaptureVisibleDebt(bctx, env.sessionID)
	bcancel()
	require.NoError(t, berr)
	require.True(t, bsnap.Empty(), "no batch debt was ever created")
	pending, perr := bfacts.PendingInclusiveDebt(env.ctx, env.sessionID)
	require.NoError(t, perr, "the pending read is batch-only: async_jobs is dropped")
	require.Empty(t, pending.Nodes)
	require.Empty(t, pending.Notices)

	att := &drainAttempt{
		sessionID:     env.sessionID,
		outcome:       drainNoTurn,
		commitNo:      drainVerdict{kind: drainDeferred, reason: "no visible debt"},
		batchSnapshot: bsnap,
		batchCaptured: true,
		pendingLeft:   len(pending.Nodes)+len(pending.Notices) > 0,
	}
	coord.accountDrainAttempt(env.ctx, att, nil)

	// FIRST ASSERTION (static trace in the report): the gate is RESET.
	// Trace: accountDrainAttempt -> drainNoTurn -> batchRefused=false
	// (empty batch snapshot), CommitRefused=false -> decideAccount A3 "no
	// debt: gate reset" (PendingLeft=false) -> executeAccountVerdict's
	// reset branch -> resetGate (drain_attempt.go).
	gate := coord.arb.snapshot(env.sessionID).Gate
	require.False(t, gate.Paced, "ON no-debt: the gate is reset via the batch pending source")
	require.True(t, gate.RetryAt.IsZero())
}

// TestBatchPendingLeftONPendingDebtPacesGate (R12-E round 7 fix 4b): a
// settled batch completion that was NOT pulled is invisible debt but
// pending-inclusive debt: the ON predicate yields pendingLeft=true and the
// accounting paces the gate (A2 "pending debt left behind"), all with
// async_jobs DROPPED (the ON branch never touches the legacy store).
func TestBatchPendingLeftONPendingDebtPacesGate(t *testing.T) {
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	_, err := env.conn.ExecContext(env.ctx, `DROP TABLE IF EXISTS async_jobs`)
	require.NoError(t, err)

	// A settled, UNPULLED batch completion (countingTool: instant settle).
	env.invokeDetached("call-pl", "view", fast)
	rootID := env.rootID("call-pl")
	require.Equal(t, "completed", env.rootTerminal(rootID))

	// Production predicate inputs: visible capture EMPTY (delivery is still
	// 'pending' -- no pull), pending-inclusive NONEMPTY (the same row set
	// with visible=false over batch_nodes; the node's completion notice is
	// unpulled). Trace the production code: hasVisibleDebt=false ->
	// bfacts != nil -> pendingLeft = len(pending.Nodes)+len(pending.Notices)
	// > 0 = true (agent_turn.go).
	bfacts := coordinatorBatchCutoverFacts(coord)
	require.NotNil(t, bfacts)
	bctx, bcancel := context.WithTimeout(env.ctx, drainAccountBudget())
	bsnap, berr := bfacts.CaptureVisibleDebt(bctx, env.sessionID)
	bcancel()
	require.NoError(t, berr)
	require.True(t, bsnap.Empty(), "the completion is NOT visible before any pull")
	pending, perr := bfacts.PendingInclusiveDebt(env.ctx, env.sessionID)
	require.NoError(t, perr, "the pending read is batch-only: async_jobs is dropped")
	require.NotZero(t, len(pending.Nodes)+len(pending.Notices), "the unpulled completion is pending-inclusive debt")

	att := &drainAttempt{
		sessionID:     env.sessionID,
		outcome:       drainNoTurn,
		commitNo:      drainVerdict{kind: drainDeferred, reason: "no visible debt"},
		batchSnapshot: bsnap,
		batchCaptured: true,
		pendingLeft:   len(pending.Nodes)+len(pending.Notices) > 0,
	}
	coord.accountDrainAttempt(env.ctx, att, nil)

	// FIRST ASSERTION (static trace in the report): the gate is PACED.
	// Trace: accountDrainAttempt -> drainNoTurn -> CommitRefused=false,
	// PendingLeft=true -> decideAccount A2 "pending debt left behind" ->
	// executeAccountVerdict's A2 branch -> paceUnreacted(sid, hintAt, true,
	// paceFreeNoTurn) -> arbPace with wait=drainRetryAfterFailure (default
	// 60s; drain_attempt.go init) -> RetryAt in the future.
	gate := coord.arb.snapshot(env.sessionID).Gate
	require.True(t, gate.Paced, "ON pending-left: the gate is paced (A2)")
	require.True(t, gate.RetryAt.After(time.Now()), "the A2 pace is the 60s retry pause, not a stale tick")
}
