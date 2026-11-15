package permission

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A session that is not denied keeps the legacy dispatch gate: a hook approval
// does not lift a restricted-run denial there (the hook is honoured later, by
// Request). Only a denied session consults the hook at the dispatch gate.
func TestSessionDenyDispatchLegacyForOtherSessions(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.AutoApproveSession("sid")
	allowlist, err := BuildRunAllowlist(RunAllowlistSpec{Restrict: true})
	require.NoError(t, err)
	svc.SetSessionRunAllowlist("sid", allowlist)
	opts := CreatePermissionRequest{SessionID: "sid", ToolCallID: "call", ToolName: "tool", Action: "run"}
	hookCtx := context.WithValue(t.Context(), hookApprovalKey{}, "call")

	restricted, allowed := svc.AuthorizeRestrictedDispatch(hookCtx, opts)
	require.True(t, restricted)
	require.False(t, allowed, "hook approval must not change the legacy dispatch gate of a non-denied session")
}

func TestSessionDenyDispatchHookPrecedenceOnlyForDeniedSession(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.DenySession("sid", "owner")
	opts := CreatePermissionRequest{SessionID: "sid", ToolCallID: "call", ToolName: "tool"}

	restricted, allowed := svc.AuthorizeRestrictedDispatch(t.Context(), opts)
	require.True(t, restricted)
	require.False(t, allowed)

	hookCtx := context.WithValue(t.Context(), hookApprovalKey{}, "call")
	restricted, allowed = svc.AuthorizeRestrictedDispatch(hookCtx, opts)
	require.False(t, restricted)
	require.True(t, allowed, "operator hook approval keeps precedence over a session denial")
}

// denyTestCtx bounds Request so a missing denial fails fast instead of waiting
// for a human answer that never comes.
func denyTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSessionDenyReleaseOfChildKeepsRoot(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.DenySession("root", "owner")
	svc.InheritSessionAutoApprove("root", "child")
	require.True(t, svc.SessionDenied("child"))

	svc.ReleaseSessionDeny("child", "owner")
	require.False(t, svc.SessionDenied("child"), "the child entry is released by its own token")
	require.True(t, svc.SessionDenied("root"), "releasing a child must not lift the root denial")

	svc.ReleaseSessionDeny("root", "owner")
	require.False(t, svc.SessionDenied("root"))
}

func TestSessionDenyAllAuthorizersAndEmptySession(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.DenySession("sid", "owner")
	opts := CreatePermissionRequest{SessionID: "sid", ToolCallID: "call", ToolName: "tool"}

	for name, authorize := range map[string]func(CreatePermissionRequest) (bool, bool){
		"run":  svc.AuthorizeRestrictedRun,
		"tool": svc.AuthorizeRestrictedTool,
	} {
		restricted, allowed := authorize(opts)
		require.True(t, restricted, name)
		require.False(t, allowed, name)
	}
	require.False(t, svc.SessionDenied(""), "the empty session id is never denied")
	svc.DenySession("", "owner")
	require.False(t, svc.SessionDenied(""))
}
