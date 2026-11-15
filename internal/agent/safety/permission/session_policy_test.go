package permission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionReadonlyPolicyAllowsReadOnlyToolsAndDeniesWrites(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.SetSessionReadonly("readonly", "run")
	for _, tool := range ReadonlySessionTools {
		t.Run(tool, func(t *testing.T) {
			active, allowed := svc.SessionPolicyDecision("readonly", tool, "read")
			require.True(t, active)
			require.True(t, allowed)
		})
	}
	for _, opts := range []CreatePermissionRequest{
		{SessionID: "readonly", ToolName: "write", Action: "write"},
		{SessionID: "readonly", ToolName: "bash", Action: "execute"},
		{SessionID: "readonly", ToolName: "unknown", Action: "read"},
		{SessionID: "readonly", ToolName: "view", Action: "write"},
	} {
		active, allowed := svc.SessionPolicyDecision(opts.SessionID, opts.ToolName, opts.Action)
		require.True(t, active)
		require.False(t, allowed)
		granted, err := svc.Request(denyTestCtx(t), opts)
		require.NoError(t, err)
		require.False(t, granted)
	}
}

func TestSessionAutoPolicyIsOwnedAndInherited(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.SetSessionAuto("parent", "run")
	svc.InheritSessionPolicy("parent", "child")
	active, allowed := svc.SessionPolicyDecision("child", "bash", "execute")
	require.True(t, active)
	require.True(t, allowed)
	svc.ReleaseSessionPolicy("parent", "wrong")
	active, allowed = svc.SessionPolicyDecision("parent", "write", "write")
	require.True(t, active)
	require.True(t, allowed)
	svc.ReleaseSessionPolicy("parent", "run")
	_, allowed = svc.SessionPolicyDecision("parent", "write", "write")
	require.False(t, allowed)
	_, allowed = svc.SessionPolicyDecision("child", "bash", "execute")
	require.False(t, allowed)
}

func TestSessionPolicyOutranksAutoApproveAndPreservesHookApproval(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.AutoApproveSession("sid")
	svc.SetSessionReadonly("sid", "run")

	for _, test := range []struct {
		name      string
		tool      string
		action    string
		approved  bool
		wantGrant bool
	}{
		{name: "readonly tool allowed despite auto approve", tool: "view", action: "read", wantGrant: true},
		{name: "write denied despite auto approve", tool: "bash", action: "execute"},
		{name: "hook approval overrides readonly policy", tool: "bash", action: "execute", approved: true, wantGrant: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := denyTestCtx(t)
			if test.approved {
				ctx = WithHookApproval(ctx, "call")
			}
			granted, err := svc.Request(ctx, CreatePermissionRequest{SessionID: "sid", ToolCallID: "call", ToolName: test.tool, Action: test.action})
			require.NoError(t, err)
			require.Equal(t, test.wantGrant, granted)
		})
	}

	svc.DenySession("sid", "deny-owner")
	granted, err := svc.Request(denyTestCtx(t), CreatePermissionRequest{SessionID: "sid", ToolCallID: "denied", ToolName: "view", Action: "read"})
	require.NoError(t, err)
	require.False(t, granted, "explicit deny still outranks auto-approve and policy allow")
	granted, err = svc.Request(WithHookApproval(denyTestCtx(t), "hook"), CreatePermissionRequest{SessionID: "sid", ToolCallID: "hook", ToolName: "bash", Action: "execute"})
	require.NoError(t, err)
	require.True(t, granted, "preserve existing hook-over-deny precedence")
}

func TestSessionPolicyConcurrentDecisionAndRelease(t *testing.T) {
	svc := newSessionDenyTestService(t)
	const iterations = 100
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		<-start
		for i := 0; i < iterations; i++ {
			svc.SetSessionAuto("sid", "run")
			_, _ = svc.SessionPolicyDecision("sid", "bash", "execute")
			svc.ReleaseSessionPolicy("sid", "run")
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		<-start
		for i := 0; i < iterations; i++ {
			svc.SetSessionReadonly("peer", "peer-run")
			_, _ = svc.SessionPolicyDecision("peer", "view", "read")
			svc.ReleaseSessionPolicy("peer", "peer-run")
		}
	}()
	close(start)
	for range 2 {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("policy concurrency timed out")
		}
	}
}
