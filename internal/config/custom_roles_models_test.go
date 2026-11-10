package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/stretchr/testify/require"
)

func TestCustomRoleSelectionUsesLiveEffortsBeforeCachedCatalog(t *testing.T) {
	t.Parallel()
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"p": {
			Models:      []catwalk.Model{{ID: "model", ReasoningLevels: []string{"low", "high"}}},
			LiveEfforts: map[string]ModelEffortInfo{"model": {Levels: []string{"high", "max"}}},
		},
	})}
	role := CustomRole{SelectedModel: SelectedModel{Provider: "p", Model: "model", ReasoningEffort: "low"}}
	require.ErrorContains(t, ValidateCustomRoleSelection(cfg, role), "not supported")
	role.ReasoningEffort = "max"
	require.NoError(t, ValidateCustomRoleSelection(cfg, role))
	role.ReasoningEffort = ""
	require.NoError(t, ValidateCustomRoleSelection(cfg, role))
	role.Model = "future-model"
	role.ReasoningEffort = "vendor-specific"
	require.NoError(t, ValidateCustomRoleSelection(cfg, role), "unknown raw models retain the existing unverified passthrough")
	role.Provider = "missing"
	require.ErrorContains(t, ValidateCustomRoleSelection(cfg, role), "not configured")
	require.Error(t, ValidateCustomRoleSelection(nil, role))
}
