package agent

import (
	"context"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestExplicitDelegationRolesUseOnlyTheirTenantSlot(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	coord.markCredentialedRoot(parent)
	creds := &CredentialSet{
		Credentials: []Credential{{Provider: "tenant-role", Type: ProviderTypeOpenAI, APIKey: "tenant-fixture-key"}},
		Models:      map[Role]ModelChoice{},
	}
	for _, role := range []Role{RoleSmart, RoleFast, RoleWorker, RoleReviewer} {
		creds.Models[role] = ModelChoice{Provider: "tenant-role", Model: "tenant-" + string(role), MaxTokens: 77}
	}
	var observed SessionAgentCall
	child := newMockAgent("role-fixture", 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		observed = call
		return agentResultWithText("tenant result"), nil
	})
	for i, role := range []RequestedModelRole{"smart", "fast", "worker", "reviewer"} {
		_, err := coord.runSubAgent(withCallCredentials(t.Context(), creds), subAgentParams{
			Agent: child, SessionID: parent, AgentMessageID: "tenant-message", ToolCallID: fmt.Sprintf("tenant-call-%d", i),
			Prompt: "p", SessionTitle: "tenant role", RequestedModelRole: role, slotModel: true,
		})
		require.NoError(t, err)
		require.Equal(t, "tenant-role", observed.SmartModel.ModelCfg.Provider)
		require.Equal(t, "tenant-"+string(role), observed.SmartModel.ModelCfg.Model)
		require.EqualValues(t, 77, observed.MaxOutputTokens)
		require.Equal(t, hbSourcePerCall, observed.RequestedModelSource)
		require.Same(t, creds, observed.Credentials)
	}
}

func TestExplicitTenantRoleRequiresTrustedConfiguredFallbackFlag(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	creds := &CredentialSet{
		Credentials: []Credential{{Provider: "tenant-role", Type: ProviderTypeOpenAI, APIKey: "tenant-fixture-key"}},
		Models:      map[Role]ModelChoice{RoleSmart: {Provider: "tenant-role", Model: "tenant-smart"}},
	}
	var observed SessionAgentCall
	child := newMockAgent("role-fixture", 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		observed = call
		return agentResultWithText("allowed fallback"), nil
	})
	params := subAgentParams{Agent: child, SessionID: parent, AgentMessageID: "m", ToolCallID: "c", Prompt: "p", SessionTitle: "role", RequestedModelRole: "reviewer"}
	before, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	_, err = coord.runSubAgent(withCallCredentials(t.Context(), creds), params)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	after, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	require.Len(t, after, len(before), "missing tenant slots cannot create children or silently use operator providers")
	allowed := creds.Clone()
	allowed.AllowConfiguredRoleFallback = true
	_, err = coord.runSubAgent(withCallCredentials(t.Context(), &allowed), params)
	require.NoError(t, err)
	require.Equal(t, "role-fixture", observed.SmartModel.ModelCfg.Provider)
	require.Equal(t, "reviewer-model", observed.SmartModel.ModelCfg.Model)
	require.Equal(t, hbSourceSlotLive, observed.RequestedModelSource)
	require.False(t, creds.AllowConfiguredRoleFallback, "the original credential contract stays immutable")
}

func TestExplicitRoleCannotRebindCredentialedParentWithoutBundle(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	coord.markCredentialedRoot(parent)
	before, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	child := newMockAgent("role-fixture", 128, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("unexpected operator invocation"), nil
	})
	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent: child, SessionID: parent, AgentMessageID: "m", ToolCallID: "c", Prompt: "p", SessionTitle: "refused", RequestedModelRole: "smart",
	})
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)
	after, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	require.Len(t, after, len(before), "a missing tenant bundle must fail before a child can select operator credentials")
}
