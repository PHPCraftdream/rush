// R12-E round 4 reaction-chain identity from BATCH evidence: the collector
// records the launched root's tool_call_id from the REAL production batch
// started response (the identity carrier -- batch started results carry no
// ClientMetadata, stated choice), the arbiter's chain accounting keys on
// those roots, and readTurnFactsBatch's rule-5 predicate defers ONLY while
// every pending node-debt row is a completion of a root the chain launched.
// COMPILE-ONLY IN THIS WORKTREE.
package agent

import (
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchStartedToolResult builds the ToolResultContent the production batch
// ingress returns for a started detached root: a plain text result in the
// REAL batchStartedResponse format, no ClientMetadata.
func batchStartedToolResult(callID string) fantasy.ToolResultContent {
	resp := batchStartedResponse(jobbatch.ID(callID))
	return fantasy.ToolResultContent{
		ToolCallID: callID,
		Result:     fantasy.ToolResultOutputContentText{Text: resp.Content},
	}
}

func TestBatchChainEvidenceCollectorAndGuardON(t *testing.T) {
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// A real batch launch the fixture submitted through the batch ingress
	// (wait-class leaf), settled with its completion held as node debt.
	env.invokeDetached("call-chain-b", "view", fast)
	rootID := env.rootID("call-chain-b")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "the settled launch is node debt")

	// Collector, fed directly (no full provider turn needed: the collector
	// is a pure step-result fold): a WAIT-class batch launch (bash sleep,
	// classified exactly like a legacy wait) records the launched root's
	// call id as chain identity and is not progress.
	ts := &turnStream{att: &drainAttempt{}}
	ts.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolCallID: "call-chain-b", ToolName: "bash", Input: `{"command":"sleep 90"}`},
		batchStartedToolResult("call-chain-b"),
	}}})
	require.Equal(t, []string{"call-chain-b"}, ts.att.chainBatchRoots,
		"the started response's call id is the recorded chain identity")
	require.False(t, ts.att.chainProgress, "a pure-wait batch launch is not progress")

	// A REAL batch action (substantial command, wait-unclassified) is
	// progress and records no identity.
	tsAct := &turnStream{att: &drainAttempt{}}
	tsAct.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolCallID: "call-chain-act", ToolName: "bash", Input: `{"command":"gh run view","run_in_background":true}`},
		batchStartedToolResult("call-chain-act"),
	}}})
	require.Empty(t, tsAct.att.chainBatchRoots, "a real batch action records no chain identity")
	require.True(t, tsAct.att.chainProgress, "a real batch action is progress")

	// Guard: three link legs with the REAL debt rows whose root
	// tool_call_ids match the recorded identity -> rule 5 defers.
	debtRows, err := env.store.BatchReactionPendingInclusiveDebtRows(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, debtRows.Nodes, 1)
	att := &drainAttempt{
		chainBatchRoots:  ts.att.chainBatchRoots,
		batchSnapshot:    session.BatchDebtSnapshot{Owner: env.sessionID, Nodes: debtRows.Nodes},
		batchDebtCallIDs: []string{"call-chain-b"},
	}
	for i := 0; i < 3; i++ {
		// The legacy snapshot half is empty: the batch branch keys on
		// att.chainBatchRoots/att.batchDebtCallIDs, not on legacy claims.
		coord.arb.chainLink(env.sessionID, att, session.BatchDebtSnapshot{})
	}
	require.GreaterOrEqual(t, coord.arb.chainLinksOf(env.sessionID), 3)
	pv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, reactionChainReason, pv.reason,
		"the chain guard defers on the batch debt the chain itself launched")
	t.Logf("BatchChain defer reason=%q", pv.reason)

	// An UNRELATED settled root (a tool_call_id the chain never launched)
	// joins the debt rows: the ownership predicate fails and the defer
	// DISAPPEARS.
	env.invokeDetached("call-chain-unrel", "view", fast)
	unrelID := env.rootID("call-chain-unrel")
	require.Equal(t, "completed", env.rootTerminal(unrelID))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "the unrelated root's debt committed")
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.NotEqual(t, reactionChainReason, pv.reason,
		"an unrelated completion is not chain-owned: no defer")
	env.logDebtRows(t)
	env.logNodeRows(t, rootID, unrelID)
}
