package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestReactionChain_RepeatedAsyncCommandAcceptance(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "reaction-chain-repeat-accept", attemptFixtureOpts{noIdle: true, noDriver: true})
	ts := &turnStream{a: &sessionAgent{asyncJobs: f.coord.asyncJobs}, call: SessionAgentCall{SessionID: f.sessID}}
	const command = `cd /x && git log --oneline -1 >/dev/null; echo q`
	launch := func(id, cmd string) *drainAttempt {
		f.seedDebt(ctx, id, true)
		row := f.row(ctx, id)
		call := fantasy.ToolCallContent{ToolCallID: id, ToolName: "bash", Input: `{"command":` + reactionJSON(cmd) + `}`}
		result := fantasy.ToolResultContent{ToolCallID: id, ClientMetadata: `{"async":true,"claim_id":"` + row.ClaimID + `"}`}
		ts.att = &drainAttempt{sessionID: f.sessID, outcome: drainAttempted}
		ts.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{call, result}}})
		return ts.att
	}
	first := launch("accept1", command)
	require.True(t, first.chainProgress)
	require.Empty(t, first.chainIdleClaims)
	f.coord.arb.chainLink(f.sessID, first, session.DebtSnapshot{})
	require.Zero(t, f.chainCount())
	previous := "accept1"
	for _, id := range []string{"accept2", "accept3", "accept4"} {
		att := launch(id, command)
		require.False(t, att.chainProgress)
		require.Equal(t, []string{f.row(ctx, id).ClaimID}, att.chainIdleClaims)
		current := id
		f.coord.arb.chainLink(f.sessID, att, session.DebtSnapshot{Jobs: []session.DebtJobRef{{ClaimID: f.row(ctx, previous).ClaimID}}})
		f.exec(ctx, `UPDATE async_jobs SET reacted=1 WHERE owner_session_id='`+f.sessID+`' AND tool_call_id='`+previous+`'`)
		previous = current
	}
	require.Equal(t, drainDeferred, f.coord.drainPermitted(ctx, f.sessID, false).kind)
	require.Equal(t, reactionChainReason, f.coord.drainPermitted(ctx, f.sessID, false).reason)
}

func TestReactionChain_CommandIdentityAndProgressReset(t *testing.T) {
	f := newAttemptFixture(t, "reaction-chain-command-identity", attemptFixtureOpts{noIdle: true, noDriver: true})
	ts := &turnStream{a: &sessionAgent{asyncJobs: f.coord.asyncJobs}, call: SessionAgentCall{SessionID: f.sessID}}
	step := func(id, name, input string) {
		call := fantasy.ToolCallContent{ToolCallID: id, ToolName: name, Input: input}
		result := fantasy.ToolResultContent{ToolCallID: id, ClientMetadata: `{"async":true,"claim_id":"claim-` + id + `"}`}
		ts.att = &drainAttempt{}
		ts.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{call, result}}})
	}
	step("go-build", "run_command", `{"program":"go","args":["build"]}`)
	require.True(t, ts.att.chainProgress)
	require.Empty(t, ts.att.chainIdleClaims)
	step("go-test", "run_command", `{"program":"go","args":["test"]}`)
	require.True(t, ts.att.chainProgress)
	require.Empty(t, ts.att.chainIdleClaims)
	command := `{"command":"cd /x && git log --oneline -1 >/dev/null; echo q"}`
	step("repeat1", "bash", command)
	require.True(t, ts.att.chainProgress)
	require.Empty(t, ts.att.chainIdleClaims)
	step("repeat2", "bash", command)
	require.False(t, ts.att.chainProgress)
	require.Len(t, ts.att.chainIdleClaims, 1)
	for _, tool := range []string{"edit", "write", "view"} {
		step("real-"+tool, tool, `{}`)
		require.True(t, ts.att.chainProgress)
	}
	step("after-real", "bash", command)
	require.True(t, ts.att.chainProgress, "real action clears launch identity")
	f.coord.arb.resetChain(f.sessID)
	step("after-human-reset", "bash", command)
	require.True(t, ts.att.chainProgress, "explicit reset clears launch identity")
}

func reactionJSON(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}
