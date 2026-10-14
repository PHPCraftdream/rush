package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func TestFinishToolAssemblyFlagOffDoesNotInstallBatchTools(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	require.False(t, batchFlagEnabled(), "flag-off baseline must stay disabled")
	c := &coordinator{asyncJobs: newWorkLedger(nil)}
	c.asyncJobs.inlineWindow = inlineWindow
	filtered := batchTestTools("bash", "view", "job_output")
	off, err := c.finishToolAssembly(context.Background(), filtered, nil, false)
	require.NoError(t, err)
	// wrapAsyncTools replaces entries in its input slice. Build a fresh
	// baseline so it does not wrap the actual path's already-wrapped entries
	// again.
	legacy := batchTestTools("bash", "view", "job_output")
	legacy = c.wrapAsyncTools(legacy)
	legacy = decoratePolicyTools(legacy, restrictedRunAuthorizer(c.permissions), nil, false)
	legacy = wrapToolsWithStallDetach(legacy)
	for _, list := range [][]fantasy.AgentTool{legacy, off.tools} {
		for _, tool := range list {
			if tool.Info().Name == "bash" {
				require.Equal(t, 1, strings.Count(tool.Info().Description, "In web and CLI sessions this tool starts asynchronously."), "bash async description must appear exactly once")
			}
		}
	}
	require.Equal(t, flagOffToolGolden(t, legacy), flagOffToolGolden(t, off.tools), "flag-off tool inventory/schema/description must be unchanged")
	for _, tool := range off.tools {
		require.NotContains(t, []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"}, tool.Info().Name)
	}
}

func flagOffToolGolden(t *testing.T, ts []fantasy.AgentTool) []string {
	t.Helper()
	var lines []string
	for _, tool := range ts {
		info := tool.Info()
		data, err := json.Marshal(info)
		require.NoError(t, err)
		lines = append(lines, info.Name+"\x00"+info.Description+"\x00"+string(data))
	}
	return lines
}

func TestBatchControlsOnlyAppearWhenAllowedInPinnedToolset(t *testing.T) {
	filtered := batchTestTools("view", "job_output", "job_kill")
	allowed := batchControlAvailability(append(filtered, batchTestTools("bash")...))
	require.Contains(t, allowed, "batch_run")
	require.Contains(t, allowed, "batch_add")
	require.Contains(t, allowed, "batch_status")
	require.Contains(t, allowed, "batch_output")
	require.Contains(t, allowed, "batch_stop")
	require.Equal(t, []string{"batch_run", "batch_add"}, batchControlAvailability(batchTestTools("bash")))
	require.Equal(t, []string{"batch_status", "batch_output"}, batchControlAvailability(batchTestTools("job_output")))
	require.Equal(t, []string{"batch_stop"}, batchControlAvailability(batchTestTools("job_kill")))
	// D1/D10 allow explicit batches of work the pinned role can already run;
	// batch controls do not widen the view-only leaf registry.
	require.Equal(t, []string{"batch_run", "batch_add"}, batchControlAvailability(batchTestTools("view")))
	require.Equal(t, []string{"await_tasks"}, batchControlAvailability(batchTestTools("await_tasks")), "await alone is visible but cannot launch work")
	require.NotContains(t, batchControlAvailability(batchTestTools("await_tasks")), "batch_run")
	require.NotContains(t, batchControlAvailability(batchTestTools("await_tasks")), "batch_add")
}

type modelControlInventory struct {
	batchModelControl
	tools []fantasy.AgentTool
}

func (m modelControlInventory) InstallBatchModelControls(*BatchLeafRegistry, BatchInvocation) ([]fantasy.AgentTool, error) {
	return m.tools, nil
}

func TestFlaggedAssemblyRegistersOnlyControlsAllowedByRole(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	c := &coordinator{asyncJobs: newWorkLedger(nil)}
	controls := make([]fantasy.AgentTool, 0, 5)
	for _, name := range []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"} {
		controls = append(controls, fantasy.NewAgentTool(name, "control", func(context.Context, batchModelParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		}))
	}
	bind := &batchCoordinatorBinding{ingress: &fakeBatchIngress{}, controls: modelControlInventory{tools: controls}}
	coordinatorBatchBindings.Store(c, bind)
	t.Cleanup(func() { coordinatorBatchBindings.Delete(c) })
	tools := batchTestTools("bash", "job_output", "job_kill")
	for _, name := range batchControlAvailability(tools) {
		if name != "await_tasks" {
			tools = append(tools, newBatchControlPlaceholder(name))
		}
	}
	built, err := c.finishToolAssembly(context.Background(), tools, nil, false)
	require.NoError(t, err)
	var names []string
	for _, tool := range built.tools {
		names = append(names, tool.Info().Name)
	}
	for _, name := range []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"} {
		require.Contains(t, names, name)
	}
}

func TestBatchModelInvocationPreservesCallScopedPolicy(t *testing.T) {
	inv := BatchInvocation{Policy: batchLeafPolicyContext{RestrictedToolset: "view"}}
	ctx := batchCallCtx("scoped-session")
	call := fantasy.ToolCall{ID: "scoped-call", Name: "batch_run"}
	got, err := modelInvocationAtCall(ctx, inv, call)
	require.NoError(t, err)
	require.Equal(t, "scoped-session", got.SessionID)
	require.Equal(t, "scoped-call", got.CallID)
	require.Equal(t, tools.GetMessageFromContext(ctx), got.MessageID)
	require.Equal(t, folderScopeSpecFrom(ctx), got.Policy.FolderScope)
	require.Equal(t, runAllowlistSpecFrom(ctx), got.Policy.RunAllowlist)
}
