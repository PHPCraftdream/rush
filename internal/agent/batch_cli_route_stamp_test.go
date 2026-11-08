package agent

// The batch CLI intake is the trusted translator for both the served HTTP
// batch intake and the standalone unattended CLI intake. Only the intake —
// never the external spec — decides the answer-route capability: the external
// BatchCLITask parser never sees a question-route field, so the stamp in the
// leaf payload can only come from the builder's own decision. These tests pin
// both directions of that decision.
//
// Production entry points exercised:
//   - NewBatchCLIBuilderWithQuestionRoute (batch_cli.go)
//   - TranslateSpec / translate: the batchLeafPayload stamp.
//
// Oracles: route=false produces a task payload WITHOUT the question_route
// key; route=true produces one WITH question_route == true.
//
// Fixture rules: the same coordinator setup as the bind-gate test — real
// coordinator, no batch actually executed.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func routeStampIngressSetup(t *testing.T) (*coordinator, *batchWorkIngress) {
	t.Helper()
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coder, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok)
	coder.AllowedTools = []string{"view"}
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
	return coord, ingress
}

func routeStampTranslate(t *testing.T, coord *coordinator, ingress *batchWorkIngress, questionRoute bool) []byte {
	t.Helper()
	builder, err := NewBatchCLIBuilderWithQuestionRoute(coord, ingress, questionRoute)
	require.NoError(t, err)
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Items: []jobbatch.Spec{{Task: &jobbatch.TaskSpec{Payload: json.RawMessage(`{"tool":"view","input":{}}`)}}}}}
	translated, release, err := builder.TranslateSpec(context.Background(), "owner", "message", spec)
	require.NoError(t, err)
	require.NotNil(t, translated.Batch)
	if release != nil {
		release()
	}
	require.Len(t, translated.Batch.Items, 1)
	return []byte(translated.Batch.Items[0].Task.Payload)
}

func TestBatchCLIBuilderUnattendedStampOmitsQuestionRoute(t *testing.T) {
	coord, ingress := routeStampIngressSetup(t)
	payload := routeStampTranslate(t, coord, ingress, false)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.NotContains(t, decoded, "question_route")
}

func TestBatchCLIBuilderServedStampSetsQuestionRoute(t *testing.T) {
	coord, ingress := routeStampIngressSetup(t)
	payload := routeStampTranslate(t, coord, ingress, true)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	route, ok := decoded["question_route"].(bool)
	require.True(t, ok, "question_route key must be present")
	require.True(t, route)
}
