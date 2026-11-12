package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func requestedRoleCoordinator(t *testing.T) (*coordinator, string) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	env := testEnv(t)
	models := []catwalk.Model{}
	for _, role := range []string{"smart", "fast", "worker", "reviewer"} {
		models = append(models, catwalk.Model{ID: role + "-model", Name: role, ContextWindow: 8192, DefaultMaxTokens: 128})
	}
	coord := newTestCoordinator(t, env, "role-fixture", config.ProviderConfig{
		ID: "role-fixture", Type: openaicompat.Name, APIKey: "fixture",
		BaseURL: "https://example.invalid/v1", Models: models,
	})
	for i, role := range []config.SelectedModelType{config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer} {
		coord.cfg.Config().Models[role] = config.SelectedModel{Provider: "role-fixture", Model: string(role) + "-model", MaxTokens: int64(100 + i)}
	}
	parent, err := env.sessions.Create(t.Context(), "requested-role parent")
	require.NoError(t, err)
	return coord, parent.ID
}

func TestRequestedRoleDispatchChangesModelNotCallPolicy(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	var calls []SessionAgentCall
	child := newMockAgent("role-fixture", 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls = append(calls, call)
		return agentResultWithText("role result"), nil
	})
	parentOptions := &CallOptions{ModelRole: config.SelectedModelTypeSmart, DisableSubAgents: true, MaxAgentDepth: 2, AgentDepthSet: true, MaxCost: 9}
	ctx := WithCallOptions(t.Context(), parentOptions)
	for i, role := range []RequestedModelRole{"smart", "fast", "worker", "reviewer"} {
		response, err := coord.runSubAgent(ctx, subAgentParams{
			Agent: child, SessionID: parent, AgentMessageID: "role-message", ToolCallID: fmt.Sprintf("role-%d", i),
			Prompt: "p", SessionTitle: "role child", RequestedModelRole: role, slotModel: true,
		})
		require.NoError(t, err)
		require.False(t, response.IsError)
		got := calls[len(calls)-1]
		require.Equal(t, role, got.RequestedModelRole)
		require.Equal(t, string(role)+"-model", got.SmartModel.ModelCfg.Model)
		require.Equal(t, hbSourceSlotLive, got.RequestedModelSource)
		require.EqualValues(t, 100+i, got.MaxOutputTokens)
		require.Equal(t, 1, got.CallOptions.MaxAgentDepth)
		if i > 0 {
			require.Equal(t, calls[0].CallOptions.ModelRole, got.CallOptions.ModelRole)
			require.Equal(t, calls[0].CallOptions.DisableSubAgents, got.CallOptions.DisableSubAgents)
			require.Equal(t, calls[0].CallOptions.AllowPeakHours, got.CallOptions.AllowPeakHours)
			require.Equal(t, calls[0].CallOptions.MaxCost, got.CallOptions.MaxCost)
			require.Equal(t, calls[0].Tools, got.Tools)
		}
	}
	require.Equal(t, config.SelectedModelTypeSmart, parentOptions.ModelRole)
	require.True(t, parentOptions.DisableSubAgents)
	require.Equal(t, 2, parentOptions.MaxAgentDepth)
	require.EqualValues(t, 9, parentOptions.MaxCost)
}

func TestRequestedRoleResumeInheritsContractAndRejectsReplacement(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	var childID string
	var calls atomic.Int32
	child := newMockAgent("role-fixture", 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls.Add(1)
		childID = call.SessionID
		require.Equal(t, RequestedModelRole("reviewer"), call.RequestedModelRole)
		require.Equal(t, "reviewer-model", call.SmartModel.ModelCfg.Model)
		return agentResultWithText("review result"), nil
	})
	params := subAgentParams{Agent: child, SessionID: parent, AgentMessageID: "m", ToolCallID: "c", Prompt: "p", SessionTitle: "review", RequestedModelRole: "reviewer", slotModel: true}
	_, err := coord.runSubAgent(t.Context(), params)
	require.NoError(t, err)
	params.ResumeSessionID = childID
	params.RequestedModelRole = ""
	_, err = coord.runSubAgent(t.Context(), params)
	require.NoError(t, err, "omitted role on resume retains the immutable explicit role")
	params.RequestedModelRole = "smart"
	_, err = coord.runSubAgent(t.Context(), params)
	require.ErrorIs(t, err, ErrAgentRoleMismatch)
	require.EqualValues(t, 2, calls.Load(), "a mismatched resume must not drive another turn")
}

func TestBuiltinRoleResumeUsesSlotModelUnderCustomParentCeiling(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	ceiling := []string{"view"}
	parentContract := &session.RoleContract{
		Version: session.RoleContractVersion, Name: "analyst",
		Model: session.ModelCfg{Provider: "role-fixture", Model: "smart-model"}, Tools: &ceiling,
	}
	require.NoError(t, parentContract.Validate())
	ctx := WithCallRoleContract(t.Context(), parentContract)
	var calls []SessionAgentCall
	child := newMockAgent("role-fixture", 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls = append(calls, call)
		return agentResultWithText("review result"), nil
	})
	params := subAgentParams{Agent: child, SessionID: parent, AgentMessageID: "m", ToolCallID: "c", Prompt: "p", SessionTitle: "review", RequestedModelRole: "reviewer", slotModel: true}
	_, err := coord.runSubAgent(ctx, params)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	childID := calls[0].SessionID

	for _, role := range []RequestedModelRole{"reviewer", ""} {
		params.ResumeSessionID = childID
		params.RequestedModelRole = role
		response, err := coord.runSubAgent(ctx, params)
		require.NoError(t, err, "resume with role %q must not require a custom role entry", role)
		require.False(t, response.IsError)
	}
	require.Len(t, calls, 3)
	stored, found, err := coord.sessions.(session.SessionRoleContractStore).GetSessionRoleContract(t.Context(), childID)
	require.NoError(t, err)
	require.True(t, found)
	for i, got := range calls {
		require.Equal(t, RequestedModelRole("reviewer"), got.RequestedModelRole, "call %d", i)
		require.Equal(t, "reviewer-model", got.SmartModel.ModelCfg.Model, "call %d", i)
		require.Equal(t, hbSourceSlotLive, got.RequestedModelSource, "call %d", i)
		require.EqualValues(t, 103, got.MaxOutputTokens, "call %d", i)
		require.True(t, stored.Equal(got.RoleContract), "call %d keeps the stored contract", i)
		require.Equal(t, "reviewer", got.RoleContract.Name, "call %d", i)
		require.True(t, roleContractAllowsName(got.RoleContract, "view"), "call %d", i)
		require.False(t, roleContractAllowsName(got.RoleContract, "bash"), "call %d tools stay capped", i)
		require.False(t, roleContractAllowsName(got.CallOptions.RoleContract, "bash"), "call %d", i)
	}
}

func TestUnconfiguredExplicitRoleRefusesBeforeChildCreation(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	delete(coord.cfg.Config().Models, config.SelectedModelTypeReviewer)
	before, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	var executions atomic.Int32
	child := newMockAgent("role-fixture", 128, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		executions.Add(1)
		return agentResultWithText("unexpected model invocation"), nil
	})
	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent: child, SessionID: parent, AgentMessageID: "m", ToolCallID: "c", Prompt: "p", SessionTitle: "refused", RequestedModelRole: "reviewer",
	})
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	require.Zero(t, executions.Load(), "unconfigured explicit role cannot reach the model")
	after, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	require.Len(t, after, len(before))
}
