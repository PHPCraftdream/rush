package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestAcquireStandaloneCoexistsWithInstalledOwner(t *testing.T) {
	if current := currentOwner(); current != nil {
		require.NoError(t, current.Close(context.Background()))
	}
	t.Cleanup(func() {
		if current := currentOwner(); current != nil {
			_ = current.Close(context.Background())
		}
	})

	installed, err := Acquire()
	require.NoError(t, err)
	require.False(t, installed.standalone)
	require.Same(t, installed, currentOwner())

	standalone, err := AcquireStandalone()
	require.NoError(t, err)
	require.NotSame(t, installed, standalone)
	require.True(t, standalone.standalone)
	require.False(t, standalone.implicit)

	busy, err := Acquire()
	require.ErrorIs(t, err, ErrOwnerBusy)
	require.Nil(t, busy)
	require.Same(t, installed, currentOwner())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, installed.Close(ctx))
	require.NoError(t, standalone.Close(ctx))
}

func TestStandaloneOwnerPassesIdentityGuards(t *testing.T) {
	if current := currentOwner(); current != nil {
		require.NoError(t, current.Close(context.Background()))
	}
	t.Cleanup(func() {
		if current := currentOwner(); current != nil {
			_ = current.Close(context.Background())
		}
	})

	standalone, err := AcquireStandalone()
	require.NoError(t, err)

	lifecycleMu.Lock()
	require.True(t, standalone.isCurrentLocked())
	lifecycleMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, standalone.Close(ctx))

	lifecycleMu.Lock()
	require.False(t, standalone.isCurrentLocked())
	lifecycleMu.Unlock()

	installed, err := Acquire()
	require.NoError(t, err)
	lifecycleMu.Lock()
	require.True(t, installed.isCurrentLocked())
	lifecycleMu.Unlock()
	require.NoError(t, installed.Close(ctx))
}

func TestStandaloneCloseCleansOnlyOwnEntries(t *testing.T) {
	if current := currentOwner(); current != nil {
		require.NoError(t, current.Close(context.Background()))
	}
	t.Cleanup(func() {
		if current := currentOwner(); current != nil {
			_ = current.Close(context.Background())
		}
	})
	t.Cleanup(func() {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		sessions.Del("foreign-b")
		states.Del("foreign-b")
		allTools.Del("foreign-b")
	})

	standalone, err := AcquireStandalone()
	require.NoError(t, err)

	lifecycleMu.Lock()
	sessions.Set("own-a", &ClientSession{owner: standalone})
	sessions.Set("foreign-b", &ClientSession{})
	states.Set("own-a", ClientInfo{})
	states.Set("foreign-b", ClientInfo{})
	allTools.Set("own-a", []*Tool{})
	allTools.Set("foreign-b", []*Tool{})
	standalone.serverEpochs["own-a"] = 1
	lifecycleMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, standalone.Close(ctx))

	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	_, ownSession := sessions.Get("own-a")
	_, ownState := states.Get("own-a")
	_, ownTools := allTools.Get("own-a")
	_, foreignSession := sessions.Get("foreign-b")
	_, foreignState := states.Get("foreign-b")
	_, foreignTools := allTools.Get("foreign-b")
	require.False(t, ownSession)
	require.False(t, ownState)
	require.False(t, ownTools)
	require.True(t, foreignSession)
	require.True(t, foreignState)
	require.True(t, foreignTools)
}

// TestStandaloneOwnerRunsItsOwnTool pins the tier-1 binding at the package
// boundary: a standalone owner's RunTool must admit and execute against its
// OWN config store, never against the process-current (installed) owner.
// If the owner threading regresses to the package-level currentOwner
// resolution, the installed owner's store binding rejects the call with
// ErrMCPConfigStoreBusy and this test fails.
func TestStandaloneOwnerRunsItsOwnTool(t *testing.T) {
	const (
		serverInstalled = "installed-owner-mcp"
		toolInstalled   = "installed-owner-tool"
		serverOwn       = "standalone-owner-mcp"
		toolOwn         = "standalone-owner-tool"
	)

	newToolServer := func(toolName string) *httptest.Server {
		server := modelmcp.NewServer(&modelmcp.Implementation{Name: toolName}, nil)
		modelmcp.AddTool(server, &modelmcp.Tool{Name: toolName},
			func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
				return &modelmcp.CallToolResult{}, nil, nil
			})
		return httptest.NewServer(modelmcp.NewStreamableHTTPHandler(
			func(*http.Request) *modelmcp.Server { return server }, nil))
	}

	httpInstalled := newToolServer(toolInstalled)
	t.Cleanup(httpInstalled.Close)
	httpOwn := newToolServer(toolOwn)
	t.Cleanup(httpOwn.Close)

	storeInstalled := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
		serverInstalled: {Type: config.MCPHttp, URL: httpInstalled.URL},
	}}, "")
	storeOwn := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
		serverOwn: {Type: config.MCPHttp, URL: httpOwn.URL},
	}}, "")

	installed, err := Acquire()
	require.NoError(t, err)
	t.Cleanup(func() { _ = installed.Close(context.Background()) })
	standalone, err := AcquireStandalone()
	require.NoError(t, err)
	t.Cleanup(func() { _ = standalone.Close(context.Background()) })

	installed.Initialize(context.Background(), nil, storeInstalled, false)
	standalone.Initialize(context.Background(), nil, storeOwn, false)
	require.Eventually(t, func() bool {
		state, ok := GetState(serverInstalled)
		return ok && state.State == StateConnected
	}, 10*time.Second, 10*time.Millisecond, "installed owner's server must connect")
	require.Eventually(t, func() bool {
		state, ok := GetState(serverOwn)
		return ok && state.State == StateConnected
	}, 10*time.Second, 10*time.Millisecond, "standalone owner's server must connect")

	// The acceptance leg: the standalone owner executes a tool against its
	// own store while the installed owner holds the process slot.
	result, err := standalone.RunTool(context.Background(), storeOwn, serverOwn, toolOwn, "{}")
	require.NoError(t, err)
	require.Equal(t, "text", result.Type)

	// The store-binding fence still holds: the standalone owner must not
	// silently serve a different store.
	_, err = standalone.RunTool(context.Background(), storeInstalled, serverInstalled, toolInstalled, "{}")
	require.ErrorIs(t, err, ErrMCPConfigStoreBusy)
}

func TestOwnerClosePreservesOtherOwnersAcrossCloseOrders(t *testing.T) {
	for _, closeStandaloneFirst := range []bool{false, true} {
		name := "installed-first"
		if closeStandaloneFirst {
			name = "standalone-first"
		}
		t.Run(name, func(t *testing.T) {
			if current := currentOwner(); current != nil {
				require.NoError(t, current.Close(context.Background()))
			}

			newToolServer := func(toolName string) *httptest.Server {
				server := modelmcp.NewServer(&modelmcp.Implementation{Name: toolName}, nil)
				modelmcp.AddTool(server, &modelmcp.Tool{Name: toolName},
					func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
						return &modelmcp.CallToolResult{}, nil, nil
					})
				return httptest.NewServer(modelmcp.NewStreamableHTTPHandler(
					func(*http.Request) *modelmcp.Server { return server }, nil))
			}

			installedName := "close-isolation-installed"
			standaloneName := "close-isolation-standalone"
			installedTool := "close-isolation-installed-tool"
			standaloneTool := "close-isolation-standalone-tool"
			installedHTTP := newToolServer(installedTool)
			t.Cleanup(installedHTTP.Close)
			standaloneHTTP := newToolServer(standaloneTool)
			t.Cleanup(standaloneHTTP.Close)
			installedStore := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
				installedName: {Type: config.MCPHttp, URL: installedHTTP.URL},
			}}, "")
			standaloneStore := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
				standaloneName: {Type: config.MCPHttp, URL: standaloneHTTP.URL},
			}}, "")

			installed, err := Acquire()
			require.NoError(t, err)
			standalone, err := AcquireStandalone()
			require.NoError(t, err)
			eventsCtx, cancelEvents := context.WithCancel(context.Background())
			defer cancelEvents()
			events := standalone.SubscribeEvents(eventsCtx)
			t.Cleanup(func() {
				_ = standalone.Close(context.Background())
				_ = installed.Close(context.Background())
			})

			installed.Initialize(context.Background(), nil, installedStore, false)
			standalone.Initialize(context.Background(), nil, standaloneStore, false)
			require.Eventually(t, func() bool {
				state, ok := installed.GetState(installedName)
				return ok && state.State == StateConnected
			}, 5*time.Second, 10*time.Millisecond)
			require.Eventually(t, func() bool {
				state, ok := standalone.GetState(standaloneName)
				return ok && state.State == StateConnected
			}, 5*time.Second, 10*time.Millisecond)
			drainMCPEvents(events)

			beforeLease := serverLeaseFor(standaloneName)
			beforeInstalledLease := serverLeaseFor(installedName)
			t.Cleanup(func() {
				leases.release(beforeLease)
				leases.release(beforeInstalledLease)
			})
			if closeStandaloneFirst {
				require.NoError(t, standalone.Close(context.Background()))
				assertLiveOwnerTool(t, installed, installedStore, installedName, installedTool)
				installed.RefreshTools(context.Background(), installedStore, installedName)
				waitForMCPStateEvent(t, events, installedName)
			} else {
				require.NoError(t, installed.Close(context.Background()))
				later, err := Acquire()
				require.NoError(t, err)
				require.Same(t, later, currentOwner())
				require.NoError(t, later.Close(context.Background()))
				assertLiveOwnerTool(t, standalone, standaloneStore, standaloneName, standaloneTool)
				standalone.RefreshTools(context.Background(), standaloneStore, standaloneName)
				waitForMCPStateEvent(t, events, standaloneName)
			}

			if closeStandaloneFirst {
				afterLease := serverLeaseFor(installedName)
				require.Same(t, beforeInstalledLease, afterLease)
				leases.release(afterLease)
			} else {
				afterLease := serverLeaseFor(standaloneName)
				require.Same(t, beforeLease, afterLease)
				leases.release(afterLease)
			}
			if closeStandaloneFirst {
				require.NoError(t, installed.Close(context.Background()))
			} else {
				require.NoError(t, standalone.Close(context.Background()))
			}
		})
	}
}

func assertLiveOwnerTool(t *testing.T, owner *Owner, store *config.ConfigStore, name, tool string) {
	t.Helper()
	state, ok := owner.GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
	require.NotNil(t, state.Client)
	result, err := owner.RunTool(context.Background(), store, name, tool, "{}")
	require.NoError(t, err)
	require.Equal(t, "text", result.Type)
	after, ok := owner.GetState(name)
	require.True(t, ok)
	require.Same(t, state.Client, after.Client, "surviving transport must not need renewal")
}

func drainMCPEvents(events <-chan pubsub.Event[Event]) {
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func waitForMCPStateEvent(t *testing.T, events <-chan pubsub.Event[Event], name string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			require.True(t, ok)
			if event.Payload.Name == name && event.Payload.State == StateConnected {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for connected event for %q", name)
		}
	}
}
