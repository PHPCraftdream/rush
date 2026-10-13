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
	// R11F round-2 P1-7 item 3: build the App through the REAL New entry
	// point (no SkipAgentSetup, no hand-assembled &App{}), so the assertion
	// is about the installed system: with the flag off the install guard in
	// New never fires and no ingress seam is ever built.
	app := newFullNewApp(t)
	require.Nil(t, app.batchWorkIngress, "flag off: New never builds the one-authority ingress seam")
	require.Nil(t, app.batchRuntime, "flag off: no batch runtime object either")
}

func TestBatchWorkIngressFlagOnInstalled(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	a := batchFactoryApp(t)
	require.NoError(t, a.installBatchRuntime(context.Background()))
	ingress := a.batchWorkIngress
	require.NotNil(t, ingress, "flag on: the one-authority seam is installed")
	t.Cleanup(func() {
		if a.batchRuntime != nil {
			// Close drains the factory-built real Runner; Stop alone leaves
			// the engine goroutines running.
			if r := a.batchRuntime.Runner(); r != nil {
				_ = r.Close(context.Background())
			}
			a.batchRuntime.Stop()
		}
		_ = a.asyncJobStore.Close(context.Background())
	})

	// Fail-closed validation fires BEFORE any store or engine call: an
	// empty invocation is refused by the typed invocation validation.
	_, err := ingress.Invoke(context.Background(), agent.BatchInvocation{}, nil, fantasy.ToolCall{})
	require.Error(t, err)
}
