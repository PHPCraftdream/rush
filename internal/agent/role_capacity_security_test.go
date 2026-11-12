package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRoleCapCollectionPreservesEmptyGroupsAndDetachedIntersections(t *testing.T) {
	contract, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)
	own := []string{"view", "grep"}
	contract.Tools = &own
	contract.Inherited = [][]string{{"view"}, {}}

	inherited, err := inheritedRoleCeilings(contract)
	require.NoError(t, err)
	require.Equal(t, [][]string{{"view"}, {}, {"view", "grep"}}, inherited)
	require.NotNil(t, inherited[1], "an explicit empty inherited cap must remain deny-all")

	merged := mergeRoleToolContracts([]*session.RoleContract{contract})
	require.Len(t, merged, 1)
	require.NoError(t, merged[0].Validate())
	require.Equal(t, [][]string{{"view", "grep"}, {"view"}, {}}, merged[0].Inherited)
	require.False(t, RoleContractAllowsTool(merged[0], "view"))
	require.False(t, RoleContractAllowsTool(merged[0], "bash"))

	inherited[0][0] = "bash"
	inherited[2][0] = "bash"
	merged[0].Inherited[0][0] = "bash"
	merged[0].Inherited[1][0] = "bash"
	require.Equal(t, "view", (*contract.Tools)[0])
	require.Equal(t, "view", contract.Inherited[0][0])

	contract.Tools = nil
	contract.Inherited = nil
	inherited, err = inheritedRoleCeilings(contract)
	require.NoError(t, err)
	require.NotNil(t, inherited, "preserve the existing non-nil empty collection shape")
	require.Empty(t, inherited)
	require.Nil(t, mergeRoleToolContracts([]*session.RoleContract{contract}))
}

func TestRoleCapCollectionRetainsMalformedPolicyPoison(t *testing.T) {
	for _, contract := range []*session.RoleContract{nil, {Version: 0}} {
		merged := mergeRoleToolContracts([]*session.RoleContract{contract})
		require.Len(t, merged, 1)
		require.Error(t, merged[0].Validate(), "a malformed cap must not disappear into unrestricted policy")
	}
}
