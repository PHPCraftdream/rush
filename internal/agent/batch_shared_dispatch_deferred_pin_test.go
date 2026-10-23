package agent

// Direction-1 regression (R2-5 shared-pin tension): an EAGER default
// toolset build (bare ctx, no trusted call state) must publish NOTHING —
// no pinned registry identity, no Construction owner — and pin lazily on
// FIRST DISPATCH. One real dispatch through the armed tool must publish
// the registry, submit through SubmitBound (RegistryID nonempty forces the
// root-owning bound path in batch_ingress.submit), execute the leaf
// through the real executor, and — after the root settles and physically
// disposes — return the capability manager and the runtime registry index
// EXACTLY to zero (design §1.2: bare builders create AND close a scope; no
// process-lifetime fallback).

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func TestSharedToolsetBuildPublishesNothingAndDispatchPinsLazily(t *testing.T) {
	t.Parallel()
	env := newLifecycleInstall(t)
	defer env.leaf.letGo()

	// Drop the fixture's own Construction pin first: this test's oracle is
	// the FULL manager, so the baseline must be exactly empty.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		owners, constructions, registries := env.install.DiskBindings.OwnerDiagnostics()
		return owners == 0 && constructions == 0 && registries == 0 &&
			env.install.Runtime.RegistryCount() == 0
	}, 5*time.Second, 10*time.Millisecond, "fixture baseline must be exactly empty")

	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	require.NoError(t, err)
	bind := &batchCoordinatorBinding{ingress: ingress, diskBindings: env.install.DiskBindings}

	// EAGER (bare-ctx) build: the shared default toolset shape.
	scope := newBuildResourceScope()
	built, err := newBatchWorkToolBuild(bind, []fantasy.AgentTool{env.leaf}, scope, true)
	require.NoError(t, err)
	scope.close()

	// Oracle 1: the build published NOTHING.
	require.Empty(t, built.registryID, "a deferred build must not publish a registry identity")
	require.Zero(t, env.install.Runtime.RegistryCount(), "no registry identity may be published at build time")
	owners, constructions, registries := env.install.DiskBindings.OwnerDiagnostics()
	require.Zero(t, owners, "no owner may exist after an undispatched shared build")
	require.Zero(t, constructions, "no construction record may exist after an undispatched shared build")
	require.Zero(t, registries, "no construction index entry may exist after an undispatched shared build")

	// Find the armed work tool.
	var armed fantasy.AgentTool
	for _, tool := range built.tools {
		if tool.Info().Name == "fs_write" {
			armed = tool
			break
		}
	}
	require.NotNil(t, armed, "the shared build must arm its work tool")
	ingressTool, ok := armed.(*batchIngressTool)
	require.True(t, ok, "the armed tool must be a batchIngressTool")
	require.NotNil(t, ingressTool.bind, "the shared build's tool must carry the deferred-pin binding")

	// Dispatch ONCE through the armed tool with its own session identity
	// (distinct session id: no cross-test attach).
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "shared-dispatch-session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "m-shared-1")
	call := fantasy.ToolCall{ID: "call-shared-1", Name: "fs_write", Input: `{"path":"shared-unused.txt"}`}
	_, err = armed.Run(ctx, call)
	require.NoError(t, err, "the shared-toolset dispatch must cross the real ingress end to end")
	require.Equal(t, 1, env.leaf.runCount(), "the dispatch executed the leaf through the real executor (real bound submit)")

	// Oracle 2: after the root settles and physically disposes, everything
	// returns exactly to zero — the lazy pin left no residue.
	require.Eventually(t, func() bool {
		owners, constructions, registries := env.install.DiskBindings.OwnerDiagnostics()
		return owners == 0 && constructions == 0 && registries == 0 &&
			env.install.Runtime.RegistryCount() == 0
	}, 10*time.Second, 10*time.Millisecond,
		"the dispatched shared toolset must return the capability manager and registry index exactly to zero")
}
