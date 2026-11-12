package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func task74AcceptanceTool(t *testing.T, built policyToolBuild, name string) fantasy.AgentTool {
	t.Helper()
	for _, tool := range built.tools {
		if tool.Info().Name == name {
			return tool
		}
	}
	t.Fatalf("installed tool %q is missing", name)
	return nil
}

func task74AcceptanceTools(coord *coordinator) []fantasy.AgentTool {
	return append(task74AssemblyInput(), tools.NewAskQuestionTool(), tools.NewBatchAwaitTasksTool(coord))
}

func TestTask74InstalledControlsDispatchAgainstCapturedAndCurrentRoleCaps(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	roleTools := []string{"view", "batch_run", "batch_add", "batch_status", "batch_output", "batch_stop", tools.AskQuestionToolName, tools.AwaitTasksToolName}
	contract := task74RoleContract("analyst", &roleTools, nil)
	installer := task74AssemblyInstaller("")
	coord, buildCtx, scope, _, _ := task74AssemblyFixture(t, installer, "task74-installed-role-caps", contract)
	defer scope.close()

	allowed := []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"}
	built, err := coord.finishToolAssembly(buildCtx, task74AcceptanceTools(coord), nil, false, allowed)
	require.NoError(t, err)
	require.Equal(t, 1, installer.installs)
	status := task74AcceptanceTool(t, built, "batch_status")
	ask := task74AcceptanceTool(t, built, tools.AskQuestionToolName)
	await := task74AcceptanceTool(t, built, tools.AwaitTasksToolName)

	response, err := status.Run(buildCtx, fantasy.ToolCall{
		ID: "task74-captured-status", Name: "batch_status", Input: `{"root_id":"task74-root"}`,
	})
	require.NoError(t, err)
	require.False(t, response.IsError, "%s", response.Content)
	var statusData map[string]any
	require.NoError(t, json.Unmarshal([]byte(response.Content), &statusData))
	require.Equal(t, "unknown", statusData["state"])

	forged, err := status.Run(context.Background(), fantasy.ToolCall{
		ID: "task74-forged-name", Name: "batch_stop", Input: `{"root_id":"task74-root"}`,
	})
	require.NoError(t, err)
	require.True(t, forged.IsError)
	require.Contains(t, forged.Content, ErrRoleToolDenied.Error())

	_, err = ask.Run(context.Background(), fantasy.ToolCall{
		ID: "task74-captured-ask", Name: tools.AskQuestionToolName,
		Input: `{"question":"Which environment?"}`,
	})
	var askErr *tools.AskQuestionError
	require.ErrorAs(t, err, &askErr)
	require.Equal(t, "Which environment?", askErr.Question)

	closedTools := []string{}
	closed := task74RoleContract("closed", &closedTools, nil)
	deniedOptions := &CallOptions{RoleContract: closed}
	deniedScope := EnsureCallBuildScope(deniedOptions)
	defer deniedScope.close()
	deniedCtx := WithCallOptions(WithCallRoleContract(context.Background(), closed), deniedOptions)
	for _, tc := range []struct {
		name  string
		tool  fantasy.AgentTool
		input string
	}{
		{name: "batch_run", tool: task74AcceptanceTool(t, built, "batch_run"), input: `{"spec":{"mode":"parallel","items":[{"tool":"view","input":{}}]}}`},
		{name: "batch_add", tool: task74AcceptanceTool(t, built, "batch_add"), input: `{"root_id":"task74-root","items":[{"tool":"view","input":{}}]}`},
		{name: "batch_status", tool: status, input: `{"root_id":"task74-root"}`},
		{name: "batch_output", tool: task74AcceptanceTool(t, built, "batch_output"), input: `{"root_id":"task74-root","node_id":"task74-node"}`},
		{name: "batch_stop", tool: task74AcceptanceTool(t, built, "batch_stop"), input: `{"root_id":"task74-root"}`},
		{name: tools.AskQuestionToolName, tool: ask, input: `{"question":"Which environment?"}`},
		{name: tools.AwaitTasksToolName, tool: await, input: `{"until":"all"}`},
	} {
		t.Run("current_cap_denies_"+tc.name, func(t *testing.T) {
			response, runErr := tc.tool.Run(deniedCtx, fantasy.ToolCall{
				ID: "task74-current-denial-" + tc.name, Name: tc.name, Input: tc.input,
			})
			require.NoError(t, runErr, "valid input is refused by policy, not by input decoding")
			require.True(t, response.IsError)
			require.Contains(t, response.Content, ErrRoleToolDenied.Error())
			if tc.name == tools.AskQuestionToolName {
				var deniedAsk *tools.AskQuestionError
				require.False(t, errors.As(runErr, &deniedAsk))
			}
			if tc.name == tools.AwaitTasksToolName {
				require.NotContains(t, response.Content, "batch live reader unavailable")
			}
		})
	}
}

func TestTask74InstalledControlFilteringIntersectsMultipleInheritedGroups(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	roleTools := []string{"view", "batch_run", "batch_add", "batch_status", "batch_output", "batch_stop", tools.AskQuestionToolName, tools.AwaitTasksToolName}
	contract := task74RoleContract("analyst", &roleTools, [][]string{
		{"view", "batch_status", tools.AskQuestionToolName},
		{"view", "batch_status", tools.AwaitTasksToolName},
	})
	installer := task74AssemblyInstaller("")
	coord, ctx, scope, _, _ := task74AssemblyFixture(t, installer, "task74-inherited-intersection", contract)
	defer scope.close()

	allowed := []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"}
	built, err := coord.finishToolAssembly(ctx, task74AcceptanceTools(coord), nil, false, allowed)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"view", "batch_status"}, task74ToolNames(built.tools))

	status := task74AcceptanceTool(t, built, "batch_status")
	response, err := status.Run(ctx, fantasy.ToolCall{
		ID: "task74-inherited-status", Name: "batch_status", Input: `{"root_id":"task74-root"}`,
	})
	require.NoError(t, err)
	require.False(t, response.IsError, "%s", response.Content)
	var statusData map[string]any
	require.NoError(t, json.Unmarshal([]byte(response.Content), &statusData))
	require.Equal(t, "unknown", statusData["state"])

	currentContract := task74RoleContract("analyst", &roleTools, [][]string{
		roleTools,
		{"view"},
	})
	currentCtx := WithCallRoleContract(context.Background(), currentContract)
	denied, err := status.Run(currentCtx, fantasy.ToolCall{
		ID: "task74-current-inherited-denial", Name: "batch_status", Input: `{"root_id":"task74-root"}`,
	})
	require.NoError(t, err, "valid input is refused by policy, not by input decoding")
	require.True(t, denied.IsError)
	require.Contains(t, denied.Content, ErrRoleToolDenied.Error())
}
