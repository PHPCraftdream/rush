package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

type policyLeafCountingAuthorizer struct {
	permission.RestrictedRunAuthorizer
	calls int
}

func (a *policyLeafCountingAuthorizer) AuthorizeRestrictedRun(opts permission.CreatePermissionRequest) (bool, bool) {
	a.calls++
	return a.RestrictedRunAuthorizer.AuthorizeRestrictedRun(opts)
}

func TestPolicyLeafRewriteDenyRestrictionOnce(t *testing.T) {
	for _, mode := range []string{"rewrite", "deny", "restricted"} {
		t.Run(mode, func(t *testing.T) {
			probe := &dispatchProbeTool{name: "bash"}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true, AllowBash: []string{"exact:echo allowed"}})
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			// Count hook invocations independently of the response metadata.
			counter := filepath.ToSlash(filepath.Join(t.TempDir(), "hooks"))
			cmd := `echo '{"updated_input":{"command":"echo allowed"}}'`
			switch mode {
			case "deny":
				cmd = `echo '{"decision":"deny","reason":"blocked"}'`
			case "restricted":
				cmd = "exit 0"
			}
			cmd = fmt.Sprintf("printf x >> '%s'; %s", counter, cmd)
			authorizer := &policyLeafCountingAuthorizer{RestrictedRunAuthorizer: svc.(permission.RestrictedRunAuthorizer)}
			wrapped := decoratePolicyTools([]fantasy.AgentTool{&asyncTool{inner: probe, name: "bash"}}, authorizer, newRunner(t, cmd), false)
			built := newPolicyToolBuild(wrapToolsWithStallDetach(wrapped))
			leaf, err := built.leaves.resolve("bash")
			require.NoError(t, err)
			require.Equal(t, []string{"hooks", "restricted", "tool"}, policyLeafBaselineStack(leaf))
			require.Equal(t, []string{"stall", "hooks", "restricted", "async", "tool"}, built.leaves.entries["bash"].layers)
			ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
			resp, err := leaf.Run(ctx, fantasy.ToolCall{ID: "call", Name: "bash", Input: `{"command":"echo denied"}`})
			require.NoError(t, err)
			count, err := os.ReadFile(counter)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(count), "x"), "hook executes exactly once")
			wantGateCalls := 1
			if mode == "deny" {
				wantGateCalls = 0
			}
			require.Equal(t, wantGateCalls, authorizer.calls, "restricted gate executes exactly once unless hook denies")
			require.Equal(t, mode != "rewrite", resp.IsError)
			require.Equal(t, mode == "rewrite", probe.called)
			if mode == "rewrite" {
				require.JSONEq(t, `{"command":"echo allowed"}`, probe.call.Input)
			}
			require.Equal(t, []string{"stall", "hooks", "restricted", "async", "tool"}, policyLeafBaselineStack(built.tools[0]), "original model stack unchanged")
		})
	}
}

func TestPolicyLeafChildHooksOmittedAndUnknownClosed(t *testing.T) {
	probe := &dispatchProbeTool{name: "view"}
	wrapped := decoratePolicyTools([]fantasy.AgentTool{probe}, nil, newRunner(t, `echo '{"decision":"deny"}'`), true)
	built := newPolicyToolBuild(wrapToolsWithStallDetach(wrapped))
	leaf, err := built.leaves.resolve("view")
	require.NoError(t, err)
	resp, err := leaf.Run(t.Context(), fantasy.ToolCall{Name: "view"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.True(t, probe.called)
	missing, err := built.leaves.resolve("bash")
	require.Error(t, err)
	require.Nil(t, missing)
	missing, err = (*policyLeafRegistry)(nil).resolve("view")
	require.Error(t, err)
	require.Nil(t, missing)
}

func TestPolicyLeafDynamicActionPreserved(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			probe := &dispatchProbeTool{name: "mcp_dynamic_tool", action: "execute"}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			spec := permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{"mcp_dynamic_tool:read"}}
			if allowed {
				spec.AllowTools = []string{"mcp_dynamic_tool:execute"}
			}
			allowlist, err := permission.BuildRunAllowlist(spec)
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			built := newPolicyToolBuild(wrapToolsWithStallDetach(decoratePolicyTools([]fantasy.AgentTool{probe}, svc.(permission.RestrictedRunAuthorizer), newRunner(t, "exit 0"), false)))
			leaf, err := built.leaves.resolve(probe.name)
			require.NoError(t, err)
			resp, err := leaf.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), fantasy.ToolCall{ID: "call", Name: probe.name})
			require.NoError(t, err)
			require.Equal(t, allowed, probe.called)
			require.Equal(t, !allowed, resp.IsError)
		})
	}
}

func TestPolicyLeafFolderCustomDiskFloor(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	cfg := coord.cfg.Config()
	cfg.Options.NoRealWorkspace = true
	scope := newFolderScope(t, env.workingDir, permission.FileOpRead, permission.FileOpList)
	agentCfg := config.Agent{AllowedTools: []string{"view", "glob", "grep", "ls", "bash", "download", "git_read", "rush_logs", "agentic_fetch", "todos"}, AllowedMCP: nil}
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprint(custom), func(t *testing.T) {
			opts := &CallOptions{FolderScope: &scope}
			if custom {
				opts.DiskProvider = fakeFloorDisk{}
			}
			built, err := coord.buildPolicyTools(WithCallOptions(t.Context(), opts), cfg, agentCfg, false)
			require.NoError(t, err)
			for _, name := range []string{"view", "glob", "grep", "ls", "bash", "download", "git_read", "rush_logs", "agentic_fetch", "fs_write"} {
				leaf, err := built.leaves.resolve(name)
				require.Error(t, err, "filtered target %s", name)
				require.Nil(t, leaf)
			}
			for _, name := range []string{"fs_read", "fs_list"} {
				leaf, err := built.leaves.resolve(name)
				if custom {
					require.NoError(t, err)
					require.Equal(t, []string{"restricted", "tool"}, policyLeafBaselineStack(leaf))
				} else {
					require.Error(t, err)
					require.Nil(t, leaf)
				}
			}
		})
	}
}

func TestPolicyLeafSessionDecorationOnce(t *testing.T) {
	probe := &dispatchProbeTool{name: "view"}
	svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
	authorizer := svc.(permission.RestrictedRunAuthorizer)
	tools := decoratePolicyTools([]fantasy.AgentTool{probe}, authorizer, nil, true)
	agent := NewSessionAgent(SessionAgentOptions{Tools: tools, RestrictedRuns: authorizer}).(*sessionAgent)
	agent.SetTools(tools)
	built := newPolicyToolBuild(agent.tools.Copy())
	leaf, err := built.leaves.resolve("view")
	require.NoError(t, err)
	require.Equal(t, []string{"logging", "restricted", "tool"}, policyLeafBaselineStack(leaf))
}
