package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestGuardTurnDiskFence_NormalizedOSRequiresExplicitIntent(t *testing.T) {
	ctx := context.Background()
	store := &fakeFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(ctx, "s-os-intent",
		session.SessionExecutionRequirement{BindingID: "dead-binding"}))
	coord := newContinuationTestCoordinator(store, newDiskCapabilityManager())
	normalized := &CallOptions{DiskProvider: tools.OSDisk()}

	err := coord.guardTurnDiskFence(ctx, "s-os-intent", normalized)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable,
		"an omitted provider normalized to OSDisk must not supersede or bypass a persistent fence")

	explicit := WithExecutionProviderIntent(ctx, "s-os-intent", ExecutionProviderOS)
	err = coord.guardTurnDiskFence(explicit, "s-os-intent", normalized)
	require.NoError(t, err,
		"explicit OSDiskProvider intent is authorized to establish a new epoch")
}
