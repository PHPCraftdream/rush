package permission

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newSessionDenyTestService(t *testing.T) *permissionService {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	return newTestService(t, false, nil).(*permissionService)
}

func TestSessionDenyRequestPublishesOnlyDeniedDecision(t *testing.T) {
	svc := newSessionDenyTestService(t)
	requests := svc.Subscribe(t.Context())
	notifications := svc.SubscribeNotifications(t.Context())
	svc.DenySession("denied", "owner")

	granted, err := svc.Request(denyTestCtx(t), CreatePermissionRequest{SessionID: "denied", ToolCallID: "call"})
	require.NoError(t, err)
	require.False(t, granted)
	select {
	case event := <-notifications:
		require.Equal(t, PermissionNotification{ToolCallID: "call", Denied: true}, event.Payload)
	case <-time.After(time.Second):
		t.Fatal("missing denied decision")
	}
	select {
	case event := <-requests:
		t.Fatalf("unexpected permission request: %+v", event.Payload)
	default:
	}
}

func TestSessionDenyOutranksAutoApproveAndAllowlist(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.allowedTools = []string{"tool"}
	svc.AutoApproveSession("sid")
	allowlist, err := BuildRunAllowlist(RunAllowlistSpec{Restrict: true, AllowTools: []string{"tool"}})
	require.NoError(t, err)
	svc.SetSessionRunAllowlist("sid", allowlist)
	svc.DenySession("sid", "owner")
	granted, err := svc.Request(denyTestCtx(t), CreatePermissionRequest{SessionID: "sid", ToolCallID: "deny", ToolName: "tool"})
	require.NoError(t, err)
	require.False(t, granted)
}

func TestSessionDenyIsSessionScoped(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.AutoApproveSession("auto")
	svc.DenySession("denied", "owner")
	granted, err := svc.Request(denyTestCtx(t), CreatePermissionRequest{SessionID: "auto", ToolCallID: "auto"})
	require.NoError(t, err)
	require.True(t, granted)

	ctx, cancel := context.WithCancel(t.Context())
	requests := svc.Subscribe(ctx)
	notifications := svc.SubscribeNotifications(ctx)
	result := make(chan error, 1)
	go func() {
		_, requestErr := svc.Request(ctx, CreatePermissionRequest{SessionID: "interactive", ToolCallID: "interactive"})
		result <- requestErr
	}()
	select {
	case event := <-requests:
		require.Equal(t, "interactive", event.Payload.SessionID)
	case <-time.After(time.Second):
		t.Fatal("interactive request was not published")
	}
	cancel()
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("interactive request decision was not published")
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("interactive request did not return")
	}
}

func TestSessionDenyOwnerTokenSemantics(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.DenySession("sid", "first")
	svc.ReleaseSessionDeny("sid", "wrong")
	require.True(t, svc.SessionDenied("sid"))
	svc.DenySession("sid", "second")
	svc.ReleaseSessionDeny("sid", "first")
	require.True(t, svc.SessionDenied("sid"))
	svc.DenySession("", "token")
	svc.DenySession("empty-token", "")
	svc.ReleaseSessionDeny("", "second")
	svc.ReleaseSessionDeny("sid", "second")
	require.False(t, svc.SessionDenied("sid"))
	require.False(t, svc.SessionDenied("empty-token"))
}

func TestSessionDenyChildInheritanceAndRelease(t *testing.T) {
	tests := []struct {
		name            string
		childToken      string
		wantChildDenied bool
	}{
		{name: "root release removes inherited child", childToken: "owner", wantChildDenied: false},
		{name: "child overwrite survives root release", childToken: "new-owner", wantChildDenied: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc := newSessionDenyTestService(t)
			svc.DenySession("parent", "owner")
			svc.InheritSessionAutoApprove("parent", "child")
			require.True(t, svc.SessionDenied("child"))
			if test.childToken != "owner" {
				svc.DenySession("child", test.childToken)
			}
			svc.ReleaseSessionDeny("parent", "owner")
			require.False(t, svc.SessionDenied("parent"))
			require.Equal(t, test.wantChildDenied, svc.SessionDenied("child"))
		})
	}
}

func TestSessionDenyHookApprovalPrecedesDeny(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.DenySession("sid", "owner")
	ctx := WithHookApproval(t.Context(), "call")
	granted, err := svc.Request(ctx, CreatePermissionRequest{SessionID: "sid", ToolCallID: "call"})
	require.NoError(t, err)
	require.True(t, granted)
}

func TestSessionDenyZeroValueState(t *testing.T) {
	var svc permissionService
	svc.DenySession("sid", "owner")
	require.True(t, svc.SessionDenied("sid"))
	svc.ReleaseSessionDeny("sid", "owner")
	require.False(t, svc.SessionDenied("sid"))
}

func TestSessionDenyStaleRootReleaseKeepsNewRoot(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.DenySession("parent", "old")
	svc.InheritSessionAutoApprove("parent", "old-child")
	svc.DenySession("parent", "new")
	svc.InheritSessionAutoApprove("parent", "new-child")
	svc.ReleaseSessionDeny("parent", "old")
	require.False(t, svc.SessionDenied("old-child"))
	require.True(t, svc.SessionDenied("parent"))
	require.True(t, svc.SessionDenied("new-child"))
}

func TestSessionDenyConcurrentRequestDenyRelease(t *testing.T) {
	svc := newSessionDenyTestService(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	svc.AutoApproveSession("sid")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessionID := "sid"
			token := "owner"
			svc.DenySession(sessionID, token)
			_, _ = svc.Request(ctx, CreatePermissionRequest{SessionID: sessionID, ToolCallID: "call"})
			svc.ReleaseSessionDeny(sessionID, token)
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent deny operations timed out")
	}
}

func TestSessionDenyRestrictedAuthorizers(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.DenySession("sid", "owner")
	opts := CreatePermissionRequest{SessionID: "sid", ToolCallID: "call"}
	restricted, allowed := svc.AuthorizeRestrictedDispatch(t.Context(), opts)
	require.True(t, restricted)
	require.False(t, allowed)
	restricted, allowed = svc.AuthorizeRestrictedRun(opts)
	require.True(t, restricted)
	require.False(t, allowed)
}

var _ SessionDenyManager = (*permissionService)(nil)
