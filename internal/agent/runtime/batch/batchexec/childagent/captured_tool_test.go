package childagent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/stretchr/testify/require"
)

type capturedTestLeaf struct{ fantasy.AgentTool }

func TestCapturedToolIdentityAndHandoff(t *testing.T) {
	leaf := &capturedTestLeaf{}
	scope := &toolwrap.PctChildToolScope{}
	captured := NewPctCapturedAgentTool(leaf, scope)
	require.Same(t, leaf, captured.PolicyLeafTool())
	require.Same(t, captured, toolwrap.CapturedToolFrom(captured))
	require.Same(t, scope, toolwrap.CapturedScopeFromTool(captured))

	key := "parent\x00call"
	first := &toolwrap.PctDelegationHandoff{}
	replacement := &toolwrap.PctDelegationHandoff{}
	require.True(t, captured.RegisterCapturedHandoff(key, first))
	require.False(t, captured.RegisterCapturedHandoff(key, replacement))
	require.Same(t, first, captured.ClaimCapturedHandoff(key))
	require.Nil(t, captured.ClaimCapturedHandoff(key))
	require.True(t, captured.RegisterCapturedHandoff(key, replacement))
	first.Release()
	require.False(t, captured.RegisterCapturedHandoff(key, &toolwrap.PctDelegationHandoff{}), "old release cannot remove the replacement")
	replacement.Release()
	third := &toolwrap.PctDelegationHandoff{}
	require.True(t, captured.RegisterCapturedHandoff(key, third), "release removes the matching registration")
	require.Same(t, third, captured.ClaimCapturedHandoff(key))
	third.Release()
}
