package agent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
)

// TestRoleCapCollectionPreservesEmptyGroupsAndDetachedIntersections keeps
// the root resolver/inherited-group API coverage. The merge semantics that
// previously ran through the removed exported toolwrap.MergeRoleToolContracts
// wrapper are preserved in the toolwrap lower suite
// (TestRoleCapCollectionRetainsMalformedPolicyPoison, package toolwrap) via
// the private mergeRoleToolContracts.
func TestRoleCapCollectionPreservesEmptyGroupsAndDetachedIntersections(t *testing.T) {
	contract, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)
	own := []string{"view", "grep"}
	contract.Tools = &own
	contract.Inherited = [][]string{{"view"}, {}}

	inherited, err := modelkit.InheritedRoleCeilings(contract)
	require.NoError(t, err)
	require.Equal(t, [][]string{{"view"}, {}, {"view", "grep"}}, inherited)
	require.NotNil(t, inherited[1], "an explicit empty inherited cap must remain deny-all")

	// The role-group API keeps the intersection deny-all for the collected
	// ceilings: an explicit empty inherited cap denies every name.
	require.False(t, RoleContractAllowsTool(contract, "view"))
	require.False(t, RoleContractAllowsTool(contract, "bash"))

	inherited[0][0] = "bash"
	inherited[2][0] = "bash"
	require.Equal(t, "view", (*contract.Tools)[0])
	require.Equal(t, "view", contract.Inherited[0][0], "ceiling mutation must not alias the resolver output")

	contract.Tools = nil
	contract.Inherited = nil
	inherited, err = modelkit.InheritedRoleCeilings(contract)
	require.NoError(t, err)
	require.NotNil(t, inherited, "preserve the existing non-nil empty collection shape")
	require.Empty(t, inherited)
}
