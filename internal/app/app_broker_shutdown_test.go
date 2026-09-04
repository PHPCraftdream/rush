package app

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func requireClosed[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case _, ok := <-ch:
		require.False(t, ok, "App shutdown must close every exposed broker channel")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for App-owned broker channel to close")
	}
}

// TestAppShutdownClosesEveryExposedBroker proves that an App shutdown closes
// subscriptions made with context.Background, including both permission
// brokers and the two App-level brokers that are not service fields.
func TestAppShutdownClosesEveryExposedBroker(t *testing.T) {
	isolateAppNewTestEnv(t)

	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store, SkipAgentSetup())
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	messages := application.Messages.Subscribe(context.Background())
	sessions := application.Sessions.Subscribe(context.Background())
	history := application.History.Subscribe(context.Background())
	permissionRequests := application.Permissions.Subscribe(context.Background())
	permissionNotifications := application.Permissions.SubscribeNotifications(context.Background())
	agentNotifications := application.AgentNotifications().Subscribe(context.Background())
	events := application.Events(context.Background())

	application.Shutdown()

	requireClosed(t, messages)
	requireClosed(t, sessions)
	requireClosed(t, history)
	requireClosed(t, permissionRequests)
	requireClosed(t, permissionNotifications)
	requireClosed(t, agentNotifications)
	requireClosed(t, events)

	// A repeated shutdown must not attempt to release resources or close any
	// broker a second time.
	application.Shutdown()
}
