package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestKtavMigratedMCPEnableDisableRemoveRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		migrate bool
	}{
		{name: "JSON control"},
		{name: "Ktav migrated", migrate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Configure migration before creating the legacy app-data fixture or
			// initializing the store, since Init performs startup-slot migration.
			restore := config.SetKtavAutoMigrateForTest(tc.migrate)
			defer restore()
			t.Setenv("RUSH_DISABLE_DEFAULT_PROVIDERS", "1")
			root := t.TempDir()
			configDir, dataDir := filepath.Join(root, "global-config"), filepath.Join(root, "global-data")
			t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
			t.Setenv("XDG_CONFIG_HOME", configDir)
			t.Setenv("RUSH_GLOBAL_DATA", dataDir)
			t.Setenv("XDG_DATA_HOME", dataDir)
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			require.NoError(t, os.MkdirAll(dataDir, 0o700))
			// With the default providers disabled, Init needs a custom provider;
			// without one it relied on locally installed CLI providers and failed
			// on CI machines that have none.
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "rush.json"), []byte(`{
  "providers": {
    "test": {
      "id": "test",
      "base_url": "https://example.invalid/v1",
      "api_key": "test-key",
      "discover_models": false,
      "models": [{"id": "model", "name": "Test model"}]
    }
  }
}`), 0o600))

			const name, toolName = "ktav-lifecycle", "ktav_echo"
			server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ktav-lifecycle-server"}, nil)
			sdkmcp.AddTool(server, &sdkmcp.Tool{Name: toolName, Description: "lifecycle probe"}, func(context.Context, *sdkmcp.CallToolRequest, any) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "roundtrip"}}}, nil, nil
			})
			httpServer := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil))
			// Register the HTTP cleanup first: LIFO cleanup releases the owner before
			// the server is closed. The handler itself never blocks.
			t.Cleanup(func() {
				for session := range server.Sessions() {
					_ = session.Close()
				}
				httpServer.CloseClientConnections()
				httpServer.Close()
			})

			// Seed an existing app-managed data slot before Init so the Ktav case
			// exercises startup migration, rather than starting from an empty file.
			legacyPath := filepath.Join(dataDir, "rush.json")
			legacyData := []byte(`{"recent_models":{"smart":[]}}`)
			require.NoError(t, os.WriteFile(legacyPath, legacyData, 0o600))
			store, err := config.Init(root, root, false)
			require.NoError(t, err)
			selectedPath := config.ActiveConfigFileName(legacyPath)
			if tc.migrate {
				require.Equal(t, filepath.Join(dataDir, "rush.ktav"), selectedPath)
				_, err = os.Stat(selectedPath)
				require.NoError(t, err, "Init must migrate before lifecycle subscription")
				require.FileExists(t, legacyPath, "successful migration retains legacy data for older binaries")
			} else {
				require.Equal(t, legacyPath, selectedPath)
			}
			require.Equal(t, selectedPath, config.ActiveConfigFileName(config.GlobalConfigData()))
			require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
				Type: config.MCPHttp, URL: httpServer.URL, Timeout: 60, Disabled: true,
			}))

			definition, ok := store.MCPConfig(name)
			require.True(t, ok)
			require.True(t, definition.Disabled)
			require.Equal(t, httpServer.URL, definition.URL)
			assertKtavFreshReload(t, root, name, selectedPath, true, true, httpServer.URL)
			owner, err := Acquire()
			require.NoError(t, err)
			// Cleanup is registered after the server cleanup, so owner release runs first.
			t.Cleanup(func() { _ = owner.Close(context.Background()) })
			// Subscribe only AFTER Acquire: reclaiming an owner left installed by an
			// earlier test resets the event broker and would close an older subscription.
			eventCtx, cancelEvents := context.WithCancel(context.Background())
			defer cancelEvents()
			events := SubscribeEvents(eventCtx)
			owner.Initialize(context.Background(), nil, store, false)
			awaitKtavEvent(t, events, pubsub.UpdatedEvent, name, StateDisabled)
			assertKtavClient(t, owner, name, StateDisabled, true)
			_, hasSession := sessions.Get(name)
			require.False(t, hasSession)
			require.Empty(t, GetServerToolNames(name))

			require.NoError(t, owner.EnableServer(context.Background(), store, name))
			awaitKtavEvent(t, events, pubsub.UpdatedEvent, name, StateStarting)
			awaitKtavEvent(t, events, pubsub.UpdatedEvent, name, StateConnected)
			connected := assertKtavClient(t, owner, name, StateConnected, false)
			firstSession := connected.Client
			currentSession, hasSession := sessions.Get(name)
			require.True(t, hasSession)
			require.Same(t, firstSession, currentSession)
			require.Equal(t, []string{toolName}, GetServerToolNames(name))
			result, err := RunTool(context.Background(), store, name, toolName, `{}`)
			require.NoError(t, err)
			require.Equal(t, "roundtrip", result.Content)
			assertKtavFreshReload(t, root, name, selectedPath, true, false, httpServer.URL)

			require.NoError(t, owner.DisableServer(context.Background(), store, name))
			awaitKtavEvent(t, events, pubsub.UpdatedEvent, name, StateDisabled)
			assertKtavClient(t, owner, name, StateDisabled, true)
			_, hasSession = sessions.Get(name)
			require.False(t, hasSession)
			require.Empty(t, GetServerToolNames(name))
			assertKtavFreshReload(t, root, name, selectedPath, true, true, httpServer.URL)

			require.NoError(t, owner.EnableServer(context.Background(), store, name))
			awaitKtavEvent(t, events, pubsub.UpdatedEvent, name, StateStarting)
			awaitKtavEvent(t, events, pubsub.UpdatedEvent, name, StateConnected)
			second := assertKtavClient(t, owner, name, StateConnected, false)
			currentSession, hasSession = sessions.Get(name)
			require.True(t, hasSession)
			require.Same(t, second.Client, currentSession)
			require.NotSame(t, firstSession, second.Client, "re-enable must establish a fresh session")
			result, err = RunTool(context.Background(), store, name, toolName, `{}`)
			require.NoError(t, err)
			require.Equal(t, "roundtrip", result.Content)
			assertKtavFreshReload(t, root, name, selectedPath, true, false, httpServer.URL)

			require.NoError(t, owner.RemoveServer(store, name))
			awaitKtavEvent(t, events, pubsub.DeletedEvent, name, StateDisabled)
			_, exists := owner.GetStates()[name]
			require.False(t, exists)
			_, hasSession = sessions.Get(name)
			require.False(t, hasSession)
			require.Empty(t, GetServerToolNames(name))
			assertNoKtavEvents(t, events)
			assertKtavFreshReload(t, root, name, selectedPath, false, false, "")
		})
	}
}

func awaitKtavEvent(t *testing.T, events <-chan pubsub.Event[Event], typ pubsub.EventType, name string, state State) {
	t.Helper()
	timer := time.NewTimer(mcpAwaitTimeout)
	defer timer.Stop()
	var duplicateStarting bool
	for {
		select {
		case event := <-events:
			require.Equal(t, name, event.Payload.Name, "unexpected MCP event while awaiting lifecycle transition: %#v", event)
			// Enable may intentionally publish one duplicate Starting before Connected.
			if state == StateConnected && event.Type == pubsub.UpdatedEvent && event.Payload.Type == EventStateChanged && event.Payload.State == StateStarting {
				require.False(t, duplicateStarting, "only one duplicate Starting event is tolerated")
				duplicateStarting = true
				continue
			}
			require.Equal(t, typ, event.Type)
			require.Equal(t, EventStateChanged, event.Payload.Type)
			require.Equal(t, state, event.Payload.State, "unexpected MCP state event: %#v err=%v", event.Payload, event.Payload.Error)
			return
		case <-timer.C:
			t.Fatalf("timed out waiting for %s event for %q", state, name)
			return
		}
	}
}

func assertKtavClient(t *testing.T, owner *Owner, name string, want State, absent bool) ClientInfo {
	t.Helper()
	info, ok := owner.GetStates()[name]
	if absent {
		require.True(t, ok)
		require.Equal(t, want, info.State)
		require.NoError(t, info.Error)
		require.Nil(t, info.Client)
		return info
	}
	require.True(t, ok)
	require.Equal(t, want, info.State)
	require.NoError(t, info.Error)
	require.NotNil(t, info.Client)
	return info
}

func assertNoKtavEvents(t *testing.T, events <-chan pubsub.Event[Event]) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected trailing MCP event after removal: %#v", event)
	default:
	}
}

func assertKtavFreshReload(t *testing.T, root, name, selectedPath string, exists, disabled bool, wantURL string) {
	t.Helper()
	freshStore, err := config.Init(root, root, false)
	require.NoError(t, err)
	assertKtavReload(t, freshStore, name, selectedPath, exists, disabled, wantURL)
}

func assertKtavReload(t *testing.T, store *config.ConfigStore, name, selectedPath string, exists, disabled bool, wantURL string) {
	t.Helper()
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	active := config.ActiveConfigFileName(filepath.Join(filepath.Dir(selectedPath), "rush.json"))
	require.Equal(t, selectedPath, active)
	info, err := os.Stat(selectedPath)
	require.NoError(t, err)
	require.False(t, info.IsDir())
	definition, ok := store.MCPConfig(name)
	require.Equal(t, exists, ok)
	if exists {
		require.Equal(t, wantURL, definition.URL)
		require.Equal(t, disabled, definition.Disabled)
		require.Equal(t, config.MCPHttp, definition.Type)
	}
}
