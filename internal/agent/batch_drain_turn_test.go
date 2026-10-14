package agent

// R12-E round 5 additions:
//   - TestBatchDrainTurnDecisionON: the turn-start decision reads the BATCH
//     (+ session-notice) debt snapshot when the flag is ON; async_jobs is
//     NEVER read (dropped table) -- the production read+decide pair the
//     runTurn block executes (agent_turn.go att block: CaptureVisibleDebt
//     once, then decideDrainTurn(hasVisibleDebt)).
//   - TestBatchChainEvidenceRepeatedCommand: the batch branch applies the
//     legacy repeated-command policy -- a FIRST substantial batch launch is
//     progress; an immediate byte-identical repeat is an idle claim (root
//     identity recorded, no progress).

import (
	"context"
	"os"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchDrainTurnDecisionON drives the production turn-start decision
// over a cutoverEnv with async_jobs DROPPED. Delivered shape: (a) the exact
// read+decide pair the runTurn block runs -- ONE CaptureVisibleDebt (the
// same call agent_turn.go makes, feeding both the decision input and
// att.batchSnapshot) followed by the production decideDrainTurn with
// hasVisibleDebt -- over a real settled batch completion; the full runTurn
// harness cannot reach this DB, so the production code is exercised at its
// two exact seams.
func TestBatchDrainTurnDecisionON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	leaf := newGatedLeaf("view")
	fast := &countingTool{name: "grep"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf, fast)
	releaseAllAt(t, leaf)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	// Headless legacy store on the SAME conn, then drop async_jobs: any
	// legacy read anywhere in this path fails with "no such table".
	coord.asyncJobs.store = session.NewAsyncJobStore(env.conn, env.dataDir, os.Getpid(), "host-dt")
	_, err := env.conn.ExecContext(env.ctx, `DROP TABLE async_jobs`)
	require.NoError(t, err)

	// A real settled batch completion: visible batch debt only.
	env.invokeDetached("call-dt", "grep", fast)
	rootID := env.rootID("call-dt")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "the settled root is visible batch debt")

	// FIRST ASSERTION (static trace in the report): the ON capture is the
	// production call; with async_jobs dropped it must still succeed and be
	// non-empty -- CaptureVisibleDebt reads batch_nodes + session_notices
	// only (batch_reaction.go batchDebtSnapshot).
	bfacts := coordinatorBatchCutoverFacts(coord)
	require.NotNil(t, bfacts)
	bctx, bcancel := context.WithTimeout(env.ctx, drainAccountBudget())
	bsnap, berr := bfacts.CaptureVisibleDebt(bctx, env.sessionID)
	bcancel()
	require.NoError(t, berr, "the ON capture never reads async_jobs (table dropped)")
	hasVisibleDebt := len(bsnap.Nodes)+len(bsnap.Notices) > 0
	require.True(t, hasVisibleDebt, "the settled root's node debt is the decision input")

	// The production decision with that input: the turn is TAKEN (the
	// provider stream would be reached), zero error.
	sa := &sessionAgent{asyncJobs: coord.asyncJobs}
	commit, verdict := sa.decideDrainTurn(env.ctx, env.sessionID, true)
	require.True(t, commit, "visible batch debt commits the Drain turn (ON path)")
	t.Logf("DrainTurnON commit=%v verdict=%+v", commit, verdict)

	// The empty-input twin: no debt -> the no-turn path (never the
	// provider), reason "no visible debt".
	commitNo, verdictNo := sa.decideDrainTurn(env.ctx, env.sessionID, false)
	require.False(t, commitNo)
	require.Equal(t, drainDeferred, verdictNo.kind)
	require.Equal(t, "no visible debt", verdictNo.reason)

	// Batch-only node refs, no legacy claims: a chainLink over the zero
	// legacy snapshot is inert (chainIdleClaims empty, batch roots own the
	// accounting) -- the ON leg passes att.snapshot zero.
	att := &drainAttempt{chainBatchRoots: []string{"call-dt"}}
	coord.arb.chainLink(env.sessionID, att, session.DebtSnapshot{})
	require.GreaterOrEqual(t, coord.arb.chainLinksOf(env.sessionID), 1,
		"the batch-only leg accounts through chainBatchRoots with a zero legacy snapshot")
	env.logDebtRows(t)
	env.logNodeRows(t, rootID)
}

// TestBatchChainEvidenceRepeatedCommand feeds the collector REAL started
// responses for two byte-identical substantial batch commands: the first is
// progress with no identity; the second is an idle claim (root identity
// recorded, no progress) -- legacy repeated-command semantics
// (coordinator_reaction_repeated_test.go) applied to the batch branch.
func TestBatchChainEvidenceRepeatedCommand(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// The collector's arbiter seam is the REAL coordinator's.
	ts := &turnStream{
		a:    &sessionAgent{asyncJobs: coord.asyncJobs},
		call: SessionAgentCall{SessionID: env.sessionID},
		att:  &drainAttempt{},
	}

	// FIRST ASSERTION (static trace in the report): a FIRST substantial
	// batch launch ("gh run view", run_in_background, wait-unclassified ->
	// agent_turn_step.go batch branch default arm) records NO identity and
	// is chain progress.
	ts.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolCallID: "call-rep-1", ToolName: "bash", Input: `{"command":"gh run view","run_in_background":true}`},
		batchStartedToolResult("call-rep-1"),
	}}})
	require.True(t, ts.att.chainProgress, "the first substantial batch launch is progress")
	require.Empty(t, ts.att.chainBatchRoots, "the first substantial launch records no chain identity")

	// The byte-identical repeat: chainLastLaunch matches the recorded
	// command -> the repeat arm appends the root identity and sets no
	// progress (legacy idle-claim semantics, batch identity vocabulary).
	ts.att.chainProgress = false
	ts.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolCallID: "call-rep-2", ToolName: "bash", Input: `{"command":"gh run view","run_in_background":true}`},
		batchStartedToolResult("call-rep-2"),
	}}})
	require.False(t, ts.att.chainProgress, "the byte-identical repeat is NOT progress")
	require.Equal(t, []string{"call-rep-2"}, ts.att.chainBatchRoots,
		"the repeated launch is recorded as a batch idle claim (root identity)")

	// A DIFFERENT substantial command is a fresh launch: progress again.
	ts.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolCallID: "call-rep-3", ToolName: "bash", Input: `{"command":"gh run list","run_in_background":true}`},
		batchStartedToolResult("call-rep-3"),
	}}})
	require.True(t, ts.att.chainProgress, "a different substantial command is progress again")
	require.Equal(t, []string{"call-rep-2"}, ts.att.chainBatchRoots,
		"the fresh launch still adds no identity")
}
