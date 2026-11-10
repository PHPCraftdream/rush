package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
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

func TestCustomRoleDiskRefreshPinsResolvedModelPromptAndTools(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	var err error
	coord.prompt, err = prompt.NewPrompt("role acceptance", `{{.Provider}}|{{.Model}}|{{with index .Config.Roles "analyst"}}{{.Model}}{{end}}`, prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)
	provider, ok := coord.cfg.Config().Providers.Get("smart-provider")
	require.True(t, ok)
	provider.APIKey = "role-acceptance-fixture-key"
	provider.BaseURL = "https://example.invalid/v1"
	provider.Models = []catwalk.Model{{ID: "before-refresh"}, {ID: "after-refresh"}}
	model := config.SelectedModel{Provider: provider.ID, Model: "before-refresh"}
	roleTools := []string{"view"}
	roles := map[string]config.CustomRole{"analyst": {SelectedModel: model, Tools: &roleTools}}
	document := map[string]any{
		"providers": map[string]config.ProviderConfig{provider.ID: provider},
		"models":    map[string]config.SelectedModel{"smart": model, "fast": model},
		"roles":     roles,
	}
	writeConfig := func() {
		t.Helper()
		raw, err := json.Marshal(document)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "rush.json"), raw, 0o600))
	}
	writeConfig()
	require.NoError(t, coord.cfg.ReloadFromDisk(t.Context()))
	pinned, generation := coord.cfg.Snapshot()
	require.NotNil(t, pinned.GetModel(provider.ID, "before-refresh"))
	require.NotNil(t, pinned.GetModel(provider.ID, "after-refresh"))
	require.NoError(t, config.ValidateCustomRoleSelection(pinned, pinned.Roles["analyst"]))
	contract, err := ResolveRoleContract(pinned, "analyst", nil)
	require.NoError(t, err)
	ctx := WithCallOptions(t.Context(), &CallOptions{RoleContract: contract, DisableSubAgents: true})
	t.Cleanup(callBuildScopeFrom(ctx).close)
	before, err := coord.resolveSessionModels(ctx, "session")
	require.NoError(t, err)
	require.Equal(t, "before-refresh", before.smart.ModelCfg.Model)
	require.Equal(t, "smart-provider|before-refresh|before-refresh", before.systemPrompt)
	require.ElementsMatch(t, []string{"view"}, pinnedToolNames(before.tools))

	changed := roles["analyst"]
	changed.Model = "after-refresh"
	changed.Tools = nil
	roles["analyst"] = changed
	writeConfig()
	// Drive the production five-second poller's tick without wall-clock waiting.
	result, err := coord.cfg.PollConfigDiskOnce(t.Context())
	require.NoError(t, err)
	require.NoError(t, result.ReloadErr)
	require.True(t, result.Reloaded)
	current, nextGeneration := coord.cfg.Snapshot()
	require.Greater(t, nextGeneration, generation)
	require.Equal(t, "before-refresh", pinned.Roles["analyst"].Model, "publication must not mutate the request's config snapshot")
	require.Equal(t, "after-refresh", current.Roles["analyst"].Model)
	require.NotNil(t, current.GetModel(provider.ID, "after-refresh"))
	require.NoError(t, config.ValidateCustomRoleSelection(current, current.Roles["analyst"]))
	after, err := coord.resolveSessionModels(ctx, "session")
	require.NoError(t, err)
	require.Equal(t, "after-refresh", after.smart.ModelCfg.Model)
	require.Equal(t, "smart-provider|after-refresh|after-refresh", after.systemPrompt)
	require.ElementsMatch(t, []string{"view"}, pinnedToolNames(after.tools), "removing the live tools field cannot widen durable admission")
	require.Equal(t, "before-refresh", before.smart.ModelCfg.Model)
	require.Equal(t, "smart-provider|before-refresh|before-refresh", before.systemPrompt)

	// Finish a build pinned before publication while the store already exposes new config.
	finished, err := coord.finishCustomRoleResolveSnapshot(ctx, pinned, before.smart, model, hbSourceSlotLive, false)
	require.NoError(t, err)
	require.Equal(t, "smart-provider|before-refresh|before-refresh", finished.systemPrompt)
	require.True(t, contract.Equal(callOptionsFrom(ctx).RoleContract))
	delete(roles, "analyst")
	writeConfig()
	result, err = coord.cfg.PollConfigDiskOnce(t.Context())
	require.NoError(t, err)
	require.NoError(t, result.ReloadErr)
	require.True(t, result.Reloaded)
	_, err = coord.resolveSessionModels(ctx, "session")
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
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
