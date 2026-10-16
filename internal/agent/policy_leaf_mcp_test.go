package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestPolicyLeafPinnedDynamicMCPFiltering(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	const serverName = "policy-leaf-server"
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: serverName}, nil)
	for _, name := range []string{"kept", "filtered"} {
		modelmcp.AddTool(server, &modelmcp.Tool{Name: name}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
			return &modelmcp.CallToolResult{}, nil, nil
		})
	}
	httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	coord.cfg.Config().MCP = config.MCPs{serverName: {Type: config.MCPHttp, URL: httpServer.URL}}
	owner, err := mcp.Acquire()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close(context.Background())) })
	coord.mcpOwner = owner
	owner.Initialize(context.Background(), env.permissions, coord.cfg, false)
	require.Eventually(t, func() bool {
		return len(tools.GetMCPTools(env.permissions, coord.cfg, env.workingDir, owner)) == 2
	}, 10*time.Second, 10*time.Millisecond)
	var kept, filtered string
	for _, tool := range tools.GetMCPTools(env.permissions, coord.cfg, env.workingDir, owner) {
		if tool.MCPToolName() == "kept" {
			kept = tool.Info().Name
		} else {
			filtered = tool.Info().Name
		}
	}
	agentCfg := config.Agent{AllowedMCP: map[string][]string{serverName: {"kept"}}}
	built, err := coord.buildPolicyTools(t.Context(), coord.cfg.Config(), agentCfg, false)
	require.NoError(t, err)
	leaf, err := built.leaves.resolve(kept)
	require.NoError(t, err)
	require.Equal(t, []string{"restricted", "tool"}, policyLeafBaselineStack(leaf))
	missing, err := built.leaves.resolve(filtered)
	require.Error(t, err)
	require.Nil(t, missing)
	// Changing the allowlist affects only a subsequent build, not this registry.
	agentCfg.AllowedMCP[serverName] = []string{"filtered"}
	next, err := coord.buildPolicyTools(t.Context(), coord.cfg.Config(), agentCfg, false)
	require.NoError(t, err)
	_, err = next.leaves.resolve(kept)
	require.Error(t, err)
	_, err = built.leaves.resolve(kept)
	require.NoError(t, err)
	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	scoped, err := coord.buildPolicyTools(WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope}), coord.cfg.Config(), agentCfg, false)
	require.NoError(t, err)
	for _, name := range []string{kept, filtered} {
		_, err := scoped.leaves.resolve(name)
		require.Error(t, err, "folder scope removes external MCP")
	}
}
