package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCustomRoleChildResumeRefreshesModelAndPreservesDurableTools(t *testing.T) {
	coord, parent, contract := customRoleCredentialDispatchFixture(t)
	ctx, _ := customRoleDispatchContext(t, contract, nil, config.SelectedModelTypeSmart)
	var calls []SessionAgentCall
	child := newMockAgent(customRoleDispatchOperator, 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls = append(calls, call)
		return agentResultWithText("custom-role dispatch completed"), nil
	})
	t.Cleanup(func() {
		if t.Failed() {
			for i, call := range calls {
				t.Logf("dispatch %d: session=%s model=%+v source=%s role=%s contract=%+v", i,
					call.SessionID, call.SmartModel.ModelCfg, call.RequestedModelSource, call.RequestedModelRole, call.RoleContract)
			}
		}
	})
	response, err := coord.runSubAgent(ctx, subAgentParams{
		Agent: child, SessionID: parent, AgentMessageID: "refresh-message-1", ToolCallID: "refresh-call-1",
		Prompt: "first request", SessionTitle: "custom-role model refresh", RequestedModelRole: customRoleDispatchName,
	})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Len(t, calls, 1)
	require.Equal(t, customRoleDispatchOperatorModel, calls[0].SmartModel.ModelCfg.Model)
	childID := calls[0].SessionID
	store := coord.sessions.(session.SessionRoleContractStore)
	bound, found, err := store.GetSessionRoleContract(t.Context(), childID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, contract.Equal(bound))

	cfg := coord.cfg.Config()
	changed := cfg.Roles[customRoleDispatchName]
	changed.Model = "refreshed-analyst-model"
	changed.ReasoningEffort = "low"
	changed.MaxTokens = 91
	changed.Tools = nil
	cfg.Roles[customRoleDispatchName] = changed

	// Omit both the role and the admission context: resume must load its durable binding.
	resumeCtx := WithCallOptions(t.Context(), &CallOptions{})
	response, err = coord.runSubAgent(resumeCtx, subAgentParams{
		Agent: child, SessionID: parent, ResumeSessionID: childID,
		AgentMessageID: "refresh-message-2", ToolCallID: "refresh-call-2", Prompt: "second request",
	})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Len(t, calls, 2)
	resumed := calls[1]
	require.Equal(t, childID, resumed.SessionID)
	require.Equal(t, RequestedModelRole(customRoleDispatchName), resumed.RequestedModelRole)
	require.Equal(t, "refreshed-analyst-model", resumed.SmartModel.ModelCfg.Model)
	require.Equal(t, "low", resumed.SmartModel.ModelCfg.ReasoningEffort)
	require.EqualValues(t, 91, resumed.SmartModel.ModelCfg.MaxTokens)
	require.Equal(t, hbSourceSlotLive, resumed.RequestedModelSource)
	require.NotNil(t, resumed.CallOptions.RoleContract)
	require.True(t, contract.Equal(resumed.CallOptions.RoleContract))
	require.True(t, RoleContractAllowsTool(resumed.CallOptions.RoleContract, "view"))
	require.False(t, RoleContractAllowsTool(resumed.CallOptions.RoleContract, "bash"))
	persisted, found, err := store.GetSessionRoleContract(t.Context(), childID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, bound.Equal(persisted), "model refresh cannot rewrite immutable durable identity")

	delete(cfg.Roles, customRoleDispatchName)
	_, err = coord.runSubAgent(WithCallOptions(t.Context(), &CallOptions{}), subAgentParams{
		Agent: child, SessionID: parent, ResumeSessionID: childID,
		AgentMessageID: "refresh-message-3", ToolCallID: "refresh-call-3", Prompt: "removed role",
	})
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	require.Len(t, calls, 2, "a removed role must fail before child execution")
}
