package sdk_test

// The task #923 acceptance oracle: two application-mode Clients in one
// process get independent MCP registries. The first Open takes the
// process-wide owner exactly as before; the second must NOT fail with
// ErrOwnerBusy — it reserves a standalone owner (mcp.AcquireStandalone,
// routed through app.WithMCPOwner) — and a server configured in the first
// client must stay invisible to the second client's model-facing surfaces
// (tool schemas and rush_info output), while each client's own server
// stays visible. Closing the second client must not disturb the first
// client's live registry.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalmcp "github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/sdk"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestTwoApplicationClientsCoexistWithIndependentMCPRegistries(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const (
		serverA     = "two-app-mcp-a"
		toolA       = "two-app-tool-a"
		markerA     = "TWO_APP_CLIENT_A_OK"
		callIDA     = "two-app-rush-info-a"
		serverB     = "two-app-mcp-b"
		toolB       = "two-app-tool-b"
		markerB     = "TWO_APP_CLIENT_B_OK"
		callIDB     = "two-app-rush-info-b"
		initTimeout = 10 * time.Second
	)

	newISOLATEDMCPServer := func(serverName, toolName string) *httptest.Server {
		server := modelmcp.NewServer(
			&modelmcp.Implementation{Name: serverName},
			&modelmcp.ServerOptions{},
		)
		modelmcp.AddTool(server, &modelmcp.Tool{Name: toolName},
			func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
				return &modelmcp.CallToolResult{}, nil, nil
			})
		httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(
			func(*http.Request) *modelmcp.Server { return server }, nil,
		))
		t.Cleanup(httpServer.Close)
		return httpServer
	}

	mcpA := newISOLATEDMCPServer(serverA, toolA)
	mcpB := newISOLATEDMCPServer(serverB, toolB)
	providerA := newR17MCPIsolationProvider(t, markerA, callIDA)
	providerB := newR17MCPIsolationProvider(t, markerB, callIDB)

	writeApplicationConfig := func(dir, serverName, mcpURL string, providerURL string) {
		t.Helper()
		config := map[string]any{
			"disable_default_providers": true,
			"providers": map[string]any{
				"probe": map[string]any{
					"id": "probe", "name": "probe", "type": "openai-compat",
					"base_url": providerURL, "api_key": "probe", "discover_models": false,
					"models": []any{map[string]any{
						"id": "probe", "name": "probe", "context_window": 200000,
						"default_max_tokens": 1000,
					}},
				},
			},
			"models": map[string]any{
				"smart": map[string]string{"provider": "probe", "model": "probe"},
				"fast":  map[string]string{"provider": "probe", "model": "probe"},
			},
			"mcp": map[string]any{
				serverName: map[string]any{
					"type": "http", "url": mcpURL, "enabled_in_cli": true,
				},
			},
		}
		configData, err := json.Marshal(config)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "rush.json"), configData, 0o600))
	}

	dirA := t.TempDir()
	writeApplicationConfig(dirA, serverA, mcpA.URL, providerA.URL)
	dirB := t.TempDir()
	writeApplicationConfig(dirB, serverB, mcpB.URL, providerB.URL)

	// First client: the process-wide MCP owner, exactly the pre-#923 path.
	clientA, err := sdk.Open(context.Background(), sdk.Options{
		WorkingDir: dirA,
		MCP:        sdk.MCPAll,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientA.Close() })

	require.Eventually(t, func() bool {
		state, ok := internalmcp.GetState(serverA)
		return ok && state.State == internalmcp.StateConnected && state.Counts.Tools == 1
	}, initTimeout, 10*time.Millisecond, "client A's MCP server must connect")

	// THE acceptance point: the second application-mode Open must not fail
	// with ErrOwnerBusy while client A is live. It reserves a standalone
	// owner via sdk's AcquireStandalone fallback and app.WithMCPOwner.
	clientB, err := sdk.Open(context.Background(), sdk.Options{
		WorkingDir: dirB,
		MCP:        sdk.MCPAll,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientB.Close() })

	require.Eventually(t, func() bool {
		state, ok := internalmcp.GetState(serverB)
		return ok && state.State == internalmcp.StateConnected && state.Counts.Tools == 1
	}, initTimeout, 10*time.Millisecond, "client B's MCP server must connect alongside client A's")

	// Client A's surfaces see only client A's server.
	runA, err := clientA.Run(context.Background(), sdk.RunRequest{
		Prompt:      "Inspect rush_info and finish with the marker.",
		Mode:        sdk.RunModeJSON,
		Stdout:      io.Discard,
		HideSpinner: true,
	})
	require.NoError(t, err)
	require.Equal(t, markerA, runA.FinalText)
	firstA, secondA := providerA.mainBodies(t)
	toolNamesA := r14ToolNamesFromBody(firstA)
	require.Contains(t, toolNamesA, "mcp_"+serverA+"_"+toolA)
	require.NotContains(t, toolNamesA, "mcp_"+serverB+"_"+toolB)
	require.Contains(t, string(secondA), serverA)
	require.NotContains(t, string(secondA), serverB)
	require.NotContains(t, string(secondA), toolB)

	// Client B's surfaces see only client B's server: client A's configured
	// server must not leak into B's tool schemas or rush_info output.
	runB, err := clientB.Run(context.Background(), sdk.RunRequest{
		Prompt:      "Inspect rush_info and finish with the marker.",
		Mode:        sdk.RunModeJSON,
		Stdout:      io.Discard,
		HideSpinner: true,
	})
	require.NoError(t, err)
	require.Equal(t, markerB, runB.FinalText)
	firstB, secondB := providerB.mainBodies(t)
	toolNamesB := r14ToolNamesFromBody(firstB)
	require.Contains(t, toolNamesB, "mcp_"+serverB+"_"+toolB)
	require.NotContains(t, toolNamesB, "mcp_"+serverA+"_"+toolA)
	require.Contains(t, string(secondB), serverB)
	require.NotContains(t, string(secondB), serverA)
	require.NotContains(t, string(secondB), toolA)

	// Closing the second client tears down only its own registry entries:
	// client B's server disappears while client A's stays connected.
	closeResult := clientB.Close()
	require.False(t, closeResult.Forced, "client B's shutdown must not go forced: it would leave its MCP close in flight")
	require.Eventually(t, func() bool {
		_, ok := internalmcp.GetState(serverB)
		return !ok
	}, initTimeout, 10*time.Millisecond, "client B's registry entries must be cleaned up on Close")
	require.Never(t, func() bool {
		state, ok := internalmcp.GetState(serverA)
		return !ok || state.State != internalmcp.StateConnected
	}, 300*time.Millisecond, 20*time.Millisecond, "client B's Close must not disturb client A's MCP session")
}
