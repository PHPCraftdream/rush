package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBindCoordinatorBatchRuntimeBindings_BindsBuiltRootAgentAndOrphanEnqueueRetainsBinding
// verifies that BindCoordinatorBatchRuntimeBindings propagates the disk
// capability manager onto the ALREADY-BUILT root session agent (the agent is
// constructed before the batch runtime binds the manager), so the durable
// orphan-enqueue producer (restartOrphanedWithRetry) can still hold a
// row-keyed Continuation owner.
//
// WHY IT FAILS ON OLD CODE: with the propagation reverted, diskCapsRef()
// returns nil, holdDurableContinuationOwner(nil, ...) returns ok=false,
// restartOrphanedWithRetry returns the typed "binding already lost" error and
// no owner is acquired — require.NoError and the owners assertions both fail.
func TestBindCoordinatorBatchRuntimeBindings_BindsBuiltRootAgentAndOrphanEnqueueRetainsBinding(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)
	coord.currentAgent = testSessionAgent(env, &mockModel{}, &mockModel{}, "")

	caps := newDiskCapabilityManager()
	provider := newFakeDiskProvider(nil)
	bindingID, err := caps.RegisterProvider(provider)
	require.NoError(t, err)

	// BindCoordinatorBatchRuntimeBindings mutates the package-global
	// batchLeafKindOverride (batch_install_ingress.go tail); save and
	// restore it so the test does not leak the override elsewhere.
	prevKindOverride := batchLeafKindOverride.Load()
	t.Cleanup(func() { batchLeafKindOverride.Store(prevKindOverride) })
	require.NoError(t, BindCoordinatorBatchRuntimeBindings(coord, &fakeBatchIngress{}, nil, caps))

	// Propagation: the root agent now sees the SAME manager instance.
	sa, ok := coord.currentAgent.(*sessionAgent)
	require.True(t, ok)
	if sa.diskCapsRef() != caps {
		t.Fatal("root session agent did not receive the batch-installed disk capability manager")
	}

	// The orphan enqueue through the ORIGINAL root agent must retain a
	// row-keyed continuation holder while the durable row waits.
	sess, err := env.sessions.Create(context.Background(), "root-diskcaps")
	require.NoError(t, err)
	call := SessionAgentCall{
		SessionID:       sess.ID,
		LogicalCallID:   "orphan-1",
		Prompt:          "recover me",
		CallOptions:     &CallOptions{DiskBindingID: bindingID, DiskProvider: provider},
		DiskRequirement: &session.SessionExecutionRequirement{BindingID: bindingID},
	}
	require.NoError(t, sa.restartOrphanedWithRetry([]SessionAgentCall{call}))
	owners, _, _ := caps.OwnerDiagnostics()
	require.Equal(t, 1, owners, "the row-keyed continuation holder must be retained while the durable row waits")

	// Release with the idempotency key the producer builds:
	// fmt.Sprintf("%s-%s", call.SessionID, call.LogicalCallID).
	releaseDurableContinuationOwner(caps, bindingID, sess.ID+"-orphan-1")
	owners, _, _ = caps.OwnerDiagnostics()
	require.Equal(t, 0, owners)
}
