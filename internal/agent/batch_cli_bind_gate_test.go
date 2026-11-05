package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// A batch submit builds its toolset, sub-agent included, with the submitting
// request's context. The sub-agent's asynchronous prompt build must not land on
// the coordinator's shared ready gate: a build that fails because that request
// ended leaves a sticky context.Canceled there, and every later run that waits
// on the gate (runSubAgent among them) then fails with it.
func TestBatchCLIBindKeepsRequestScopedBuildsOffTheSharedGate(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coder, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok)
	coder.AllowedTools = []string{"view", "agent"}
	coord.cfg.Config().Agents[config.AgentCoder] = coder
	caps := NewDiskProviderRegistry()
	registries := NewBatchLeafRegistryStore()
	caps.setRegistryDrop(registries.Delete)
	ingress := &batchWorkIngress{registries: registries, diskBindings: caps}
	prevProcess := processBatchBinding.Load()
	t.Cleanup(func() {
		coordinatorBatchBindings.Delete(coord)
		processBatchBinding.Store(prevProcess)
	})
	require.NoError(t, BindCoordinatorBatchRuntimeBindings(coord, ingress, nil, caps))
	builder, err := NewBatchCLIBuilder(coord, ingress)
	require.NoError(t, err)
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Items: []jobbatch.Spec{{Task: &jobbatch.TaskSpec{Payload: json.RawMessage(`{"tool":"view","input":{}}`)}}}}}

	// The request is already over when its builds run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, release, _ := builder.TranslateSpec(ctx, "owner", "message", spec)
	if release != nil {
		release()
	}

	require.NoError(t, coord.readyWg.Wait(), "a request-scoped build left its error on the shared ready gate")
}
