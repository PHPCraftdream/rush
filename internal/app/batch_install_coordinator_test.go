// R11F (M3) app-side wiring tests: the exported one-authority ingress seam
// exists only on the flag-on path and fails closed before any store call.

package app

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

func TestBatchWorkIngressFlagOffNil(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	// Flag off: the install guard in New never fires, so no ingress seam is
	// ever built (the M2 golden test pins the same for the runtime object).
	a := batchFactoryApp(t)
	require.Nil(t, a.batchWorkIngress, "flag off: no ingress seam")
}

func TestBatchWorkIngressFlagOnInstalled(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	a := batchFactoryApp(t)
	require.NoError(t, a.installBatchRuntime(context.Background()))
	ingress := a.batchWorkIngress
	require.NotNil(t, ingress, "flag on: the one-authority seam is installed")
	t.Cleanup(func() {
		if a.batchRuntime != nil {
			a.batchRuntime.Stop()
		}
		_ = a.asyncJobStore.Close(context.Background())
	})

	// Fail-closed validation fires BEFORE any store or engine call: an
	// empty invocation is refused by the typed invocation validation.
	_, err := ingress.Invoke(context.Background(), agent.BatchInvocation{}, nil, fantasy.ToolCall{})
	require.Error(t, err)
}
