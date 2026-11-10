package agent

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

const (
	customRoleDispatchName          = "analyst"
	customRoleDispatchOperator      = "operator-analyst-provider"
	customRoleDispatchOperatorModel = "operator-analyst-model"
	customRoleDispatchTenant        = "tenant-analyst-provider"
	customRoleDispatchTenantModel   = "tenant-analyst-model"
)

func customRoleCredentialDispatchFixture(t *testing.T) (*coordinator, string, *session.RoleContract) {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, true)
	cfg := coord.cfg.Config()
	cfg.Roles = make(map[string]config.CustomRole)
	cfg.Providers.Set(customRoleDispatchOperator, config.ProviderConfig{
		ID:     customRoleDispatchOperator,
		Type:   openai.Name,
		APIKey: "operator-fixture-key",
		Models: []catwalk.Model{{ID: customRoleDispatchOperatorModel, DefaultMaxTokens: 77}},
	})
	allowedTools := []string{"view"}
	cfg.Roles[customRoleDispatchName] = config.CustomRole{
		SelectedModel: config.SelectedModel{
			Provider:        customRoleDispatchOperator,
			Model:           customRoleDispatchOperatorModel,
			ReasoningEffort: "high",
			MaxTokens:       77,
		},
		Tools: &allowedTools,
	}
	contract, err := ResolveRoleContract(cfg, customRoleDispatchName, nil)
	require.NoError(t, err)
	parent, err := coord.sessions.Create(t.Context(), "custom-role credential dispatch")
	require.NoError(t, err)
	return coord, parent.ID, contract
}

func customRoleTenantCredentialSet() *CredentialSet {
	return &CredentialSet{
		Credentials: []Credential{{
			Provider: customRoleDispatchTenant,
			Type:     ProviderTypeOpenAI,
			APIKey:   "tenant-fixture-key",
			Models: []CredentialModel{
				{ID: "tenant-smart-model", DefaultMaxTokens: 111},
				{ID: "tenant-fast-model", DefaultMaxTokens: 112},
				{ID: customRoleDispatchTenantModel, DefaultMaxTokens: 213},
			},
		}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: customRoleDispatchTenant, Model: "tenant-smart-model", ReasoningEffort: "medium"},
			RoleFast:  {Provider: customRoleDispatchTenant, Model: "tenant-fast-model"},
			Role(customRoleDispatchName): {
				Provider:        customRoleDispatchTenant,
				Model:           customRoleDispatchTenantModel,
				ReasoningEffort: "low",
				MaxTokens:       213,
			},
		},
	}
}

func customRoleDispatchContext(t *testing.T, contract *session.RoleContract, creds *CredentialSet, modelRole config.SelectedModelType) (context.Context, *CallOptions) {
	t.Helper()
	opts := &CallOptions{ModelRole: modelRole, RoleContract: contract.Clone()}
	ctx := WithCallOptions(t.Context(), opts)
	t.Cleanup(opts.buildScope.close)
	ctx = WithSelectedRoleContract(ctx, contract)
	if creds != nil {
		ctx = withCallCredentials(ctx, creds)
	}
	return ctx, opts
}

func TestCustomRoleCredentialsDispatchUsesExactTenantSlotWithoutChangingPolicy(t *testing.T) {
	coord, parent, contract := customRoleCredentialDispatchFixture(t)
	creds := customRoleTenantCredentialSet()
	ctx, opts := customRoleDispatchContext(t, contract, creds, config.SelectedModelTypeSmart)

	resolved, err := coord.resolveCredentialsModels(ctx, parent, creds)
	require.NoError(t, err)
	require.Equal(t, customRoleDispatchTenant, resolved.smart.ModelCfg.Provider)
	require.Equal(t, customRoleDispatchTenantModel, resolved.smart.ModelCfg.Model)
	require.Equal(t, "low", resolved.smart.ModelCfg.ReasoningEffort)
	require.EqualValues(t, 213, resolved.smart.ModelCfg.MaxTokens)
	require.Equal(t, customRoleDispatchTenant, resolved.providerCfg.ID)
	require.Equal(t, hbSourcePerCall, resolved.smartSource)
	require.Len(t, resolved.tools, 1)
	require.Equal(t, "view", resolved.tools[0].Info().Name)

	// The tenant model changes only the model selection. It cannot rewrite
	// the caller's builtin slot or the operator-admitted tool ceiling.
	require.Equal(t, config.SelectedModelTypeSmart, opts.ModelRole)
	require.Equal(t, []string{"view"}, *opts.RoleContract.Tools)
	require.True(t, RoleContractAllowsTool(opts.RoleContract, "view"))
	require.False(t, RoleContractAllowsTool(opts.RoleContract, "bash"))
	require.False(t, coord.workerSubAgentActiveForCall(ctx, coord.cfg.Config()), "a custom role must not activate the worker cascade")
	require.False(t, resolved.orchestrator)

	selected, err := coord.resolveRequestedDelegationModel(ctx, coord.cfg.Config(), parent, customRoleDispatchName)
	require.NoError(t, err)
	require.Equal(t, customRoleDispatchTenant, selected.Model.ModelCfg.Provider)
	require.Equal(t, customRoleDispatchTenantModel, selected.Model.ModelCfg.Model)
	require.Equal(t, "low", selected.Model.ModelCfg.ReasoningEffort)
	require.EqualValues(t, 213, selected.Model.ModelCfg.MaxTokens)
	require.Equal(t, customRoleDispatchTenant, selected.Provider.ID)
	require.Equal(t, hbSourcePerCall, selected.Source)
	require.Same(t, creds, selected.Credentials)
	require.Equal(t, config.SelectedModelTypeSmart, opts.ModelRole)
	require.Equal(t, []string{"view"}, *opts.RoleContract.Tools)

	parentDepth, err := MaxAgentDepthFrom(ctx)
	require.NoError(t, err)
	var dispatched *SessionAgentCall
	child := newMockAgent(customRoleDispatchTenant, 213, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		dispatched = &call
		return agentResultWithText("tenant custom-role child"), nil
	})
	response, err := coord.runSubAgent(ctx, subAgentParams{
		Agent: child, SessionID: parent, AgentMessageID: "tenant-role-message", ToolCallID: "tenant-role-call",
		Prompt: "use the exact tenant role", SessionTitle: "tenant custom role", RequestedModelRole: customRoleDispatchName,
	})
	require.NoError(t, err)
	require.False(t, response.IsError, "%s", response.Content)
	require.NotNil(t, dispatched)
	require.Equal(t, customRoleDispatchTenant, dispatched.SmartModel.ModelCfg.Provider)
	require.Equal(t, customRoleDispatchTenantModel, dispatched.SmartModel.ModelCfg.Model)
	require.Equal(t, creds, dispatched.Credentials)
	require.Equal(t, RequestedModelRole(customRoleDispatchName), dispatched.RequestedModelRole)
	require.Empty(t, dispatched.CallOptions.ModelRole, "a custom tenant model must not activate a builtin child policy role")
	require.True(t, dispatched.CallOptions.AgentDepthSet)
	require.Equal(t, parentDepth-1, dispatched.CallOptions.MaxAgentDepth, "choosing a custom tenant model cannot reset delegation depth")
	require.True(t, contract.Equal(dispatched.RoleContract))
	require.True(t, contract.Equal(dispatched.CallOptions.RoleContract))
	require.False(t, RoleContractAllowsTool(dispatched.RoleContract, "write"))
}

func TestCustomRoleCredentialsMissingExactSlotFailsBeforeDelegationEffects(t *testing.T) {
	coord, parent, contract := customRoleCredentialDispatchFixture(t)
	creds := customRoleTenantCredentialSet()
	delete(creds.Models, Role(customRoleDispatchName))
	creds.Credentials[0].Type = ProviderType("unsupported-provider-type")
	ctx, _ := customRoleDispatchContext(t, contract, creds, config.SelectedModelTypeSmart)

	// A present tenant smart slot is not a substitute for the requested
	// custom role. Make its provider unconstructable: the exact-role error
	// must win before even attempting to construct the unrelated smart model.
	_, err := coord.resolveCredentialsModels(ctx, parent, creds)
	require.ErrorContains(t, err, `credential set does not cover requested custom role "analyst"`)
	require.NotContains(t, err.Error(), "unsupported-provider-type")
	_, err = coord.resolveRequestedDelegationModel(ctx, coord.cfg.Config(), parent, customRoleDispatchName)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	require.ErrorContains(t, err, "tenant does not cover analyst")

	before, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	var childCalls int
	child := newMockAgent(customRoleDispatchOperator, 128, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		childCalls++
		t.Errorf("delegated agent ran without the exact tenant custom-role slot")
		return agentResultWithText("unexpected"), nil
	})
	_, err = coord.runSubAgent(ctx, subAgentParams{
		Agent: child, SessionID: parent, AgentMessageID: "missing-role-message", ToolCallID: "missing-role-call",
		Prompt: "must not run", SessionTitle: "missing custom-role slot", RequestedModelRole: customRoleDispatchName,
	})
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	require.Zero(t, childCalls)
	after, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	require.Len(t, after, len(before), "a missing exact custom-role slot cannot create a child session")
}

func TestCustomRoleTrustedCredentialFallbackUsesConfiguredExactRole(t *testing.T) {
	coord, parent, contract := customRoleCredentialDispatchFixture(t)
	creds := customRoleTenantCredentialSet().Clone()
	delete(creds.Models, Role(customRoleDispatchName))
	creds.AllowConfiguredRoleFallback = true
	ctx, opts := customRoleDispatchContext(t, contract, &creds, config.SelectedModelTypeSmart)

	resolved, err := coord.resolveCredentialsModels(ctx, parent, &creds)
	require.NoError(t, err)
	require.Equal(t, customRoleDispatchOperator, resolved.smart.ModelCfg.Provider)
	require.Equal(t, customRoleDispatchOperatorModel, resolved.smart.ModelCfg.Model)
	require.Equal(t, "high", resolved.smart.ModelCfg.ReasoningEffort)
	require.NotEqual(t, creds.Models[RoleSmart].Model, resolved.smart.ModelCfg.Model)
	require.Equal(t, customRoleDispatchOperator, resolved.providerCfg.ID)
	require.Equal(t, []string{"view"}, *opts.RoleContract.Tools)

	selected, err := coord.resolveRequestedDelegationModel(ctx, coord.cfg.Config(), parent, customRoleDispatchName)
	require.NoError(t, err)
	require.Equal(t, customRoleDispatchOperator, selected.Model.ModelCfg.Provider)
	require.Equal(t, customRoleDispatchOperatorModel, selected.Model.ModelCfg.Model)
	require.Equal(t, "high", selected.Model.ModelCfg.ReasoningEffort)
	require.NotEqual(t, creds.Models[RoleSmart].Model, selected.Model.ModelCfg.Model)
}

func TestCustomRoleCredentialBoundaryFailsClosedOnContinuationWithoutBundle(t *testing.T) {
	coord, parent, contract := customRoleCredentialDispatchFixture(t)
	coord.markCredentialedRoot(parent)
	ctx, _ := customRoleDispatchContext(t, contract, nil, config.SelectedModelTypeSmart)

	before, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	var childCalls int
	child := newMockAgent(customRoleDispatchOperator, 128, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		childCalls++
		t.Errorf("credential-bound custom-role continuation ran without its credential bundle")
		return agentResultWithText("unexpected"), nil
	})
	_, err = coord.runSubAgent(ctx, subAgentParams{
		Agent: child, SessionID: parent, AgentMessageID: "boundary-message", ToolCallID: "boundary-call",
		Prompt: "must stay on the tenant boundary", SessionTitle: "credential-bound custom role", RequestedModelRole: customRoleDispatchName,
	})
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)
	require.Zero(t, childCalls)
	after, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	require.Len(t, after, len(before), "a credential-bound continuation cannot create an operator-backed child")
}
