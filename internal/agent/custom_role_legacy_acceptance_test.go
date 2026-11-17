package agent

import (
	"testing"

	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

func TestFromSessionCallOptionsSpec_RejectsV1WithRoleContract(t *testing.T) {
	contract, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)

	opts, err := fromSessionCallOptionsSpec(&session.CallOptionsSpec{
		Version:          1,
		DisableSubAgents: true,
		RoleContract:     contract,
	})
	require.ErrorIs(t, err, ErrCallOptionsSpecVersionUnsupported)
	require.Nil(t, opts, "a rejected legacy spec must not partially decode")
}
