package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type task74ControlInstaller struct {
	*installedBatchModelControl
	installs int
	omit     string
}

func (i *task74ControlInstaller) InstallBatchModelControls(registry *BatchLeafRegistry, inv BatchInvocation) ([]fantasy.AgentTool, error) {
	i.installs++
	controls, err := i.installedBatchModelControl.InstallBatchModelControls(registry, inv)
	if err != nil || i.omit == "" {
		return controls, err
	}
	filtered := controls[:0]
	for _, control := range controls {
		if control.Info().Name != i.omit {
			filtered = append(filtered, control)
		}
	}
	return filtered, nil
}

func task74AssemblyFixture(t *testing.T, controls *task74ControlInstaller, callID string, contract *session.RoleContract) (*coordinator, context.Context, *buildResourceScope, *DiskProviderRegistry, *fakeBatchIngress) {
	t.Helper()
	coord := newBatchRuntimeCoordinator()
	ingress := &fakeBatchIngress{}
	caps := NewDiskProviderRegistry()
	caps.setRegistryDrop(func(registryID string) {
		ingress.mu.Lock()
		delete(ingress.regs, registryID)
		ingress.mu.Unlock()
	})
	coordinatorBatchBindings.Store(coord, &batchCoordinatorBinding{
		ingress:      ingress,
		controls:     controls,
		diskBindings: caps,
	})
	t.Cleanup(func() { coordinatorBatchBindings.Delete(coord) })

	callOptions := &CallOptions{}
	scope := EnsureCallBuildScope(callOptions)
	require.NotNil(t, scope)
	ctx := WithCallOptions(batchCallCtx(callID), callOptions)
	if contract != nil {
		ctx = WithCallRoleContract(ctx, contract)
	}
	return coord, ctx, scope, caps, ingress
}

func task74AssemblyInstaller(omit string) *task74ControlInstaller {
	return &task74ControlInstaller{
		installedBatchModelControl: NewInstalledBatchModelControl(nil, nil, nil, nil, nil, "", "", nil, nil).(*installedBatchModelControl),
		omit:                       omit,
	}
}

func task74AssemblyInput() []fantasy.AgentTool {
	tools := batchTestTools("view")
	for _, name := range []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"} {
		tools = append(tools, newBatchControlPlaceholder(name))
	}
	return tools
}

func task74ToolNames(tools []fantasy.AgentTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Info().Name)
	}
	return names
}

func task74AssertInstalledStatus(t *testing.T, ctx context.Context, built policyToolBuild, callID string) {
	t.Helper()
	var status fantasy.AgentTool
	for _, tool := range built.tools {
		if tool.Info().Name == "batch_status" {
			status = tool
			break
		}
	}
	require.NotNil(t, status, "the surviving grant must be a concrete installed control")
	response, err := status.Run(ctx, fantasy.ToolCall{ID: callID, Name: "batch_status", Input: `{"root_id":"task74-root"}`})
	require.NoError(t, err)
	var statusData map[string]any
	require.NoError(t, json.Unmarshal([]byte(response.Content), &statusData))
	require.Equal(t, "unknown", statusData["state"], "the real installed control returns its structured status")
}

func TestTask74FinishToolAssemblyInstallsAllowedBuiltinControls(t *testing.T) {
	installer := task74AssemblyInstaller("")
	coord, ctx, scope, _, _ := task74AssemblyFixture(t, installer, "task74-builtin", nil)
	defer scope.close()

	allowed := []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"}
	built, err := coord.finishToolAssembly(ctx, task74AssemblyInput(), nil, false, allowed)
	require.NoError(t, err)
	require.Equal(t, 1, installer.installs, "trusted nonshared assembly must install its real model controls")
	for _, name := range allowed {
		require.Contains(t, task74ToolNames(built.tools), name, "an effective builtin grant must be installed")
	}
	task74AssertInstalledStatus(t, ctx, built, "task74-status")
}

func TestTask74FinishToolAssemblyAppliesRoleAndInheritedControlCeilings(t *testing.T) {
	roleTools := []string{"view", "batch_status", "batch_output"}
	contract := task74RoleContract("analyst", &roleTools, [][]string{{"view", "batch_status"}})
	installer := task74AssemblyInstaller("")
	coord, ctx, scope, _, _ := task74AssemblyFixture(t, installer, "task74-role-cap", contract)
	defer scope.close()

	// The base build grants every native control; the role's own and inherited
	// caps intersect that floor down to batch_status only.
	allowed := []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"}
	built, err := coord.finishToolAssembly(ctx, task74AssemblyInput(), nil, false, allowed)
	require.NoError(t, err)
	names := task74ToolNames(built.tools)
	require.Contains(t, names, "batch_status")
	for _, denied := range []string{"batch_run", "batch_add", "batch_output", "batch_stop"} {
		require.NotContains(t, names, denied, "installation must not surface a control denied by either role ceiling")
	}
	require.Equal(t, 1, installer.installs)
	task74AssertInstalledStatus(t, ctx, built, "task74-role-status")
}

func TestTask74FinishToolAssemblyCannotWidenBaseAllowedControls(t *testing.T) {
	installer := task74AssemblyInstaller("")
	coord, ctx, scope, _, _ := task74AssemblyFixture(t, installer, "task74-base-allowed-floor", nil)
	defer scope.close()

	built, err := coord.finishToolAssembly(ctx, task74AssemblyInput(), nil, false, []string{"batch_status"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"view", "batch_status"}, task74ToolNames(built.tools))
	for _, denied := range []string{"batch_run", "batch_add", "batch_output", "batch_stop"} {
		require.NotContains(t, task74ToolNames(built.tools), denied, "installation must not add controls outside the base allowed grant list")
	}
	require.Equal(t, 1, installer.installs, "the installer may return every control but must run once")
	task74AssertInstalledStatus(t, ctx, built, "task74-base-floor-status")
}

func TestTask74FinishToolAssemblyEmptyRoleInstallsNoTools(t *testing.T) {
	emptyTools := []string{}
	contract := task74RoleContract("closed", &emptyTools, nil)
	installer := task74AssemblyInstaller("")
	coord, ctx, scope, _, _ := task74AssemblyFixture(t, installer, "task74-empty-role", contract)
	defer scope.close()

	built, err := coord.finishToolAssembly(ctx, task74AssemblyInput(), nil, false,
		[]string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"})
	require.NoError(t, err)
	require.Empty(t, built.tools, "an explicit empty role ceiling denies every base and control tool")
	require.Zero(t, installer.installs, "no denied control should be minted when there are no effective grants")
}

func TestTask74FinishToolAssemblyRejectsMissingEffectiveControlAndCleansUp(t *testing.T) {
	installer := task74AssemblyInstaller("batch_status")
	coord, ctx, scope, caps, ingress := task74AssemblyFixture(t, installer, "task74-missing-control", nil)

	_, err := coord.finishToolAssembly(ctx, task74AssemblyInput(), nil, false, []string{"batch_status"})
	require.ErrorContains(t, err, `batch control "batch_status" was not installed`)
	require.Equal(t, 1, installer.installs)
	_, resolveErr := ingress.Registry("reg-1")
	require.Error(t, resolveErr, "failBuildScope must drop the pinned identity")
	require.Zero(t, caps.ownerCount(), "the failed assembly must close its construction scope")
	scope.close()
}
