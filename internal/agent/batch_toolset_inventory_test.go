package agent

// R12-H1 inventory tests: per-toolset batch control inventories, plus the
// old-name independence acceptance test.
//
// Static anchors each subtest pins (see the per-case comments):
//   - internal/config/config.go allToolNames: the append of the five
//     native names into the coder's AllowedTools (SetupAgents bakes them at
//     fixture-build time, so the flag is set BEFORE the fixture).
//   - internal/agent/batch_allowed_controls.go batchControlAvailability: the
//     by-name grant loop for batch_status/batch_output/batch_stop, the
//     constructed-presence grant for await_tasks, and the work-leaf grant
//     for batch_run/batch_add.
//   - internal/agent/coordinator_tools.go buildToolsAgentConfigForCall +
//     workerLayeredToolNames grants batch_stop only with trusted
//     descendant-only scope enforced by Stop.
//   - internal/agent/coordinator_tools.go applyCallFolderScope +
//     folderScopeCommandToolSet: the flag-on command set carries the native
//     twins so they strip with the command tools.
//   - internal/agent/coordinator_tools_reviewer.go reviewerReadOnlyToolNames:
//     the reviewer role's read-only allowlist holds no native controls.
//   - internal/agent/coordinator_tools.go applyNoRealWorkspaceToolFloor:
//     the no-real-workspace floor strips constructed tools, never by name.

import (
	"context"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// batchInventoryNativeControls are the five native batch control names.
var batchInventoryNativeControls = []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"}

// batchInventoryControls mints the five control tools the model-control
// inventory installs, mirroring TestFlaggedAssemblyRegistersOnlyControlsAllowedByRole.
func batchInventoryControls() []fantasy.AgentTool {
	controls := make([]fantasy.AgentTool, 0, 5)
	for _, name := range batchInventoryNativeControls {
		controls = append(controls, fantasy.NewAgentTool(name, "control", func(context.Context, batchModelParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		}))
	}
	// The stub hands the SAME tool objects to every build (parent and
	// sub-agent builds run concurrently); fantasy's Info() lazily writes
	// schema.Required, so warm it once here, before sharing.
	for _, control := range controls {
		_ = control.Info()
	}
	return controls
}

// bindBatchInventoryControls stores the flag-on binding on the coordinator
// (controls inventory + fake ingress + empty disk registry) with cleanup.
func bindBatchInventoryControls(t *testing.T, coord *coordinator) {
	t.Helper()
	bind := &batchCoordinatorBinding{
		ingress:      &fakeBatchIngress{},
		controls:     modelControlInventory{tools: batchInventoryControls()},
		diskBindings: NewDiskProviderRegistry(),
	}
	coordinatorBatchBindings.Store(coord, bind)
	t.Cleanup(func() { coordinatorBatchBindings.Delete(coord) })
}

// batchInventoryBuildNames runs ONE buildTools with trusted call state
// (minted per build, like the production per-call assembly context) and
// returns the built tool names. tune customizes this build's CallOptions
// (ModelRole/FolderScope/DisableSubAgents); the binding (flag ON) must be
// stored by the caller before invoking this for a flag-on build.
func batchInventoryBuildNames(t *testing.T, coord *coordinator, agentCfg config.Agent, isSubAgent bool, tune func(*CallOptions)) []string {
	t.Helper()
	callOpts := &CallOptions{}
	if tune != nil {
		tune(callOpts)
	}
	scope := EnsureCallBuildScope(callOpts)
	require.NotNil(t, scope, "the trusted call state carries its own build scope")
	defer scope.close()
	ctx := WithCallOptions(batchCallCtx("batch-inventory"), callOpts)
	built, err := coord.buildTools(ctx, coord.cfg.Config(), agentCfg, isSubAgent)
	require.NoError(t, err)
	names := make([]string, 0, len(built))
	for _, tool := range built {
		names = append(names, tool.Info().Name)
	}
	return names
}

// newBatchInventoryCoordinator builds the worker-tool test coordinator (the
// batch world has no in-process ledger to attach; the assembly reads only
// the coordinator's own runtime state). The flag must be set by the caller
// BEFORE this runs whenever the case's AllowedTools must include the native
// names (SetupAgents bakes allToolNames() at fixture-build time).
func newBatchInventoryCoordinator(t *testing.T, env fakeEnv, includeWorker bool) *coordinator {
	t.Helper()
	return newWorkerToolTestCoordinator(t, env, includeWorker)
}

// contains reports whether names holds want.
func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestBatchToolsetInventoryBothPreparationModes (name kept for the lane
// disposition trace) runs, per toolset family, ONE batch-only build and
// asserts its control inventory. The deleted flag-OFF halves (they asserted
// the legacy runtime's job_output/job_kill coexistence) are obsolete
// contracts: the batch runtime is unconditional and those tools are removed.
// AllowedTools (and therefore the by-name grants) is baked ON at
// fixture-build time in every case.
func TestBatchToolsetInventoryBothPreparationModes(t *testing.T) {
	// The fixture must bake the flag-ON AllowedTools: setenv FIRST.

	t.Run("default coder", func(t *testing.T) {
		env := testEnv(t)
		coord := newBatchInventoryCoordinator(t, env, false)
		coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
		require.True(t, ok, "coder agent must be configured")

		// Batch-only: all five natives present once the binding is in; no
		// binding => no control placeholders.
		noBind := batchInventoryBuildNames(t, coord, coderCfg, false, nil)
		for _, name := range batchInventoryNativeControls {
			require.NotContains(t, noBind, name, "an unbound build must never surface native control %q", name)
		}
		bindBatchInventoryControls(t, coord)
		on := batchInventoryBuildNames(t, coord, coderCfg, false, nil)
		for _, name := range batchInventoryNativeControls {
			require.Contains(t, on, name, "the coder build must surface native control %q", name)
		}
		require.Contains(t, on, "await_tasks")
	})

	t.Run("worker sub-agent", func(t *testing.T) {
		env := testEnv(t)
		coord := newBatchInventoryCoordinator(t, env, true)
		taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
		require.True(t, ok, "task agent must be configured")

		// Worker layering grants batch_stop; its actual invocation carries
		// trusted descendant-only scope enforced by Stop.
		bindBatchInventoryControls(t, coord)
		on := batchInventoryBuildNames(t, coord, taskCfg, true, nil)
		require.Contains(t, on, "batch_status")
		require.Contains(t, on, "batch_output")
		require.Contains(t, on, "batch_stop", "worker stop is granted with enforced descendant-only authorization")
		require.NotContains(t, on, "job_output", "the removed legacy poll tool is gone from the worker layering")
		require.Contains(t, on, "batch_run")
		require.Contains(t, on, "batch_add")
		require.Contains(t, on, "await_tasks")
	})

	t.Run("reviewer role", func(t *testing.T) {
		env := testEnv(t)
		coord := newBatchInventoryCoordinator(t, env, false)
		coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
		require.True(t, ok, "coder agent must be configured")
		reviewer := func(opts *CallOptions) { opts.ModelRole = config.SelectedModelTypeReviewer }

		// reviewerReadOnlyToolNames keeps none of the native controls —
		// parity with the legacy names it never held.
		bindBatchInventoryControls(t, coord)
		on := batchInventoryBuildNames(t, coord, coderCfg, false, reviewer)
		require.NotContains(t, on, "batch_status")
		require.NotContains(t, on, "batch_output")
		require.NotContains(t, on, "batch_stop")
		require.Contains(t, on, "view", "the reviewer's read-only set must keep view")
	})

	scopedCase := func(t *testing.T, keepCommands bool) {
		env := testEnv(t)
		coord := newBatchInventoryCoordinator(t, env, false)
		coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
		require.True(t, ok, "coder agent must be configured")
		scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
			WorkingDir:       env.workingDir,
			KeepCommandTools: keepCommands,
			Entries: []permission.FolderScopeEntry{
				{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead, permission.FileOpCreate}},
			},
		})
		require.NoError(t, err)
		scoped := func(opts *CallOptions) { opts.FolderScope = &scope }

		// The native twins strip with the command tools; the by-name read
		// controls survive exactly when the commands do.
		bindBatchInventoryControls(t, coord)
		on := batchInventoryBuildNames(t, coord, coderCfg, false, scoped)
		if keepCommands {
			require.Contains(t, on, "batch_status")
			require.Contains(t, on, "batch_output")
			require.Contains(t, on, "batch_stop")
			require.Contains(t, on, "bash")
		} else {
			require.NotContains(t, on, "batch_status", "the native twins strip with the command tools")
			require.NotContains(t, on, "batch_output")
			require.NotContains(t, on, "batch_stop")
			require.Contains(t, on, "batch_run", "explicit batches of work the scoped set can run stay granted")
			require.Contains(t, on, "batch_add")
			require.Contains(t, on, "fs_read", "sanity: the scope's granted fs_read stays")
		}
	}

	t.Run("scoped, commands kept", func(t *testing.T) { scopedCase(t, true) })
	t.Run("scoped, commands stripped", func(t *testing.T) { scopedCase(t, false) })

	t.Run("restricted no-real-workspace floor, sub-agents banned", func(t *testing.T) {
		env := testEnv(t)
		coord := newBatchInventoryCoordinator(t, env, false)
		cfg := coord.cfg.Config()
		if cfg.Options == nil {
			t.Fatalf("fixture config must carry a non-nil Options for the NoRealWorkspace floor")
		}
		cfg.Options.NoRealWorkspace = true
		coderCfg, ok := cfg.Agents[config.AgentCoder]
		require.True(t, ok, "coder agent must be configured")
		banned := func(opts *CallOptions) { opts.DisableSubAgents = true }

		// The floor strips CONSTRUCTED tools, not by-name grants — the
		// native names survive in AllowedTools, so the read controls stay.
		// The floor also leaves the non-host work leaves (todos, fetch,
		// sourcegraph) constructed and allowed, so batch_run/batch_add stay
		// granted; the delegation tool is gone with the sub-agent ban.
		bindBatchInventoryControls(t, coord)
		on := batchInventoryBuildNames(t, coord, coderCfg, false, banned)
		require.Contains(t, on, "await_tasks")
		require.Contains(t, on, "batch_status")
		require.Contains(t, on, "batch_output")
		require.Contains(t, on, "batch_stop")
		require.Contains(t, on, "batch_run", "todos/fetch/sourcegraph survive the floor and stay batchable")
		require.Contains(t, on, "batch_add")
		require.NotContains(t, on, "bash")
		require.NotContains(t, on, "agent")
	})
}

// TestWorkerBatchGrantsNativeReadsIndependentlyOfLegacy pins the H4-cutover
// safety of the worker layering (batch_allowed_controls.go
// workerToolNamesBatch): the native batch_status/batch_output grants are
// declared unconditionally -- appended to whatever list the caller supplies,
// never derived from any legacy entry name.
func TestWorkerBatchGrantsNativeReadsIndependentlyOfLegacy(t *testing.T) {
	t.Parallel()

	// The production worker list (post-cutover: it carries no poll-tool
	// entry at all) always gets the native reads layered in.
	batched := workerToolNamesBatch(workerToolNames)
	require.NotContains(t, batched, "job_output",
		"the deleted legacy poll name is absent from the worker layering")
	require.Contains(t, batched, "batch_status")
	require.Contains(t, batched, "batch_output")
	// Worker stop is available only with trusted scoped authorization on the
	// installed per-call tool; the detached worker-name helper grants it.
	require.Contains(t, batched, "batch_stop", "worker stop is available through the scoped native control")

	// Independence proof: a stripped list that carries NONE of the native
	// read names still comes out with them appended. Under any derivation
	// that translated a legacy entry name, the grants would silently vanish
	// here.
	stripped := make([]string, 0, len(workerToolNames))
	for _, name := range workerToolNames {
		if name == "batch_status" || name == "batch_output" {
			continue
		}
		stripped = append(stripped, name)
	}
	simulated := workerToolNamesBatch(stripped)
	require.NotContains(t, simulated, "job_output")
	require.Contains(t, simulated, "batch_status",
		"native read grants must survive a grant list that lacks them")
	require.Contains(t, simulated, "batch_output",
		"native read grants must survive a grant list that lacks them")
	// The rest of the layering is untouched by the cutover: await_tasks and
	// bash are declared in coordinator_tools.go's workerToolNames
	// independent of job_output.
	require.Contains(t, simulated, "await_tasks")
	require.Contains(t, simulated, "bash")
}

func TestWorkerStopScopeComesFromTrustedCoordinatorBuild(t *testing.T) {
	env := testEnv(t)
	coord := newBatchInventoryCoordinator(t, env, true)
	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "task agent must be configured")

	var (
		capturedMu sync.Mutex
		captured   []BatchInvocation
	)
	controls := make([]fantasy.AgentTool, 0, 1)
	controls = append(controls, fantasy.NewAgentTool("batch_stop", "control", func(context.Context, batchModelParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	}))
	controlInstaller := modelControlCapture{
		modelControlInventory: modelControlInventory{tools: controls},
		capture: func(inv BatchInvocation) {
			capturedMu.Lock()
			defer capturedMu.Unlock()
			captured = append(captured, inv)
		},
	}
	bind := &batchCoordinatorBinding{
		ingress:      &fakeBatchIngress{},
		controls:     controlInstaller,
		diskBindings: NewDiskProviderRegistry(),
	}
	coordinatorBatchBindings.Store(coord, bind)
	t.Cleanup(func() { coordinatorBatchBindings.Delete(coord) })

	build := func(isSubAgent bool, callID string) {
		t.Helper()
		callOpts := &CallOptions{}
		scope := EnsureCallBuildScope(callOpts)
		require.NotNil(t, scope)
		defer scope.close()
		ctx := WithCallOptions(batchCallCtx(callID), callOpts)
		cfg := coord.cfg.Config().Agents[config.AgentCoder]
		if isSubAgent {
			cfg = taskCfg
		}
		built, err := coord.buildTools(ctx, coord.cfg.Config(), cfg, isSubAgent)
		require.NoError(t, err)
		require.NotEmpty(t, built)
	}

	build(true, "trusted-worker")
	build(false, "trusted-root")
	capturedMu.Lock()
	defer capturedMu.Unlock()
	seen := map[string]int{}
	for _, inv := range captured {
		seen[inv.SessionID]++
		switch inv.SessionID {
		case "trusted-worker":
			require.True(t, inv.WorkerStopScope, "sub-agent build receives trusted worker stop authority")
		case "trusted-root":
			require.False(t, inv.WorkerStopScope, "root build does not receive worker stop authority")
		}
	}
	require.Positive(t, seen["trusted-worker"], "worker build installed its controls")
	require.Positive(t, seen["trusted-root"], "root build installed its controls")
}

type modelControlCapture struct {
	modelControlInventory
	capture func(BatchInvocation)
}

func (m modelControlCapture) InstallBatchModelControls(reg *BatchLeafRegistry, inv BatchInvocation) ([]fantasy.AgentTool, error) {
	m.capture(inv)
	return m.tools, nil
}

// TestNativeControlsNeverResolveThroughOldNames is the R12-H1 old-name
// independence acceptance test: with the flag ON and the coder's AllowedTools
// stripped of every native name (while keeping everything else, including
// job_output and job_kill), the flag-on build must surface NONE of the
// by-name read controls. A revert of batchControlAvailability to the pre-H1
// presence lookups (present["job_output"]/present["job_kill"]) fails here
// naming the control that resolved through the old name.
func TestNativeControlsNeverResolveThroughOldNames(t *testing.T) {
	env := testEnv(t)
	coord := newBatchInventoryCoordinator(t, env, false)

	// coord.cfg.Config() returns the live config pointer (this package's
	// tests mutate cfg.Config() in place), so replace the coder's
	// AllowedTools with a filtered copy: remove all five native names,
	// keep everything else (job_output, job_kill, await_tasks, bash, view, ...).
	cfg := coord.cfg.Config()
	coderCfg, ok := cfg.Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	filtered := make([]string, 0, len(coderCfg.AllowedTools))
	for _, name := range coderCfg.AllowedTools {
		if contains(batchInventoryNativeControls, name) {
			continue
		}
		filtered = append(filtered, name)
	}
	coderCfg.AllowedTools = filtered
	cfg.Agents[config.AgentCoder] = coderCfg

	bindBatchInventoryControls(t, coord)
	names := batchInventoryBuildNames(t, coord, cfg.Agents[config.AgentCoder], false, nil)

	// The read controls are granted BY NAME only: with the native names gone
	// from AllowedTools they must vanish. The removed legacy names
	// (job_output/job_kill) resolve nothing at all any more.
	for _, name := range []string{"batch_status", "batch_output", "batch_stop"} {
		require.NotContains(t, names, name,
			"native control %q must never resolve through the legacy job_kill/job_output names", name)
	}
	require.NotContains(t, names, "job_output", "the removed legacy poll tool is gone from every build")
	require.NotContains(t, names, "job_kill", "the removed legacy stop tool is gone from every build")
}
