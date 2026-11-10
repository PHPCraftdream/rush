package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type mcpOutputTestSessions struct {
	session.Service
	store *session.MCPOutputStore
}

func (s mcpOutputTestSessions) MCPOutputStore() *session.MCPOutputStore { return s.store }

func TestMCPOutputFinalAssemblyControlsRetentionAndRetrievalAuthority(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: "final-output"}, nil)
	modelmcp.AddTool(server, &modelmcp.Tool{Name: "source"}, func(context.Context, *modelmcp.CallToolRequest, struct{}) (*modelmcp.CallToolResult, any, error) {
		return &modelmcp.CallToolResult{Content: []modelmcp.Content{&modelmcp.TextContent{Text: strings.Repeat("retained\n", tools.MaxOutputLength)}}}, nil, nil
	})
	httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	owner, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close(context.Background())) })
	cfg := *coord.cfg.Config()
	cfg.MCP = config.MCPs{"final-output": {Type: config.MCPHttp, URL: httpServer.URL}}
	coord.cfg = config.NewLibraryStore(&cfg, env.workingDir)
	coord.mcpOwner = owner
	owner.Initialize(t.Context(), nil, coord.cfg, false)
	require.NoError(t, owner.WaitForInit(t.Context()))
	transaction := func(ctx context.Context, _ string, effect func(*sql.Tx) error) error {
		tx, err := env.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		if err := effect(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	coord.sessions = mcpOutputTestSessions{Service: env.sessions, store: session.NewMCPOutputStore(transaction, transaction)}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	build := func(allowMCP bool, allowResource bool, scoped bool) map[string]fantasy.AgentTool {
		t.Helper()
		agentCfg := cfg.Agents[config.AgentCoder]
		agentCfg.AllowedTools = nil
		if allowResource {
			agentCfg.AllowedTools = []string{tools.ReadMCPResourceToolName}
		}
		agentCfg.AllowedMCP = map[string][]string{}
		if allowMCP {
			agentCfg.AllowedMCP["final-output"] = []string{"source"}
		}
		opts := &CallOptions{DisableSubAgents: true}
		if scoped {
			opts.FolderScope = &permission.FolderScope{}
		}
		scope := EnsureCallBuildScope(opts)
		defer scope.close()
		list, err := coord.buildTools(WithCallOptions(ctx, opts), &cfg, agentCfg, false)
		require.NoError(t, err)
		result := make(map[string]fantasy.AgentTool)
		for _, tool := range list {
			result[tool.Info().Name] = tool
		}
		return result
	}
	broad := build(true, true, false)
	require.Contains(t, broad, "mcp_final-output_source")
	require.Contains(t, broad, tools.ReadMCPResourceToolName)
	response, err := broad["mcp_final-output_source"].Run(ctx, fantasy.ToolCall{ID: "final-inventory-call", Name: "mcp_final-output_source", Input: `{}`})
	require.NoError(t, err)
	require.False(t, response.IsError)
	var ref string
	require.NoError(t, env.conn.QueryRowContext(t.Context(), `SELECT id FROM mcp_outputs WHERE tool_call_id = 'final-inventory-call'`).Scan(&ref))
	input, err := json.Marshal(tools.ReadMCPResourceParams{MCPName: "final-output", URI: "rush-mcp-output://" + ref})
	require.NoError(t, err)
	read := func(tool fantasy.AgentTool) fantasy.ToolResponse {
		result, err := tool.Run(ctx, fantasy.ToolCall{ID: "inventory-read", Name: tools.ReadMCPResourceToolName, Input: string(input)})
		require.NoError(t, err)
		return result
	}
	require.False(t, read(broad[tools.ReadMCPResourceToolName]).IsError)
	narrowed := build(false, true, false)
	require.NotContains(t, narrowed, "mcp_final-output_source")
	require.Contains(t, narrowed, tools.ReadMCPResourceToolName)
	require.True(t, read(narrowed[tools.ReadMCPResourceToolName]).IsError, "an old reference cannot restore a source removed from the final tool inventory")
	withoutResource := build(true, false, false)
	require.NotContains(t, withoutResource, tools.ReadMCPResourceToolName, "retention must not add a retrieval tool")
	scoped := build(true, true, true)
	require.NotContains(t, scoped, "mcp_final-output_source")
	require.NotContains(t, scoped, tools.ReadMCPResourceToolName, "folder-scope floors must remove the entire MCP escape hatch")
}

func TestMCPOutputActivationRequiresTrustedAdapter(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	access := coord.mcpOutputAccess()
	require.Nil(t, access.Store, "the base service must not activate artifact writes without finalization admission")
	require.Empty(t, access.Allowed)
	admitMCPOutputSources(access, []fantasy.AgentTool{newProviderOptionsTestTool("mcp_untrusted_prefix")})
	require.Empty(t, access.Allowed, "an arbitrary tool name must not manufacture an MCP source grant")
}
