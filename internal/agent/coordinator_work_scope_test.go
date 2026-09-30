// refreshSubAgentCompletion's "no final text" wording (doc sec.3.8, step 6,
// "Итог делегации"): a child whose last finished turn produced no text at
// all (only reasoning and/or tool calls) must not resurface the stale text
// captured when the delegation was parked.
package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// writeParkedChildTurnNoText mirrors work_ledger_delegation_test.go's
// writeParkedChildTurn, but the finished assistant message carries a tool
// call and no text content at all -- the exact shape refreshSubAgentCompletion
// must special-case.
func writeParkedChildTurnNoText(t *testing.T, env fakeEnv, sessionID string, reason message.FinishReason) {
	t.Helper()
	row, err := env.messages.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "tc-1", Name: "bash", Input: `{"command":"echo hi"}`, Finished: true},
		},
	})
	require.NoError(t, err)
	row.AddFinish(reason, "", "")
	require.NoError(t, env.messages.Update(t.Context(), row))
}

// TestRefreshSubAgentCompletion_NoFinalTextUsesFixedWording pins doc
// sec.3.8: the parent gets "завершено без итогового ответа" instead of the
// text captured when the delegation was parked (arm time), when the
// child's last finished turn has no text at all.
func TestRefreshSubAgentCompletion_NoFinalTextUsesFixedWording(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "no-final-text child")
	require.NoError(t, err)
	writeParkedChildTurnNoText(t, env, child.ID, message.FinishReasonEndTurn)

	coord := &coordinator{}
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord
	coord.messages = env.messages

	captured := AsyncCompletion{Content: "child yielded: gate still running", ToolCallID: "parent-call"}
	got := coord.refreshSubAgentCompletion(child.ID, captured)

	require.Equal(t, "завершено без итогового ответа", got.Content)
	require.NotContains(t, got.Content, "gate still running", "the stale arm-time text must never resurface")
	require.False(t, got.IsError, "an end_turn finish is not an error, even with no text")
}

// TestRefreshSubAgentCompletion_TextStillWins is the control: a child whose
// last finished turn DOES have text keeps using that text, unaffected by
// the no-text special case above.
func TestRefreshSubAgentCompletion_TextStillWins(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "final-text child")
	require.NoError(t, err)
	writeParkedChildTurn(t, env, child.ID, "the real final answer", message.FinishReasonEndTurn)

	coord := &coordinator{}
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord
	coord.messages = env.messages

	got := coord.refreshSubAgentCompletion(child.ID, AsyncCompletion{Content: "stale"})
	require.Equal(t, "the real final answer", got.Content)
}
