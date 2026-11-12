package app

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestPrepareExecuteRunCustomRoleAdmissionSnapshotsExactProfile(t *testing.T) {
	a := newModelEffortApp(t)
	if err := a.config.SetCustomRole(config.ScopeWorkspace, "Research Analyst", config.CustomRoleUpdate{Model: config.SelectedModel{Provider: "openaicompat", Model: "raw-role-model", Think: true}}); err != nil {
		t.Fatal(err)
	}
	_, cancel, _, err := a.prepareExecuteRun(context.Background(), RunRequest{Prompt: "p", HideSpinner: true, Overrides: RunOverrides{RequestedRole: "Research Analyst", ModelRole: config.SelectedModelTypeFast}})
	if cancel != nil {
		cancel()
	}
	require.ErrorContains(t, err, "cannot be combined")
	_, cancel, setup, err := a.prepareExecuteRun(context.Background(), RunRequest{Prompt: "p", HideSpinner: true, Overrides: RunOverrides{RequestedRole: "Research Analyst", ReasoningEffort: "high"}})
	require.NoError(t, err)
	t.Cleanup(cancel)
	require.Empty(t, setup.callOpts.ModelRole)
	require.False(t, setup.callOpts.DisableSubAgents, "a custom profile does not change the caller's base tool policy")
	require.Equal(t, "Research Analyst", setup.callOpts.RoleContract.Name)
	require.Equal(t, "raw-role-model", setup.callOpts.RoleContract.Model.Model)
	require.True(t, setup.callOpts.RoleContract.Model.Think)
	require.Equal(t, "high", setup.callOpts.RoleContract.Model.ReasoningEffort)
}

func TestResolveRunRoleRejectsUnknownAndMalformedWithoutFallback(t *testing.T) {
	a := newModelEffortApp(t)
	for _, role := range []string{"missing", " smart", "smart"} {
		require.Error(t, a.resolveRunRole(&RunOverrides{RequestedRole: role}))
	}
}

func TestResolveRunRoleUsesNewPublishedConfigForEachExecution(t *testing.T) {
	a := newModelEffortApp(t)
	require.NoError(t, a.config.SetCustomRole(config.ScopeWorkspace, "writer", config.CustomRoleUpdate{Model: config.SelectedModel{Provider: "openaicompat", Model: "m1"}}))
	first := &RunOverrides{RequestedRole: "writer"}
	require.NoError(t, a.resolveRunRole(first))
	require.Equal(t, "m1", first.roleContract.Model.Model)
	require.NoError(t, a.config.SetCustomRole(config.ScopeWorkspace, "writer", config.CustomRoleUpdate{Model: config.SelectedModel{Provider: "openaicompat", Model: "m2"}}))
	second := &RunOverrides{RequestedRole: "writer"}
	require.NoError(t, a.resolveRunRole(second))
	require.Equal(t, "m2", second.roleContract.Model.Model)
	require.Equal(t, "m1", first.roleContract.Model.Model)
}

func TestRoleContractSnapshotIsDeepCopied(t *testing.T) {
	allow := []string{"view", "grep"}
	cfg := &config.Config{Roles: map[string]config.CustomRole{"reader": {SelectedModel: config.SelectedModel{Provider: "p", Model: "m"}, Tools: &allow}}}
	cfg.Providers = csync.NewMapFrom(map[string]config.ProviderConfig{"p": {ID: "p"}})
	contract, err := agent.ResolveRoleContract(cfg, "reader", nil)
	require.NoError(t, err)
	copy := contract.Clone()
	allow[0] = "bash"
	require.Equal(t, "view", (*copy.Tools)[0])
	require.NoError(t, copy.Validate())
	require.Equal(t, session.RoleContractVersion, copy.Version)
}

func TestResolveRunRolePreservesExplicitDelegationPolicy(t *testing.T) {
	a := newModelEffortApp(t)
	require.NoError(t, a.config.SetCustomRole(config.ScopeWorkspace, "policy-profile", config.CustomRoleUpdate{
		Model: config.SelectedModel{Provider: "openaicompat", Model: "role-model"},
	}))
	for _, disabled := range []bool{false, true} {
		overrides := &RunOverrides{RequestedRole: "policy-profile", DisableSubAgents: disabled}
		require.NoError(t, a.resolveRunRole(overrides))
		require.Equal(t, disabled, overrides.DisableSubAgents, "model-profile selection must preserve the caller's delegation policy")
		require.NotNil(t, overrides.roleContract)
	}
}
