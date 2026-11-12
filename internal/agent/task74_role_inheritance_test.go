package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func task74RoleContract(name string, tools *[]string, inherited [][]string) *session.RoleContract {
	return &session.RoleContract{
		Version:   session.RoleContractVersion,
		Name:      name,
		Model:     session.ModelCfg{Provider: "provider", Model: "model"},
		Tools:     tools,
		Inherited: inherited,
	}
}

func TestTask74RoleContractInheritsRejectsInvalidParentContracts(t *testing.T) {
	child := task74RoleContract("analyst", nil, nil)
	for _, tc := range []struct {
		name   string
		parent *session.RoleContract
	}{
		{
			name:   "unsupported version",
			parent: &session.RoleContract{Version: session.RoleContractVersion + 1, Name: "parent", Model: session.ModelCfg{Provider: "provider", Model: "model"}},
		},
		{
			name: "null inherited group",
			parent: &session.RoleContract{
				Version:   session.RoleContractVersion,
				Name:      "parent",
				Model:     session.ModelCfg{Provider: "provider", Model: "model"},
				Inherited: [][]string{nil},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, roleContractInherits(child, tc.parent), "invalid inherited contracts must fail closed")
			require.Error(t, validateSelectedBatchRole(child, "analyst", tc.parent), "selected-role admission must reject an invalid parent")
		})
	}
}

func TestTask74RoleContractInheritsPreservesValidCeilingsAndRejectsMismatch(t *testing.T) {
	parentTools := []string{"view", "batch_status"}
	parent := task74RoleContract("parent", &parentTools, [][]string{{"view"}})
	want, err := inheritedRoleCeilings(parent)
	require.NoError(t, err)
	child := task74RoleContract("analyst", nil, want)

	require.True(t, roleContractInherits(child, parent), "matching inherited groups preserve every parent ceiling")
	require.NoError(t, validateSelectedBatchRole(child, "analyst", parent))

	mismatch := task74RoleContract("analyst", nil, [][]string{{"view", "batch_status"}})
	require.False(t, roleContractInherits(mismatch, parent), "a changed inherited group must not widen the selected role")
	require.Error(t, validateSelectedBatchRole(mismatch, "analyst", parent))
	require.False(t, roleContractInherits(nil, parent), "a nil child must fail closed instead of panicking")
}
