package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRequestedRoleContinuationRefreshesModelWithoutChangingAuthority(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	child, err := coord.sessions.CreateTaskSession(t.Context(), "requested-role-continuation", parent, "worker child")
	require.NoError(t, err)
	store := coord.sessions.(session.AgentModelRoleStore)
	require.NoError(t, store.BindAgentModelRole(t.Context(), child.ID, "worker"))
	opts := &CallOptions{ModelRole: config.SelectedModelTypeFast, DisableSubAgents: true, MaxAgentDepth: 1, AgentDepthSet: true, MaxCost: 7}
	ctx := WithCallOptions(t.Context(), opts)
	t.Cleanup(opts.buildScope.close)
	before := *opts
	taskPrompt, prefix := "unchanged task prompt", "unchanged prefix"
	original := SessionAgentCall{
		SessionID: child.ID, RequestedModelRole: "worker", RequestedModelSource: "old-source",
		CallOptions: opts, Tools: batchTestTools("view"), SystemPrompt: &taskPrompt, SystemPromptPrefix: &prefix, MaxCost: 7,
	}
	got, err := coord.refreshRequestedDelegationCall(ctx, parent, original)
	require.NoError(t, err)
	require.Equal(t, "worker-model", got.SmartModel.ModelCfg.Model)
	require.Equal(t, RequestedModelRole("worker"), got.RequestedModelRole)
	require.Equal(t, hbSourceSlotLive, got.RequestedModelSource)
	require.Same(t, opts, got.CallOptions)
	require.Equal(t, before, *opts)
	require.Equal(t, original.Tools, got.Tools)
	require.Equal(t, original.SystemPrompt, got.SystemPrompt)
	require.Equal(t, original.SystemPromptPrefix, got.SystemPromptPrefix)
	require.Equal(t, original.MaxCost, got.MaxCost)
	wake := newDrainCall((subAgentDriver{call: got}).callFor("reaction"))
	require.Equal(t, got.RequestedModelRole, wake.RequestedModelRole)
	require.Equal(t, got.RequestedModelSource, wake.RequestedModelSource)
	require.Equal(t, before.ModelRole, wake.CallOptions.ModelRole)
}

func TestRequestedRoleQueueRestoresSavedContractAndRefusesMismatch(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	child, err := coord.sessions.CreateTaskSession(t.Context(), "requested-role-queue", parent, "review child")
	require.NoError(t, err)
	store := coord.sessions.(session.AgentModelRoleStore)
	require.NoError(t, store.BindAgentModelRole(t.Context(), child.ID, "reviewer"))
	got, err := coord.restoreRequestedDelegationCall(t.Context(), SessionAgentCall{SessionID: child.ID})
	require.NoError(t, err)
	require.Equal(t, RequestedModelRole("reviewer"), got.RequestedModelRole)
	require.Equal(t, "reviewer-model", got.SmartModel.ModelCfg.Model)
	_, err = coord.restoreRequestedDelegationCall(t.Context(), SessionAgentCall{SessionID: child.ID, RequestedModelRole: "worker"})
	require.ErrorIs(t, err, ErrAgentRoleMismatch)
	require.ErrorIs(t, coord.refuseLostRequestedRoleDriver(t.Context(), child.ID), ErrAgentRoleUnconfigured, "a lost task driver cannot silently become a coder on operator smart")
	require.NoError(t, coord.refuseLostRequestedRoleDriver(t.Context(), parent), "ordinary root fallback is unchanged")
}
