package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/modelcontrol"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// TestFinishToolAssemblyFlagOffDoesNotInstallBatchTools was DELETEd as an
// obsolete contract: there is no flag-off build left to pin -- batch-only
// assembly is unconditional, and the legacy async-job wrapper seams are gone
// entirely (covered by the inventory tests below).

// allowedBatchReadControls mirrors the flag-on allToolNames grants the
// default coder toolset carries.
var allowedBatchReadControls = []string{"batch_status", "batch_output", "batch_stop"}

func TestBatchControlsOnlyAppearWhenAllowedInPinnedToolset(t *testing.T) {
	full := append(batchTestTools("view", "job_output", "job_kill"), batchTestTools("bash")...)
	require.Equal(t,
		[]string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"},
		batchinst.ControlAvailability(full, allowedBatchReadControls, batchWorkTarget))
	// The read/stop controls follow the by-name grant, not the presence of
	// any command tool: a bash-only build whose AllowedTools carries them
	// still offers them.
	require.Equal(t, []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"},
		batchinst.ControlAvailability(batchTestTools("bash"), allowedBatchReadControls, batchWorkTarget))
	// R12-H1 old-name independence: the legacy control tools NEVER grant
	// native controls. A revert to the pre-H1 job_output/job_kill presence
	// lookups fails right here.
	require.Empty(t, batchinst.ControlAvailability(batchTestTools("job_output", "job_kill"), nil, batchWorkTarget))
	// "view" is a work leaf, so run/add are offered alongside whichever
	// read/stop controls the final AllowedTools grants by name.
	require.Equal(t, []string{"batch_run", "batch_add", "batch_status", "batch_output"},
		batchinst.ControlAvailability(batchTestTools("view"), []string{"batch_status", "batch_output"}, batchWorkTarget))
	require.Equal(t, []string{"batch_run", "batch_add", "batch_stop"},
		batchinst.ControlAvailability(batchTestTools("view"), []string{"batch_stop"}, batchWorkTarget))
	// D1/D10 allow explicit batches of work the pinned role can already run;
	// without a by-name grant the view-only leaf registry is not widened.
	require.Equal(t, []string{"batch_run", "batch_add"}, batchinst.ControlAvailability(batchTestTools("view"), nil, batchWorkTarget))
	require.Equal(t, []string{"await_tasks"},
		batchinst.ControlAvailability(batchTestTools("await_tasks"), nil, batchWorkTarget), "await alone is visible but cannot launch work")
	require.NotContains(t, batchinst.ControlAvailability(batchTestTools("await_tasks"), nil, batchWorkTarget), "batch_run")
	require.NotContains(t, batchinst.ControlAvailability(batchTestTools("await_tasks"), nil, batchWorkTarget), "batch_add")
}

type modelControlInventory struct {
	batchModelControl
	tools []fantasy.AgentTool
}

func (m modelControlInventory) InstallBatchModelControls(*toolwrap.PolicyLeafRegistry, BatchInvocation) ([]fantasy.AgentTool, error) {
	return m.tools, nil
}

func TestFlaggedAssemblyRegistersOnlyControlsAllowedByRole(t *testing.T) {
	c := newBatchRuntimeCoordinator()
	controls := make([]fantasy.AgentTool, 0, 5)
	for _, name := range []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"} {
		controls = append(controls, fantasy.NewAgentTool(name, "control", func(context.Context, modelcontrol.Params, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		}))
	}
	bind := &batchCoordinatorBinding{ingress: &fakeBatchIngress{}, controls: modelControlInventory{tools: controls}, diskBindings: NewDiskProviderRegistry()}
	coordinatorBatchBindings.Store(c, bind)
	t.Cleanup(func() { fixtureCoordinatorBatchBindings.Delete(c) })
	tools := batchTestTools("bash", "job_output", "job_kill")
	granted := batchinst.ControlAvailability(tools, allowedBatchReadControls, batchWorkTarget)
	for _, name := range granted {
		if name != "await_tasks" {
			tools = append(tools, modelcontrol.NewPlaceholder(name))
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
	defer scope.Close()
	ctx := WithCallOptions(batchCallCtx("flagged-assembly"), callOpts)
	built, err := c.finishToolAssembly(ctx, tools, nil, false, granted)
	require.NoError(t, err)
	var names []string
	for _, tool := range built.Tools {
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
