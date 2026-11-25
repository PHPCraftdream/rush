package agent

import (
	"fmt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// The self-contained rewrite/deny, child-hook and dynamic-action coverage now
// lives next to the moved wrappers in
// internal/agent/runtime/policy/toolwrap/role_policy_test.go; the root keeps
// the coordinator/session integration around those fixtures.

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
				leaf, err := built.Leaves.Resolve(name)
				require.Error(t, err, "filtered target %s", name)
				require.Nil(t, leaf)
			}
			for _, name := range []string{"fs_read", "fs_list"} {
				leaf, err := built.Leaves.Resolve(name)
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
	tools := toolwrap.DecoratePolicyTools([]fantasy.AgentTool{probe}, authorizer, nil, true)
	agent := NewSessionAgent(SessionAgentOptions{Tools: tools, RestrictedRuns: authorizer}).(*sessionAgent)
	agent.SetTools(tools)
	built := toolwrap.NewPolicyToolBuild(agent.tools.Copy())
	leaf, err := built.Leaves.Resolve("view")
	require.NoError(t, err)
	require.Equal(t, []string{"logging", "restricted", "tool"}, policyLeafBaselineStack(leaf))
}
