package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
	"github.com/stretchr/testify/require"
)

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
	jsonConfigPath := filepath.Join(env.workingDir, "rush.json")
	writeConfig := func(path string) map[string]config.CustomRole {
		t.Helper()
		raw, err := json.Marshal(document)
		require.NoError(t, err)
		if filepath.Ext(path) == ".ktav" {
			raw, err = ktavcodec.ConvertJSONToKtav(raw)
			require.NoError(t, err)
		}
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		diskBytes, err := os.ReadFile(path)
		require.NoError(t, err)
		if filepath.Ext(path) == ".ktav" {
			tree, err := ktavcodec.DecodeKtav(diskBytes)
			require.NoError(t, err)
			diskBytes, err = ktavcodec.ToJSON(tree)
			require.NoError(t, err)
		}
		var serialized struct {
			Roles map[string]config.CustomRole `json:"roles"`
		}
		require.NoError(t, json.Unmarshal(diskBytes, &serialized))
		return serialized.Roles
	}
	writeConfig(jsonConfigPath)
	require.NoError(t, coord.cfg.ReloadFromDisk(t.Context()))
	configPath, err := filepath.Abs(filepath.Join(env.workingDir, "rush.ktav"))
	require.NoError(t, err)
	require.FileExists(t, configPath)
	require.Contains(t, coord.cfg.LoadedPaths(), configPath)
	pinned, generation := coord.cfg.Snapshot()
	require.NotNil(t, pinned.GetModel(provider.ID, "before-refresh"))
	require.NotNil(t, pinned.GetModel(provider.ID, "after-refresh"))
	require.NoError(t, config.ValidateCustomRoleSelection(pinned, pinned.Roles["analyst"]))
	contract, err := ResolveRoleContract(pinned, "analyst", nil)
	require.NoError(t, err)
	ctx := WithCallOptions(t.Context(), &CallOptions{RoleContract: contract, DisableSubAgents: true})
	t.Cleanup(callBuildScopeFrom(ctx).Close)
	before, err := coord.resolveSessionModels(ctx, "session")
	require.NoError(t, err)
	require.Equal(t, "before-refresh", before.smart.ModelCfg.Model)
	require.Equal(t, "smart-provider|before-refresh|before-refresh", before.systemPrompt)
	require.ElementsMatch(t, []string{"view"}, pinnedToolNames(before.tools))

	changed := roles["analyst"]
	changed.Model = "after-refresh"
	changed.Tools = nil
	roles["analyst"] = changed
	serializedRoles := writeConfig(configPath)
	require.Equal(t, "after-refresh", serializedRoles["analyst"].Model)
	require.Nil(t, serializedRoles["analyst"].Tools)
	// Drive the production five-second poller's tick without wall-clock waiting.
	result, err := coord.cfg.PollConfigDiskOnce(t.Context())
	require.NoError(t, err)
	require.NoError(t, result.ReloadErr)
	require.True(t, result.Reloaded)
	current, nextGeneration := coord.cfg.Snapshot()
	require.Greater(t, nextGeneration, generation)
	require.Equal(t, "before-refresh", pinned.Roles["analyst"].Model, "publication must not mutate the request's config snapshot")
	require.NotNil(t, pinned.Roles["analyst"].Tools)
	require.Equal(t, []string{"view"}, *pinned.Roles["analyst"].Tools)
	require.Equal(t, "after-refresh", current.Roles["analyst"].Model)
	require.Nil(t, current.Roles["analyst"].Tools)
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
	finished, err := newModelsComponent(coord).finishCustomRoleResolveSnapshot(ctx, pinned, before.smart, model, callbeat.HbSourceSlotLive, false)
	require.NoError(t, err)
	require.Equal(t, "smart-provider|before-refresh|before-refresh", finished.systemPrompt)
	require.True(t, contract.Equal(callOptionsFrom(ctx).RoleContract))
	delete(roles, "analyst")
	serializedRoles = writeConfig(configPath)
	require.NotContains(t, serializedRoles, "analyst")
	result, err = coord.cfg.PollConfigDiskOnce(t.Context())
	require.NoError(t, err)
	require.NoError(t, result.ReloadErr)
	require.True(t, result.Reloaded)
	_, err = coord.resolveSessionModels(ctx, "session")
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
}
