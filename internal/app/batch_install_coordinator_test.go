// R11F (M3) app-side wiring tests: the exported one-authority ingress seam
// is built by the unconditional persisted install, and it fails closed
// before any store call. In library mode (SkipAgentSetup, no data dir) New
// builds no seam at all.

package app

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

func TestBatchWorkIngressLibraryModeNil(t *testing.T) {
	// R11F round-2 P1-7 item 3: build the App through the REAL New entry
	// point with SkipAgentSetup, so the assertion is about the installed
	// system: in library mode the install guard in New never fires and no
	// ingress seam is ever built.
	app := newFullNewAppSkipAgentSetup(t)
	require.Nil(t, app.batchWorkIngress, "library mode: New never builds the one-authority ingress seam")
	require.Nil(t, app.batchRuntime, "library mode: no batch runtime object either")
}

func TestBatchWorkIngressInstalled(t *testing.T) {
	a := batchFactoryApp(t)
	require.NoError(t, a.installBatchRuntime(context.Background()))
	ingress := a.batchWorkIngress
	require.NotNil(t, ingress, "the one-authority seam is installed")
	t.Cleanup(func() {
		if a.batchRuntime != nil {
			// Close drains the factory-built real Runner; Stop alone leaves
			// the engine goroutines running.
			if r := a.batchRuntime.Runner(); r != nil {
				_ = r.Close(context.Background())
			}
			a.batchRuntime.Stop()
		}
	})
	// installBatchRuntime ensured the HostStore; release its lock here
	// (this t.Cleanup runs after the one above, so LIFO order is fine).
	t.Cleanup(func() { _ = a.batchHostRelease(context.Background()) })

	// Fail-closed validation fires BEFORE any store or engine call: an
	// empty invocation is refused by the typed invocation validation.
	_, err := ingress.Invoke(context.Background(), agent.BatchInvocation{}, nil, fantasy.ToolCall{})
	require.Error(t, err)
}
