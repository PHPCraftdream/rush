package modelkit

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

func customRoleResolverConfig() *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("p", config.ProviderConfig{ID: "p"})
	empty := []string{}
	return &config.Config{
		Providers: providers,
		Roles: map[string]config.CustomRole{
			"analyst": {SelectedModel: config.SelectedModel{Provider: "p", Model: "m", ReasoningEffort: "high", Think: true, MaxTokens: 77}, Tools: &empty},
		},
	}
}

func TestResolveRoleContractSnapshotsExactCustomDefinition(t *testing.T) {
	cfg := customRoleResolverConfig()
	resolved, err := ResolveRoleContract(cfg, "analyst", nil)
	require.NoError(t, err)
	require.NoError(t, resolved.Validate())
	require.Equal(t, session.RoleContractVersion, resolved.Version)
	require.Equal(t, "analyst", resolved.Name)
	require.Equal(t, "p", resolved.Model.Provider)
	require.Equal(t, "m", resolved.Model.Model)
	require.Equal(t, "high", resolved.Model.ReasoningEffort)
	require.True(t, resolved.Model.Think)
	require.EqualValues(t, 77, resolved.Model.MaxTokens)
	require.NotNil(t, resolved.Tools)
	require.Empty(t, *resolved.Tools)
	cfg.Roles["analyst"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "p", Model: "changed"}}
	require.Equal(t, "m", resolved.Model.Model, "admission snapshot must not alias later config edits")
}

func TestResolveRoleContractRejectsBeforeAnyFallback(t *testing.T) {
	cfg := customRoleResolverConfig()
	for _, name := range []string{"missing", "Smart", " analyst", "analyst/child"} {
		_, err := ResolveRoleContract(cfg, name, nil)
		require.Error(t, err, name)
	}
	cfg.Roles["broken"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "absent", Model: "m"}}
	_, err := ResolveRoleContract(cfg, "broken", nil)
	require.ErrorIs(t, err, agentcall.ErrAgentRoleUnconfigured)
}

func TestResolveRoleContractPreservesEmptyInheritedDenyAll(t *testing.T) {
	parentTools := []string{}
	parent := &session.RoleContract{Version: session.RoleContractVersion, Name: "parent", Model: session.ModelCfg{Provider: "p", Model: "pm"}, Tools: &parentTools}
	resolved, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", parent)
	require.NoError(t, err)
	require.Len(t, resolved.Inherited, 1)
	require.NotNil(t, resolved.Inherited[0], "present empty parent cap must remain an array")
	require.Empty(t, resolved.Inherited[0])
	require.False(t, RoleContractAllowsTool(resolved, "view"))
}

func TestWithCallOptionsRetainsResourceIdentityAndContractCloneHelperIsDetached(t *testing.T) {
	contract, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)
	original := &agentcall.CallOptions{RoleContract: contract, MaxCost: 9}
	agentcall.EnsureCallBuildScope(original)
	t.Cleanup(original.BuildScope.Close)
	copy := WithRoleContractCopy(original, contract)
	require.NotSame(t, original, copy)
	require.Same(t, original.BuildScope, copy.BuildScope, "resource ownership handles remain borrowed")
	require.NotSame(t, original.RoleContract, copy.RoleContract)
	*copy.RoleContract.Tools = append(*copy.RoleContract.Tools, "write")
	require.Empty(t, *original.RoleContract.Tools)
}

func TestCustomRoleModelResolveUsesPinnedConfigWithoutChangingAuthority(t *testing.T) {
	oldConfig := customRoleResolverConfig()
	parentTools := []string{"view"}
	parent := &session.RoleContract{
		Version: session.RoleContractVersion,
		Tools:   &parentTools,
	}
	contract, err := ResolveRoleContract(oldConfig, "analyst", parent)
	require.NoError(t, err)
	admitted := contract.Clone()

	newConfig := customRoleResolverConfig()
	newConfig.Providers.Set("new-provider", config.ProviderConfig{ID: "new-provider"})
	temperature := 0.25
	newConfig.Roles["analyst"] = config.CustomRole{SelectedModel: config.SelectedModel{
		Provider: "new-provider", Model: "new-model", ReasoningEffort: "low", MaxTokens: 99,
		Temperature:     &temperature,
		ProviderOptions: map[string]any{"nested": map[string]any{"mode": "new"}},
	}}
	for _, resolve := range []struct {
		name string
		call func(*config.Config, *session.RoleContract) (config.SelectedModel, string, error)
	}{
		{name: "model", call: CurrentOrSnapshotRoleModel},
		{name: "prompt", call: RoleContractPromptSource},
	} {
		t.Run(resolve.name, func(t *testing.T) {
			selected, source, err := resolve.call(newConfig, contract)
			require.NoError(t, err)
			require.Equal(t, "new-provider", selected.Provider)
			require.Equal(t, "new-model", selected.Model)
			require.Equal(t, "low", selected.ReasoningEffort)
			require.False(t, selected.Think)
			require.EqualValues(t, 99, selected.MaxTokens)
			require.Equal(t, callbeat.HbSourceSlotLive, source)
			require.Equal(t, 0.25, *selected.Temperature)
			*selected.Temperature = 0.75
			selected.ProviderOptions["nested"].(map[string]any)["mode"] = "mutated"
			require.Equal(t, 0.25, *newConfig.Roles["analyst"].Temperature)
			require.Equal(t, "new", newConfig.Roles["analyst"].ProviderOptions["nested"].(map[string]any)["mode"])

			pinned, _, err := resolve.call(oldConfig, contract)
			require.NoError(t, err)
			require.Equal(t, "m", pinned.Model, "an active request keeps its pinned config generation")
			require.True(t, admitted.Equal(contract), "refresh must not replace durable identity or ceilings")
			require.NotNil(t, contract.Tools)
			require.Empty(t, *contract.Tools)
			require.Equal(t, [][]string{{"view"}}, contract.Inherited)
			require.False(t, RoleContractAllowsTool(contract, "view"), "removing the configured cap must not widen admitted tools")
			require.False(t, RoleContractAllowsTool(contract, "write"))
		})
	}
}

func TestCustomRoleNextModelResolveFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*config.Config)
	}{
		{name: "removed", change: func(cfg *config.Config) { delete(cfg.Roles, "analyst") }},
		{name: "wrong case", change: func(cfg *config.Config) {
			cfg.Roles["Analyst"] = cfg.Roles["analyst"]
			delete(cfg.Roles, "analyst")
		}},
		{name: "missing model", change: func(cfg *config.Config) {
			role := cfg.Roles["analyst"]
			role.Model = ""
			cfg.Roles["analyst"] = role
		}},
		{name: "missing provider", change: func(cfg *config.Config) {
			role := cfg.Roles["analyst"]
			role.Provider = "unconfigured-provider"
			cfg.Roles["analyst"] = role
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := customRoleResolverConfig()
			contract, err := ResolveRoleContract(cfg, "analyst", nil)
			require.NoError(t, err)
			admitted := contract.Clone()
			test.change(cfg)
			for _, resolve := range []func(*config.Config, *session.RoleContract) (config.SelectedModel, string, error){
				CurrentOrSnapshotRoleModel, RoleContractPromptSource,
			} {
				_, _, err := resolve(cfg, contract)
				require.ErrorIs(t, err, agentcall.ErrAgentRoleUnconfigured)
				require.True(t, admitted.Equal(contract))
			}
		})
	}
}

func TestCustomRoleResolverRejectsUnavailableExactKey(t *testing.T) {
	cfg := customRoleResolverConfig()
	_, err := ResolveRoleContract(cfg, "missing", nil)
	require.ErrorIs(t, err, agentcall.ErrAgentRoleUnconfigured)
	_, err = ResolveRoleContract(cfg, "Analyst", nil)
	require.ErrorIs(t, err, agentcall.ErrAgentRoleUnconfigured)
}
