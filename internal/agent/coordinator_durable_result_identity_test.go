package agent

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestRunSessionAgentCallRecordsExecutedDurableIdentity(t *testing.T) {
	const providerID = "test-durable-result-identity"
	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{{
			ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096,
		}},
	})
	sess, err := env.sessions.Create(t.Context(), "durable-result-identity")
	require.NoError(t, err)
	var executedID string
	coord.currentAgent = newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		row, createErr := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "durable continuation completed"},
			},
		})
		require.NoError(t, createErr)
		row.AddFinish(message.FinishReasonEndTurn, "", "")
		require.NoError(t, env.messages.Update(t.Context(), row))
		require.NotNil(t, call.OnAssistantMessageCreated)
		call.OnAssistantMessageCreated(row.ID)
		executedID = row.ID
		return agentResultWithText("durable continuation completed"), nil
	})

	recorder := NewCallResultRecorder()
	_, err = coord.RunSessionAgentCall(WithCallResultRecorder(t.Context(), recorder), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "continue",
	})
	require.NoError(t, err)
	require.NotEmpty(t, executedID)
	require.Equal(t, executedID, recorder.Resolve())
	require.True(t, recorder.Owns(executedID))
}
