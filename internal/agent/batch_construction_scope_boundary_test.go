package agent

// R12-H2 chunk D, acceptance item 15 (construction-boundary part, design
// §3 row "15 + construction boundary").
//
// Production entry points exercised:
//   - BindCallDiskProvider (disk_provider_call.go): mints the immutable
//     binding handle and acquires the Call owner.
//   - newBatchWorkToolBuild (batch_install_tools.go): the REAL coordinator
//     tool construction — pins the leaf registry, publishes the construction
//     record and registers the Construction owner's release in the call's
//     build-resource scope.
//   - buildResourceScope.close (disk_provider_capability.go) and the
//     manager's prune rules.
//
// Oracles: variant A constructs a toolset, invokes NOTHING and closes the
// scope — the construction record disappears while the provider binding is
// retained by its Call owner until that owner releases. Variant B drives a
// REAL finishToolAssembly assembly failure AFTER the pin (a bound
// coordinator whose BatchModelControl stub refuses InstallBatchModelControls;
// the assembly pins the registry through newBatchWorkToolBuild first, then
// fails in the controls step) — build returns an error, the call's own
// build scope is closed by the error path (failBuildScope), the Construction
// owner is released (owner set back to the pre-build count) and the pinned
// registry identity no longer resolves through the ingress. Final oracle:
// NO provider owner remains once all legitimate owners end (binding deleted,
// owner set empty).
//
// Production mutation caught: a build scope that fails to release its
// Construction owner on close (or a manager that keeps a construction/binding
// alive without any owner key) leaks provider-capturing toolsets forever;
// a manager that prunes the binding while the Call owner is still held breaks
// the same-SDK-turn second tool call.
//
// Fixture rules: real temporary SQLite stores are not needed here (no batch
// is submitted); the provider is fake BEHAVIOR (a counting fake), not a fake
// operation; the test releases its own Call and Construction owners before
// expecting deletion.

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// failingModelControls is a stub BatchModelControl whose install step ALWAYS
// fails: the shape of an assembly error raised AFTER newBatchWorkToolBuild
// has already pinned the registry.
type failingModelControls struct{ err error }

// BatchModelControl interface; the test never reaches these — the install
// step always fails first.
func (f failingModelControls) Run(context.Context, BatchInvocation, *policyLeafRegistry, batchModelSpec) (jobbatch.ID, error) {
	return jobbatch.ID(""), f.err
}

func (f failingModelControls) Add(context.Context, string, string, string, BatchInvocation, *policyLeafRegistry, []batchModelItem) ([]jobbatch.ID, error) {
	return nil, f.err
}

func (f failingModelControls) StatusNode(context.Context, string, string, string) (any, error) {
	return nil, f.err
}

func (f failingModelControls) Output(context.Context, string, string, string, uint64) (any, error) {
	return nil, f.err
}

func (f failingModelControls) Stop(context.Context, string, string, string) (any, error) {
	return nil, f.err
}

func (f failingModelControls) InstallBatchModelControls(*BatchLeafRegistry, BatchInvocation) ([]fantasy.AgentTool, error) {
	return nil, f.err
}

func TestConstructionScopeBoundary(t *testing.T) {
	// -- Variant A: construct, invoke nothing, close the call/build scope. --
	caps := NewDiskProviderRegistry()
	fake := newH2aGatingDisk()
	opts, releaseCall, err := BindCallDiskProvider(caps, fake, "h2a-construction-call-1", nil)
	require.NoError(t, err)
	bindingID := opts.DiskBindingID
	require.NotEmpty(t, bindingID)
	// The Call owner holds the binding from mint time, before any tool is
	// constructed.
	_, err = caps.Lookup(bindingID)
	require.NoError(t, err, "the binding must be resolvable from Call-owner acquisition")

	registries := NewBatchLeafRegistryStore()
	caps.setRegistryDrop(registries.Delete)
	bind := &batchCoordinatorBinding{
		ingress:      &batchWorkIngress{registries: registries, diskBindings: caps},
		diskBindings: caps,
	}
	leaf := &countingTool{name: "view"}
	scope := newBuildResourceScope()
	built, err := newBatchWorkToolBuild(bind, []fantasy.AgentTool{leaf}, scope, false)
	require.NoError(t, err, "the real construction path pins under the call's build scope")
	require.NotEmpty(t, built.registryID)

	// While the build scope is open the construction record exists: the
	// identity index resolves it and the manager keeps the pin.
	_, err = registries.Resolve(built.registryID)
	require.NoError(t, err, "pinned registry must stay published while the build scope is open")

	// The call uses the toolset ZERO times and the scope closes (final call
	// completion / resolution failure shape).
	scope.close()
	_, err = registries.Resolve(built.registryID)
	require.Error(t, err, "closing the build scope must delete the unused construction record")
	// The binding survives: the Call owner retains the PROVIDER without
	// retaining the obsolete toolset (R12-H2 §1.1).
	_, err = caps.Lookup(bindingID)
	require.NoError(t, err, "the Call owner must retain the binding after the unused construction dies")

	// -- Variant B: a REAL assembly failure AFTER the pin. The coordinator
	// is bound (BindCoordinatorBatchRuntimeBindings shape) to a stub
	// BatchModelControl whose InstallBatchModelControls returns an error;
	// finishToolAssembly pins the registry through newBatchWorkToolBuild
	// FIRST (construction owner registered in the call's build scope) and
	// only then fails in the controls step — the error path must close the
	// CALL's own build scope (failBuildScope, coordinator_tools.go) so the
	// pinned construction does not outlive the failed assembly. --
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	failing := failingModelControls{err: errors.New("assembly failure: controls install refused")}
	failedBind := &batchCoordinatorBinding{
		ingress:      &batchWorkIngress{registries: registries, diskBindings: caps},
		controls:     failing,
		diskBindings: caps,
	}
	failedCoord := &coordinator{asyncJobs: newWorkLedger(nil)}
	failedCoord.asyncJobs.inlineWindow = inlineWindow
	coordinatorBatchBindings.Store(failedCoord, failedBind)
	t.Cleanup(func() { coordinatorBatchBindings.Delete(failedCoord) })

	// Trusted call state with NO custom provider: the assembly must use the
	// call's OWN build scope (EnsureCallBuildScope), never the shared one.
	callOpts := &CallOptions{}
	failedScope := EnsureCallBuildScope(callOpts)
	require.NotNil(t, failedScope, "the trusted call state carries its own build scope")
	failedCtx := WithCallOptions(batchCallCtx("construction-boundary-b"), callOpts)

	dropped := make(map[string]struct{}, 1)
	caps.setRegistryDrop(func(id string) {
		dropped[id] = struct{}{}
		registries.Delete(id)
	})
	preBuildOwners := caps.ownerCount()
	_, err = failedCoord.finishToolAssembly(failedCtx, batchTestTools("bash", "view", "job_output"), nil, false, nil)
	caps.setRegistryDrop(registries.Delete)
	require.Error(t, err, "the controls stub must fail the assembly after the pin")
	require.Contains(t, err.Error(), "assembly failure", "the failure is the stub's, raised through the real assembly")
	require.Equal(t, preBuildOwners, caps.ownerCount(),
		"the failed assembly must release its Construction owner: owner set back to the pre-build count")
	require.NotEmpty(t, dropped, "the pinned registry identity was dropped from the index")
	for id := range dropped {
		_, err := registries.Resolve(id)
		require.Error(t, err, "the pinned registry identity of the failed assembly must no longer resolve")
	}

	// -- Final oracle: no provider owner remains once all legitimate owners
	// end. --
	require.Equal(t, 1, caps.ownerCount(), "exactly the Call owner remains before release")
	releaseCall()
	require.Equal(t, 0, caps.ownerCount(), "no owner may remain after the call owner releases")
	_, err = caps.Lookup(bindingID)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable, "the binding must be deleted when its owner set is empty")
	require.Zero(t, fake.closeCount(), "Rush borrows the provider and never closes it")
}
