package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
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
