package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
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

func TestRoleContractRestartRebuildDispatchesOnlyAdmittedNativeAndMCPTools(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	const serverName = "role-acceptance"
	var keptRuns, deniedRuns atomic.Int32
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: serverName}, nil)
	for _, name := range []string{"kept", "denied"} {
		modelmcp.AddTool(server, &modelmcp.Tool{Name: name}, func(context.Context, *modelmcp.CallToolRequest, struct{}) (*modelmcp.CallToolResult, any, error) {
			if name == "kept" {
				keptRuns.Add(1)
			} else {
				deniedRuns.Add(1)
			}
			return &modelmcp.CallToolResult{Content: []modelmcp.Content{&modelmcp.TextContent{Text: "role MCP reached"}}}, nil, nil
		})
	}
	httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	require.NoError(t, coord.cfg.PersistMCPConfig(config.ScopeGlobal, serverName, config.MCPConfig{Type: config.MCPHttp, URL: httpServer.URL}))
	admission := coord.cfg.SnapshotMCPAdmission(serverName)
	require.True(t, admission.Exists)
	require.True(t, admission.HasMCPInput, "a disk-backed store requires a persisted MCP admission source")
	owner, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close(context.Background())) })
	coord.mcpOwner = owner
	owner.Initialize(t.Context(), env.permissions, coord.cfg, false)
	require.NoError(t, owner.WaitForInit(t.Context()))
	state, found := owner.GetState(serverName)
	require.True(t, found)
	require.NoError(t, state.Error)
	require.Equal(t, mcp.StateConnected, state.State)
	require.ElementsMatch(t, []string{"kept", "denied"}, owner.GetServerToolNames(serverName))
	external := tools.GetMCPTools(env.permissions, coord.cfg, env.workingDir, owner)
	require.Len(t, external, 2)
	cfg, _ := coord.cfg.Snapshot()
	var kept, denied string
	var definitions []cliprovider.BridgeTool
	for _, tool := range external {
		info := tool.Info()
		name := info.Name
		schema, err := json.Marshal(map[string]any{"type": "object", "properties": info.Parameters, "required": info.Required})
		require.NoError(t, err)
		if tool.MCPToolName() == "kept" {
			kept = name
		} else {
			denied = name
		}
		definitions = append(definitions, cliprovider.BridgeTool{
			WireName: serverName + "__" + tool.MCPToolName(), CanonicalName: name,
			InputSchema: schema,
		})
	}
	require.Equal(t, "mcp_"+serverName+"_kept", kept)
	require.Equal(t, "mcp_"+serverName+"_denied", denied)
	roleTools := []string{"view", kept}
	cfg.Roles = map[string]config.CustomRole{
		"analyst": {SelectedModel: cfg.Models[config.SelectedModelTypeSmart], Tools: &roleTools},
	}
	contract, err := ResolveRoleContract(cfg, "analyst", nil)
	require.NoError(t, err)

	// Close and reopen the contract database: no in-memory role binding survives.
	directory := t.TempDir()
	conn, err := db.Connect(t.Context(), directory)
	require.NoError(t, err)
	store := session.NewService(db.New(conn), conn)
	child, err := store.Create(t.Context(), "role restart")
	require.NoError(t, err)
	require.NoError(t, store.(session.SessionRoleContractStore).BindSessionRoleContract(t.Context(), child.ID, contract))
	require.NoError(t, db.Release(directory))
	conn, err = db.Connect(t.Context(), directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(directory)) })
	store = session.NewService(db.New(conn), conn)
	restored, found, err := store.(session.SessionRoleContractStore).GetSessionRoleContract(t.Context(), child.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, contract.Equal(restored))

	changed := cfg.Roles["analyst"]
	changed.Tools = nil
	cfg.Roles["analyst"] = changed
	path := filepath.Join(env.workingDir, "role-proof.txt")
	require.NoError(t, os.WriteFile(path, []byte("role native reached\n"), 0o600))
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallRoleContract(ctx, restored)
	t.Cleanup(callBuildScopeFrom(ctx).close)
	base := config.Agent{AllowedTools: []string{"view", "write"}}
	built, err := coord.buildPolicyTools(ctx, cfg, base, false)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"view", kept}, pinnedToolNames(built.tools))
	bridge, err := newCLIToolBridgeForContext(ctx, built.leaves, testPinnedInvoke, definitions)
	require.NoError(t, err)
	var advertised []string
	for _, tool := range bridge.Tools() {
		advertised = append(advertised, tool.CanonicalName)
		require.True(t, json.Valid(tool.InputSchema))
	}
	require.ElementsMatch(t, []string{"view", kept}, advertised)
	for _, cli := range []bool{false, true} {
		for _, name := range []string{"view", kept} {
			input := `{}`
			if name == "view" {
				key := "file_path"
				if cli {
					key = "path"
				}
				raw, err := json.Marshal(map[string]string{key: path})
				require.NoError(t, err)
				input = string(raw)
			}
			call := fantasy.ToolCall{ID: "role-dispatch", Name: name, Input: input}
			var response fantasy.ToolResponse
			if cli {
				response, err = bridge.Call(ctx, call)
			} else {
				leaf, resolveErr := built.leaves.resolve(name)
				require.NoError(t, resolveErr)
				response, err = leaf.Run(ctx, call)
				forged, forgedErr := leaf.Run(ctx, fantasy.ToolCall{ID: "forged", Name: denied, Input: `{}`})
				require.NoError(t, forgedErr)
				require.True(t, forged.IsError)
				require.Contains(t, forged.Content, ErrRoleToolDenied.Error())
			}
			require.NoError(t, err)
			require.False(t, response.IsError, "%s", response.Content)
			if name == "view" {
				require.Contains(t, response.Content, "role native reached")
			} else {
				require.Contains(t, response.Content, "role MCP reached")
			}
		}
	}
	for _, name := range []string{"write", denied} {
		_, err := built.leaves.resolve(name)
		require.Error(t, err, "rebuild cannot recover tools outside the durable ceiling")
		_, err = bridge.Call(t.Context(), fantasy.ToolCall{Name: name, Input: `{}`})
		require.Error(t, err, "lost invocation context cannot widen the pinned CLI bridge")
	}
	closed := WithCallRoleContract(ctx, &session.RoleContract{Version: session.RoleContractVersion, Tools: &[]string{}})
	_, err = bridge.Call(closed, fantasy.ToolCall{Name: kept, Input: `{}`})
	require.ErrorIs(t, err, ErrRoleToolDenied, "a stale bridge must honor the current narrower ceiling")
	require.EqualValues(t, 2, keptRuns.Load())
	require.Zero(t, deniedRuns.Load())
	require.True(t, contract.Equal(restored), "dispatch and config cap removal cannot rewrite durable authority")

	folder := newFolderScope(t, env.workingDir, permission.FileOpRead)
	scopedCtx := WithCallOptions(t.Context(), &CallOptions{FolderScope: &folder, RoleContract: restored.Clone()})
	t.Cleanup(callBuildScopeFrom(scopedCtx).close)
	scoped, err := coord.buildPolicyTools(scopedCtx, cfg, base, false)
	require.NoError(t, err)
	require.Empty(t, scoped.tools, "folder-scope substitutions and MCP removal cannot widen a durable role cap")
	scopedBridge, err := newCLIToolBridgeForContext(scopedCtx, scoped.leaves, testPinnedInvoke, definitions)
	require.NoError(t, err)
	require.Empty(t, scopedBridge.Tools())
	_, err = scopedBridge.Call(scopedCtx, fantasy.ToolCall{Name: kept, Input: `{}`})
	require.Error(t, err)
	require.EqualValues(t, 2, keptRuns.Load(), "a removed scope capability must not reach the external server")

	for _, test := range []struct {
		name, tools string
		want        []string
	}{
		{name: "missing", want: []string{"view", "write", kept, denied}},
		{name: "empty", tools: `,"tools":[]`},
		{name: "wildcards", tools: `,"tools":["v?ew","mcp_*_kept"]`, want: []string{"view", kept}},
		{name: "exact case", tools: `,"tools":["View","MCP_*"]`},
		{name: "literal brackets", tools: `,"tools":["[view]","mcp_[role-acceptance]_kept"]`},
		{name: "null", tools: `,"tools":null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var definition config.CustomRole
			err := json.Unmarshal([]byte(`{"provider":"smart-provider","model":"smart-model"`+test.tools+`}`), &definition)
			if test.name == "null" {
				require.ErrorContains(t, err, "null")
				return
			}
			require.NoError(t, err)
			cfg.Roles["analyst"] = definition
			admitted, err := ResolveRoleContract(cfg, "analyst", nil)
			require.NoError(t, err)
			caseCtx := WithCallRoleContract(t.Context(), admitted)
			t.Cleanup(callBuildScopeFrom(caseCtx).close)
			built, err := coord.buildPolicyTools(caseCtx, cfg, base, false)
			require.NoError(t, err)
			require.ElementsMatch(t, test.want, pinnedToolNames(built.tools))
			bridge, err := newCLIToolBridgeForContext(caseCtx, built.leaves, testPinnedInvoke, definitions)
			require.NoError(t, err)
			var names []string
			for _, tool := range bridge.Tools() {
				names = append(names, tool.CanonicalName)
				require.True(t, json.Valid(tool.InputSchema))
			}
			require.ElementsMatch(t, test.want, names, "native, external MCP, and CLI must expose the same admitted ceiling")
		})
	}
	t.Run("unrestricted role cannot elevate base native or MCP grants", func(t *testing.T) {
		cfg.Roles["analyst"] = config.CustomRole{SelectedModel: cfg.Models[config.SelectedModelTypeSmart]}
		admitted, err := ResolveRoleContract(cfg, "analyst", nil)
		require.NoError(t, err)
		caseCtx := WithCallRoleContract(t.Context(), admitted)
		t.Cleanup(callBuildScopeFrom(caseCtx).close)
		limited := config.Agent{AllowedTools: []string{"view"}, AllowedMCP: map[string][]string{}}
		built, err := coord.buildPolicyTools(caseCtx, cfg, limited, false)
		require.NoError(t, err)
		require.Equal(t, []string{"view"}, pinnedToolNames(built.tools))
		bridge, err := newCLIToolBridgeForContext(caseCtx, built.leaves, testPinnedInvoke, definitions)
		require.NoError(t, err)
		require.Len(t, bridge.Tools(), 1)
		require.Equal(t, "view", bridge.Tools()[0].CanonicalName)
		for _, name := range []string{"write", kept, denied} {
			_, err := bridge.Call(caseCtx, fantasy.ToolCall{Name: name, Input: `{}`})
			require.Error(t, err)
		}
		require.EqualValues(t, 2, keptRuns.Load())
		require.Zero(t, deniedRuns.Load())
	})
}
