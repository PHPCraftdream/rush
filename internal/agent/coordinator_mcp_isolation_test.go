package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestExternalMCPProxyListToolsUsesConsumingConfig(t *testing.T) {
	env := testEnv(t)
	const (
		serverName = "application-proxy-mcp"
		toolName   = "application-proxy-tool"
	)

	server := modelmcp.NewServer(&modelmcp.Implementation{Name: serverName}, nil)
	modelmcp.AddTool(server, &modelmcp.Tool{Name: toolName}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
		return &modelmcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(
		func(*http.Request) *modelmcp.Server { return server }, nil,
	))
	t.Cleanup(httpServer.Close)

	applicationStore := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
		serverName: {Type: config.MCPHttp, URL: httpServer.URL},
	}}, "")
	owner, err := mcp.Acquire()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close(context.Background())) })
	owner.Initialize(context.Background(), env.permissions, applicationStore, false)
	require.Eventually(t, func() bool {
		state, ok := mcp.GetState(serverName)
		return ok && state.State == mcp.StateConnected && state.Counts.Tools == 1
	}, 10*time.Second, 10*time.Millisecond)

	proxy := &externalMCPProxy{cfg: applicationStore}
	got := proxy.ListTools()
	require.Len(t, got, 1)
	require.Equal(t, serverName, got[0].ServerName)
	require.Equal(t, toolName, got[0].Name)

	libraryStore := config.NewLibraryStore(&config.Config{MCP: config.MCPs{}}, "")
	require.Empty(t, (&externalMCPProxy{cfg: libraryStore}).ListTools())
}
