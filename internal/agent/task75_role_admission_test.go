package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestTask75ConfiguredAutoAndUnknownRolesDispatchAsCustomRoles(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	coord, parent := requestedRoleCoordinator(t)
	if coord.cfg.Config().Roles == nil {
		coord.cfg.Config().Roles = make(map[string]config.CustomRole)
	}
	coord.cfg.Config().Roles["auto"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "role-fixture", Model: "fast-model"}}
	coord.cfg.Config().Roles["unknown"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "role-fixture", Model: "reviewer-model"}}
	var calls atomic.Int32
	child := newMockAgent("role-fixture", 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls.Add(1)
		require.NotNil(t, call.CallOptions)
		require.NotNil(t, call.CallOptions.RoleContract)
		require.Equal(t, string(call.RequestedModelRole), call.CallOptions.RoleContract.Name)
		switch call.RequestedModelRole {
		case "auto":
			require.Equal(t, "fast-model", call.SmartModel.ModelCfg.Model)
		case "unknown":
			require.Equal(t, "reviewer-model", call.SmartModel.ModelCfg.Model)
		default:
			t.Errorf("unexpected custom role %q", call.RequestedModelRole)
		}
		return agentResultWithText("ok"), nil
	})

	for i, role := range []RequestedModelRole{"auto", "unknown"} {
		response, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent: child, SessionID: parent, AgentMessageID: "task75-configured", ToolCallID: string(rune('a' + i)),
			Prompt: "p", SessionTitle: "configured custom role", RequestedModelRole: role,
		})
		require.NoError(t, err)
		require.False(t, response.IsError)
	}
	require.EqualValues(t, 2, calls.Load())
}

func TestTask75UnknownCustomRoleRefusesBeforeDelegationEffects(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	coord, parent := requestedRoleCoordinator(t)
	if coord.cfg.Config().Roles == nil {
		coord.cfg.Config().Roles = make(map[string]config.CustomRole)
	}
	coord.cfg.Config().Roles["broken"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "role-fixture"}}
	before, err := coord.sessions.ListAll(t.Context())
	require.NoError(t, err)
	var calls atomic.Int32
	child := newMockAgent("role-fixture", 128, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		calls.Add(1)
		return agentResultWithText("unexpected"), nil
	})
	for _, role := range []RequestedModelRole{"unknown", "broken"} {
		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent: child, SessionID: parent, AgentMessageID: "task75-missing", ToolCallID: string(role),
			Prompt: "must not run", SessionTitle: "unconfigured custom role", RequestedModelRole: role,
		})
		require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
		require.Zero(t, calls.Load(), "unconfigured custom role must not invoke a child/provider")
		after, err := coord.sessions.ListAll(t.Context())
		require.NoError(t, err)
		require.Len(t, after, len(before), "custom-role admission must precede child-session creation")
	}
}

func TestTask75UnknownCustomRoleRefusesAtResolverBeforeModelConstruction(t *testing.T) {
	cfg := &config.Config{Roles: map[string]config.CustomRole{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	coord := &coordinator{}
	_, err := coord.resolveRequestedDelegationModel(t.Context(), cfg, "", "unknown")
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
}

func TestTask75UnknownCustomRoleRefusesAtBatchAdmissionBeforeEffects(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("p", config.ProviderConfig{ID: "p"})
	cfg := &config.Config{Providers: providers, Roles: map[string]config.CustomRole{
		"broken": {SelectedModel: config.SelectedModel{Provider: "absent", Model: "m"}},
	}}
	ctx := WithParentRoleConfigSnapshot(t.Context(), cfg)
	for _, role := range []string{"unknown", "broken"} {
		t.Run(role, func(t *testing.T) {
			rawInput := json.RawMessage(`{"prompt":"must not run","role":"` + role + `"}`)
			// Syntax parsing deliberately accepts any valid custom name; only
			// the configured admission boundary knows whether it is defined.
			_, err := (BatchExternalSpec{Tool: AgentToolName, Input: rawInput}).Spec()
			require.NoError(t, err)
			spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{
				batchRoleAdmissionTask(rawInput),
			}}}
			_, err = AdmitBatchRoleContracts(ctx, nil, spec)
			require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
		})
	}
}

func TestTask75DurableCustomRoleWithoutContractFailsClosed(t *testing.T) {
	data := session.SessionAgentCallData{RequestedModelRole: "unknown"}
	_, err := FromSessionAgentCallData(data)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
}

func TestTask75RoleContractResolverAllowsConfiguredMagicLikeNamesOnly(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("p", config.ProviderConfig{ID: "p"})
	cfg := &config.Config{
		Providers: providers,
		Roles: map[string]config.CustomRole{
			"auto":    {SelectedModel: config.SelectedModel{Provider: "p", Model: "automatic"}},
			"unknown": {SelectedModel: config.SelectedModel{Provider: "p", Model: "unknown-model"}},
		},
	}
	for _, name := range []string{"auto", "unknown"} {
		contract, err := ResolveRoleContract(cfg, name, nil)
		require.NoError(t, err)
		require.Equal(t, name, contract.Name)
	}
	_, err := ResolveRoleContract(cfg, "missing", nil)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	cfg.Roles["broken"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "p"}}
	_, err = ResolveRoleContract(cfg, "broken", nil)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
}
