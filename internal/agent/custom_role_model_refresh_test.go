package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

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
		{name: "model", call: currentOrSnapshotRoleModel},
		{name: "prompt", call: roleContractPromptSource},
	} {
		t.Run(resolve.name, func(t *testing.T) {
			selected, source, err := resolve.call(newConfig, contract)
			require.NoError(t, err)
			require.Equal(t, "new-provider", selected.Provider)
			require.Equal(t, "new-model", selected.Model)
			require.Equal(t, "low", selected.ReasoningEffort)
			require.False(t, selected.Think)
			require.EqualValues(t, 99, selected.MaxTokens)
			require.Equal(t, hbSourceSlotLive, source)
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
				currentOrSnapshotRoleModel, roleContractPromptSource,
			} {
				_, _, err := resolve(cfg, contract)
				require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
				require.True(t, admitted.Equal(contract))
			}
		})
	}
}
