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
	off, err := c.finishToolAssembly(context.Background(), filtered, nil, false, nil)
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

// allowedBatchReadControls mirrors the flag-on allToolNames grants the
// default coder toolset carries.
var allowedBatchReadControls = []string{"batch_status", "batch_output", "batch_stop"}

func TestBatchControlsOnlyAppearWhenAllowedInPinnedToolset(t *testing.T) {
	full := append(batchTestTools("view", "job_output", "job_kill"), batchTestTools("bash")...)
	require.Equal(t,
		[]string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"},
		batchControlAvailability(full, allowedBatchReadControls))
	// The read/stop controls follow the by-name grant, not the presence of
	// any command tool: a bash-only build whose AllowedTools carries them
	// still offers them.
	require.Equal(t, []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"},
		batchControlAvailability(batchTestTools("bash"), allowedBatchReadControls))
	// R12-H1 old-name independence: the legacy control tools NEVER grant
	// native controls. A revert to the pre-H1 job_output/job_kill presence
	// lookups fails right here.
	require.Empty(t, batchControlAvailability(batchTestTools("job_output", "job_kill"), nil))
	// "view" is a work leaf, so run/add are offered alongside whichever
	// read/stop controls the final AllowedTools grants by name.
	require.Equal(t, []string{"batch_run", "batch_add", "batch_status", "batch_output"},
		batchControlAvailability(batchTestTools("view"), []string{"batch_status", "batch_output"}))
	require.Equal(t, []string{"batch_run", "batch_add", "batch_stop"},
		batchControlAvailability(batchTestTools("view"), []string{"batch_stop"}))
	// D1/D10 allow explicit batches of work the pinned role can already run;
	// without a by-name grant the view-only leaf registry is not widened.
	require.Equal(t, []string{"batch_run", "batch_add"}, batchControlAvailability(batchTestTools("view"), nil))
	require.Equal(t, []string{"await_tasks"},
		batchControlAvailability(batchTestTools("await_tasks"), nil), "await alone is visible but cannot launch work")
	require.NotContains(t, batchControlAvailability(batchTestTools("await_tasks"), nil), "batch_run")
	require.NotContains(t, batchControlAvailability(batchTestTools("await_tasks"), nil), "batch_add")
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
	bind := &batchCoordinatorBinding{ingress: &fakeBatchIngress{}, controls: modelControlInventory{tools: controls}, diskBindings: NewDiskProviderRegistry()}
	coordinatorBatchBindings.Store(c, bind)
	t.Cleanup(func() { coordinatorBatchBindings.Delete(c) })
	tools := batchTestTools("bash", "job_output", "job_kill")
	for _, name := range batchControlAvailability(tools, allowedBatchReadControls) {
		if name != "await_tasks" {
			tools = append(tools, newBatchControlPlaceholder(name))
		}
	}
	// Round-2 construction ownership: a build with trusted call state runs
	// non-deferred, which is the only path that installs the model controls
	// in the assembled tools (the shared default toolset defers controls to
	// per-call install). Mint trusted call state with its own build scope,
	// matching the production per-call assembly context.
	callOpts := &CallOptions{}
	scope := EnsureCallBuildScope(callOpts)
	require.NotNil(t, scope, "the trusted call state carries its own build scope")
	defer scope.close()
	ctx := WithCallOptions(batchCallCtx("flagged-assembly"), callOpts)
	built, err := c.finishToolAssembly(ctx, tools, nil, false, allowedBatchReadControls)
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
