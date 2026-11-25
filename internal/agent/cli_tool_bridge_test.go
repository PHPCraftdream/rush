package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/clibridge"
	"github.com/PHPCraftdream/rush/internal/agent/safety/agentguard"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func testPinnedInvoke(ctx context.Context, tool fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return tool.Run(ctx, call)
}

func TestCLIToolBridgePinnedMappingAndMissing(t *testing.T) {
	built := toolwrap.NewPolicyToolBuild([]fantasy.AgentTool{&dispatchProbeTool{name: "view"}, &dispatchProbeTool{name: "mcp_server_tool"}})
	external := cliprovider.BridgeTool{WireName: "server__tool", CanonicalName: "mcp_server_tool", InputSchema: json.RawMessage(`{"type":"object"}`)}
	var got fantasy.ToolCall
	bridge, err := clibridge.NewCLIToolBridge(newCLIToolBridgeForContext, built.Leaves, func(_ context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		require.Equal(t, call.Name, leaf.Info().Name, "Pinned target not raw factory")
		got = call
		return fantasy.NewTextResponse("before\n<!-- crush-claude-init:v9 -->delegate rush<!-- /crush-claude-init -->\nafter"), nil
	}, []cliprovider.BridgeTool{external})
	require.NoError(t, err)
	require.Len(t, bridge.Tools(), 2, "Only filtered existing targets advertised")
	resp, err := bridge.Call(t.Context(), fantasy.ToolCall{ID: "same-id", Name: "view", Input: `{"path":"CLAUDE.md","start_line":2,"end_line":4}`})
	require.NoError(t, err)
	require.NotContains(t, resp.Content, "delegate rush", "Read presentation must strip delegation guidance")
	require.Equal(t, "same-id", got.ID)
	require.JSONEq(t, `{"file_path":"CLAUDE.md","offset":1,"limit":3}`, got.Input)
	_, err = bridge.Call(t.Context(), fantasy.ToolCall{ID: "external-id", Name: "mcp_server_tool", Input: `{"path":"raw","glob":"raw"}`})
	require.NoError(t, err)
	require.JSONEq(t, `{"path":"raw","glob":"raw"}`, got.Input, "External canonical mapping must not translate builtin fields")
	for _, name := range []string{"bash", "Read", "server__tool", "todos"} {
		_, err = bridge.Call(t.Context(), fantasy.ToolCall{Name: name, Input: `{}`})
		require.Error(t, err, "Missing targets have no substitute")
	}
	_, err = clibridge.NewCLIToolBridge(newCLIToolBridgeForContext, built.Leaves, nil, nil)
	require.Error(t, err)
	_, err = clibridge.NewCLIToolBridge(newCLIToolBridgeForContext, built.Leaves, testPinnedInvoke, []cliprovider.BridgeTool{{WireName: "external__read", CanonicalName: "view"}})
	require.Error(t, err, "Ambiguous canonical mapping rejected")
	snapshot := bridge.Tools()
	snapshot[0].InputSchema[0] = 'x'
	require.True(t, json.Valid(bridge.Tools()[0].InputSchema), "Advertisement is immutable")
}

func TestCLIToolBridgeHooksRewriteDenyOnce(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(fmt.Sprint(deny), func(t *testing.T) {
			counter := filepath.ToSlash(filepath.Join(t.TempDir(), "hooks"))
			require.NoError(t, os.WriteFile(counter, nil, 0o600))
			command := `echo '{"updated_input":{"command":"echo allowed"}}'`
			if deny {
				command = `echo '{"decision":"deny","reason":"blocked"}'`
			}
			probe := &dispatchProbeTool{name: "bash"}
			built := toolwrap.NewPolicyToolBuild(toolwrap.DecoratePolicyTools([]fantasy.AgentTool{probe}, nil, newRunner(t, fmt.Sprintf("printf x >> '%s'; %s", counter, command)), false))
			bridge, err := clibridge.NewCLIToolBridge(newCLIToolBridgeForContext, built.Leaves, testPinnedInvoke, nil)
			require.NoError(t, err)
			resp, err := bridge.Call(t.Context(), fantasy.ToolCall{ID: "unchanged", Name: "bash", Input: `{"command":"echo denied"}`})
			require.NoError(t, err)
			count, err := os.ReadFile(counter)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(count), "x"), "Hook runs exactly once through bridge")
			require.Equal(t, deny, resp.IsError)
			require.Equal(t, !deny, probe.called)
			if !deny {
				require.Equal(t, "unchanged", probe.call.ID)
				require.JSONEq(t, `{"command":"echo allowed"}`, probe.call.Input)
			}
		})
	}
}

type cliPermissionCounter struct {
	permission.Service
	requests []permission.CreatePermissionRequest
}

func (p *cliPermissionCounter) Request(_ context.Context, request permission.CreatePermissionRequest) (bool, error) {
	p.requests = append(p.requests, request)
	return false, nil
}

func TestCLIToolBridgePermissionGuardIdentityOnce(t *testing.T) {
	perms := &cliPermissionCounter{}
	guards := 0
	leaf := fantasy.NewAgentTool("bash", "probe", func(ctx context.Context, input tools.BashParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		guards++
		if err := agentguard.CheckAll(input.Command); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		_, err := perms.Request(ctx, permission.CreatePermissionRequest{ToolCallID: call.ID, ToolName: call.Name, SessionID: tools.GetSessionFromContext(ctx)})
		return fantasy.NewTextErrorResponse("denied"), err
	})
	bridge, err := clibridge.NewCLIToolBridge(newCLIToolBridgeForContext, toolwrap.NewPolicyToolBuild([]fantasy.AgentTool{leaf}).Leaves, testPinnedInvoke, nil)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "owner-session")
	_, err = bridge.Call(ctx, fantasy.ToolCall{ID: "permission-ref", Name: "bash", Input: `{"command":"echo x"}`})
	require.NoError(t, err)
	require.Equal(t, 1, guards, "Agentguard once per accepted call")
	require.Len(t, perms.requests, 1, "Permission once per accepted call")
	require.Equal(t, "permission-ref", perms.requests[0].ToolCallID, "Permission reference unchanged")
	require.Equal(t, "owner-session", perms.requests[0].SessionID)
	response, err := bridge.Call(ctx, fantasy.ToolCall{ID: "blocked", Name: "bash", Input: `{"command":"claude"}`})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, 2, guards)
	require.Len(t, perms.requests, 1, "Agentguard denial must precede permission")
}

func TestCLIToolBridgeTransportNoFallback(t *testing.T) {
	runs := 0
	leaf := fantasy.NewAgentTool("view", "probe", func(_ context.Context, _ tools.ViewParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		runs++
		return fantasy.NewTextResponse("raw fallback"), nil
	})
	bridge, err := clibridge.NewCLIToolBridge(newCLIToolBridgeForContext, toolwrap.NewPolicyToolBuild([]fantasy.AgentTool{leaf}).Leaves, func(context.Context, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.ToolResponse{}, fmt.Errorf("transport unavailable")
	}, nil)
	require.NoError(t, err)
	_, err = bridge.Call(t.Context(), fantasy.ToolCall{ID: "fixed", Name: "view", Input: `{"path":"x"}`})
	require.ErrorContains(t, err, "transport unavailable", "Transport error must not fallback to leaf.Run")
	require.Zero(t, runs, "Raw fallback must never execute")
}

func TestCLIToolBridgeRestrictedPinnedGenerations(t *testing.T) {
	var wg sync.WaitGroup
	var calls []func()
	for _, allowed := range []bool{true, false} {
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		allowedTools := []string{"write:write"}
		if allowed {
			allowedTools = []string{"view:read"}
		}
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true, AllowTools: allowedTools})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
		probe := &dispatchProbeTool{name: "view"}
		built := toolwrap.NewPolicyToolBuild(toolwrap.DecoratePolicyTools([]fantasy.AgentTool{probe}, svc.(permission.RestrictedRunAuthorizer), nil, true))
		generation := fmt.Sprint(allowed)
		ctx := context.WithValue(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), privateFetchIngressKey{}, generation)
		bridge, err := clibridge.NewCLIToolBridge(newCLIToolBridgeForContext, built.Leaves, func(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			require.Equal(t, generation, ctx.Value(privateFetchIngressKey{}), "Pinned generation retained")
			expected, err := built.Leaves.Resolve("view")
			require.NoError(t, err)
			require.Equal(t, expected, leaf, "Invocation resolves its own registry, not another generation")
			return leaf.Run(ctx, call)
		}, nil)
		require.NoError(t, err)
		calls = append(calls, func() {
			resp, err := bridge.Call(ctx, fantasy.ToolCall{ID: generation, Name: "view", Input: `{"path":"x"}`})
			require.NoError(t, err)
			require.Equal(t, !allowed, resp.IsError, "Restricted pinned policy must fail closed")
			require.Equal(t, allowed, probe.called)
		})
	}
	for _, call := range calls {
		wg.Go(call)
	}
	wg.Wait()
}
