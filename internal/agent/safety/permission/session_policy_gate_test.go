package permission

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionPolicyOutranksPreGateAllowlist(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.allowedTools = []string{"bash"}
	svc.SetSessionReadonly("readonly", "owner")

	granted, err := svc.Request(t.Context(), CreatePermissionRequest{SessionID: "readonly", ToolCallID: "call", ToolName: "bash", Action: "execute"})
	require.NoError(t, err)
	require.False(t, granted, "a readonly session must not inherit the global allowlist")

	granted, err = svc.Request(t.Context(), CreatePermissionRequest{SessionID: "other", ToolCallID: "call2", ToolName: "bash", Action: "execute"})
	require.NoError(t, err)
	require.True(t, granted, "ungated sessions keep the allowlist")
}

func TestDispatchPolicyDenialYieldsOnlyToHookApproval(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.SetSessionReadonly("readonly", "owner")
	opts := CreatePermissionRequest{SessionID: "readonly", ToolCallID: "call", ToolName: "bash", Action: "execute"}

	restricted, allowed := svc.AuthorizeRestrictedDispatch(t.Context(), opts)
	require.True(t, restricted)
	require.False(t, allowed)

	restricted, allowed = svc.AuthorizeRestrictedDispatch(WithHookApproval(t.Context(), "call"), opts)
	require.False(t, restricted)
	require.True(t, allowed)
}

func TestAuthorizeRestrictedEntryPointsHonourSessionPolicy(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.SetSessionReadonly("readonly", "owner")
	svc.SetSessionAuto("auto", "owner-auto")
	for name, authorize := range map[string]func(CreatePermissionRequest) (bool, bool){
		"run":  svc.AuthorizeRestrictedRun,
		"tool": svc.AuthorizeRestrictedTool,
	} {
		t.Run(name, func(t *testing.T) {
			restricted, allowed := authorize(CreatePermissionRequest{SessionID: "readonly", ToolName: "bash", Action: "execute"})
			require.True(t, restricted)
			require.False(t, allowed)
			restricted, allowed = authorize(CreatePermissionRequest{SessionID: "readonly", ToolName: "view", Action: "read"})
			require.True(t, restricted)
			require.True(t, allowed)
			restricted, allowed = authorize(CreatePermissionRequest{SessionID: "auto", ToolName: "bash", Action: "execute"})
			require.True(t, restricted)
			require.True(t, allowed)
		})
	}
}
