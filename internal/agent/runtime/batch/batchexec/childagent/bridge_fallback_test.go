package childagent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorBridgeFallback(t *testing.T) {
	ctx := context.Background()
	inv := BridgeInvocation{RegistryID: "pinned", AgentDepth: 2, AgentDepthSet: true}
	read := func(context.Context) (BridgeInvocation, bool) { return inv, true }
	require.Nil(t, CoordinatorBridgeFallback(nil, nil, nil)) //nolint:staticcheck // the nil-context guard is the behavior under test
	require.Nil(t, CoordinatorBridgeFallback(ctx, func(context.Context) (BridgeInvocation, bool) {
		return BridgeInvocation{}, false
	}, nil))
	require.Nil(t, CoordinatorBridgeFallback(ctx, read, func(got BridgeInvocation) (*toolwrap.PolicyLeafRegistry, func(*session.RoleContract) PinnedInvoke, error) {
		require.Equal(t, inv, got)
		return nil, nil, nil
	}))
	require.Nil(t, CoordinatorBridgeFallback(ctx, read, func(BridgeInvocation) (*toolwrap.PolicyLeafRegistry, func(*session.RoleContract) PinnedInvoke, error) {
		return nil, func(*session.RoleContract) PinnedInvoke {
			t.Fatal("registry error must stop before pinning")
			return nil
		}, errors.New("no registry")
	}))

	allowed := []string{"view"}
	contract := &session.RoleContract{Tools: &allowed}
	ctx = toolwrap.WithRoleToolContract(ctx, contract)
	registry := toolwrap.NewPolicyToolBuild(nil).Leaves
	bridge := CoordinatorBridgeFallback(ctx, read, func(got BridgeInvocation) (*toolwrap.PolicyLeafRegistry, func(*session.RoleContract) PinnedInvoke, error) {
		require.Equal(t, inv, got)
		return registry, func(snapshot *session.RoleContract) PinnedInvoke {
			original := toolwrap.RoleContractFromContext(ctx)
			require.Equal(t, original, snapshot)
			require.NotSame(t, original, snapshot)
			(*snapshot.Tools)[0] = "other"
			require.Equal(t, []string{"view"}, *original.Tools)
			return func(context.Context, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.ToolResponse{}, nil
			}
		}, nil
	})
	require.NotNil(t, bridge)
}
