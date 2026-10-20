package agent

// Deferred-construction lifetime regression: a prune between two dispatches
// of the SAME armed deferred tool deletes the identity-index entry together
// with the construction record while the tool's pin-once identity
// (ensurePinned) stays minted. Without the production repair, dispatch 2
// re-creates only the construction record (holdConstruction) and the root
// goes terminal without a Settled envelope: the executor's ResolveLeaf
// fails with "unknown pinned registry identity". Removing the holdPinned
// repin (or reverting to the original holdConstruction behavior) reproduces
// exactly that failure on dispatch 2; removing the inv.RegistryID refresh
// makes the executor resolve the stale id instead of the live one.

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func TestDeferredRepinAfterPruneReconstructsIndexEntry(t *testing.T) {
	// No t.Parallel: the lifecycle env shares folder-scope globals with the
	// other lifecycle tests (same choice as the lifecycle suite).
	env := newLifecycleInstall(t)
	defer env.leaf.letGo()

	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	require.NoError(t, err)
	bind := &batchCoordinatorBinding{ingress: ingress, diskBindings: env.install.DiskBindings}

	scope := newBuildResourceScope()
	built, err := newBatchWorkToolBuild(bind, []fantasy.AgentTool{env.leaf}, scope, true)
	require.NoError(t, err)
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
	require.NotNil(t, ingressTool.bind, "the deferred build must carry the binding")

	// Dispatch 1: pins lazily, holds the construction, and runs the real
	// executor end to end (the leaf completes; nothing is gated).
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "repin-session-1")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "m-repin-1")
	call := fantasy.ToolCall{ID: "call-repin-1", Name: "fs_write", Input: `{"path":"` + env.targetPath + `"}`}
	_, err = armed.Run(ctx, call)
	require.NoError(t, err, "the first dispatch must cross the real ingress end to end")
	require.Equal(t, 1, env.leaf.runCount())

	// Drop the fixture's own Construction pin: the oracle below is the FULL
	// manager, exactly like the shared-dispatch test.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		owners, constructions, registries := env.install.DiskBindings.OwnerDiagnostics()
		return owners == 0 && constructions == 0 && registries == 0 &&
			env.install.Runtime.RegistryCount() == 0
	}, 10*time.Second, 10*time.Millisecond,
		"after disposal the capability manager and registry index must be exactly zero")

	// The pinned identity survived in the TOOL but not in the index: the
	// prune deleted the identity-index entry together with the construction
	// record. This is the defect precondition, proven.
	ingressTool.mu.Lock()
	firstID := ingressTool.id
	ingressTool.mu.Unlock()
	require.NotEmpty(t, firstID, "the deferred tool keeps its minted pin-once identity")
	_, err = env.registries.Resolve(firstID)
	require.Error(t, err, "the prune deleted the identity-index entry the tool still names")

	// Dispatch 2 through the SAME armed tool: with the production repair the
	// tool detects the stale index, re-pins a fresh identity, holds ITS
	// construction, and the invocation names the live registry.
	ctx = context.WithValue(context.Background(), tools.SessionIDContextKey, "repin-session-2")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "m-repin-2")
	call = fantasy.ToolCall{ID: "call-repin-2", Name: "fs_write", Input: `{"path":"` + env.targetPath + `"}`}
	_, err = armed.Run(ctx, call)
	require.NoError(t, err, "the repair must re-pin and dispatch despite the stale index")
	ingressTool.mu.Lock()
	secondID := ingressTool.id
	ingressTool.mu.Unlock()
	require.NotEmpty(t, secondID)
	require.NotEqual(t, firstID, secondID, "the tool must re-pin under a fresh identity")
	// The re-pinned identity may already be pruned once dispatch 2 settled
	// (zero retention), so it is NOT resolved here: the successful dispatch
	// above plus the execution count prove the invocation named a live one.
	require.Equal(t, 2, env.leaf.runCount(), "exactly one execution per dispatch, no duplicate")

	// The re-pin is not process-lifetime retention: it dies by prune exactly
	// like the first pin.
	require.Eventually(t, func() bool {
		owners, constructions, registries := env.install.DiskBindings.OwnerDiagnostics()
		return owners == 0 && constructions == 0 && registries == 0 &&
			env.install.Runtime.RegistryCount() == 0
	}, 10*time.Second, 10*time.Millisecond,
		"the re-pinned identity must return the manager and index exactly to zero")
}
