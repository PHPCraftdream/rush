package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRoleToolGuardEnforcesCapturedAndCurrentCeilings(t *testing.T) {
	runs := 0
	newProbe := func(name string) fantasy.AgentTool {
		return fantasy.NewAgentTool(name, "policy probe", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			runs++
			return fantasy.NewTextResponse("ran"), nil
		})
	}
	noTools := []string{}
	empty := &session.RoleContract{Version: session.RoleContractVersion, Tools: &noTools}
	zeroGuard := roleGuardTool{AgentTool: newProbe("probe"), contracts: []*session.RoleContract{empty}}
	resp, err := zeroGuard.Run(context.Background(), fantasy.ToolCall{ID: "zero-forged", Name: "forged"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	resp, err = zeroGuard.Run(context.Background(), fantasy.ToolCall{ID: "zero-canonical", Name: "probe"})
	require.NoError(t, err)
	require.True(t, resp.IsError, "an explicit empty list denies the canonical tool too")
	require.Zero(t, runs, "a denied forged or zero-tools dispatch must not reach the inner tool")

	capturedTools := []string{"probe"}
	captured := &session.RoleContract{Version: session.RoleContractVersion, Tools: &capturedTools}
	capturedGuard := roleGuardTool{AgentTool: newProbe("write"), contracts: []*session.RoleContract{captured}}
	resp, err = capturedGuard.Run(context.Background(), fantasy.ToolCall{ID: "captured", Name: "write"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Zero(t, runs, "a captured cap still denies after invocation context is lost")

	currentOptions := &CallOptions{RoleContract: captured}
	currentScope := EnsureCallBuildScope(currentOptions)
	defer currentScope.close()
	currentCtx := WithCallOptions(context.Background(), currentOptions)
	currentGuard := roleGuardTool{AgentTool: newProbe("write"), contracts: []*session.RoleContract{captured}}
	resp, err = currentGuard.Run(currentCtx, fantasy.ToolCall{ID: "current", Name: "write"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Zero(t, runs, "the current call ceiling denies a mutable shared-set tool")

	shared := []fantasy.AgentTool{newProbe("probe"), newProbe("write")}
	sharedOptions := &CallOptions{RoleContract: captured}
	sharedScope := EnsureCallBuildScope(sharedOptions)
	defer sharedScope.close()
	sharedCtx := WithCallOptions(context.Background(), sharedOptions)
	pruned := filterRoleContractTools(sharedCtx, shared)
	require.Len(t, pruned, 1)
	require.Equal(t, "probe", pruned[0].Info().Name)
	require.Len(t, shared, 2, "filtering a shared wider tool generation must not mutate its source")
	resp, err = pruned[0].Run(context.Background(), fantasy.ToolCall{ID: "allowed", Name: "probe"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, 1, runs, "an allowed dispatch remains functional")
}
