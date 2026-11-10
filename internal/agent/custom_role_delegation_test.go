package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRequestedCustomRoleUsesSelectedContractBeforeCredentialFallback(t *testing.T) {
	contract, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)
	coord := &coordinator{}
	creds := &CredentialSet{Models: map[Role]ModelChoice{RoleSmart: {Provider: "tenant", Model: "tenant-smart"}}}
	ctx := WithSelectedRoleContract(withCallCredentials(t.Context(), creds), contract)
	_, err = coord.resolveRequestedDelegationModel(ctx, customRoleResolverConfig(), "", "analyst")
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured, "missing exact tenant role slot cannot use tenant smart or operator fallback")
	creds.AllowConfiguredRoleFallback = true
	_, err = coord.resolveRequestedDelegationModel(ctx, customRoleResolverConfig(), "", "analyst")
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured, "fallback still fails when the operator profile cannot be constructed")
}

func TestResolveDelegationRequestedRoleRestoresNamedContract(t *testing.T) {
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "custom-role-child")
	require.NoError(t, err)
	store := env.sessions.(session.SessionRoleContractStore)
	contract, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)
	require.NoError(t, store.BindSessionRoleContract(t.Context(), child.ID, contract))
	role, err := resolveDelegationRequestedRole(t.Context(), env.sessions, child.ID, "")
	require.NoError(t, err)
	require.Equal(t, RequestedModelRole("analyst"), role)
	_, err = resolveDelegationRequestedRole(t.Context(), env.sessions, child.ID, "reviewer")
	require.ErrorIs(t, err, ErrAgentRoleMismatch)
}

func TestRootRoleBindingScopesContractToLogicalCall(t *testing.T) {
	env := testEnv(t)
	root, err := env.sessions.Create(t.Context(), "root-role-contract")
	require.NoError(t, err)
	store := env.sessions.(session.SessionRootRoleContractStore)
	first, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)
	call := SessionAgentCall{SessionID: root.ID, LogicalCallID: "root-logical-1", CallOptions: &CallOptions{RoleContract: first}}
	require.NoError(t, bindRootRoleContract(t.Context(), env.sessions, call))
	restored, err := restoreRootRoleContract(t.Context(), env.sessions, SessionAgentCall{SessionID: root.ID, LogicalCallID: "root-logical-1"})
	require.NoError(t, err)
	require.True(t, first.Equal(restored.CallOptions.RoleContract))
	second, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", &session.RoleContract{Version: session.RoleContractVersion, Tools: &[]string{"view"}})
	require.NoError(t, err)
	newCall := SessionAgentCall{SessionID: root.ID, LogicalCallID: "root-logical-2", RoleContract: second}
	require.NoError(t, bindRootRoleContract(t.Context(), env.sessions, newCall))
	logicalID, current, found, err := store.GetRootRoleContract(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "root-logical-2", logicalID)
	require.True(t, second.Equal(current))
	stale, err := restoreRootRoleContract(t.Context(), env.sessions, SessionAgentCall{SessionID: root.ID, LogicalCallID: "root-logical-1"})
	require.NoError(t, err)
	require.Nil(t, stale.CallOptions, "a prior root execution is not pinned into a new logical call")
}
