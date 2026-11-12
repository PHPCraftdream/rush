package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/session"
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
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
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
	original := &CallOptions{RoleContract: contract, MaxCost: 9}
	EnsureCallBuildScope(original)
	t.Cleanup(original.buildScope.close)
	copy := WithRoleContractCopy(original, contract)
	require.NotSame(t, original, copy)
	require.Same(t, original.buildScope, copy.buildScope, "resource ownership handles remain borrowed")
	require.NotSame(t, original.RoleContract, copy.RoleContract)
	*copy.RoleContract.Tools = append(*copy.RoleContract.Tools, "write")
	require.Empty(t, *original.RoleContract.Tools)
}
