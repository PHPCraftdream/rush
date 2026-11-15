package permission

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLegacyRequestPreGateSuppressesHookNotification(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.allowedTools = []string{"tool"}
	notifications := svc.SubscribeNotifications(t.Context())
	ctx := WithHookApproval(context.Background(), "call")

	granted, err := svc.Request(ctx, CreatePermissionRequest{
		SessionID:  "ungated",
		ToolCallID: "call",
		ToolName:   "tool",
	})
	require.NoError(t, err)
	require.True(t, granted)
	select {
	case event := <-notifications:
		t.Fatalf("unexpected notification: %+v", event.Payload)
	default:
	}
}

func TestReadonlyPolicyRequestAllowsHookApproval(t *testing.T) {
	svc := newSessionDenyTestService(t)
	svc.SetSessionReadonly("readonly", "owner")
	notifications := svc.SubscribeNotifications(t.Context())

	granted, err := svc.Request(WithHookApproval(t.Context(), "call"), CreatePermissionRequest{
		SessionID:  "readonly",
		ToolCallID: "call",
		ToolName:   "bash",
		Action:     "execute",
	})
	require.NoError(t, err)
	require.True(t, granted)
	select {
	case event := <-notifications:
		require.Equal(t, "call", event.Payload.ToolCallID)
		require.True(t, event.Payload.Granted)
		require.False(t, event.Payload.Denied)
	case <-time.After(time.Second):
		t.Fatal("missing hook-approved notification")
	}
}
